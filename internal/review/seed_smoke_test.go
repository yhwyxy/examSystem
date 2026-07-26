package review

import (
	"context"
	"testing"
)

// TestSeedSmoke 验证 seedSubmission 三段 INSERT 全部对得上 schema, 跑通即 schema 字段名:
//   exam_runs(id, paper_id, round_no, status, duration_minutes, public_token_hash,
//              snapshot_path, snapshot_hash, opened_at, closed_at, created_at)
//   exam_sessions(id, run_id, employee_id, name, session_token_hash,
//                 started_at, deadline_at, status, submitted_at, created_at, updated_at)
//   submissions(id, name, employee_id, paper_id, run_id, answers_json,
//              grading_detail_json, objective_score, subjective_score_machine,
//              subjective_score_final, total_score, review_status, grading_status,
//              grading_generation, submitted_at)
//
// 缺 TEST_DATABASE_URL 则 t.Skip.
func TestSeedSmoke(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	truncate(ctx, pool)

	id, err := seedSubmission(ctx, pool)
	if err != nil {
		t.Fatalf("seedSubmission: %v", err)
	}
	if id != 5001 {
		t.Fatalf("seed id=%d want 5001", id)
	}

	// 回读验证 submissions 行落位.
	var rs string
	if err := pool.QueryRow(ctx,
		`SELECT review_status FROM submissions WHERE id = $1`, id).Scan(&rs); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if rs != "reviewed" {
		t.Fatalf("review_status=%q want 'reviewed'", rs)
	}
	truncate(ctx, pool)
}
