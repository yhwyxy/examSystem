// Package httpapi - ratelimit.go
// 把 internal/ratelimit 的 token bucket 接到热点路由上:
//   - POST /api/admin/login            10/min burst 3  (key=IP; 防口令暴破)
//   - POST /api/submit /session/submit 300/min burst 30 (key=IP; NAT 教室场景下
//     多考生共享出口 IP, 不能用 5/min 级别的 per-IP 限制)
//   - POST /api/exam/start             300/min burst 30 (key=IP; 同上)
//   - PUT/GET /api/exam/sessions/{id}/draft|status 60/min burst 10 (key=session id;
//     每会话独立配额, 与前端 5s 自动保存/轮询节奏匹配)
//
// Limiter 为 nil (测试构造 Dependencies 未注入) 时全部直通.
package httpapi

import (
	"net"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/yhwyxy/examSystem/internal/ratelimit"
)

// rateLimitByIP 按 "class:clientIP" 限流包装 handler.
func rateLimitByIP(l *ratelimit.Limiter, p ratelimit.Preset, class string,
	next http.HandlerFunc) http.HandlerFunc {
	return rateLimitBy(l, p, func(r *http.Request) string {
		return class + ":" + clientIPKey(r)
	}, next)
}

// rateLimitBySessionID 按 "class:{id path param}" 限流 (draft/status 每会话配额).
func rateLimitBySessionID(l *ratelimit.Limiter, p ratelimit.Preset, class string,
	next http.HandlerFunc) http.HandlerFunc {
	return rateLimitBy(l, p, func(r *http.Request) string {
		id := chi.URLParam(r, "id")
		if id == "" {
			id = clientIPKey(r)
		}
		return class + ":" + id
	}, next)
}

// rateLimitBy 通用包装: keyFn 出 key, 拒绝时回 429 + Retry-After.
func rateLimitBy(l *ratelimit.Limiter, p ratelimit.Preset,
	keyFn func(*http.Request) string, next http.HandlerFunc) http.HandlerFunc {
	if l == nil {
		return next
	}
	return func(w http.ResponseWriter, r *http.Request) {
		d := l.Allow(keyFn(r), p)
		if !d.Allowed {
			secs := int(d.RetryAfter.Seconds()) + 1
			w.Header().Set("Retry-After", strconv.Itoa(secs))
			writeAdminError(w, http.StatusTooManyRequests, "RATE_LIMITED",
				"too many requests; retry after "+strconv.Itoa(secs)+"s")
			return
		}
		next(w, r)
	}
}

// clientIPKey 取 RemoteAddr host 部分 (RealIP middleware 已把 X-Forwarded-For
// 归一进 RemoteAddr).
func clientIPKey(r *http.Request) string {
	if h, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return h
	}
	return r.RemoteAddr
}
