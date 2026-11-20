package httpapi

import (
	"context"
	"encoding/json"
	"github.com/yhwyxy/examSystem/internal/review"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAdminE2E(t *testing.T) {
	pool := testSubmPool(t)
	id := setupSubmData(t, pool)
	ctx := context.Background()
	before := `[{"question_id":"q","type":"short_answer","max_score":10,"score":3,"machine_score":3,"final_score":3,"need_manual_review":true}]`
	if _, err := pool.Exec(ctx, `UPDATE submissions SET grading_detail_json=$1,review_status='need_review' WHERE id=$2`, []byte(before), id); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO grading_history (submission_id,generation,grading_detail_json,subjective_score_machine,subjective_score_final,total_score,review_status)
		SELECT id,grading_generation,grading_detail_json,subjective_score_machine,subjective_score_final,total_score,review_status FROM submissions WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	router := newSubmRouter(t, pool, review.NewService(pool, nil))
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/admin/submissions/"+fmtID(id)+"/review", strings.NewReader(`{"question_id":"q","new_score":8,"note":"checked"}`)))
	if response.Code != http.StatusOK {
		t.Fatal(response.Code, response.Body.String())
	}
	response = httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/admin/submissions/"+fmtID(id), nil))
	if response.Code != http.StatusOK {
		t.Fatal(response.Code, response.Body.String())
	}
	var result struct {
		Submission struct {
			ReviewStatus string           `json:"review_status"`
			Total        float64          `json:"total_score"`
			Detail       []map[string]any `json:"grading_detail"`
		} `json:"submission"`
		History []struct {
			Detail []map[string]any `json:"grading_detail"`
		} `json:"grading_history"`
		Logs []any `json:"review_logs"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Submission.ReviewStatus != "reviewed" || result.Submission.Total != 12 || len(result.Logs) != 1 || len(result.History) != 1 {
		t.Fatalf("unexpected response: %s", response.Body.String())
	}
	if result.Submission.Detail[0]["final_score"] != float64(8) || result.Submission.Detail[0]["machine_score"] != float64(3) || result.History[0].Detail[0]["final_score"] != float64(3) {
		t.Fatalf("history or machine score overwritten: %s", response.Body.String())
	}
}
