// Package jobs 复刻 Python insert_grading_job: 在 submit 路径上将一条
// grading_jobs 行入队, 供评分 worker (Task 7) 用 FOR UPDATE SKIP LOCKED
// 消费.
//
// 设计要点 (Task 6):
//   - repository.go: 纯 PG 层, 单方法 EnqueueTx 在调用方 tx 内 INSERT
//                  grading_jobs (status='queued', generation>=1)
//   - 所有写入由调用方包事务, Repository 不开/提交事务, 保证与 submissions
//                  INSERT 同一原子单元 (plan Step 2 "短事务" 要求)
package jobs

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ErrInvalidGeneration 表示 generation <= 0, 违反 grading_jobs CHECK 约束.
var ErrInvalidGeneration = errors.New("INVALID_GENERATION")

// JobSpec 是入队所需的最小字段集合 (与 grading_jobs 列对齐).
type JobSpec struct {
	SubmissionID int64
	PaperID      string
	RunID        string
	Generation   int64
}

// Repository 是 grading_jobs 的持久层. 零状态, 通过 pgx.Tx 暴露操作.
type Repository struct{}

// NewRepository 返回零状态实例.
func NewRepository() *Repository { return &Repository{} }

// EnqueueTx 在调用方事务内 INSERT 一条 grading_jobs (status='queued').
// generation 必须 >= 1; 否则返回 ErrInvalidGeneration. available_at 默认 now().
func (r *Repository) EnqueueTx(ctx context.Context, tx pgx.Tx, spec JobSpec) error {
	if spec.Generation < 1 {
		return fmt.Errorf("%w: generation=%d", ErrInvalidGeneration, spec.Generation)
	}
	const sql = `
INSERT INTO grading_jobs
    (submission_id, paper_id, run_id, generation, status, attempts, max_attempts,
     available_at, created_at, updated_at)
VALUES ($1, $2, $3, $4, 'queued', 0, 5, $5, $5, $5)`
	now := time.Now().UTC()
	if _, err := tx.Exec(ctx, sql,
		spec.SubmissionID, spec.PaperID, spec.RunID, spec.Generation, now,
	); err != nil {
		return fmt.Errorf("jobs.EnqueueTx: %w", err)
	}
	return nil
}
