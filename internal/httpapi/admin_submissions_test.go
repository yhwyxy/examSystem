package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/xuri/excelize/v2"
	"github.com/yhwyxy/examSystem/internal/db"
	"github.com/yhwyxy/examSystem/internal/papers"
	"github.com/yhwyxy/examSystem/internal/review"
	"github.com/yhwyxy/examSystem/internal/testutil"
)

// testSubmPool gives every test a migrated schema so packages can run concurrently.
func testSubmPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, _ := testutil.NewSchema(t)
	if _, err := db.Migrate(context.Background(), pool); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return pool
}

// newSubmRouter 用真 pool 构造 admin 路由子树, 兼容顶层 /stats /regrade (path C 修正).
func newSubmRouter(t *testing.T, pool *pgxpool.Pool, reviewSvc *review.Service) http.Handler {
	t.Helper()
	return newSubmRouterWith(t, pool, reviewSvc, nil)
}

func newSubmRouterWith(t *testing.T, pool *pgxpool.Pool, reviewSvc *review.Service,
	papersStore *papers.Store) http.Handler {
	t.Helper()
	r := chi.NewRouter()
	deps := Dependencies{Pool: pool, Review: reviewSvc, Papers: papersStore}
	// Mount admin 子树 (顶层 stats/regrade + MountAdminSubmissions list/detail/export/review/delete)
	r.Route("/admin", func(adm chi.Router) {
		if pool == nil {
			adm.Get("/stats", serviceNotReady("POOL_NIL"))
			adm.Get("/stats/breakdown", serviceNotReady("POOL_NIL"))
			adm.Post("/regrade/{id}", serviceNotReady("REVIEW_NIL"))
		} else {
			adm.Get("/stats", statsSubmissionsHandler(deps))
			adm.Get("/stats/breakdown", adminStatisticsBreakdownHandler(pool))
			adm.Post("/regrade/{id}", regradeSubmissionHandler(deps))
		}
		MountAdminSubmissions(adm, deps)
	})
	r.Route("/api", func(api chi.Router) {})
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
		(name, employee_id, paper_id, paper_name, run_id, department,
		 answers_json, grading_detail_json,
		 objective_score, subjective_score_machine, subjective_score_final, total_score,
		 review_status, grading_status, grading_generation, submitted_at)
		VALUES ('tester', 'emp-1', 'smoke-1', '专业1', 'run-1', '部门1',
		 '[]'::jsonb, '[]'::jsonb,
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
func TestHistoricalGradeLabelsDistinguishOldScoresFromCurrentGenerationHistory(t *testing.T) {
	pool := testSubmPool(t)
	router := newSubmRouter(t, pool, nil)
	id := setupSubmData(t, pool)
	ctx := context.Background()
	check := func(historical, legacy bool) {
		t.Helper()
		for _, path := range []string{"/admin/submissions", fmt.Sprintf("/admin/submissions/%d", id)} {
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
			if recorder.Code != http.StatusOK {
				t.Fatalf("%s status=%d body=%s", path, recorder.Code, recorder.Body.String())
			}
			var body struct {
				Submission  map[string]any   `json:"submission"`
				Submissions []map[string]any `json:"submissions"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			row := body.Submission
			if len(body.Submissions) > 0 {
				row = body.Submissions[0]
			}
			if row["historical_grade"] != historical || row["run_is_legacy"] != legacy || row["round_no"] != float64(1) || row["total_score"] != float64(7) {
				t.Fatalf("incorrect historical presentation on %s: %+v", path, row)
			}
		}
	}
	check(false, false)
	if _, err := pool.Exec(ctx, `UPDATE submissions SET graded_at=now()-interval '1 day' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	check(true, false)
	if _, err := pool.Exec(ctx, `INSERT INTO grading_history(submission_id,generation,grading_detail_json,
subjective_score_machine,subjective_score_final,total_score,review_status) VALUES($1,1,'[]',2,3,7,'reviewed')`, id); err != nil {
		t.Fatal(err)
	}
	check(false, false)
	if _, err := pool.Exec(ctx, `UPDATE exam_runs SET is_legacy=true WHERE id='run-1'`); err != nil {
		t.Fatal(err)
	}
	check(true, true)
}

func TestAdminSubm_List(t *testing.T) {
	pool := testSubmPool(t)
	r := newSubmRouter(t, pool, nil)
	subID := setupSubmData(t, pool)
	defer setupSubmData(t, pool) // 复用清表做 teardown

	req := httptest.NewRequest(http.MethodGet, "/admin/submissions", nil)
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
	subID := setupSubmData(t, pool)
	defer setupSubmData(t, pool)
	if _, err := pool.Exec(context.Background(), `UPDATE submissions SET reviewed_at=now() WHERE id=$1`, subID); err != nil {
		t.Fatalf("mark submission reviewed: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/admin/stats", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		SubmittedCount     int64   `json:"submitted_count"`
		AvgScore           float64 `json:"avg_score"`
		MaxScore           float64 `json:"max_score"`
		MinScore           float64 `json:"min_score"`
		PendingReview      int64   `json:"pending_review"`
		LowConfidenceCount int64   `json:"low_confidence_count"`
		ActiveWorkers      int64   `json:"active_workers"`
		ScoringQueueDepth  int64   `json:"scoring_queue_depth"`
		Scores             struct {
			Machine struct {
				Count        int64   `json:"count"`
				AverageScore float64 `json:"avg_total_score"`
			} `json:"machine"`
			ReviewedFinal struct {
				Count        int64   `json:"count"`
				AverageScore float64 `json:"avg_total_score"`
			} `json:"reviewed_final"`
			Published struct {
				Available bool     `json:"available"`
				Count     int64    `json:"count"`
				Average   *float64 `json:"avg_total_score"`
			} `json:"published"`
		} `json:"scores"`
		Counts map[string]int64 `json:"counts"`
		Total  int64            `json:"total"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Total != 1 || resp.SubmittedCount != 1 {
		t.Fatalf("total=%d submitted_count=%d want 1", resp.Total, resp.SubmittedCount)
	}
	if resp.AvgScore != 7 || resp.MaxScore != 7 || resp.MinScore != 7 {
		t.Fatalf("scores avg=%v max=%v min=%v want 7", resp.AvgScore, resp.MaxScore, resp.MinScore)
	}
	if resp.PendingReview != 0 || resp.LowConfidenceCount != 0 {
		t.Fatalf("pending_review=%d low_confidence_count=%d want 0", resp.PendingReview, resp.LowConfidenceCount)
	}
	if resp.ActiveWorkers != 0 || resp.ScoringQueueDepth != 0 {
		t.Fatalf("active_workers=%d scoring_queue_depth=%d want 0", resp.ActiveWorkers, resp.ScoringQueueDepth)
	}
	if resp.Scores.Machine.Count != 1 || resp.Scores.Machine.AverageScore != 6 {
		t.Fatalf("machine scores=%+v want count=1 average=6", resp.Scores.Machine)
	}
	if resp.Scores.ReviewedFinal.Count != 1 || resp.Scores.ReviewedFinal.AverageScore != 7 {
		t.Fatalf("reviewed scores=%+v want count=1 average=7", resp.Scores.ReviewedFinal)
	}
	if resp.Scores.Published.Available || resp.Scores.Published.Count != 0 || resp.Scores.Published.Average != nil {
		t.Fatalf("published scores=%+v want explicitly unavailable", resp.Scores.Published)
	}
	if resp.Counts["reviewed"] != 1 {
		t.Fatalf("counts.reviewed=%d want 1", resp.Counts["reviewed"])
	}
}

func TestAdminSubm_StatsQueueAndWorkerCounts(t *testing.T) {
	pool := testSubmPool(t)
	r := newSubmRouter(t, pool, nil)
	subID := setupSubmData(t, pool)
	defer setupSubmData(t, pool)

	ctx := context.Background()
	_, err := pool.Exec(ctx, `
		INSERT INTO grading_jobs
			(submission_id, paper_id, run_id, generation, status, lease_owner, lease_until, available_at)
		VALUES
			($1, 'smoke-1', 'run-1', 2, 'leased', 'worker-a', now() + interval '1 minute', now()),
			($1, 'smoke-1', 'run-1', 3, 'leased', 'worker-a', now() + interval '1 minute', now()),
			($1, 'smoke-1', 'run-1', 4, 'leased', 'worker-expired', now() - interval '1 second', now()),
			($1, 'smoke-1', 'run-1', 5, 'queued', NULL, NULL, now() - interval '1 second'),
			($1, 'smoke-1', 'run-1', 6, 'queued', NULL, NULL, now() + interval '1 minute')`, subID)
	if err != nil {
		t.Fatalf("seed grading jobs: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/admin/stats", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		ActiveWorkers     int64 `json:"active_workers"`
		ScoringQueueDepth int64 `json:"scoring_queue_depth"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.ActiveWorkers != 1 {
		t.Fatalf("active_workers=%d want 1 distinct unexpired worker", resp.ActiveWorkers)
	}
	if resp.ScoringQueueDepth != 1 {
		t.Fatalf("scoring_queue_depth=%d want 1 due queued job", resp.ScoringQueueDepth)
	}
}

func TestPublishedStatsUseLatestImmutableVersion(t *testing.T) {
	pool := testSubmPool(t)
	router := newSubmRouter(t, pool, nil)
	submissionID := setupSubmData(t, pool)
	ctx := context.Background()
	snapshotHash := strings.Repeat("a", 64)
	answerHash, gradingHash := strings.Repeat("b", 64), strings.Repeat("c", 64)
	if _, err := pool.Exec(ctx, `UPDATE exam_runs SET snapshot_hash=$1,max_score=10,passing_score=5 WHERE id='run-1'`, snapshotHash); err != nil {
		t.Fatal(err)
	}
	var similarityID, anomalyID int64
	if err := pool.QueryRow(ctx, `INSERT INTO answer_similarity_batches
(run_id,snapshot_hash,answer_set_hash,rule_version,status,submission_count,completed_at)
VALUES ('run-1',$1,$2,'lexical-overlap-v1','completed',1,now()) RETURNING id`, snapshotHash, answerHash).Scan(&similarityID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO score_anomaly_batches
(run_id,similarity_batch_id,snapshot_hash,answer_set_hash,grading_set_hash,rule_version,status,submission_count,completed_at)
VALUES ('run-1',$1,$2,$3,$4,'score-consistency-v1','completed',1,now()) RETURNING id`, similarityID, snapshotHash, answerHash, gradingHash).Scan(&anomalyID); err != nil {
		t.Fatal(err)
	}
	var firstID, secondID int64
	if err := pool.QueryRow(ctx, `INSERT INTO score_publication_batches
(run_id,paper_id,paper_name,version,actor,snapshot_hash,answer_set_hash,grading_set_hash,similarity_batch_id,anomaly_batch_id,submission_count)
VALUES ('run-1','smoke-1','专业1',1,'publisher-a',$1,$2,$3,$4,$5,1) RETURNING id`, snapshotHash, answerHash, gradingHash, similarityID, anomalyID).Scan(&firstID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO score_publication_grades
(batch_id,submission_id,participant_name,employee_id,department,grading_generation,review_status,objective_score,subjective_machine,subjective_final,total_score,max_score)
VALUES ($1,$2,'tester','emp-1','部门1',1,'reviewed',4,2,3,7,10)`, firstID, submissionID); err != nil {
		t.Fatal(err)
	}
	gradingHash2 := strings.Repeat("d", 64)
	var anomalyID2 int64
	if err := pool.QueryRow(ctx, `INSERT INTO score_anomaly_batches
(run_id,similarity_batch_id,snapshot_hash,answer_set_hash,grading_set_hash,rule_version,status,submission_count,completed_at)
VALUES ('run-1',$1,$2,$3,$4,'score-consistency-v2','completed',1,now()) RETURNING id`, similarityID, snapshotHash, answerHash, gradingHash2).Scan(&anomalyID2); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO score_publication_batches
(run_id,paper_id,paper_name,version,supersedes_batch_id,correction_reason,actor,snapshot_hash,answer_set_hash,grading_set_hash,similarity_batch_id,anomaly_batch_id,submission_count)
	VALUES ('run-1','smoke-1','专业1',2,$1,'复核后更正分数','publisher-a',$2,$3,$4,$5,$6,1) RETURNING id`, firstID, snapshotHash, answerHash, gradingHash2, similarityID, anomalyID2).Scan(&secondID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO score_publication_grades
(batch_id,submission_id,participant_name,employee_id,department,grading_generation,review_status,objective_score,subjective_machine,subjective_final,total_score,max_score)
VALUES ($1,$2,'tester','emp-1','部门1',1,'reviewed',4,2,4,8,10)`, secondID, submissionID); err != nil {
		t.Fatal(err)
	}
	statsResponse := httptest.NewRecorder()
	router.ServeHTTP(statsResponse, httptest.NewRequest(http.MethodGet, "/admin/stats", nil))
	if statsResponse.Code != http.StatusOK {
		t.Fatalf("stats status=%d body=%s", statsResponse.Code, statsResponse.Body.String())
	}
	var stats struct {
		Scores struct {
			Published struct {
				Available bool    `json:"available"`
				Count     int64   `json:"count"`
				Average   float64 `json:"avg_total_score"`
			} `json:"published"`
		} `json:"scores"`
	}
	if err := json.NewDecoder(statsResponse.Body).Decode(&stats); err != nil {
		t.Fatal(err)
	}
	if !stats.Scores.Published.Available || stats.Scores.Published.Count != 1 || stats.Scores.Published.Average != 8 {
		t.Fatalf("published summary=%+v", stats.Scores.Published)
	}
	breakdownResponse := httptest.NewRecorder()
	router.ServeHTTP(breakdownResponse, httptest.NewRequest(http.MethodGet, "/admin/stats/breakdown", nil))
	if breakdownResponse.Code != http.StatusOK {
		t.Fatalf("breakdown status=%d body=%s", breakdownResponse.Code, breakdownResponse.Body.String())
	}
	var breakdown statisticsBreakdownResponse
	if err := json.NewDecoder(breakdownResponse.Body).Decode(&breakdown); err != nil {
		t.Fatal(err)
	}
	for _, group := range []statsBreakdown{breakdown.ByProfession[0], breakdown.ByDepartment[0], breakdown.ByRound[0]} {
		if !group.PublishedReady || group.Published.Count != 1 || group.Published.Average == nil || *group.Published.Average != 8 || group.Published.Distribution["80-89%"] != 1 {
			t.Fatalf("published breakdown=%+v", group.Published)
		}
	}
}

func TestAdminStatsBreakdownUsesRunThresholdsAndQuestionEvidence(t *testing.T) {
	pool := testSubmPool(t)
	r := newSubmRouter(t, pool, nil)
	subID := setupSubmData(t, pool)
	defer setupSubmData(t, pool)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `UPDATE exam_runs SET max_score=10,passing_score=6 WHERE id='run-1'`); err != nil {
		t.Fatalf("set run thresholds: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE submissions SET reviewed_at=now(), grading_detail_json=$1 WHERE id=$2`,
		[]byte(`[{
			"question_id":"q-subj","type":"short_answer","score":2,
			"machine_score":2,"final_score":3,"max_score":4
		}]`), subID); err != nil {
		t.Fatalf("seed question evidence: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/admin/stats/breakdown?paper_id=smoke-1&department=%E9%83%A8%E9%97%A8%31", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Filters      map[string]string   `json:"filters"`
		ByProfession []statsBreakdown    `json:"by_profession"`
		ByDepartment []statsBreakdown    `json:"by_department"`
		ByRound      []statsBreakdown    `json:"by_round"`
		ByQuestion   []questionScoreRate `json:"by_question"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Filters["paper_id"] != "smoke-1" || resp.Filters["department"] != "部门1" {
		t.Fatalf("filters=%v", resp.Filters)
	}
	if len(resp.ByProfession) != 1 || resp.ByProfession[0].Key != "专业1" {
		t.Fatalf("profession breakdown=%+v", resp.ByProfession)
	}
	if len(resp.ByDepartment) != 1 || resp.ByDepartment[0].Key != "部门1" {
		t.Fatalf("department breakdown=%+v", resp.ByDepartment)
	}
	if len(resp.ByRound) != 1 || resp.ByRound[0].RoundNo == nil || *resp.ByRound[0].RoundNo != 1 {
		t.Fatalf("round breakdown=%+v", resp.ByRound)
	}
	for _, group := range []statsBreakdown{resp.ByProfession[0], resp.ByDepartment[0], resp.ByRound[0]} {
		if group.Machine.Average == nil || *group.Machine.Average != 6 || group.Machine.PassRate == nil || *group.Machine.PassRate != 1 {
			t.Fatalf("machine metrics=%+v, want avg 6 and 100%% pass", group.Machine)
		}
		if group.ReviewedFinal.Average == nil || *group.ReviewedFinal.Average != 7 || group.ReviewedFinal.PassRate == nil || *group.ReviewedFinal.PassRate != 1 {
			t.Fatalf("reviewed metrics=%+v, want avg 7 and 100%% pass", group.ReviewedFinal)
		}
		if group.Review.RequiredCount != 1 || group.Review.CompletedCount != 1 || group.Review.ProgressRate == nil || *group.Review.ProgressRate != 1 {
			t.Fatalf("review progress=%+v, want 1/1 complete", group.Review)
		}
		if group.Machine.Distribution["60-69%"] != 1 || group.ReviewedFinal.Distribution["70-79%"] != 1 {
			t.Fatalf("score distributions machine=%v reviewed=%v", group.Machine.Distribution, group.ReviewedFinal.Distribution)
		}
		if group.PublishedReady || group.Published.Count != 0 {
			t.Fatalf("published metrics should be unavailable pending P5: %+v", group)
		}
	}
	if len(resp.ByQuestion) != 1 {
		t.Fatalf("question breakdown=%+v, want one question", resp.ByQuestion)
	}
	question := resp.ByQuestion[0]
	if question.QuestionID != "q-subj" || question.AttemptCount != 1 || question.ReviewedCount != 1 ||
		question.MachineScoreRate == nil || *question.MachineScoreRate != 0.5 ||
		question.ReviewedScoreRate == nil || *question.ReviewedScoreRate != 0.75 {
		t.Fatalf("question rate=%+v", question)
	}
}

// TestAdminSubm_Detail: GET /admin/submissions/{id} 返 submission + review_logs 空数组.
func TestAdminSubm_Detail(t *testing.T) {
	pool := testSubmPool(t)
	r := newSubmRouter(t, pool, nil)
	subID := setupSubmData(t, pool)
	defer setupSubmData(t, pool)

	req := httptest.NewRequest(http.MethodGet, "/admin/submissions/"+fmtID(subID), nil)
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

	req := httptest.NewRequest(http.MethodDelete, "/admin/submissions",
		strings.NewReader(`{"ids":[`+fmtID(subID)+`]}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Success bool  `json:"success"`
		Deleted int64 `json:"deleted"`
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

// TestAdminSubm_Export: GET /admin/submissions/export 返 xlsx (Content-Type 与首字节 ZIP magic).
func TestAdminSubm_Export(t *testing.T) {
	pool := testSubmPool(t)
	r := newSubmRouter(t, pool, nil)
	setupSubmData(t, pool)
	defer setupSubmData(t, pool)

	req := httptest.NewRequest(http.MethodGet, "/admin/submissions/export", nil)
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

	f, err := excelize.OpenReader(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("open xlsx: %v", err)
	}
	defer func() { _ = f.Close() }()
	rows, err := f.GetRows("总表")
	if err != nil {
		t.Fatalf("get rows: %v", err)
	}
	// 分块格式: 标题行 + 表头 + 1 数据行 (detail 为空 -> 无题列).
	if len(rows) != 3 {
		t.Fatalf("rows=%d want 3 (title+header+data)", len(rows))
	}
	if rows[0][0] != "专业1" {
		t.Fatalf("title=%q want 专业1", rows[0][0])
	}
	if !slices.Equal(rows[1], []string{"专业", "工号", "姓名", "部门", "总分", "时间"}) {
		t.Fatalf("header=%q", rows[1])
	}
	if rows[2][0] != "专业1" || rows[2][1] != "emp-1" || rows[2][2] != "tester" ||
		rows[2][3] != "部门1" || rows[2][4] != "7" {
		t.Fatalf("row=%q", rows[2])
	}
	if rows[2][5] == "" {
		t.Fatalf("submitted_at empty")
	}
}

// TestAdminSubm_Export_PaperNameFallback: paper_name 为 NULL (主提交路径写入)
// 时, 专业列回退 paper_id, 与列表页 paper_name || paper_id 一致.
func TestAdminSubm_Export_PaperNameFallback(t *testing.T) {
	pool := testSubmPool(t)
	r := newSubmRouter(t, pool, nil)
	setupSubmData(t, pool)
	defer setupSubmData(t, pool)

	if _, err := pool.Exec(context.Background(),
		`UPDATE submissions SET paper_name = NULL`); err != nil {
		t.Fatalf("null paper_name: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/admin/submissions/export", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	f, err := excelize.OpenReader(bytes.NewReader(rr.Body.Bytes()))
	if err != nil {
		t.Fatalf("open xlsx: %v", err)
	}
	defer func() { _ = f.Close() }()
	rows, err := f.GetRows("总表")
	if err != nil {
		t.Fatalf("get rows: %v", err)
	}
	if rows[0][0] != "smoke-1" { // paper_id 兜底 (标题行)
		t.Fatalf("专业=%q want smoke-1", rows[0][0])
	}
}

// TestAdminSubm_ChinesePaperName: papers store 提供中文名时, 列表/导出的专业列
// 显示中文名, 优先于 submissions.paper_name 与 paper_id.
func TestAdminSubm_ChinesePaperName(t *testing.T) {
	pool := testSubmPool(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "smoke-1.json"),
		[]byte(`{"name":"机械专业","questions":[]}`), 0o644); err != nil {
		t.Fatalf("write paper doc: %v", err)
	}
	store, err := papers.NewStore(dir)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	r := newSubmRouterWith(t, pool, nil, store)
	setupSubmData(t, pool)
	defer setupSubmData(t, pool)

	// 列表: paper_name 应为中文名.
	req := httptest.NewRequest(http.MethodGet, "/admin/submissions", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Submissions []struct {
			PaperName *string `json:"paper_name"`
			PaperID   string  `json:"paper_id"`
		} `json:"submissions"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(resp.Submissions) != 1 || resp.Submissions[0].PaperName == nil ||
		*resp.Submissions[0].PaperName != "机械专业" {
		t.Fatalf("list paper_name=%v want 机械专业", resp.Submissions[0].PaperName)
	}

	// 导出: 专业列应为中文名.
	req = httptest.NewRequest(http.MethodGet, "/admin/submissions/export", nil)
	rr = httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("export status=%d body=%s", rr.Code, rr.Body.String())
	}
	f, err := excelize.OpenReader(bytes.NewReader(rr.Body.Bytes()))
	if err != nil {
		t.Fatalf("open xlsx: %v", err)
	}
	defer func() { _ = f.Close() }()
	rows, err := f.GetRows("总表")
	if err != nil {
		t.Fatalf("get rows: %v", err)
	}
	if rows[0][0] != "机械专业" {
		t.Fatalf("export 专业=%q want 机械专业", rows[0][0])
	}
}

// TestAdminSubm_Export_TruncateScore: 分数保留两位小数, 多余位直接舍弃 (非四舍五入).
func TestAdminSubm_Export_TruncateScore(t *testing.T) {
	pool := testSubmPool(t)
	r := newSubmRouter(t, pool, nil)
	subID := setupSubmData(t, pool)
	defer setupSubmData(t, pool)

	// q2 的 1.13 用于锁定浮点修正: 无 1e-9 修正时 1.13*100 会被误截成 1.12.
	detail := `[{"question_id":"q1","type":"single_choice","student_answer":"A","max_score":2,"score":0.666667,"final_score":0.666667},` +
		`{"question_id":"q2","type":"single_choice","student_answer":"B","max_score":1,"score":1.13,"final_score":1.13}]`
	if _, err := pool.Exec(context.Background(),
		`UPDATE submissions SET total_score = 75.233333,
		 objective_score = 4.666667, subjective_score_final = 70.566666,
		 grading_detail_json = $1::jsonb WHERE id = $2`,
		detail, subID); err != nil {
		t.Fatalf("set scores: %v", err)
	}

	// 列表 API: 分数只保留两位小数.
	req := httptest.NewRequest(http.MethodGet, "/admin/submissions", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Submissions []struct {
			Objective       float64 `json:"objective_score"`
			SubjectiveFinal float64 `json:"subjective_score_final"`
			Total           float64 `json:"total_score"`
		} `json:"submissions"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Submissions) != 1 {
		t.Fatalf("len=%d want 1", len(resp.Submissions))
	}
	it := resp.Submissions[0]
	if it.Objective != 4.66 || it.SubjectiveFinal != 70.56 || it.Total != 75.23 {
		t.Fatalf("list scores=%+v want objective=4.66 subjective=70.56 total=75.23", it)
	}

	// 导出: 题分 0.66/2, 总分 75.23.
	req = httptest.NewRequest(http.MethodGet, "/admin/submissions/export", nil)
	rr = httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("export status=%d body=%s", rr.Code, rr.Body.String())
	}
	f, err := excelize.OpenReader(bytes.NewReader(rr.Body.Bytes()))
	if err != nil {
		t.Fatalf("open xlsx: %v", err)
	}
	defer func() { _ = f.Close() }()
	rows, err := f.GetRows("总表")
	if err != nil {
		t.Fatalf("get rows: %v", err)
	}
	checkRow(t, rows[2], []string{"专业1", "emp-1", "tester", "部门1",
		"A（0.66/2）", "B（1.13/1）", "75.23", ""}, "truncate")
}

// TestAdminSubm_Export_FilterPaper: 两条不同 paper_id 提交, ?paper_id= 只导出匹配行.
func TestAdminSubm_Export_FilterPaper(t *testing.T) {
	pool := testSubmPool(t)
	r := newSubmRouter(t, pool, nil)
	subID := setupSubmData(t, pool) // 种子行: paper_id='smoke-1', name='tester'
	defer setupSubmData(t, pool)

	// 给 tester 行写 3 题 detail (含 composite 2 子题), 验证题列展开.
	testerDetail := `[{"question_id":"q1","type":"single_choice","student_answer":"A","max_score":4,"score":4,"final_score":4},` +
		`{"question_id":"q2","type":"short_answer","student_answer":"RESTful 设计原则","max_score":6,"score":5,"final_score":5.5},` +
		`{"question_id":"q3","type":"composite","is_composite":true,"max_score":10,"score":9,"final_score":9,"sub_results":[` +
		`{"sub_question_id":"q3a","student_answer":"子题作答1","max_score":4,"score":4,"final_score":4},` +
		`{"sub_question_id":"q3b","student_answer":"子题作答2","max_score":6,"score":5,"final_score":5}]}]`
	if _, err := pool.Exec(context.Background(),
		`UPDATE submissions SET grading_detail_json = $1::jsonb WHERE id = $2`,
		testerDetail, subID); err != nil {
		t.Fatalf("set tester detail: %v", err)
	}

	// 追加第二条: paper_id='smoke-2', name='bob'.
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `INSERT INTO exam_runs
		(id, paper_id, round_no, status, duration_minutes, public_token_hash,
		 snapshot_path, snapshot_hash, opened_at, closed_at, created_at)
		VALUES ('run-2', 'smoke-2', 1, 'closed', 60, 'pub-tok-2',
		 '/tmp/s2.json', 'snap2', NOW(), NOW(), NOW())`); err != nil {
		t.Fatalf("seed exam_runs run-2: %v", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO exam_sessions
		(id, run_id, employee_id, name, session_token_hash,
		 started_at, deadline_at, status, created_at, updated_at)
		VALUES ('sess-2', 'run-2', 'emp-2', 'bob', 'tok2',
		 NOW(), NOW() + interval '1 hour', 'submitted', NOW(), NOW())`); err != nil {
		t.Fatalf("seed exam_sessions sess-2: %v", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO submissions
		(name, employee_id, paper_id, paper_name, run_id, department,
		 answers_json, grading_detail_json,
		 objective_score, subjective_score_machine, subjective_score_final, total_score,
		 review_status, grading_status, grading_generation, submitted_at)
		VALUES ('bob', 'emp-2', 'smoke-2', '专业2', 'run-2', '部门2',
		 '[]'::jsonb,
		 '[{"question_id":"q1","type":"single_choice","student_answer":"B","max_score":4,"score":4,"final_score":4}]'::jsonb,
		 5, 1, 2, 6, 'reviewed', 'done', 1, NOW())`); err != nil {
		t.Fatalf("seed submissions run-2: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	exportRows := func(query string) [][]string {
		t.Helper()
		u := "/admin/submissions/export"
		if query != "" {
			u += "?" + query
		}
		req := httptest.NewRequest(http.MethodGet, u, nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
		}
		f, err := excelize.OpenReader(bytes.NewReader(rr.Body.Bytes()))
		if err != nil {
			t.Fatalf("open xlsx: %v", err)
		}
		defer func() { _ = f.Close() }()
		rows, err := f.GetRows("总表")
		if err != nil {
			t.Fatalf("get rows: %v", err)
		}
		return rows
	}

	// 表头: 专业/工号/姓名/部门 + 4 个题列 (q1,q2,q3:q3a,q3:q3b) + 总分/时间.
	wantHeader := []string{"专业", "工号", "姓名", "部门",
		"1. 作答内容 - 得分", "2. 作答内容 - 得分",
		"3. 作答内容 - 得分", "4. 作答内容 - 得分", "总分", "时间"}
	rows := exportRows("paper_id=smoke-1")
	if len(rows) != 3 {
		t.Fatalf("smoke-1 rows=%d want 3 (title+header+data)", len(rows))
	}
	if rows[0][0] != "专业1" {
		t.Fatalf("smoke-1 title=%q want 专业1", rows[0][0])
	}
	if !slices.Equal(rows[1], wantHeader) {
		t.Fatalf("smoke-1 header=%q want %q", rows[1], wantHeader)
	}
	checkRow(t, rows[2], []string{"专业1", "emp-1", "tester", "部门1",
		"A（4/4）", "RESTful 设计原则（5.5/6）",
		"子题作答1（4/4）", "子题作答2（5/6）", "7", ""}, "smoke-1")

	rows = exportRows("paper_id=smoke-2")
	if len(rows) != 3 {
		t.Fatalf("smoke-2 rows=%d want 3 (title+header+data)", len(rows))
	}
	if rows[0][0] != "专业2" {
		t.Fatalf("smoke-2 title=%q want 专业2", rows[0][0])
	}
	// 过滤后只有 bob 行 -> 列集合仅含 bob 自己的 q1.
	if !slices.Equal(rows[1], []string{"专业", "工号", "姓名", "部门",
		"1. 作答内容 - 得分", "总分", "时间"}) {
		t.Fatalf("smoke-2 header=%q", rows[1])
	}
	checkRow(t, rows[2], []string{"专业2", "emp-2", "bob", "部门2", "B（4/4）", "6", ""}, "smoke-2")

	rows = exportRows("employee_id=emp-1")
	if len(rows) != 3 {
		t.Fatalf("employee_id rows=%d want 3 (title+header+data)", len(rows))
	}
	checkRow(t, rows[2], []string{"专业1", "emp-1", "tester", "部门1",
		"A（4/4）", "RESTful 设计原则（5.5/6）",
		"子题作答1（4/4）", "子题作答2（5/6）", "7", ""}, "employee_id")

	rows = exportRows("keyword=bob")
	if len(rows) != 3 {
		t.Fatalf("keyword rows=%d want 3 (title+header+data)", len(rows))
	}
	checkRow(t, rows[2], []string{"专业2", "emp-2", "bob", "部门2", "B（4/4）", "6", ""}, "keyword")

	rows = exportRows("ids=" + fmtID(subID))
	if len(rows) != 3 {
		t.Fatalf("ids rows=%d want 3 (title+header+data)", len(rows))
	}
	checkRow(t, rows[2], []string{"专业1", "emp-1", "tester", "部门1",
		"A（4/4）", "RESTful 设计原则（5.5/6）",
		"子题作答1（4/4）", "子题作答2（5/6）", "7", ""}, "ids")

	rows = exportRows("")
	// 全量: 按 paper_id 分块, smoke-1 块 (标题/表头/tester) 在前, 空行,
	// smoke-2 块 (标题/表头/bob). 各块列集合独立, 不跨专业 union.
	if len(rows) != 7 {
		t.Fatalf("no-filter rows=%d want 7 (2 blocks)", len(rows))
	}
	if rows[0][0] != "专业1" || !slices.Equal(rows[1], wantHeader) {
		t.Fatalf("no-filter block1 title/header=%q %q", rows[0], rows[1])
	}
	checkRow(t, rows[2], []string{"专业1", "emp-1", "tester", "部门1",
		"A（4/4）", "RESTful 设计原则（5.5/6）",
		"子题作答1（4/4）", "子题作答2（5/6）", "7", ""}, "no-filter tester")
	if len(rows[3]) != 0 { // 块间空行 (GetRows 对空行返回空切片)
		t.Fatalf("no-filter blank row=%q want empty", rows[3])
	}
	if rows[4][0] != "专业2" || !slices.Equal(rows[5],
		[]string{"专业", "工号", "姓名", "部门", "1. 作答内容 - 得分", "总分", "时间"}) {
		t.Fatalf("no-filter block2 title/header=%q %q", rows[4], rows[5])
	}
	checkRow(t, rows[6], []string{"专业2", "emp-2", "bob", "部门2", "B（4/4）", "6", ""}, "no-filter bob")
}

// TestAdminSubm_Export_PerPaperSheets: 多 sheet 导出, "总表" + 每专业一个分表.
func TestAdminSubm_Export_PerPaperSheets(t *testing.T) {
	pool := testSubmPool(t)
	r := newSubmRouter(t, pool, nil)
	subID := setupSubmData(t, pool)
	defer setupSubmData(t, pool)

	testerDetail := `[{"question_id":"q1","type":"single_choice","student_answer":"A","max_score":4,"score":4,"final_score":4},` +
		`{"question_id":"q2","type":"short_answer","student_answer":"RESTful 设计原则","max_score":6,"score":5,"final_score":5.5},` +
		`{"question_id":"q3","type":"composite","is_composite":true,"max_score":10,"score":9,"final_score":9,"sub_results":[` +
		`{"sub_question_id":"q3a","student_answer":"子题作答1","max_score":4,"score":4,"final_score":4},` +
		`{"sub_question_id":"q3b","student_answer":"子题作答2","max_score":6,"score":5,"final_score":5}]}]`
	if _, err := pool.Exec(context.Background(),
		`UPDATE submissions SET grading_detail_json = $1::jsonb WHERE id = $2`,
		testerDetail, subID); err != nil {
		t.Fatalf("set tester detail: %v", err)
	}
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `INSERT INTO exam_runs
		(id, paper_id, round_no, status, duration_minutes, public_token_hash,
		 snapshot_path, snapshot_hash, opened_at, closed_at, created_at)
		VALUES ('run-2', 'smoke-2', 1, 'closed', 60, 'pub-tok-2',
		 '/tmp/s2.json', 'snap2', NOW(), NOW(), NOW())`); err != nil {
		t.Fatalf("seed exam_runs run-2: %v", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO exam_sessions
		(id, run_id, employee_id, name, session_token_hash,
		 started_at, deadline_at, status, created_at, updated_at)
		VALUES ('sess-2', 'run-2', 'emp-2', 'bob', 'tok2',
		 NOW(), NOW() + interval '1 hour', 'submitted', NOW(), NOW())`); err != nil {
		t.Fatalf("seed exam_sessions sess-2: %v", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO submissions
		(name, employee_id, paper_id, paper_name, run_id, department,
		 answers_json, grading_detail_json,
		 objective_score, subjective_score_machine, subjective_score_final, total_score,
		 review_status, grading_status, grading_generation, submitted_at)
		VALUES ('bob', 'emp-2', 'smoke-2', '专业2', 'run-2', '部门2',
		 '[]'::jsonb,
		 '[{"question_id":"q1","type":"single_choice","student_answer":"B","max_score":4,"score":4,"final_score":4}]'::jsonb,
		 5, 1, 2, 6, 'reviewed', 'done', 1, NOW())`); err != nil {
		t.Fatalf("seed submissions run-2: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/admin/submissions/export", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	f, err := excelize.OpenReader(bytes.NewReader(rr.Body.Bytes()))
	if err != nil {
		t.Fatalf("open xlsx: %v", err)
	}
	defer func() { _ = f.Close() }()

	// sheet 顺序: 总表在前, 分表按 paper_id 顺序 (smoke-1 在 smoke-2 前).
	if got := f.GetSheetList(); !slices.Equal(got, []string{"总表", "专业1", "专业2"}) {
		t.Fatalf("sheets=%q want [总表 专业1 专业2]", got)
	}

	// 总表: 仍是分块格式.
	rows, err := f.GetRows("总表")
	if err != nil {
		t.Fatalf("get 总表 rows: %v", err)
	}
	if len(rows) != 7 {
		t.Fatalf("总表 rows=%d want 7 (2 blocks)", len(rows))
	}
	if rows[0][0] != "专业1" || rows[4][0] != "专业2" {
		t.Fatalf("总表 blocks=%q %q want 专业1/专业2", rows[0], rows[4])
	}

	// 分表 专业1: 题头 + tester 行 (4 题列).
	rows, err = f.GetRows("专业1")
	if err != nil {
		t.Fatalf("get 专业1 rows: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("专业1 rows=%d want 2 (header+1)", len(rows))
	}
	if !slices.Equal(rows[0], []string{"专业", "工号", "姓名", "部门",
		"1. 作答内容 - 得分", "2. 作答内容 - 得分",
		"3. 作答内容 - 得分", "4. 作答内容 - 得分", "总分", "时间"}) {
		t.Fatalf("专业1 header=%q", rows[0])
	}
	checkRow(t, rows[1], []string{"专业1", "emp-1", "tester", "部门1",
		"A（4/4）", "RESTful 设计原则（5.5/6）",
		"子题作答1（4/4）", "子题作答2（5/6）", "7", ""}, "专业1")

	// 分表 专业2: 题头 + bob 行 (1 题列).
	rows, err = f.GetRows("专业2")
	if err != nil {
		t.Fatalf("get 专业2 rows: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("专业2 rows=%d want 2 (header+1)", len(rows))
	}
	if !slices.Equal(rows[0], []string{"专业", "工号", "姓名", "部门",
		"1. 作答内容 - 得分", "总分", "时间"}) {
		t.Fatalf("专业2 header=%q", rows[0])
	}
	checkRow(t, rows[1], []string{"专业2", "emp-2", "bob", "部门2", "B（4/4）", "6", ""}, "专业2")
}

// checkRow 逐列断言导出行; 时间列 (最后列) 仅断言非空.
func checkRow(t *testing.T, got, want []string, label string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s row len=%d want %d", label, len(got), len(want))
	}
	for i := range want {
		if i == len(want)-1 {
			if got[i] == "" {
				t.Fatalf("%s submitted_at empty", label)
			}
			continue
		}
		if got[i] != want[i] {
			t.Fatalf("%s row[%d]=%q want %q", label, i, got[i], want[i])
		}
	}
}
