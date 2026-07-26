// Package runs 复刻 Python backend.exam_run_service 的 open_run / begin_close /
// get_public_exam 路径, 由 Go + PostgreSQL 实现.
//
// 设计要点 (Task 5):
//   - repository.go: 纯 PG 层 CRUD with advisory locks (per-slug)
//   - service.go: 业务流 -- Open/BeginClose/GetPublicExam, 调 papers.Store +
//     SHA256 token hash + crypto/rand 32-byte URL-safe token
//   - service_test.go: TDD test 覆盖状态机/竞态 (Step 1 11 类 case)
//
// 安全: token 用 crypto/rand 32 字节 + base64url 编码, SHA256 存储;
// 校验时 constant-time compare.
package runs

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// RunStatus 是 exam_runs.status 状态枚举.
type RunStatus string

const (
	StatusOpen    RunStatus = "open"
	StatusClosing RunStatus = "closing"
	StatusClosed  RunStatus = "closed"
)

// Run 是 exam_runs 一行的领域模型 (与 schema 字段名对齐).
type Run struct {
	ID               string
	PaperID          string
	RoundNo          int
	PublicTokenHash  string
	Status           RunStatus
	DurationMinutes  int
	SnapshotPath     string
	SnapshotHash     string
	IsLegacy         bool
	OpenedAt         time.Time
	ClosingStartedAt *time.Time
	FinalizeAt       *time.Time
	ClosedAt         *time.Time
	CreatedAt        time.Time
}

// Repository 是 exam_runs 的持久层. 通过 pgx.Tx 暴露操作, 由调用方包事务.
type Repository struct{}

// NewRepository 返回零状态实例 (无连接池, 全 stateless).
func NewRepository() *Repository { return &Repository{} }

// ErrActiveRunExists 表示同 paper 已有 open/closing run, 部分唯一索引冲突.
var ErrActiveRunExists = errors.New("ACTIVE_RUN_EXISTS")

// ErrRunNotFound 表示按 ID 或 token_hash 查无 run.
var ErrRunNotFound = errors.New("RUN_NOT_FOUND")

// AdvisoryLockKey 用于 per-paper 串行化 Open (避免同时两个 round 即将 INSERT 时
// active unique index 没冲突但 round_no 取相同时被撞).
//
// 选 paper slug 的 32-bit hash; 空 slug 时用 0.
func advisoryLockKeyByPaper(slug string) int64 {
	if slug == "" {
		return 0
	}
	h := sha256.Sum256([]byte(slug))
	// 取前 4 字节作 32-bit unsigned, 后转 int64
	return int64(uint32(h[0])<<24 | uint32(h[1])<<16 | uint32(h[2])<<8 | uint32(h[3]))
}

// MaxRoundForPaper 返回某 paper_id 当前的最大 round_no; 无 run 则 0.
func (r *Repository) MaxRoundForPaper(ctx context.Context, tx pgx.Tx, paperID string) (int, error) {
	var maxRound int
	err := tx.QueryRow(ctx,
		"SELECT COALESCE(max(round_no), 0) FROM exam_runs WHERE paper_id = $1",
		paperID).Scan(&maxRound)
	if err != nil {
		return 0, fmt.Errorf("runs.MaxRoundForPaper: %w", err)
	}
	return maxRound, nil
}

// Open 创建一个新 run: status=open, round_no = max(paper_round)+1, 写入新 token.
// 调用方负责传入 id (高熵 UUID/slug) + sha256(token) 作为 PublicTokenHash.
//
// 若 paper 已存在 open/closing 状态的 run, INSERT 会撞 uq_exam_runs_active ->
// 返 ErrActiveRunExists (PostgreSQL 错误码 23505).
func (r *Repository) Open(ctx context.Context, tx pgx.Tx,
	id, paperID, snapshotPath, snapshotHash, publicTokenHash string,
	durationMinutes int) (*Run, error) {
	// 先 per-paper advisory lock 串行化, 防止 round_no 竞态
	if _, err := tx.Exec(ctx,
		"SELECT pg_advisory_xact_lock($1)", advisoryLockKeyByPaper(paperID)); err != nil {
		return nil, fmt.Errorf("runs.Open: advisory lock: %w", err)
	}
	maxRound, err := r.MaxRoundForPaper(ctx, tx, paperID)
	if err != nil {
		return nil, err
	}
	roundNo := maxRound + 1
	now := time.Now().UTC()
	var run Run
	err = tx.QueryRow(ctx, `
		INSERT INTO exam_runs (
			id, paper_id, round_no, public_token_hash, status,
			duration_minutes, snapshot_path, snapshot_hash, is_legacy,
			opened_at, created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,false,$9,$9)
		RETURNING id, paper_id, round_no, public_token_hash, status,
		          duration_minutes, snapshot_path, snapshot_hash, is_legacy,
		          opened_at, closing_started_at, finalize_at, closed_at, created_at
	`,
		id, paperID, roundNo, publicTokenHash, string(StatusOpen),
		durationMinutes, snapshotPath, snapshotHash,
		now,
	).Scan(&run.ID, &run.PaperID, &run.RoundNo, &run.PublicTokenHash, &run.Status,
		&run.DurationMinutes, &run.SnapshotPath, &run.SnapshotHash, &run.IsLegacy,
		&run.OpenedAt, &run.ClosingStartedAt, &run.FinalizeAt, &run.ClosedAt, &run.CreatedAt)
	if err != nil {
		// 23505 unique_violation -> ACTIVE_RUN_EXISTS
		if isUniqueViolation(err) {
			return nil, ErrActiveRunExists
		}
		return nil, fmt.Errorf("runs.Open insert: %w", err)
	}
	return &run, nil
}

// isUniqueViolation 检 pgx 错误是否 PostgreSQL unique_violation (23505).
// 用 errors.As 而非裸类型断言, 以穿透 pgconn.PgError 在 Scan/Exec 路径中可能的 wrap.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "23505"
	}
	return false
}

// scanRun 把 SELECT 标准列序读到 Run 结构.
func scanRun(row pgx.Row) (*Run, error) {
	run := &Run{}
	err := row.Scan(
		&run.ID, &run.PaperID, &run.RoundNo, &run.PublicTokenHash, &run.Status,
		&run.DurationMinutes, &run.SnapshotPath, &run.SnapshotHash, &run.IsLegacy,
		&run.OpenedAt, &run.ClosingStartedAt, &run.FinalizeAt, &run.ClosedAt, &run.CreatedAt,
	)
	if err != nil {
		return nil, err
	}
	return run, nil
}

// runColumns 是 SELECT 的固定列序, 与 scanRun 一致.
const runColumns = "id, paper_id, round_no, public_token_hash, status, " +
	"duration_minutes, snapshot_path, snapshot_hash, is_legacy, " +
	"opened_at, closing_started_at, finalize_at, closed_at, created_at"

// FindByPublicTokenHash 按 SHA256(public_token) 查 run. 无 -> ErrRunNotFound.
//
// 安全: 调用方传入 *sha256* hash, 不能传明文 token; repository 不再做 hash.
func (r *Repository) FindByPublicTokenHash(ctx context.Context, tx pgx.Tx,
	publicTokenHash string) (*Run, error) {
	row := tx.QueryRow(ctx,
		"SELECT "+runColumns+" FROM exam_runs WHERE public_token_hash = $1",
		publicTokenHash)
	run, err := scanRun(row)
	if err != nil {
		if isNoRows(err) {
			return nil, ErrRunNotFound
		}
		return nil, fmt.Errorf("runs.FindByToken: %w", err)
	}
	return run, nil
}

// FindByID 按 id 查 run. 无 -> ErrRunNotFound.
func (r *Repository) FindByID(ctx context.Context, tx pgx.Tx, id string) (*Run, error) {
	row := tx.QueryRow(ctx,
		"SELECT "+runColumns+" FROM exam_runs WHERE id = $1", id)
	run, err := scanRun(row)
	if err != nil {
		if isNoRows(err) {
			return nil, ErrRunNotFound
		}
		return nil, fmt.Errorf("runs.FindByID: %w", err)
	}
	return run, nil
}

// FindDueClosingTx 列出所有 "closing" 状态且已到 FinalizeAt 的 run id (FOR UPDATE SKIP LOCKED).
// 调用方在事务内完成 finalize 后 commit/rollback. 锁仅 lock 这一 SELECT row.
func (r *Repository) FindDueClosingTx(ctx context.Context, tx pgx.Tx) ([]string, error) {
	rows, err := tx.Query(ctx, `
SELECT id FROM exam_runs
WHERE status = $1 AND finalize_at IS NOT NULL AND finalize_at <= now()
ORDER BY finalize_at, id
FOR UPDATE SKIP LOCKED
LIMIT 32`, string(StatusClosing))
	if err != nil {
		return nil, fmt.Errorf("runs.FindDueClosingTx query: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if cerr := rows.Scan(&id); cerr != nil {
			return nil, fmt.Errorf("runs.FindDueClosingTx scan: %w", cerr)
		}
		out = append(out, id)
	}
	if cerr := rows.Err(); cerr != nil {
		return nil, fmt.Errorf("runs.FindDueClosingTx rows.Err: %w", cerr)
	}
	return out, nil
}

// isNoRows 检 pgx.ErrNoRows.
func isNoRows(err error) bool {
	return errors.Is(err, pgx.ErrNoRows)
}

// ErrAlreadyClosed 表示已事务 attempt close 但已是 closed 不可逆.
var ErrAlreadyClosed = errors.New("RUN_ALREADY_CLOSED")

// BeginClose 把 run 从 open 切到 closing; finalize_at 设为 now + grace.
// 已 closing/closed 都刷新 finalize_at, 但状态不回退; 已 closed 返 ErrAlreadyClosed.
func (r *Repository) BeginClose(ctx context.Context, tx pgx.Tx,
	runID string, graceDuration time.Duration) (*Run, error) {
	// 单 UPDATE: open -> closing; closing 维持 closing+刷新 finalize_at;
	// closed 触发 cardinality 0 (operator 已 CLOSED)
	now := time.Now().UTC()
	finalizeAt := now.Add(graceDuration)
	_, err := tx.Exec(ctx, `
		UPDATE exam_runs
		SET status = 'closing',
		    finalize_at = $2,
		    closing_started_at = COALESCE(closing_started_at, $3)
		WHERE id = $1 AND status = 'open'
	`, runID, finalizeAt, now)
	if err != nil {
		return nil, fmt.Errorf("runs.BeginClose: %w", err)
	}
	// 再统一 finalizeAt 字段同步(已 closing 也刷一次 -- 与 Python 等价)
	_, _ = tx.Exec(ctx, "UPDATE exam_runs SET finalize_at = $2 WHERE id = $1 AND status='closing'",
		runID, finalizeAt)
	// 重新读出当前最新状态
	run, err := r.FindByID(ctx, tx, runID)
	if err != nil {
		return nil, err
	}
	if run.Status == StatusClosed {
		return nil, ErrAlreadyClosed
	}
	return run, nil
}

// FindOpenByPaper 按 paper_id 查最近一条 open 状态 run (UI exam-link 用).
// 设计前提: 每 paper 同时最多 1 个 open run (uq_exam_runs_active 保证), 故 LIMIT 1.
// 无 open run -> 返 (nil, nil).
func (r *Repository) FindOpenByPaper(ctx context.Context, tx pgx.Tx, paperID string) (*Run, error) {
	row := tx.QueryRow(ctx, `
		SELECT id, paper_id, round_no, public_token_hash, status,
		       duration_minutes, snapshot_path, snapshot_hash, is_legacy,
		       opened_at, closing_started_at, finalize_at, closed_at, created_at
		FROM exam_runs
		WHERE paper_id = $1 AND status = 'open'
		LIMIT 1`, paperID)
	run, err := scanRun(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("runs.FindOpenByPaper: %w", err)
	}
	return run, nil
}
