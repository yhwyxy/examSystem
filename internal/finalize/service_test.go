package finalize_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yhwyxy/examSystem/internal/db"
	"github.com/yhwyxy/examSystem/internal/finalize"
	"github.com/yhwyxy/examSystem/internal/runs"
	"github.com/yhwyxy/examSystem/internal/sessions"
	"github.com/yhwyxy/examSystem/internal/submissions"
	"github.com/yhwyxy/examSystem/internal/testutil"
)

func skipIfNoPG(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if os.Getenv("TEST_DATABASE_URL") == "" && os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL / TEST_DATABASE_URL 未设置")
	}
	pool, cleanup := testutil.NewSchema(t)
	t.Cleanup(cleanup) // Go 1.15+ testutil cleanup
	if _, err := db.Migrate(context.Background(), pool); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return pool
}

// TestFinalizeRun_ClosesRunAndAutoSubmits 集成测试:
//   - 构造 closing run + 2 active sessions (含 draft_json 含客观题答案)
//   - 调 FinalizeRun -> 期望 run status=closed, 两 sessions 被自动提交为 submissions (~graded)
// 注: 这是 plan Step 1 "故障注入 + 收卷完整测试".
func TestFinalizeRun_ClosesRunAndAutoSubmits(t *testing.T) {
	pool := skipIfNoPG(t)
	ctx := context.Background()
	now := time.Now().UTC()

	// 插入 closing run + paper snapshot fixture (复用 tests/fixtures/contract/paper.json).
	_, err := pool.Exec(ctx, `
INSERT INTO exam_runs (id, paper_id, public_token_hash, status, snapshot_path,
                       snapshot_hash, round_no, duration_minutes, opened_at,
                       finalize_at, closing_started_at, created_at)
VALUES ('run-fin', 'paper-fin', 'phtok-fin', 'closing', '/tmp/x.json',
        'sha-fin', 1, 60, $1, $2, $2, $1)`, now, now.Add(-time.Second))
	if err != nil {
		t.Fatalf("insert exam_runs: %v", err)
	}
	// 两 active sessions 各存不同 draft answer
	_, err = pool.Exec(ctx, `
INSERT INTO exam_sessions (id, run_id, employee_id, name, department, session_token_hash,
                           started_at, deadline_at, status, draft_json, draft_revision,
                           created_at, updated_at)
VALUES ('sess-fin-1', 'run-fin', 'emp1', 'tester1', 'dept', 'h-tok-1',
        $1, $2, 'active', '{"q_single":"A"}', 5, $1, $1),
       ('sess-fin-2', 'run-fin', 'emp2', 'tester2', 'dept', 'h-tok-2',
        $1, $2, 'active', '{"q_single":"B"}', 3, $1, $1)`, now,
		now.Add(time.Minute))
	if err != nil {
		t.Fatalf("insert exam_sessions: %v", err)
	}
	runsRepo := runs.NewRepository()
	sessRepo := sessions.NewRepository()
	subRepo := submissions.NewRepository()
	finalizeSvc := finalize.NewService(pool, runsRepo, sessRepo, subRepo, 30)
	nS, closed, ferr := finalizeSvc.FinalizeRun(ctx, "run-fin")
	if ferr != nil {
		t.Fatalf("FinalizeRun: %v", ferr)
	}
	if !closed {
		t.Fatalf("FinalizeRun returned closed=false")
	}
	if nS != 2 {
		t.Errorf("auto-submitted sessions = %d, want 2", nS)
	}
	var runStatus string
	err = pool.QueryRow(ctx, `SELECT status FROM exam_runs WHERE id=$1`, "run-fin").Scan(&runStatus)
	if err != nil {
		t.Fatalf("query run status: %v", err)
	}
	if runStatus != "closed" {
		t.Errorf("run status = %q, want 'closed'", runStatus)
	}
	var nSub int
	err = pool.QueryRow(ctx, `SELECT count(*) FROM submissions WHERE run_id=$1`, "run-fin").Scan(&nSub)
	if err != nil {
		t.Fatalf("count submissions: %v", err)
	}
	if nSub != 2 {
		t.Errorf("submissions count = %d, want 2", nSub)
	}
	// 校验 auto_submit_reason 字段 = admin_closed
	var reasons []string
	rows, err := pool.Query(ctx, `SELECT auto_submit_reason FROM submissions
		WHERE run_id=$1 AND employee_id IN ('emp1','emp2')`, "run-fin")
	if err != nil {
		t.Fatalf("query reasons: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var r string
		_ = rows.Scan(&r)
		reasons = append(reasons, r)
	}
	if len(reasons) != 2 {
		t.Errorf("got %d reasons, want 2", len(reasons))
	}
	for _, r := range reasons {
		if r != "admin_closed" {
			t.Errorf("auto_submit_reason = %q, want 'admin_closed'", r)
		}
	}
}
