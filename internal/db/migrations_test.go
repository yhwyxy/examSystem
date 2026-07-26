package db

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yhwyxy/examSystem/internal/testutil"
)

// TestMigrate_Fresh 首次应用所有迁移文件, 应返回所有版本号.
func TestMigrate_Fresh(t *testing.T) {
	pool, cleanup := testutil.NewSchema(t)
	defer cleanup()
	defer pool.Close()

	res, err := Migrate(context.Background(), pool)
	if err != nil {
		t.Fatalf("Migrate fresh: %v", err)
	}
	if len(res.Applied) == 0 {
		t.Fatalf("Expected at least 1 applied migration, got 0")
	}
	if len(res.Skipped) != 0 {
		t.Errorf("Fresh migrate should not skip any, got %d skipped: %v",
			len(res.Skipped), res.Skipped)
	}
	// Applied 必含 0001_initial
	has0001 := false
	for _, v := range res.Applied {
		if v == "0001_initial" {
			has0001 = true
		}
	}
	if !has0001 {
		t.Errorf("0001_initial not applied, applied=%v", res.Applied)
	}
}

// TestMigrate_Idempotent 再跑一次 Migrate 应无新应用、全部 skipped.
func TestMigrate_Idempotent(t *testing.T) {
	pool, cleanup := testutil.NewSchema(t)
	defer cleanup()
	defer pool.Close()

	ctx := context.Background()
	if _, err := Migrate(ctx, pool); err != nil {
		t.Fatalf("first Migrate: %v", err)
	}
	res, err := Migrate(ctx, pool)
	if err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	if len(res.Applied) != 0 {
		t.Errorf("idempotent: expected 0 new applied, got %v", res.Applied)
	}
	if len(res.Skipped) == 0 {
		t.Errorf("idempotent: expected skipped > 0")
	}
}

// TestMigrate_ChecksumMismatch 改 schema_migrations.checksum 后再 Migrate
// 必须以 ErrChecksumMismatch 失败.
func TestMigrate_ChecksumMismatch(t *testing.T) {
	pool, cleanup := testutil.NewSchema(t)
	defer cleanup()
	defer pool.Close()

	ctx := context.Background()
	if _, err := Migrate(ctx, pool); err != nil {
		t.Fatalf("first Migrate: %v", err)
	}

	// 篡改 schema_migrations.checksum
	if _, err := pool.Exec(ctx, fmt.Sprintf(
		`UPDATE %s SET checksum='deadbeef' WHERE version='0001_initial';`,
		schemaMigrationsTable)); err != nil {
		t.Fatalf("tamper checksum: %v", err)
	}

	_, err := Migrate(ctx, pool)
	if err == nil {
		t.Fatalf("expected ErrChecksumMismatch, got nil")
	}
	if !strings.Contains(err.Error(), "checksum mismatch") {
		t.Errorf("error should mention checksum mismatch, got: %v", err)
	}
}

// TestMigrate_SchemaIsolation 各独立 schema 的 Migrate 互不影响.
func TestMigrate_SchemaIsolation(t *testing.T) {
	pool1, cleanup1 := testutil.NewSchema(t)
	defer cleanup1()
	pool2, cleanup2 := testutil.NewSchema(t)
	defer cleanup2()
	defer pool1.Close()
	defer pool2.Close()

	ctx := context.Background()
	if _, err := Migrate(ctx, pool1); err != nil {
		t.Fatalf("pool1 Migrate: %v", err)
	}
	if _, err := Migrate(ctx, pool2); err != nil {
		t.Fatalf("pool2 Migrate: %v", err)
	}
	// 两个 schema 各自有自己的 schema_migrations 行
	var n1, n2 int
	if err := pool1.QueryRow(ctx, fmt.Sprintf(
		`SELECT count(*) FROM %s;`, schemaMigrationsTable)).Scan(&n1); err != nil {
		t.Fatalf("pool1 count: %v", err)
	}
	if err := pool2.QueryRow(ctx, fmt.Sprintf(
		`SELECT count(*) FROM %s;`, schemaMigrationsTable)).Scan(&n2); err != nil {
		t.Fatalf("pool2 count: %v", err)
	}
	if n1 == 0 || n2 == 0 {
		t.Errorf("both schemas should have rows, got n1=%d n2=%d", n1, n2)
	}
}

// TestMigrate_AdvisoryLockConcurrent 并发两个 Migrate 只有一个能跑过,
// 第二个应阻塞直到第一个完成后跳过相同版本(不会出错).
func TestMigrate_AdvisoryLockConcurrent(t *testing.T) {
	pool, cleanup := testutil.NewSchema(t)
	defer cleanup()
	defer pool.Close()

	ctx := context.Background()
	// 第一次确保 schema_migrations 表存在
	if _, err := Migrate(ctx, pool); err != nil {
		t.Fatalf("first Migrate: %v", err)
	}

	// 重新创建两个独立 pool 连同一个 schema, 模拟并发 migrator 进程
	// (这里直接复用同一个 pool, advisory lock 串行化内部所有 tx)
	var wg sync.WaitGroup
	var err1, err2 error
	var done1, done2 bool

	wg.Add(2)
	go func() {
		defer wg.Done()
		_, err := Migrate(ctx, pool)
		if err != nil {
			err1 = err
			return
		}
		done1 = true
	}()
	go func() {
		defer wg.Done()
		// 给一点点延迟, 确保第一个先抢到锁
		time.Sleep(50 * time.Millisecond)
		_, err := Migrate(ctx, pool)
		if err != nil {
			err2 = err
			return
		}
		done2 = true
	}()
	wg.Wait()

	if err1 != nil {
		t.Errorf("concurrent migrator 1 returned err: %v", err1)
	}
	if err2 != nil {
		t.Errorf("concurrent migrator 2 returned err: %v", err2)
	}
	if !done1 || !done2 {
		t.Errorf("both migrators should complete; done1=%v done2=%v", done1, done2)
	}
}
