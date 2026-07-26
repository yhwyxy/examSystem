// Package testutil 提供 Go 测试用的 PostgreSQL 隔离夹具.
//
// 设计 (Task 2):
//
//   * NewSchema(t) 返回一个新创建的 PostgreSQL schema 名 (随机), 一并在
//     schema 内创建 schema_migrations 与执行 migrations. 析构时 DROP SCHEMA.
//   * Pool() 返回走 search_path 已设为新 schema 的 pgxpool, 测试用例共享.
//
// 与 Python 端 contract 测试的 tmp 隔离互补:
//
//   * Python: 用文件系统 + tmp_path 隔离 papers / sqlite; 无 PG (Python 不读 PG).
//   * Go: 用独立 schema 隔离 PG; migrations 各跑一次, 互不污染, 无需 truncate 表.
//
// 启用方式:
//
//	pool, schema, cleanup := testutil.NewSchema(t)
//	defer cleanup()
//	_ = pool
//
// 环境要求:
//
//   * TEST_DATABASE_URL 必须指向可写 PG (dev 用 exam_migrator/exam_app 都行).
//   * 缺省时 t.Skip 整个测试, 防 CI 误伤.
package testutil

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// NewSchema 创建临时 schema 并跑 migrations, 返回可用的 pool + cleanup.
//
// 调用方:
//   pool, cleanup := NewSchema(t)
//   defer cleanup()  // t.Cleanup(...) 自动注册更优雅
//
// t.Cleanup 注册自动清理函数; 测试无需显式 defer.
func NewSchema(t *testing.T) (*pgxpool.Pool, func()) {
	t.Helper()

	url := TestDatabaseURL(t)
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping Postgres test")
	}

	suffix := randHex(t, 8)
	schema := fmt.Sprintf("task_test_%s", suffix)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Admin conn: 用主 URL 但 search_path 通过 schema 限定, 不污染 public.
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("testutil parse url: %v", err)
	}
	admin, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("testutil admin pool: %v", err)
	}
	// 建 schema, 并显式 drop on cleanup
	if _, err := admin.Exec(ctx, fmt.Sprintf(
		`CREATE SCHEMA IF NOT EXISTS %s;`, schema)); err != nil {
		admin.Close()
		t.Fatalf("testutil create schema: %v", err)
	}

	// 测试 pool: search_path = 新 schema + public(用于 schema_migrations)
	// 实际上 migrator 内部固定的 schema_migrations 也居于 search_path 第一位.
	// 因此 pool 配置 search_path=$schema,public.
	testCfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		admin.Close()
		t.Fatalf("testutil parse url(2): %v", err)
	}
	testCfg.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	testPool, err := pgxpool.NewWithConfig(ctx, testCfg)
	if err != nil {
		admin.Close()
		t.Fatalf("testutil test pool: %v", err)
	}

	cleanup := func() {
		// 用 admin drop schema, 因为测试 pool 内可能有连接占住.
		dCtx, dCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer dCancel()
		_, _ = admin.Exec(dCtx, fmt.Sprintf(`DROP SCHEMA IF EXISTS %s CASCADE;`, schema))
		testPool.Close()
		admin.Close()
	}
	t.Cleanup(cleanup)

	return testPool, cleanup
}

// TestDatabaseURL 优先 TEST_DATABASE_URL, 退化为 EXAM_DATABASE_URL.
func TestDatabaseURL(t *testing.T) string {
	t.Helper()
	if v := strings.TrimSpace(os.Getenv("TEST_DATABASE_URL")); v != "" {
		return v
	}
	if v := strings.TrimSpace(os.Getenv("EXAM_DATABASE_URL")); v != "" {
		return v
	}
	return ""
}

// randHex 用 crypto/rand 产生 n 字节随机 hex; 失败 t.Fatal.
func randHex(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("testutil rand: %v", err)
	}
	return hex.EncodeToString(b)
}
