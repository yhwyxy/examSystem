// Package settings 管理端设置: 存于 DB app_settings 表 (key/value jsonb).
//
// 两个用途:
//   - admin.password: 管理端密码 bcrypt hash. 优先于 config.yaml 明文密码,
//     admin UI "更改管理员密码" 写入此处; 未设置时回退 config admin.password (明文).
//   - scoring: 评分方式选择 (local / remote_reranker / llm) + API 凭据.
//     worker 启动/轮询时从同一张表读取并据此构建评分引擎, 无需重启.
package settings

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound 表示 key 不存在.
var ErrNotFound = fmt.Errorf("settings: key not found")

// Store 是 app_settings 表的读写入口.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore 构造 settings.Store (依赖 DB pool).
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// GetRaw 读 key 的原始 jsonb 字节. 不存在返回 ErrNotFound.
func (s *Store) GetRaw(ctx context.Context, key string) ([]byte, error) {
	var raw []byte
	err := s.pool.QueryRow(ctx,
		`SELECT value FROM app_settings WHERE key = $1`, key).Scan(&raw)
	if err != nil {
		if isNoRows(err) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return raw, nil
}

// SetRaw 写 key (upsert). value 须为合法 JSON.
func (s *Store) SetRaw(ctx context.Context, key string, value []byte) error {
	if !json.Valid(value) {
		return fmt.Errorf("settings: invalid json for key %s", key)
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO app_settings (key, value, updated_at)
		VALUES ($1, $2::jsonb, now())
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now()`,
		key, value)
	return err
}

// Delete 删 key (幂等).
func (s *Store) Delete(ctx context.Context, key string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM app_settings WHERE key = $1`, key)
	return err
}

func isNoRows(err error) bool {
	if err == nil {
		return false
	}
	// pgx.ErrNoRows / "no rows in result set"
	return err.Error() == "no rows in result set"
}
