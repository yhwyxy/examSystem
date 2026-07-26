// Package httpapi 实现 examSystem 的 HTTP 路由与中间件。
//
// Task 1 仅建立可测试启动的框架: /api/health + 静态资源 + reload-config。
// 后续 Task 在此基础上叠加业务路由 (papers / sessions / submissions / admin / stats)。
package httpapi

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
)

// JSONError 是统一的 API 错误响应包围。
type JSONError struct {
	Error   string `json:"error"`
	Message string `json:"message,omitempty"`
}

// WriteJSON 写一个带 Content-Type: application/json 的响应。
// status 是 HTTP 状态码;  body 任意会被 json.Marshal。
func WriteJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// WriteError 写一个 {error, message} 响应。
func WriteError(w http.ResponseWriter, status int, errCode, message string) {
	WriteJSON(w, status, JSONError{Error: errCode, Message: message})
}

// NotFoundJSON 是 /api/* 未知路由的 JSON 404 处理器。
// 静态未知路径不能 fallback 使用本 handler (避免 leak exam.html)。
func NotFoundJSON(w http.ResponseWriter, r *http.Request) {
	WriteError(w, http.StatusNotFound, "not_found", "API endpoint not found")
}

// MethodNotAllowedJSON 是 /api/* 不允许方法时返回的 JSON 405。
func MethodNotAllowedJSON(w http.ResponseWriter, r *http.Request) {
	WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Method not allowed")
}

// MountAPI404 把 /api/ 路径下的 404 / 405 处理器挂载到 router。
// 业务路由由各 Task 阶段挂载; 未挂的 /api 路径必须返回 JSON 404, 不可回退到首页。
func MountAPI404(r chi.Router) {
	r.NotFound(NotFoundJSON)
	r.MethodNotAllowed(MethodNotAllowedJSON)
}
