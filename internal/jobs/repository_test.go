// Package jobs - repository_test.go
// 集成测试: 用 testutil.NewSchema + Migrate + 真表验证 EnqueueTx 行为.
package jobs

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yhwyxy/examSystem/internal/db"
	"github.com/yhwyxy/examSystem/internal/testutil"
)

// setupPG: 创建 schema + migrate + 插入 minimal exam_runs + submissions row,
// 返回 pool + 一个 submission id + cleanup.
func setupPG(t *testing.T) (*pgxpool.Pool, int64, func()) {
	t.Helper()
	pool, schemaCleanup := testutil.NewSchema(t)
	ctx := context.Background()
	if _, err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	// exam_runs 一行: id / paper_id / status / snapshot_path / snapshot_hash
	now := time.Now().UTC()
	_, err := pool.Exec(ctx, `INSERT INTO exam_runs
		(id, paper_id, public_token_hash, status, snapshot_path, snapshot_hash,
		 round_no, duration_minutes, opened_at, created_at)
		VALUES ('run-jobs-test', 'paper-jobs', 'phtokhash-jobs', 'open',
		        '/tmp/x.json', 'abc', 1, 60, $1, $1)`, now)
	if err != nil {
		t.Fatalf("insert exam_runs: %v", err)
	}
	// submissions 一行 (与 Python insert_submission_pending 同列).
	var subID int64
	err = pool.QueryRow(ctx, `INSERT INTO submissions
		(name, employee_id, paper_id, run_id, answers_json,
		 objective_score, grading_detail_json, grading_status, review_status,
		 grading_generation, submitted_at)
		VALUES ('tester', 'emp1', 'paper-jobs', 'run-jobs-test', '{}',
		        0, '[]', 'pending', 'grading', 0, $1)
		RETURNING id`, now).Scan(&subID)
	if err != nil {
		t.Fatalf("insert submissions: %v", err)
	}
	return pool, subID, schemaCleanup
}

// TestEnqueueTx_InsertsQueuedRow 在真表上验证 EnqueueTx 写 grading_jobs 行
// 字段: status='queued', generation=1, attempts=0, max_attempts=5, submission_id 同传.
func TestEnqueueTx_InsertsQueuedRow(t *testing.T) {
	pool, subID, cleanup := setupPG(t)
	defer cleanup()

	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	repo := NewRepository()
	if err := repo.EnqueueTx(ctx, tx, JobSpec{
		SubmissionID: subID,
		PaperID:      "paper-jobs",
		RunID:        "run-jobs-test",
		Generation:   1,
	}); err != nil {
		t.Fatalf("EnqueueTx: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	var (
		status          string
		attempts        int
		maxAttempts     int
		gen             int64
		dbSubmissionID  int64
	)
	err = pool.QueryRow(ctx, `SELECT status, attempts, max_attempts, generation, submission_id
		FROM grading_jobs WHERE submission_id = $1 ORDER BY id DESC LIMIT 1`, subID).Scan(
		&status, &attempts, &maxAttempts, &gen, &dbSubmissionID)
	if err != nil {
		t.Fatalf("query grading_jobs: %v", err)
	}
	if status != "queued" {
		t.Errorf("status = %q, want 'queued'", status)
	}
	if attempts != 0 {
		t.Errorf("attempts = %d, want 0", attempts)
	}
	if maxAttempts != 5 {
		t.Errorf("max_attempts = %d, want 5", maxAttempts)
	}
	if gen != 1 {
		t.Errorf("generation = %d, want 1", gen)
	}
	if dbSubmissionID != subID {
		t.Errorf("submission_id mismatch %d != %d", dbSubmissionID, subID)
	}
}

// TestEnqueueTx_RejectsGenerationZero 验证 Go 层 (不需 PG) generation<1 报错.
// gen<1 在 SQL Exec 之前被拒绝, 不需要真实事务, ctx 用 background 即可.
func TestEnqueueTx_RejectsGenerationZero(t *testing.T) {
	repo := NewRepository()
	err := repo.EnqueueTx(context.Background(), nil, JobSpec{
		PaperID:    "paper-jobs",
		RunID:      "run-jobs-test",
		Generation: 0,
	})
	if err == nil {
		t.Fatalf("expected ErrInvalidGeneration for gen=0")
	}
	if !errors.Is(err, ErrInvalidGeneration) {
		t.Fatalf("err kind mismatch, got %v want ErrInvalidGeneration", err)
	}
}

// TestService_EnqueueTx_AcceptsJSONPayload 验证 Service.EnqueueTx
// (sessions.JobEnqueuer 兼容接口) 接收 string subID + JSON payload 入队成功.
func TestService_EnqueueTx_AcceptsJSONPayload(t *testing.T) {
	pool, subID, cleanup := setupPG(t)
	defer cleanup()

	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	svc := NewService(nil)
	payload := []byte(`{"paper_id":"paper-jobs","run_id":"run-jobs-test","generation":1}`)
	if err := svc.EnqueueTx(ctx, tx, strconv.FormatInt(subID, 10), payload); err != nil {
		t.Fatalf("Service.EnqueueTx: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	var gen int64
	err = pool.QueryRow(ctx, `SELECT generation FROM grading_jobs
		WHERE submission_id = $1 ORDER BY id DESC LIMIT 1`, subID).Scan(&gen)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if gen != 1 {
		t.Errorf("gen = %d, want 1", gen)
	}
}

var _ = strconv.Itoa
