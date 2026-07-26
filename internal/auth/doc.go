// Package auth 实现 Task 9 admin 鉴权:
//
// 单 HTTP token (24h 过期); constant-time compare; 不在日志记 password/token.
// Store: 内存 map[token]=expiry + RWMutex; Login(password) (token, error);
// RequireAdmin middleware 从 Authorization Bearer 取 token 校验有效期+格式.
// enable_auth=false: RequireAdmin 直接放行 (与 Python 基线 parity).
package auth
