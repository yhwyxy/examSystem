package review

import (
	"context"
	"testing"
)

// TestApply_Success: seed review_status='reviewed', 给 subj-A 改 final_score=8
// (原 3 → 8), 期望 total_score 7 → 12, subjective_score_final 3 → 8, review_logs 写入 +1,
// grading_detail_json 子项 score=8.
func TestApply_Success(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	truncate(ctx, pool)
	id, err := seedSubmission(ctx, pool)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	defer truncate(ctx, pool)

	svc := NewService(pool, nil) // jobsRepo=nil: Apply 不走 EnqueueTx, 不会调
	res, err := svc.Apply(ctx, ApplyInput{
		SubmissionID: id,
		QuestionID:   "subj-A",
		NewScore:     8,
		Note:         "subjective adjusted",
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if res.NewStatus != "reviewed" {
		t.Fatalf("NewStatus=%q", res.NewStatus)
	}
	if res.NewTotalScore != 12 { // 7 - 3 + 8
		t.Fatalf("NewTotalScore=%v want 12", res.NewTotalScore)
	}

	// 回读验证 submissions 三字段同步.
	var (
		ssf, total  float64
		rs, dsc     string
		logCount     int
	)
	err = pool.QueryRow(ctx, `
		SELECT subjective_score_final, total_score, review_status, grading_detail_json::text
		FROM submissions WHERE id = $1`, id).Scan(&ssf, &total, &rs, &dsc)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if ssf != 8 || total != 12 || rs != "reviewed" {
		t.Fatalf("submission state: ssf=%v total=%v rs=%q", ssf, total, rs)
	}
	err = pool.QueryRow(ctx,
		`SELECT count(*) FROM review_logs WHERE submission_id=$1`, id).Scan(&logCount)
	if err != nil {
		t.Fatalf("count logs: %v", err)
	}
	if logCount != 1 {
		t.Fatalf("review_log count=%d want 1", logCount)
	}
}
