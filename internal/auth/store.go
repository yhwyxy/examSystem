package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"sync"
	"time"
)

// 错误语义 (HTTP 层映射).
var (
	ErrAuthDisabled       = errors.New("auth: disabled")
	ErrInvalidCredentials = errors.New("auth: invalid credentials")
	ErrInvalidToken       = errors.New("auth: invalid token")
	ErrTokenExpired       = errors.New("auth: token expired")
)

// tokenTTL 单 token 有效期. 与 Python 基线 86400s 一致 (plan Step 1).
const tokenTTL = 24 * time.Hour

// Store 线程安全 token 仓库. 单进程; 多实例时需要换 Redis 等共享存储.
type Store struct {
	mu       sync.RWMutex
	tokens   map[string]time.Time          // token -> expiry
	secretFn func() (string, error)        // admin password secret loader
	enabled  bool
}

// NewStore 构造一个 token 内存 store. enable=false 时 RequireAdmin 放行.
// secretFn 返回 admin password 明文 (HTTP 层用 Login(password) 校验).
func NewStore(enable bool, secretFn func() (string, error)) *Store {
	return &Store{
		tokens:   make(map[string]time.Time),
		secretFn: secretFn,
		enabled:  enable,
	}
}

// Enabled 返回是否启用 admin auth.
func (s *Store) Enabled() bool {
	if s == nil {
		return false
	}
	return s.enabled
}

// Login 校验 admin password (constant-time). 成功生成新 token (32 字节 URL-safe)
// 写入 store (expiry = now + tokenTTL) + 返回 token.
func (s *Store) Login(password string) (string, error) {
	if s == nil || !s.enabled {
		return "", ErrAuthDisabled
	}
	if s.secretFn == nil {
		return "", ErrInvalidCredentials
	}
	secret, err := s.secretFn()
	if err != nil {
		return "", ErrInvalidCredentials
	}
	if subtle.ConstantTimeCompare([]byte(secret), []byte(password)) != 1 {
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
