package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
)

// ctxKey 是 *Store上下文 value 的 key 类型 (避免冲突).
type ctxKey int

const adminTokenKey ctxKey = 1

// RequireAdmin 是 chi 路由中间件: 校验 Authorization: Bearer <token>.
// enabled=false: 直接放行 (plan: 与 Python enable_auth=false parity).
// 失败时返回 401 {"error":{"code":"UNAUTHORIZED",...}}, 不泄露具体原因.
//
// 透传 admin token 到 ctx (仅 enabled 时), 便于 handler 引用 (可选).
func (s *Store) RequireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s == nil || !s.enabled {
			next.ServeHTTP(w, r)
			return
		}
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") {
			writeAuthError(w, http.StatusUnauthorized, "UNAUTHORIZED",
				"missing bearer token")
			return
		}
		tok := strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
		if err := s.VerifyToken(tok); err != nil {
			writeAuthError(w, http.StatusUnauthorized, "UNAUTHORIZED",
				"invalid or expired token")
			return
		}
		ctx := context.WithValue(r.Context(), adminTokenKey, tok)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// writeAuthError 与 httpapi.Error shape parity: {"error":{"code":..,"message":..}}.
func writeAuthError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"code": code, "message": msg},
	})
}
