package ratelimit

import (
	"sync/atomic"
	"testing"
	"time"
)

// newFakeClock 起始于 2026-01-01 UTC (固定时刻便于断言).
func newFakeClock() *fakeClock {
	f := &fakeClock{}
	f.now.Store(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).UnixNano())
	return f
}

// TestAllowBurst 验 burst 上限:Burst=2 的 PresetSuccessive 第 3 次必拒.
func TestAllowBurst(t *testing.T) {
	f := newFakeClock()
	l := newLimiterWith(1000, time.Hour, time.Hour, f.Now)
	defer l.Stop()
	p := PresetSubmit // Burst=2, RatePerMin=5
	for i := 0; i < 2; i++ {
		if d := l.Allow("k", p); !d.Allowed {
			t.Fatalf("call %d want allowed, got denied (rem=%v)", i, d.Remaining)
		}
	}
	d := l.Allow("k", p)
	if d.Allowed {
		t.Fatalf("3rd call want denied, got allowed")
	}
	if d.RetryAfter <= 0 {
		t.Errorf("RetryAfter = %v, want >0", d.RetryAfter)
	}
}

// TestRefill: 用完 Burst=2 后 advance 时钟 13 秒 (rate=5/60≈0.0833/s,
// 13 秒应恢复 >1 token), 第 4 次 Allow 应再次成功.
func TestRefill(t *testing.T) {
	f := newFakeClock()
	l := newLimiterWith(1000, time.Hour, time.Hour, f.Now)
	defer l.Stop()
	p := PresetSubmit
	for i := 0; i < 2; i++ {
		l.Allow("k1", p)
	}
	if d := l.Allow("k1", p); d.Allowed {
		t.Fatalf("3rd ought be denied (got allowed)")
	}
	f.Advance(13 * time.Second)
	d2 := l.Allow("k1", p)
	if !d2.Allowed {
		t.Fatalf("after refill want allowed; got denied (rem=%v retry=%v)",
			d2.Remaining, d2.RetryAfter)
	}
}

// fakeClock 是注入的时钟, 测试里 Advance 把 now 往前推.
type fakeClock struct {
	now atomic.Int64 // unix nano
}

func (f *fakeClock) Now() time.Time {
	return time.Unix(0, f.now.Load())
}
func (f *fakeClock) Advance(d time.Duration) {
	f.now.Add(int64(d))
}

// TestSweepExpire: 创建 1 bucket, 推 ttl+1m, 手动 sweepExpired, 应清空.
func TestSweepExpire(t *testing.T) {
	f := newFakeClock()
	l := newLimiterWith(1000, time.Minute, time.Hour, f.Now)
	defer l.Stop()
	l.Allow("k-sweep", PresetDraftStatus)
	if got := l.bucketsApprox(); got != 1 {
		t.Fatalf("before sweep = %d, want 1", got)
	}
	f.Advance(time.Minute + time.Second)
	l.sweepExpired()
	if got := l.bucketsApprox(); got != 0 {
		t.Fatalf("after sweep = %d, want 0", got)
	}
}

// TestKeysCap 后推超 keysCap 上限触发 enforceMaxKeys, 应截到 keysCap.
func TestKeysCap(t *testing.T) {
	f := newFakeClock()
	l := newLimiterWith(3, time.Hour, time.Hour, f.Now)
	defer l.Stop()
	for i := 0; i < 5; i++ {
		key := string(rune('a' + (byte(i))))
		l.Allow(key, PresetDraftStatus)
		// 推 1ns 让每条 lastTouched 互不相同 (LRU 可解 tie)
		f.Advance(time.Nanosecond)
	}
	if got, want := l.bucketsApprox(), 3; got != want {
		t.Fatalf("after enforceMaxKeys = %d, want %d", got, want)
	}
}
