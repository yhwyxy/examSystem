// 管理端密码: bcrypt hash 存 app_settings["admin.password"].
//
// 优先级: DB bcrypt hash > config.yaml admin.password 明文.
// 首改密码时若无 DB hash, 需校验旧密码为 config 明文 (兼容现有部署);
// 写成功后在 DB 留下 hash, 后续校验一律走 DB, config 明文成为兜底/仅首改.
package settings

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

// adminPasswordKey 是 app_settings 中管理密码的 key.
const adminPasswordKey = "admin.password"

// PasswordHashDoc 是 app_settings["admin.password"] 的 value 结构.
type PasswordHashDoc struct {
	Hash      string `json:"hash"`
	Algorithm string `json:"algorithm,omitempty"` // 固定 "bcrypt"
	UpdatedAt string `json:"updated_at,omitempty"`
}

// MinPasswordLen 是管理端密码最小长度 (内部系统, 6 位起).
const MinPasswordLen = 6

// ErrWeakPassword 表示新密码不满足强度要求.
var ErrWeakPassword = errors.New("新密码过短 (至少 6 位)")

// ErrOldPasswordMismatch 表示旧密码校验失败.
var ErrOldPasswordMismatch = errors.New("旧密码错误")

// ErrPasswordUnset 表示当前未配置可校验的密码来源.
var ErrPasswordUnset = errors.New("未配置管理端密码")

// LoadPasswordHash 读 DB 中 bcrypt hash. 未设置返回 nil,nil.
func (s *Store) LoadPasswordHash(ctx context.Context) (*PasswordHashDoc, error) {
	raw, err := s.GetRaw(ctx, adminPasswordKey)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	var doc PasswordHashDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("settings: parse admin.password: %w", err)
	}
	if doc.Hash == "" {
		return nil, nil
	}
	return &doc, nil
}

// SavePasswordHash 写 bcrypt hash 到 DB.
func (s *Store) SavePasswordHash(ctx context.Context, plain string) error {
	if len(plain) < MinPasswordLen {
		return ErrWeakPassword
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(plain), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("settings: bcrypt: %w", err)
	}
	doc := PasswordHashDoc{Hash: string(hash), Algorithm: "bcrypt"}
	raw, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	return s.SetRaw(ctx, adminPasswordKey, raw)
}

// HashPassword 只计算 hash (供纯函数测试 / 预置).
func HashPassword(plain string) (string, error) {
	if len(plain) < MinPasswordLen {
		return "", ErrWeakPassword
	}
	b, err := bcrypt.GenerateFromPassword([]byte(plain), bcrypt.DefaultCost)
	return string(b), err
}

// NormalizePasswordInput 去掉首尾空白并拒绝空串.
func NormalizePasswordInput(p string) (string, error) {
	p = strings.TrimSpace(p)
	if len(p) < MinPasswordLen {
		return "", ErrWeakPassword
	}
	return p, nil
}
