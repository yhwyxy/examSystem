package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"sync"
	"time"
)

// 错误语义 (HTTP 层映射).
var (
	ErrAuthDisabled               = errors.New("auth: disabled")
	ErrInvalidCredentials         = errors.New("auth: invalid credentials")
	ErrInvalidToken               = errors.New("auth: invalid token")
	ErrTokenExpired               = errors.New("auth: token expired")
	ErrWeakPassword               = errors.New("auth: new password too weak")
	ErrPasswordStoreNotConfigured = errors.New("auth: password store not configured")
)

// tokenTTL 单 token 有效期. 与 Python 基线 86400s 一致 (plan Step 1).
const tokenTTL = 24 * time.Hour

// PasswordProvider 是 Store 校验/修改管理密码的外部依赖.
//   - Check: 校验密码是否匹配当前管理密码 (DB bcrypt hash 或 config 明文).
//   - Set:   以新密码覆盖旧密码 (写入 DB bcrypt hash).
//
// 为 nil 时 Login/ChangePassword 分别回退/报错 (见各自实现).
type PasswordProvider struct {
	Check func(ctx context.Context, password string) (bool, error)
	Set   func(ctx context.Context, newPassword string) error
}

// Store 线程安全 token 仓库. 单进程; 多实例时需要换 Redis 等共享存储.
type Store struct {
	mu       sync.RWMutex
	tokens   map[string]time.Time // token -> expiry
	enabled  bool
	pw       PasswordProvider       // nil = 未配置 (Login 全拒绝; 兼容旧 secretFn 迁移)
	secretFn func() (string, error) // 兼容旧构造: 仅当 pw.Check 未设置时用
}

// NewStore 构造一个 token 内存 store. enable=false 时 RequireAdmin 放行.
// secretFn 是旧式明文密码加载器 (config.yaml admin.password); 新代码优先用
// SetPasswordProvider 注入 PasswordProvider (DB bcrypt), 未注入时回退 secretFn.
func NewStore(enable bool, secretFn func() (string, error)) *Store {
	return &Store{
		tokens:   make(map[string]time.Time),
		secretFn: secretFn,
		enabled:  enable,
	}
}

// SetPasswordProvider 注入密码校验/修改回调 (DB bcrypt 优先于 secretFn).
func (s *Store) SetPasswordProvider(pw PasswordProvider) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pw = pw
}

// Enabled 返回是否启用 admin auth.
func (s *Store) Enabled() bool {
	if s == nil {
		return false
	}
	return s.enabled
}

// VerifyLegacyPassword 用 secretFn (config.yaml admin.password 明文) 校验密码.
// 供 settings 接线方在无 DB bcrypt hash 时兜底 (首个密码写入 DB 前的登录/首改).
func (s *Store) VerifyLegacyPassword(password string) (bool, error) {
	if s.secretFn == nil {
		return false, ErrInvalidCredentials
	}
	secret, err := s.secretFn()
	if err != nil {
		return false, ErrInvalidCredentials
	}
	return subtle.ConstantTimeCompare([]byte(secret), []byte(password)) == 1, nil
}

// checkPassword 按优先级校验: pw.Check (DB bcrypt) > secretFn (config 明文).
// 均未配置 -> 拒绝.
func (s *Store) checkPassword(ctx context.Context, password string) (bool, error) {
	if s.pw.Check != nil {
		return s.pw.Check(ctx, password)
	}
	if s.secretFn == nil {
		return false, ErrInvalidCredentials
	}
	secret, err := s.secretFn()
	if err != nil {
		return false, ErrInvalidCredentials
	}
	return subtle.ConstantTimeCompare([]byte(secret), []byte(password)) == 1, nil
}

// Login 校验 admin password (constant-time). 成功生成新 token (32 字节 URL-safe)
// 写入 store (expiry = now + tokenTTL) + 返回 token.
func (s *Store) Login(ctx context.Context, password string) (string, error) {
	if s == nil || !s.enabled {
		return "", ErrAuthDisabled
	}
	ok, err := s.checkPassword(ctx, password)
	if err != nil || !ok {
		return "", ErrInvalidCredentials
	}
	tok, err := randomToken()
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	s.tokens[tok] = time.Now().Add(tokenTTL)
	s.mu.Unlock()
	return tok, nil
}

// ChangePassword 校验旧密码 -> 强度校验 -> 写新密码 -> 使所有旧 token 失效.
// 密码变化后强制要求重新登录 (旧 token 全废).
func (s *Store) ChangePassword(ctx context.Context, oldPassword, newPassword string) error {
	if s == nil || !s.enabled {
		return ErrAuthDisabled
	}
	if s.pw.Set == nil {
		return ErrPasswordStoreNotConfigured
	}
	// 旧密码校验: 必须匹配当前密码 (防未授权改密)
	ok, err := s.checkPassword(ctx, oldPassword)
	if err != nil || !ok {
		return ErrInvalidCredentials
	}
	if len(newPassword) < 6 {
		return ErrWeakPassword
	}
	if subtle.ConstantTimeCompare([]byte(oldPassword), []byte(newPassword)) == 1 {
		return ErrWeakPassword // 新旧相同, 无意义
	}
	if err := s.pw.Set(ctx, newPassword); err != nil {
		return err
	}
	s.InvalidateAll()
	return nil
}

// InvalidateAll 清空全部已签发 token (改密/回收后调用).
func (s *Store) InvalidateAll() {
	s.mu.Lock()
	s.tokens = make(map[string]time.Time)
	s.mu.Unlock()
}

// VerifyToken 校验 token: 格式 (32 byte base64)、存在、未过期. 失败返回对应错误.
// 任一校验 false: 拒绝 (避免在日志透露具体是哪步出错).
func (s *Store) VerifyToken(token string) error {
	if s == nil || !s.enabled {
		return ErrAuthDisabled
	}
	if token == "" {
		return ErrInvalidToken
	}
	s.mu.RLock()
	expiry, ok := s.tokens[token]
	s.mu.RUnlock()
	if !ok {
		return ErrInvalidToken
	}
	if time.Now().After(expiry) {
		s.mu.Lock()
		delete(s.tokens, token)
		s.mu.Unlock()
		return ErrTokenExpired
	}
	return nil
}

// randomToken 生成 32 字节 URL-safe base64 (无 padding).
func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
