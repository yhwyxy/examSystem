package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/yhwyxy/examSystem/internal/review"
)

// testSubmPool 全包集成测用连 exam_system 库, 缺 env t.Skip 同 review 包策略 (C1 方案).
//
// 注意: 多个集成测包共用同一 exam_system 库, 跑 go test ./... 时必须 -p 1 串行,
// 否则 review 包 admin_submissions_test 包同时 truncate/seed 同表竞态导致 TestApply_Success
// 等单测偶发 fail. 这是单测库分用同一实例的内在局限, 与生产代码无关, 后续可分库 / schema.
var (
	submPoolOnce sync.Once
	submPoolInst *pgxpool.Pool
	submPoolErr  error
)

func testSubmPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	submPoolOnce.Do(func() {
		dsn := os.Getenv("TEST_DATABASE_URL")
		if dsn == "" {
			submPoolErr = fmt.Errorf("TEST_DATABASE_URL unset")
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		p, err := pgxpool.New(ctx, dsn)
		if err != nil {
			submPoolErr = err
			return
		}
		if err := p.Ping(ctx); err != nil {
			submPoolErr = err
			p.Close()
			return
		}
		submPoolInst = p
	})
	if submPoolErr != nil {
		t.Skipf("admin_submissions integration test skipped: %v", submPoolErr)
	}
	return submPoolInst
}

// newSubmRouter 用真 pool 构造 admin 路由子树 (仅测 submissions 子树).
func newSubmRouter(t *testing.T, pool *pgxpool.Pool, reviewSvc *review.Service) http.Handler {
	t.Helper()
	r := chi.NewRouter()
	deps := Dependencies{Pool: pool, Review: reviewSvc}
	MountAdminSubmissions(r, deps)
	return r
}

// setupSubmData 清表 + 简短 seed 1 条 submissions 行. 返回 submissions.id.
func setupSubmData(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	ctx := context.Background()
	for _, tbl := range []string{
		"review_logs", "grading_jobs", "submissions",
		"exam_sessions", "exam_runs",
	} {
		if _, err := pool.Exec(ctx, "TRUNCATE TABLE "+tbl+" RESTART IDENTITY CASCADE"); err != nil {
			t.Fatalf("truncate %s: %v", tbl, err)
		}
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `INSERT INTO exam_runs
		(id, paper_id, round_no, status, duration_minutes, public_token_hash,
		 snapshot_path, snapshot_hash, opened_at, closed_at, created_at)
		VALUES ('run-1', 'smoke-1', 1, 'closed', 60, 'pub-tok',
		 '/tmp/s.json', 'snap', NOW(), NOW(), NOW())`); err != nil {
		t.Fatalf("seed exam_runs: %v", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO exam_sessions
		(id, run_id, employee_id, name, session_token_hash,
		 started_at, deadline_at, status, created_at, updated_at)
		VALUES ('sess-1', 'run-1', 'emp-1', 'tester', 'tok',
		 NOW(), NOW() + interval '1 hour', 'submitted', NOW(), NOW())`); err != nil {
		t.Fatalf("seed exam_sessions: %v", err)
	}

	var subID int64
	err = tx.QueryRow(ctx, `INSERT INTO submissions
		(name, employee_id, paper_id, run_id, answers_json, grading_detail_json,
		 objective_score, subjective_score_machine, subjective_score_final, total_score,
		 review_status, grading_status, grading_generation, submitted_at)
		VALUES ('tester', 'emp-1', 'smoke-1', 'run-1', '[]'::jsonb, '[]'::jsonb,
		 4, 2, 3, 7, 'reviewed', 'done', 1, NOW())
		RETURNING id`).Scan(&subID)
	if err != nil {
		t.Fatalf("seed submissions: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return subID
}

// TestAdminSubm_List: seed 1 条, GET /submissions 返 200 + submissions 数组 len=1.
func TestAdminSubm_List(t *testing.T) {
	pool := testSubmPool(t)
	r := newSubmRouter(t, pool, nil)
	subID := setupSubmData(t, pool)
	defer setupSubmData(t, pool) // 复用清表做 teardown

	req := httptest.NewRequest(http.MethodGet, "/submissions", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Submissions []map[string]any `json:"submissions"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Submissions) != 1 {
		t.Fatalf("len=%d want 1", len(resp.Submissions))
	}
	id, _ := resp.Submissions[0]["id"].(float64)
	if int64(id) != subID {
		t.Fatalf("id=%v want %d", id, subID)
	}
}

// TestAdminSubm_Stats: 单条 review_status='reviewed', stats 返 total=1, counts.reviewed=1.
func TestAdminSubm_Stats(t *testing.T) {
	pool := testSubmPool(t)
	r := newSubmRouter(t, pool, nil)
	setupSubmData(t, pool)
	defer setupSubmData(t, pool)

	req := httptest.NewRequest(http.MethodGet, "/submissions/stats", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Counts map[string]int64 `json:"counts"`
		Total  int64            `json:"total"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Total != 1 {
		t.Fatalf("total=%d want 1", resp.Total)
	}
	if resp.Counts["reviewed"] != 1 {
		t.Fatalf("counts.reviewed=%d want 1", resp.Counts["reviewed"])
	}
}

// TestAdminSubm_Detail: GET /submissions/{id} 返 submission + review_logs 空数组.
func TestAdminSubm_Detail(t *testing.T) {
	pool := testSubmPool(t)
	r := newSubmRouter(t, pool, nil)
	subID := setupSubmData(t, pool)
	defer setupSubmData(t, pool)

	req := httptest.NewRequest(http.MethodGet, "/submissions/"+fmtID(subID), nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Submission map[string]any `json:"submission"`
		ReviewLogs []any          `json:"review_logs"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Submission == nil {
		t.Fatal("submission nil")
	}
	if len(resp.ReviewLogs) != 0 {
		t.Fatalf("review_logs len=%d want 0", len(resp.ReviewLogs))
	}
	if resp.Submission["review_status"] != "reviewed" {
		t.Fatalf("rs=%v", resp.Submission["review_status"])
	}
}

// TestAdminSubm_Delete: DELETE /submissions body {"ids":[N]} 返 success + deleted=1.
func TestAdminSubm_Delete(t *testing.T) {
	pool := testSubmPool(t)
	r := newSubmRouter(t, pool, nil)
	subID := setupSubmData(t, pool)
	defer setupSubmData(t, pool)

	req := httptest.NewRequest(http.MethodDelete, "/submissions",
		strings.NewReader(`{"ids":[`+fmtID(subID)+`]}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Success bool   `json:"success"`
		Deleted int64  `json:"deleted"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Success || resp.Deleted != 1 {
		t.Fatalf("resp=%+v", resp)
	}
}

// fmtID int64 -> string 避免 strconv import.
func fmtID(id int64) string {
	return fmt.Sprintf("%d", id)
}

// TestAdminSubm_Export: GET /submissions/export 返 xlsx (Content-Type 与首字节 ZIP magic).
func TestAdminSubm_Export(t *testing.T) {
	pool := testSubmPool(t)
	r := newSubmRouter(t, pool, nil)
	setupSubmData(t, pool)
	defer setupSubmData(t, pool)

	req := httptest.NewRequest(http.MethodGet, "/submissions/export", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	ct := rr.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet") {
		t.Fatalf("Content-Type=%q want xlsx", ct)
	}
	body := rr.Body.Bytes()
	if len(body) < 100 {
		t.Fatalf("body too small: %d bytes", len(body))
	}
	if !strings.HasPrefix(string(body), "PK\x03\x04") {
		t.Fatalf("body not ZIP magic (xlsx expected): first 4 bytes=%q", body[:4])
	}
}
