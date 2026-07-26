package db

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/yhwyxy/examSystem/migrations"
)

// schemaMigrationsTable 是 migrator 自己维护的版本表名.
//
// Task 2 规范要求:
//   schema_migrations(version text primary key, checksum text not null, applied_at timestamptz not null)
//
// 由 EnsureTable() 在首次 Migrate 调用前确保存在; 此表自身不算 migration 0001 内容,
// 因此不会出现在 schema_migrations 行里 (避免 bootstrap 表与表内自指).
const schemaMigrationsTable = "schema_migrations"

// ErrChecksumMismatch 表示 SQL 文件被修改后再次迁移失败.
// 测试通过修改 schema_migrations.checksum 来触发此错.
var ErrChecksumMismatch = errors.New("checksum mismatch: migration file has been modified")

// MigrationRecord 是 schema_migrations 内一行.
type MigrationRecord struct {
	Version   string
	Checksum  string
	AppliedAt time.Time
}

// Result 是一次 Migrate 调用的总结, 便于 CLI / 测试断言.
type Result struct {
	Applied []string // 这次新应用的版本
	Skipped []string // 这次跳过(已应用且 checksum 一致)
}

// EnsureSchemaMigrationsTable 创建 schema_migrations 表(若不存在).
// 在 advisory lock 内执行以保证幂等.
func ensureSchemaMigrationsTable(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, fmt.Sprintf(`
		CREATE TABLE IF NOT EXISTS %s (
			version    text PRIMARY KEY,
			checksum   text NOT NULL,
			applied_at timestamptz NOT NULL DEFAULT now()
		);`, schemaMigrationsTable))
	return err
}

// appliedVersions 返回当前 schema_migrations 的版本 -> checksum.
func appliedVersions(ctx context.Context, tx pgx.Tx) (map[string]string, error) {
	rows, err := tx.Query(ctx, fmt.Sprintf(
		`SELECT version, checksum FROM %s;`, schemaMigrationsTable))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var v, c string
		if err := rows.Scan(&v, &c); err != nil {
			return nil, err
		}
		out[v] = c
	}
	return out, rows.Err()
}

// Migrate 应用所有未应用的 migration 文件.
//
// 阻塞与互斥:
//   * pg_advisory_xact_lock 一个固定 lock key, 串行化并发 migrator.
//   * 锁随事务退出自动释放, 无需显式 unlock.
//
// 校验:
//   * 已应用版本 -> 比对 checksum, 不一致返 ErrChecksumMismatch.
//   * 未应用版本 -> 在独立事务中执行, 失败回滚并不写 schema_migrations.
//   * 同一 Migrate 调用内部多次 Begin/Commit, 每个文件一个事务.
//
// 返回 Result.Applied 按 version 升序; Skipped 同.
// 当 Result.Applied 为空时, 调用方可输出 "database schema is current".
func Migrate(ctx context.Context, target ExecCapable) (*Result, error) {
	files, err := migrations.All()
	if err != nil {
		return nil, fmt.Errorf("migrate plan: load files: %w", err)
	}
	if len(files) == 0 {
		return &Result{}, nil
	}
	// 升序应用
	sort.Slice(files, func(i, j int) bool {
		return files[i].Version < files[j].Version
	})

	// 先开一个 outer tx 拿 advisory lock + 确保 schema_migrations 表存在 +
	// 读已应用版本清单, 然后 COMMIT. 之后每个文件独立事务(避免大长事务).
	lockTx, err := target.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("migrate begin lock tx: %w", err)
	}
	// pg_advisory_xact_lock 是 int 试 key; 选固定 key 防止与他人冲突
	const advLockKey = 0x4558414d // 'EXAM' 的 32-bit hash
	if _, err := lockTx.Exec(ctx,
		"SELECT pg_advisory_xact_lock($1);", advLockKey); err != nil {
		_ = lockTx.Rollback(ctx)
		return nil, fmt.Errorf("migrate advisory lock: %w", err)
	}
	if err := ensureSchemaMigrationsTable(ctx, lockTx); err != nil {
		_ = lockTx.Rollback(ctx)
		return nil, fmt.Errorf("migrate ensure schema_migrations: %w", err)
	}
	versions, err := appliedVersions(ctx, lockTx)
	if err != nil {
		_ = lockTx.Rollback(ctx)
		return nil, fmt.Errorf("migrate read versions: %w", err)
	}
	if err := lockTx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("migrate commit versions-read tx: %w", err)
	}

	res := &Result{Applied: []string{}, Skipped: []string{}}
	for _, f := range files {
		// 已应用 -> 校验 checksum
		if cur, ok := versions[f.Version]; ok {
			if cur != f.Checksum {
				return res, fmt.Errorf("%w: version=%s expected=%s got=%s",
					ErrChecksumMismatch, f.Version, f.Checksum, cur)
			}
			res.Skipped = append(res.Skipped, f.Version)
			continue
		}
		// 未应用 -> 独立事务执行
		applyTx, err := target.Begin(ctx)
		if err != nil {
			return res, fmt.Errorf("migrate begin tx for %s: %w", f.Version, err)
		}
		if _, err := applyTx.Exec(ctx, f.Source); err != nil {
			_ = applyTx.Rollback(ctx)
			return res, fmt.Errorf("migrate exec %s: %w", f.Version, err)
		}
		// 写 version + checksum (在同一事务)
		if _, err := applyTx.Exec(ctx, fmt.Sprintf(
			`INSERT INTO %s (version, checksum) VALUES ($1, $2);`,
			schemaMigrationsTable), f.Version, f.Checksum); err != nil {
			_ = applyTx.Rollback(ctx)
			return res, fmt.Errorf("migrate insert version for %s: %w", f.Version, err)
		}
		if err := applyTx.Commit(ctx); err != nil {
			return res, fmt.Errorf("migrate commit %s: %w", f.Version, err)
		}
		res.Applied = append(res.Applied, f.Version)
	}

	return res, nil
}

// ExecCapable 是 Migrate 的最小依赖;
// 在生产用 *pgxpool.Pool 实现它, 测试可用任意 pgx.Tx 兼容的 stub.
//
//此处抽象让 migrator 测试可在 tx 层直接驱动, 或接 *pgxpool.Pool.
type ExecCapable interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}
