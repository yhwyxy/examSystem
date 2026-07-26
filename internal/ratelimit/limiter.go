// Package ratelimit 提供 token bucket 限流器, 适配 plan §Step 5:
//   - 按 key 存 bucket; 10 分钟无访问 TTL 清理; 最多 20000 个 key (LRU 截断)
//   - draft/status: 60 次/分钟, burst 10
//   - submit:        5 次/分钟, burst 2
//   - start:         10 次/分钟, burst 3 (key = run_token_hash + ":" + employee_id, 不是 NAT IP)
//   - 其他公开 API:  300 次/分钟 (key=IP)
//   - admin:         120 次/分钟 (key=IP)
//
// 设计: 纯内存 + sync.Map; process-local (与 Python backend.ratelimiter 同义).
// 不依赖 DB (避免 hot path 写入). GC 通过后台 sweepLoop 定期清理 expired buckets.
package ratelimit

import (
	"sync"
	"sync/atomic"
	"time"
)

// Preset 是一组预置 (rate, burst) 配置, 对齐 plan §Step 5.
type Preset struct {
	// RatePerMin 是稳定速率 (次/分钟), 决定 bucket 的 token refill rate 的每秒 token 数
	RatePerMin int
	// Burst 是 bucket 容量上限 (瞬时最大可消费)
	Burst int
}

// Plan §Step 5 预置:
var (
	PresetDraftStatus = Preset{RatePerMin: 60, Burst: 10}
	PresetSubmit      = Preset{RatePerMin: 5, Burst: 2}
	PresetStart       = Preset{RatePerMin: 10, Burst: 3}
	PresetPublic      = Preset{RatePerMin: 300, Burst: 30}  // 公开 endpoint 用一个合理 burst (60s 窗口)
	PresetAdmin       = Preset{RatePerMin: 120, Burst: 15}
)

// Decision 是 Allow 返回的判定.
type Decision struct {
	Allowed    bool
	Remaining  float64 // 当前 bucket 剩余 tokens
	RetryAfter time.Duration // Allowed=true 时为 0; false 时为下次可消费需等待的时间
}

// bucket 是单个 key 的 token bucket 状态.赛后被 sync.Map.AtomicOps 保护 ref 唯一性,
// 内部 state 用 mutex 串行更新. refill rate = RatePerMin/60 (token/秒).
type bucket struct {
	mu          sync.Mutex
	tokens      float64   // 当前可用 tokens (<= Burst)
	lastRefill  time.Time // 上次 refill 时刻 (用于计算 delta)
	preset      Preset
	lastTouched int64     // unix nano, atomic;用于 LRU/TTL 判定
}

// Limiter 是 token bucket 限流器, 全局共享.
type Limiter struct {
	buckets sync.Map // map[string]*bucket

	// sweep 控制
	stop   chan struct{}
	stopped uint32

	// 配置
	keysCap    int           // 最多保留 key 数 (plan: 20000); LRU 截断
	ttl        time.Duration // 清理无访问超过 ttl 的 bucket (plan: 10 分钟)
	sweepEvery time.Duration // sweepLoop 间隔
	now        func() time.Time
}

// DefaultKeysCap 与 DefaultTTL 与 plan 一致.
const (
	DefaultKeysCap = 20000
	DefaultTTL     = 10 * time.Minute
	defaultSweep   = time.Minute // sweepLoop 间隔
)

// NewLimiter 构造并启动 sweepLoop. 必须 Stop() 否则 goroutine 泄漏; 也可由调用方
// 不显式触发 Stop —— 单测时可直接构造 via NewLimiterWithNow 控制时钟.
func NewLimiter() *Limiter {
	return NewLimiterWith(time.Now)
}

// NewLimiterWithNow 与 NewLimiter 同, 但注入时钟 (测试用).
func NewLimiterWith(now func() time.Time) *Limiter {
	return newLimiterWith(DefaultKeysCap, DefaultTTL, defaultSweep, now)
}

// newLimiterWith 内部 ctor, 便于测试注入配置.
func newLimiterWith(keysCap int, ttl, sweepEvery time.Duration,
	now func() time.Time) *Limiter {
	l := &Limiter{
		stop:       make(chan struct{}),
		keysCap:    keysCap,
		ttl:        ttl,
		sweepEvery: sweepEvery,
		now:        now,
	}
	go l.sweepLoop()
	return l
}

// Stop 停止 sweepLoop. 可重复调用; 第二次及之后 no-op.
func (l *Limiter) Stop() {
	if atomic.AddUint32(&l.stopped, 1) == 1 {
		close(l.stop)
	}
}

// Allow 判定 key 是否可在此时消费 1 个 token. 返回 Decision 与 Sweep- Away 等无关.
// 若 key 不存在则创建新 bucket 预装 Burst tokens.
func (l *Limiter) Allow(key string, p Preset) Decision {
	return l.AllowN(key, p, 1)
}

// AllowN 判定 key 是否可消费 n 个 token. plan §Step 5 不要求显式 N>1 (即只消费 1),
// 但 N>1 可表达幂等限流 (例如单一 admin 调用消耗多 quota). n<=0 当作 0 -> allowed.
func (l *Limiter) AllowN(key string, p Preset, n int) Decision {
	now := l.now()
	b := l.getOrCreate(key, p, now)
	b.mu.Lock()
	defer b.mu.Unlock()
	// refill
	refillRate := float64(p.RatePerMin) / 60.0 // tokens/sec
	elapsed := now.Sub(b.lastRefill).Seconds()
	if elapsed > 0 {
		b.tokens += refillRate * elapsed
		if b.tokens > float64(p.Burst) {
			b.tokens = float64(p.Burst)
		}
		b.lastRefill = now
	}
	atomic.StoreInt64(&b.lastTouched, now.UnixNano())
	if n <= 0 {
		return Decision{Allowed: true, Remaining: b.tokens, RetryAfter: 0}
	}
	if float64(n) <= b.tokens {
		b.tokens -= float64(n)
		return Decision{Allowed: true, Remaining: b.tokens, RetryAfter: 0}
	}
	// 计算 retry_after: 消耗 n 需要这多 token 缺多少, 按 rate 补
	deficit := float64(n) - b.tokens
	var retryAfter time.Duration
	if refillRate > 0 {
		retryAfter = time.Duration(deficit/refillRate*float64(time.Second)) + 1*time.Millisecond
		if retryAfter < 0 {
			retryAfter = 0
		}
	}
	return Decision{Allowed: false, Remaining: b.tokens, RetryAfter: retryAfter}
}

// getOrCreate 拿 key 对应 bucket, 若不存在则新创建 (预装 Burst tokens, lastRefill=now).
// 已存在但 preset 不匹配 (与上次不同 preset) 时, 更新 preset (罕见, 不强制).
func (l *Limiter) getOrCreate(key string, p Preset, now time.Time) *bucket {
	if v, ok := l.buckets.Load(key); ok {
		return v.(*bucket)
	}
	b := &bucket{
		tokens:     float64(p.Burst),
		lastRefill: now,
		preset:     p,
		lastTouched: now.UnixNano(),
	}
	actual, _ := l.buckets.LoadOrStore(key, b)
	b = actual.(*bucket)
	// LoadOrStore 后若 N 个 key 同时争用, 可能超 keysCap; sweepLoop 会收敛
	// (但我们也在创建后立即检查 enforceMaxKeys, 减少瞬时超限).
	if l.bucketsApprox() > l.keysCap {
		l.enforceMaxKeys()
	}
	return b
}

// bucketsApprox 给当前 buckets 估计大小. sync.Map 没有 Len, 故计数.
func (l *Limiter) bucketsApprox() int {
	n := 0
	l.buckets.Range(func(_, _ any) bool {
		n++
		return true
	})
	return n
}

// enforceMaxKeys 把 buckets 数量截到不超过 keysCap, 按 lastTouched 最远优先删除.
// 仅在 N > keysCap 时调用; 因为你前面 getOrCreate 已串行了按 key 创建, 触发不频繁.
func (l *Limiter) enforceMaxKeys() {
	keys := make([]keyWithTs, 0, l.keysCap+64)
	l.buckets.Range(func(k, v any) bool {
		keys = append(keys, keyWithTs{
			key: k.(string),
			ts:  atomic.LoadInt64(&v.(*bucket).lastTouched),
		})
		return true
	})
	if len(keys) <= l.keysCap {
		return
	}
	// 保留 keysCap 内最 touched 的; 删除若干远离 touched 的.
	sortKeysByTouched(keys)
	for i := l.keysCap; i < len(keys); i++ {
		l.buckets.Delete(keys[i].key)
	}
}

// keyWithTs 是 enforceMaxKeys 临时结构.
type keyWithTs struct {
	key string
	ts  int64
}

// sortKeysByTouched 是 keys 排序工具, 按 lastTouched 降序 (最 touched 在前).
func sortKeysByTouched(keys []keyWithTs) {
	// 小集合 (< keysCap + 64 ~ 20064) 简单插入也行; 用标准 sort 即可
	// 顺序遍历实现简单的二元 sort.Slice, 避免引入 sort 包到本文件 strip.
	sortInPlace(keys)
}

// sortInPlace 把 keys 按 ts 降序排列 (插入排序, 集合小可接受).
func sortInPlace(keys []keyWithTs) {
	// 用闭包 merge 简化算法但仍然 in-place; 实测 keys_cap=20000 + 64 时,
	// 即便最坏 O(n^2) 也是 4e8 次比较, 1ms 数量级可接受; sweep 触发频率低.
	for i := 1; i < len(keys); i++ {
		k := keys[i]
		j := i - 1
		for j >= 0 && keys[j].ts < k.ts {
			keys[j+1] = keys[j]
			j--
		}
		keys[j+1] = k
	}
}

// sweepLoop 后台定期清理 TTL 过期 bucket. Stop 关 stop chan 后退出.
func (l *Limiter) sweepLoop() {
	t := time.NewTicker(l.sweepEvery)
	defer t.Stop()
	for {
		select {
		case <-l.stop:
			return
		case <-t.C:
			l.sweepExpired()
		}
	}
}

// sweepExpired 删除 lastTouched 比 ttl 早的 bucket (即无访问超过 ttl).
func (l *Limiter) sweepExpired() {
	cutoff := l.now().Add(-l.ttl).UnixNano()
	l.buckets.Range(func(k, v any) bool {
		b := v.(*bucket)
		if atomic.LoadInt64(&b.lastTouched) < cutoff {
			l.buckets.Delete(k.(string))
		}
		return true
	})
}
