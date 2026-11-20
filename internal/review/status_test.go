package review

import (
	"context"
	"encoding/json"
	"testing"
)

func TestAggregateReviewStatus(t *testing.T) {
	tests := []struct {
		name, detail, want string
	}{
		{"automatic", `[{"type":"short_answer","review_status":"high_confidence","machine_score":10}]`, "auto_scored"},
		{"unfinished other question", `[{"manually_reviewed":true},{"need_manual_review":true}]`, "need_review"},
		{"manual resolves machine flags", `[{"manually_reviewed":true,"need_manual_review":true,"detail":{"low_confidence":true}}]`, "reviewed"},
		{"partly manual", `[{"manually_reviewed":true},{"review_status":"high_confidence"}]`, "partial_manual"},
		{"composite unfinished", `[{"manually_reviewed":true,"sub_results":[{"manually_reviewed":true},{"detail":{"need_manual_review":true}}]}]`, "need_review"},
		{"composite complete", `[{"sub_results":[{"manually_reviewed":true,"low_confidence":true},{"manually_reviewed":true}]}]`, "reviewed"},
		{"objective and automatic", `[{"type":"single_choice","review_status":"auto_scored"},{"review_status":"high_confidence"}]`, "auto_scored"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := aggregateReviewStatus([]byte(tt.detail))
			if err != nil || got != tt.want {
				t.Fatalf("got %q, %v; want %q", got, err, tt.want)
			}
		})
	}
}

func TestApplyDoesNotApproveOtherQuestions(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	truncate(ctx, pool)
	defer truncate(ctx, pool)
	id, err := seedSubmission(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	detail := `[ {"question_id":"first","type":"short_answer","max_score":5,"machine_score":3,"score":3,"final_score":3,"need_manual_review":true},
		{"question_id":"second","type":"essay","max_score":5,"machine_score":0,"score":0,"final_score":0,"need_manual_review":true} ]`
	if _, err := pool.Exec(ctx, `UPDATE submissions SET grading_detail_json=$1,review_status='need_review' WHERE id=$2`, []byte(detail), id); err != nil {
		t.Fatal(err)
	}
	svc := NewService(pool, nil)
	res, err := svc.Apply(ctx, ApplyInput{SubmissionID: id, QuestionID: "first", NewScore: 4, Note: "确认第一题评分"})
	if err != nil || res.NewStatus != "need_review" {
		t.Fatalf("first review: %+v, %v", res, err)
	}
	var raw []byte
	if err := pool.QueryRow(ctx, `SELECT grading_detail_json FROM submissions WHERE id=$1`, id).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var items []map[string]any
	if err := json.Unmarshal(raw, &items); err != nil {
		t.Fatal(err)
	}
	if items[0]["score"] != float64(3) || items[0]["machine_score"] != float64(3) || items[0]["final_score"] != float64(4) {
		t.Fatalf("machine scores overwritten: %s", raw)
	}
	res, err = svc.Apply(ctx, ApplyInput{SubmissionID: id, QuestionID: "second", NewScore: 2, Note: "确认第二题评分"})
	if err != nil || res.NewStatus != "reviewed" {
		t.Fatalf("second review: %+v, %v", res, err)
	}
}

func TestSubquestionReviewKeepsSiblingPending(t *testing.T) {
	raw := []byte(`[{"question_id":"parent","type":"composite","score":3,"final_score":3,"sub_results":[{"sub_question_id":"a","final_score":3,"max_score":5,"need_manual_review":true},{"sub_question_id":"b","final_score":0,"max_score":5,"need_manual_review":true}]}]`)
	updated, err := setDetailScoreInPlace(raw, "parent", "a", 4)
	if err != nil {
		t.Fatal(err)
	}
	if status, _ := aggregateReviewStatus(updated); status != "need_review" {
		t.Fatal(status)
	}
	updated, err = setDetailScoreInPlace(updated, "parent", "b", 2)
	if err != nil {
		t.Fatal(err)
	}
	if status, _ := aggregateReviewStatus(updated); status != "reviewed" {
		t.Fatal(status)
	}
}

func TestSubquestionReviewReplacesWholeQuestionOverride(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	id, err := seedSubmission(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	detail := `[{"question_id":"parent","type":"composite","max_score":10,"machine_score":2,"final_score":7,"review_action":"whole_question","manually_reviewed":true,"sub_results":[{"sub_question_id":"a","type":"short_answer","final_score":1,"max_score":5},{"sub_question_id":"b","type":"short_answer","final_score":1,"max_score":5}]}]`
	if _, err := pool.Exec(ctx, `UPDATE submissions SET grading_detail_json=$1,subjective_score_final=7,total_score=11 WHERE id=$2`, []byte(detail), id); err != nil {
		t.Fatal(err)
	}
	res, err := NewService(pool, nil).Apply(ctx, ApplyInput{SubmissionID: id, QuestionID: "parent", SubQID: "a", NewScore: 4, Note: "核对子题a评分", Actor: "reviewer-sub"})
	if err != nil {
		t.Fatal(err)
	}
	if res.NewTotalScore != 9 {
		t.Fatalf("total=%g, want objective 4 + subquestion total 5", res.NewTotalScore)
	}
	var subjective float64
	if err := pool.QueryRow(ctx, `SELECT subjective_score_final FROM submissions WHERE id=$1`, id).Scan(&subjective); err != nil {
		t.Fatal(err)
	}
	if subjective != 5 {
		t.Fatalf("subjective=%g, want 5", subjective)
	}
	var actor, sub string
	var generation int64
	if err := pool.QueryRow(ctx, "SELECT actor,sub_question_id,grading_generation FROM review_logs WHERE submission_id=$1", id).Scan(&actor, &sub, &generation); err != nil || actor != "reviewer-sub" || sub != "a" || generation != 1 {
		t.Fatalf("audit actor=%q sub=%q generation=%d err=%v", actor, sub, generation, err)
	}
}
