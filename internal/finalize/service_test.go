package finalize_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yhwyxy/examSystem/internal/db"
	"github.com/yhwyxy/examSystem/internal/finalize"
	"github.com/yhwyxy/examSystem/internal/jobs"
	"github.com/yhwyxy/examSystem/internal/objective"
	"github.com/yhwyxy/examSystem/internal/papers"
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
//
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
	var nJobs int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM grading_jobs WHERE run_id=$1 AND generation=1`, "run-fin").Scan(&nJobs); err != nil {
		t.Fatalf("count grading jobs: %v", err)
	}
	if nJobs != 2 {
		t.Errorf("grading jobs count = %d, want 2", nJobs)
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

func TestFinalizeRun_PureObjectiveDoesNotQueueGrading(t *testing.T) {
	pool := skipIfNoPG(t)
	ctx := context.Background()
	now := time.Now().UTC()
	doc := map[string]any{
		"paper_id":  "objective-finalize",
		"name":      "objective-finalize",
		"exam_info": map[string]any{"title": "Objective", "description": "Objective", "total_score": 10, "passing_score": 6},
		"questions": []any{map[string]any{
			"id": "q1", "type": "single_choice", "question": "Question", "score": 10,
			"options": []any{map[string]any{"key": "A", "text": "A"}, map[string]any{"key": "B", "text": "B"}},
			"answer":  "A",
		}},
	}
	path := filepath.Join(t.TempDir(), "objective.json")
	body, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatalf("write snapshot: %v", err)
	}

	hash := sha256.Sum256(body)
	if _, err := pool.Exec(ctx, `INSERT INTO exam_runs
		(id, paper_id, public_token_hash, status, snapshot_path, snapshot_hash, round_no, duration_minutes,
		 opened_at, finalize_at, closing_started_at, created_at)
		VALUES ('run-objective-fin', 'objective-finalize', 'tok', 'closing', $1, $3, 1, 60,
		 $2, $2, $2, $2)`, path, now, hex.EncodeToString(hash[:])); err != nil {
		t.Fatalf("insert run: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO exam_sessions
		(id, run_id, employee_id, name, session_token_hash, started_at, deadline_at,
		 status, draft_json, draft_revision, created_at, updated_at)
		VALUES ('sess-objective-fin', 'run-objective-fin', 'emp-objective', 'tester', 'tok',
		 $1, $2, 'active', '{"q1":"A"}', 1, $1, $1)`, now, now.Add(time.Minute)); err != nil {
		t.Fatalf("insert session: %v", err)
	}

	svc := finalize.NewService(pool, runs.NewRepository(), sessions.NewRepository(), submissions.NewRepository(), 30)
	if _, closed, err := svc.FinalizeRun(ctx, "run-objective-fin"); err != nil {
		t.Fatalf("FinalizeRun: %v", err)
	} else if !closed {
		t.Fatal("FinalizeRun returned closed=false")
	}
	var status, review string
	var score float64
	if err := pool.QueryRow(ctx, `SELECT grading_status, review_status, total_score
		FROM submissions WHERE run_id = 'run-objective-fin'`).Scan(&status, &review, &score); err != nil {
		t.Fatalf("query submission: %v", err)
	}
	if status != "done" || review != "auto_scored" || score != 10 {
		t.Errorf("submission = (%q, %q, %v), want (done, auto_scored, 10)", status, review, score)
	}
	var jobs int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM grading_jobs WHERE run_id = 'run-objective-fin'`).Scan(&jobs); err != nil {
		t.Fatalf("count grading jobs: %v", err)
	}
	if jobs != 0 {
		t.Errorf("grading jobs = %d, want 0", jobs)
	}
}

func TestScanExpiredSessionsSubmitsSavedDraftAndIsIdempotent(t *testing.T) {
	pool := skipIfNoPG(t)
	ctx := context.Background()
	now := time.Now().UTC()
	doc := map[string]any{
		"paper_id": "timed-exam",
		"name":     "Timed exam",
		"exam_info": map[string]any{
			"title": "Timed exam", "description": "Timed exam", "total_score": 10, "passing_score": 6,
		},
		"questions": []any{map[string]any{
			"id": "q1", "type": "single_choice", "question": "Question", "score": 10,
			"options": []any{map[string]any{"key": "A", "text": "A"}, map[string]any{"key": "B", "text": "B"}},
			"answer":  "A",
		}},
	}
	path := filepath.Join(t.TempDir(), "timed-exam.json")
	body, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatalf("write snapshot: %v", err)
	}
	hash := sha256.Sum256(body)
	if _, err := pool.Exec(ctx, `INSERT INTO exam_runs
		(id, paper_id, public_token_hash, status, snapshot_path, snapshot_hash, round_no, duration_minutes, opened_at, created_at)
		VALUES ('run-timed-exam', 'timed-exam', 'timed-token-hash', 'open', $1, $2, 1, 60, $3, $3)`,
		path, hex.EncodeToString(hash[:]), now.Add(-2*time.Hour)); err != nil {
		t.Fatalf("insert run: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO exam_sessions
		(id, run_id, employee_id, name, session_token_hash, started_at, deadline_at, status, draft_json, draft_revision, created_at, updated_at)
		VALUES
		 ('sess-timeout-due', 'run-timed-exam', 'emp-due', 'Due student', 'due-token-hash', $1, $2, 'active', '{"q1":"A"}', 3, $1, $1),
		 ('sess-timeout-grace', 'run-timed-exam', 'emp-grace', 'Grace student', 'grace-token-hash', $1, $3, 'active', '{"q1":"B"}', 1, $1, $1),
		 ('sess-timeout-active', 'run-timed-exam', 'emp-active', 'Active student', 'active-token-hash', $1, $4, 'active', '{"q1":"B"}', 1, $1, $1)`,
		now.Add(-time.Hour), now.Add(-2*time.Minute), now.Add(-10*time.Second), now.Add(time.Hour)); err != nil {
		t.Fatalf("insert sessions: %v", err)
	}

	svc := finalize.NewService(pool, runs.NewRepository(), sessions.NewRepository(), submissions.NewRepository(), 30)
	count, err := svc.ScanExpiredSessions(ctx)
	if err != nil {
		t.Fatalf("ScanExpiredSessions: %v", err)
	}
	if count != 1 {
		t.Fatalf("auto-submitted = %d, want 1", count)
	}
	var dueStatus, graceStatus, activeStatus string
	if err := pool.QueryRow(ctx, `SELECT status FROM exam_sessions WHERE id='sess-timeout-due'`).Scan(&dueStatus); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT status FROM exam_sessions WHERE id='sess-timeout-grace'`).Scan(&graceStatus); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT status FROM exam_sessions WHERE id='sess-timeout-active'`).Scan(&activeStatus); err != nil {
		t.Fatal(err)
	}
	if dueStatus != "submitted" || graceStatus != "active" || activeStatus != "active" {
		t.Fatalf("session statuses due/grace/active = %q/%q/%q", dueStatus, graceStatus, activeStatus)
	}
	var answer, reason string
	var score float64
	if err := pool.QueryRow(ctx, `SELECT answers_json->>'q1', auto_submit_reason, total_score FROM submissions WHERE run_id='run-timed-exam' AND employee_id='emp-due'`).Scan(&answer, &reason, &score); err != nil {
		t.Fatalf("query auto-submission: %v", err)
	}
	if answer != "A" || reason != "individual_timeout" || score != 10 {
		t.Fatalf("auto-submission answer/reason/score = %q/%q/%v", answer, reason, score)
	}
	count, err = svc.ScanExpiredSessions(ctx)
	if err != nil {
		t.Fatalf("repeat ScanExpiredSessions: %v", err)
	}
	if count != 0 {
		t.Fatalf("repeat auto-submitted = %d, want 0", count)
	}
	var submissionsCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM submissions WHERE run_id='run-timed-exam'`).Scan(&submissionsCount); err != nil {
		t.Fatal(err)
	}
	if submissionsCount != 1 {
		t.Fatalf("submissions = %d, want exactly 1", submissionsCount)
	}
}

func TestScanExpiredSessionsRacesManualSubmitWithoutDuplicate(t *testing.T) {
	pool := skipIfNoPG(t)
	ctx := context.Background()
	now := time.Now().UTC()
	doc := map[string]any{
		"paper_id": "timed-race",
		"name":     "Timed race",
		"exam_info": map[string]any{
			"title": "Timed race", "description": "Timed race", "total_score": 10, "passing_score": 6,
		},
		"questions": []any{map[string]any{
			"id": "q1", "type": "single_choice", "question": "Question", "score": 10,
			"options": []any{map[string]any{"key": "A", "text": "A"}, map[string]any{"key": "B", "text": "B"}},
			"answer":  "A",
		}},
	}
	path := filepath.Join(t.TempDir(), "timed-race.json")
	body, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatalf("write snapshot: %v", err)
	}
	hash := sha256.Sum256(body)
	if _, err := pool.Exec(ctx, `INSERT INTO exam_runs
		(id, paper_id, public_token_hash, status, snapshot_path, snapshot_hash, round_no, duration_minutes, opened_at, created_at)
		VALUES ('run-timed-race', 'timed-race', 'timed-race-token-hash', 'open', $1, $2, 1, 60, $3, $3)`,
		path, hex.EncodeToString(hash[:]), now.Add(-time.Hour)); err != nil {
		t.Fatalf("insert run: %v", err)
	}
	const token = "timed-race-session-token"
	tokenHash := sha256.Sum256([]byte(token))
	if _, err := pool.Exec(ctx, `INSERT INTO exam_sessions
		(id, run_id, employee_id, name, session_token_hash, started_at, deadline_at, status, draft_json, draft_revision, created_at, updated_at)
		VALUES ('sess-timed-race', 'run-timed-race', 'emp-race', 'Race student',
		 $1, $2, now() - interval '1 second', 'active', '{"q1":"A"}', 1, $2, $2)`,
		hex.EncodeToString(tokenHash[:]), now.Add(-time.Minute)); err != nil {
		t.Fatalf("insert session: %v", err)
	}

	finalizeSvc := finalize.NewService(pool, runs.NewRepository(), sessions.NewRepository(), submissions.NewRepository(), 0)
	submitSvc := submissions.NewService(pool, submissions.NewRepository(), papers.LoadSnapshot,
		func(q map[string]any, answer any, partial bool) (map[string]any, error) {
			return objective.Grade(q, answer, partial)
		}, jobs.NewService(nil), 30)
	start := make(chan struct{})
	type submitOutcome struct {
		result *submissions.SubmitResult
		err    error
	}
	manualDone := make(chan submitOutcome, 1)
	scanDone := make(chan struct {
		count int
		err   error
	}, 1)
	go func() {
		<-start
		answers := map[string]any{"q1": "A"}
		answerJSON, _ := json.Marshal(answers)
		result, err := submitSvc.Submit(ctx, submissions.SubmitRequest{
			SessionToken: token, Answers: answerJSON, AnswersMap: answers,
		})
		manualDone <- submitOutcome{result: result, err: err}
	}()
	go func() {
		<-start
		count, err := finalizeSvc.ScanExpiredSessions(ctx)
		scanDone <- struct {
			count int
			err   error
		}{count: count, err: err}
	}()
	close(start)
	manual := <-manualDone
	scan := <-scanDone
	if manual.err != nil {
		t.Fatalf("manual Submit: %v", manual.err)
	}
	if scan.err != nil {
		t.Fatalf("ScanExpiredSessions: %v", scan.err)
	}
	if manual.result == nil || manual.result.SubmissionID == "" {
		t.Fatal("manual Submit returned no submission id")
	}
	var count int
	var reason string
	if err := pool.QueryRow(ctx, `SELECT count(*), COALESCE(max(auto_submit_reason), '') FROM submissions WHERE run_id='run-timed-race'`).Scan(&count, &reason); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("concurrent submit paths created %d submissions, want 1", count)
	}
	if reason != "" && reason != "individual_timeout" {
		t.Fatalf("submission reason = %q, want manual or individual timeout", reason)
	}
	var sessionStatus string
	if err := pool.QueryRow(ctx, `SELECT status FROM exam_sessions WHERE id='sess-timed-race'`).Scan(&sessionStatus); err != nil {
		t.Fatal(err)
	}
	if sessionStatus != "submitted" {
		t.Fatalf("session status = %q, want submitted", sessionStatus)
	}
}
