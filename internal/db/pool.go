// Package db 提供 examSystem 的 PostgreSQL 连接池与版本化迁移.
//
// 设计 (Task 2):
//   - pool.go: Open(ctx, *config.Config) -> *pgxpool.Pool, 统一 connect_timeout /
//     statement_timeout / max_conns / min_conns 应用, 返回带强类型 Resource 清理.
//   - migrations.go: Migrate(ctx, pool) -> error, 用 pg_advisory_lock 守护跨进程
//     串行化, 每个迁移文件在独立事务中执行, checksum 写 schema_migrations.
//   - 重入 / 重跑: 已应用版本被跳过; 内容被修改则 checksum mismatch 返回错误.
//   - 测试隔离: internal/testutil/postgres.go 提供 random-schema setup, 每个测试
//     独立 schema, 互不污染.
package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yhwyxy/examSystem/internal/config"
)

// Open 创建配置就绪的 pgxpool.Pool.
//
// 解析 URL 时 pgxpool 内部已统一参数; 我们额外把 connect_timeout /
// statement_timeout 注入, 保证与 config.yaml 一致, 避免运行时 query 拖垮 DB.
func Open(ctx context.Context, cfg *config.Config) (*pgxpool.Pool, error) {
	if cfg == nil || cfg.Database.URL == "" {
		return nil, fmt.Errorf("db.Open: config.Database.URL is empty")
	}

	pcfg, err := pgxpool.ParseConfig(cfg.Database.URL)
	if err != nil {
		return nil, fmt.Errorf("db.Open parse url: %w", err)
	}

	// 连接级超时 / statement 超时
	if cfg.Database.MaxConns > 0 {
		pcfg.MaxConns = cfg.Database.MaxConns
	}
	if cfg.Database.MinConns >= 0 {
		pcfg.MinConns = cfg.Database.MinConns
	}
	if cfg.Database.ConnectTimeoutSeconds > 0 {
		pcfg.ConnConfig.ConnectTimeout = time.Duration(cfg.Database.ConnectTimeoutSeconds) * time.Second
	}

	pool, err := pgxpool.NewWithConfig(ctx, pcfg)
	if err != nil {
		return nil, fmt.Errorf("db.Open new pool: %w", err)
	}

	// statement_timeout 由 SET session 命令应用, 通过 AfterConnect 注入
	if cfg.Database.StatementTimeoutSeconds > 0 {
		stmt := fmt.Sprintf("SET statement_timeout = %d",
			cfg.Database.StatementTimeoutSeconds*1000)
		pcfg.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
			_, err := c.Exec(ctx, stmt)
			return err
		}
		// 重新构造 pool (因为 AfterConnect 必须在 New 之前设)
		pool.Close()
		pool, err = pgxpool.NewWithConfig(ctx, pcfg)
		if err != nil {
			return nil, fmt.Errorf("db.Open new pool (after connect hook): %w", err)
		}
	}

	// 探活: 启动时确保 DB 可达
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("db.Open ping: %w", err)
	}

	return pool, nil
}
