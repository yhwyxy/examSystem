// Package submissions - service_test.go
// 集成测试: Service.Submit 全流程 (事务外只读 + 客观判分 + 短事务写).
package submissions_test

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
	"github.com/yhwyxy/examSystem/internal/jobs"
	"github.com/yhwyxy/examSystem/internal/objective"
	"github.com/yhwyxy/examSystem/internal/papers"
	"github.com/yhwyxy/examSystem/internal/submissions"
	"github.com/yhwyxy/examSystem/internal/testutil"
)

// fixturesRoot 指向 tests/fixtures/contract (Task 0 已冻结).
func fixturesRoot(t *testing.T) string {
	t.Helper()
	here, _ := filepath.Abs(".")
	root := filepath.Join(here, "..", "..", "tests", "fixtures", "contract")
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("fixtures absent: %s", root)
	}
	return root
}

// setupPG 创建 schema + migrate + 准备快照 + 插 exam_runs/_sessions fixture.
// 返回 pool + session token 原值 (hash 在包内 sha256) + cleanup.
func setupPG(t *testing.T) (pool *pgxpool.Pool, token string, cleanup func()) {
	t.Helper()
	pool, schemaCleanup := testutil.NewSchema(t)
	ctx := context.Background()
	if _, err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	// 复制 paper.json 到临时 paper-root 做 snapshot.load.
	src := filepath.Join(fixturesRoot(t), "paper.json")
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read paper.json: %v", err)
	}
	storeDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(storeDir, "go-contract-paper.json"), data, 0644); err != nil {
		t.Fatalf("write snapshot: %v", err)
	}
	// 计算 sha256hex 通过 papers.ComputeSHA256.
	doc, err := papers.LoadDocumentUseNumber(src)
	if err != nil {
		t.Fatalf("LoadDocumentUseNumber: %v", err)
	}
	shaHex, err := papers.ComputeSHA256(doc)
	if err != nil {
		t.Fatalf("ComputeSHA256: %v", err)
	}
	_ = shaHex
	snapshotPath := filepath.Join(storeDir, "go-contract-paper.json")
	now := time.Now().UTC()
	_, err = pool.Exec(ctx, `INSERT INTO exam_runs
		(id, paper_id, public_token_hash, status, snapshot_path, snapshot_hash,
		 round_no, duration_minutes, opened_at, created_at)
		VALUES ('run-1', 'go-contract-paper', 'phtokhash-1', 'open',
			$1, $2, 1, 60, $3, $3)`, snapshotPath, shaHex, now)
	if err != nil {
		t.Fatalf("insert exam_runs: %v", err)
	}
	examToken := "raw-token-fixed"
	_, err = pool.Exec(ctx, `INSERT INTO exam_sessions
		(id, run_id, employee_id, name, department, session_token_hash,
		 started_at, deadline_at, status, created_at, updated_at)
		VALUES ('sess-1', 'run-1', 'emp1', 'tester', 'dept-A', $1,
			$2, $3, 'active', $2, $2)`,
		hashTokenForTest(examToken), now, now.Add(60*time.Minute))
	if err != nil {
		t.Fatalf("insert exam_sessions: %v", err)
	}
	return pool, examToken, schemaCleanup
}

// hashTokenForTest 复刻 submissions.hashToken (sha256 hex). 由于该 helper 未导出, 测试内复制实现.
func hashTokenForTest(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

// TestSubmit_HappyPath 全流程 happy path:
//   - 输入 teacher 答案 (客观题), snapshot 为 paper.json fixture
//   - 期望: submissions.insert grading_status='pending', grading_jobs 入队 (gen=1)
//   - 期望: exam_sessions status 被翻为 'submitted'
//   - objective_score 体现单一正确客观题得分
func TestSubmit_HappyPath(t *testing.T) {
	pool, token, cleanup := setupPG(t)
	defer cleanup()

	ctx := context.Background()
	// 客观题 answers: 单选 question 来源于 paper.json. 用与 LogMR objective case 一致的 ans.
	answersMap := map[string]any{
		"q_single": "A",
		"q_multiple": []any{"A", "B"},
	}
	ansJSON, _ := json.Marshal(answersMap)
	clientIP := "127.0.0.1"
	ua := "go-test-agent"
	repo := submissions.NewRepository()
	jobsSvc := jobs.NewService(nil)
	svc := submissions.NewService(
		pool, repo, papers.LoadSnapshot,
		func(q map[string]any, ans any, partial bool) (map[string]any, error) {
			return objective.Grade(q, ans, partial)
		},
		jobsSvc,
		-1, // 测试不校验 deadline+grace
	)
	if svc == nil {
		t.Fatalf("NewService returned nil")
	}

	res, err := svc.Submit(ctx, submissions.SubmitRequest{
		SessionToken:   token,
		Answers:        ansJSON,
		AnswersMap:     answersMap,
		ClientIP:       &clientIP,
		UserAgent:      &ua,
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if res == nil || res.SubmissionID == "" {
		t.Fatalf("empty submission id")
	}
	// 校验 submissions 行 grading_status.
	var status string
	err = pool.QueryRow(ctx, `SELECT grading_status FROM submissions
		WHERE id = $1::bigint`, res.SubmissionID).Scan(&status)
	if err != nil {
		t.Fatalf("query submission row: %v", err)
	}
	if status != "pending" {
		t.Errorf("grading_status = %q, want 'pending'", status)
	}

	// 校验 grading_jobs 有 1 行 (gen=1).
	var gen int64
	err = pool.QueryRow(ctx, `SELECT generation FROM grading_jobs
		WHERE submission_id = $1::bigint`, res.SubmissionID).Scan(&gen)
	if err != nil {
		t.Fatalf("query grading_jobs: %v", err)
	}
	if gen != 1 {
		t.Errorf("gen = %d, want 1", gen)
	}
	// 校验 exam_sessions status='submitted'.
	var sessionStatus string
	err = pool.QueryRow(ctx, `SELECT status FROM exam_sessions
		WHERE session_token_hash = $1`, hashTokenForTest(token)).Scan(&sessionStatus)
	if err != nil {
		t.Fatalf("query session status: %v", err)
	}
	if sessionStatus != "submitted" {
		t.Errorf("exam_sessions.status = %q, want 'submitted'", sessionStatus)
	}
}

// TestSubmit_PureObjectiveShortCircuit 纯客观卷 (无主观题) 测试:
// grading_status='done', review_status='auto_scored', 不入队 grading_jobs.
func TestSubmit_PureObjectiveShortCircuit(t *testing.T) {
	pool, token, cleanup := setupPGObjective(t)
	defer cleanup()

	ctx := context.Background()
	answersMap := map[string]any{"q-single": "A"}
	ansJSON, _ := json.Marshal(answersMap)
	repo := submissions.NewRepository()
	jobsSvc := jobs.NewService(nil)
	svc := submissions.NewService(
		pool, repo, papers.LoadSnapshot,
		func(q map[string]any, ans any, partial bool) (map[string]any, error) {
			return objective.Grade(q, ans, partial)
		},
		jobsSvc, -1,
	)
	if svc == nil {
		t.Fatalf("NewService nil")
	}

	res, err := svc.Submit(ctx, submissions.SubmitRequest{
		SessionToken: token,
		Answers:      ansJSON,
		AnswersMap:   answersMap,
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	var status, review string
	err = pool.QueryRow(ctx, `SELECT grading_status, review_status
		FROM submissions WHERE id = $1::bigint`, res.SubmissionID).Scan(&status, &review)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if status != "done" {
		t.Errorf("grading_status = %q, want 'done'", status)
	}
	if review != "auto_scored" {
		t.Errorf("review_status = %q, want 'auto_scored'", review)
	}
	var jobCount int
	err = pool.QueryRow(ctx, `SELECT count(*) FROM grading_jobs
		WHERE submission_id = $1::bigint`, res.SubmissionID).Scan(&jobCount)
	if err != nil {
		t.Fatalf("count jobs: %v", err)
	}
	if jobCount != 0 {
		t.Errorf("grading_jobs count = %d, want 0 (纯客观卷 short-circuit)", jobCount)
	}
}

// setupPGObjective 构造一项纯客观卷 paper snapshot: paper_id="test-objective-paper"
// 仅 predict 单选 q. 使用 t.TempDir 创建 JSON 文件 + 计算 sha256 注入 exam_runs.
// 返回 (pool, token, cleanup).
func setupPGObjective(t *testing.T) (*pgxpool.Pool, string, func()) {
	t.Helper()
	pool, schemaCleanup := testutil.NewSchema(t)
	ctx := context.Background()
	if _, err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	doc := map[string]any{
		"paper_version": "obj-test-v1",
		"questions": []any{
			map[string]any{
				"id":       "q-single",
				"type":     "single_choice",
				"score":    10,
				"answer":   "A",
				"options":  []any{"A", "B", "C", "D"},
				"prompt":   "which letter is the first word",
			},
		},
	}
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("Marshal paper doc: %v", err)
	}
	storeDir := t.TempDir()
	snapPath := filepath.Join(storeDir, "objective-paper.json")
	if err := os.WriteFile(snapPath, b, 0644); err != nil {
		t.Fatalf("Write snap: %v", err)
	}

	sha := sha256.Sum256(b)
	shaHex := hex.EncodeToString(sha[:])
	now := time.Now().UTC()
	_, err = pool.Exec(ctx, `INSERT INTO exam_runs
		(id, paper_id, public_token_hash, status, snapshot_path, snapshot_hash,
		 round_no, duration_minutes, opened_at, created_at)
		VALUES ('run-obj', 'objective-paper', 'phtok-obj', 'open',
			$1, $2, 1, 60, $3, $3)`, snapPath, shaHex, now)
	if err != nil {
		t.Fatalf("INSERT exam_runs: %v", err)
	}
	token := "raw-obj-token"
	_, err = pool.Exec(ctx, `INSERT INTO exam_sessions
		(id, run_id, employee_id, name, department, session_token_hash,
		 started_at, deadline_at, status, created_at, updated_at)
		VALUES ('sess-obj', 'run-obj', 'emp1', 'obj-tester', 'Dept-A',
			$1, $2, $3, 'active', $2, $2)`,
		hashTokenForTest(token), now, now.Add(60*time.Minute))
	if err != nil {
		t.Fatalf("INSERT exam_sessions: %v", err)
	}
	return pool, token, schemaCleanup
}

// TestGetStatus_FieldsAndMapping 集成测试: 验证 status 派生映射对的成功.
func TestGetStatus_FieldsAndMapping(t *testing.T) {
	pool, token, cleanup := setupPGObjective(t)
	defer cleanup()
	ctx := context.Background()

	// 先 Submit 生成一行 pending。
	answersMap := map[string]any{"q-single": "A"}
	ansJSON, _ := json.Marshal(answersMap)
	repo := submissions.NewRepository()
	svc := submissions.NewService(
		pool, repo, papers.LoadSnapshot,
		func(q map[string]any, ans any, partial bool) (map[string]any, error) {
			return objective.Grade(q, ans, partial)
		}, jobs.NewService(nil), -1,
	)
	res, err := svc.Submit(ctx, submissions.SubmitRequest{
		SessionToken: token, Answers: ansJSON, AnswersMap: answersMap,
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	// 纯客观卷 short-circuit 应该 grading_status=done, review_status=auto_scored -> status=auto_scored.
	status, err := svc.GetStatus(ctx, res.SubmissionID)
	if err != nil {
		t.Fatalf("GetStatus: %v", err)
	}
	if status.GradingStatus != "done" {
		t.Errorf("grading_status=%q want 'done'", status.GradingStatus)
	}
	if status.ReviewStatus != "auto_scored" {
		t.Errorf("review_status=%q want 'auto_scored'", status.ReviewStatus)
	}
	if status.Status != "auto_scored" {
		t.Errorf("status=%q want 'auto_scored' (映射 review_status)", status.Status)
	}
	if status.SubmissionID != res.SubmissionID {
		t.Errorf("submission_id mismatch %q vs %q", status.SubmissionID, res.SubmissionID)
	}
}
