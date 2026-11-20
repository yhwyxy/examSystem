package review

import (
	"context"
	"errors"
	"github.com/yhwyxy/examSystem/internal/jobs"
	"testing"
)

func TestRegradeAuditIsAtomicWithQueueAndPreservesPriorScores(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	id, err := seedSubmission(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	svc := NewService(pool, jobs.NewRepository())
	if _, err := NewService(pool, nil).Regrade(ctx, RegradeInput{SubmissionID: id, Reason: "请求重新核对评分"}); !errors.Is(err, ErrQueueUnavailable) {
		t.Fatal(err)
	}
	if _, err := svc.Regrade(ctx, RegradeInput{SubmissionID: id, Actor: "reviewer-a", Reason: " "}); !errors.Is(err, ErrReasonRequired) {
		t.Fatal(err)
	}
	// Force the real queue insertion to fail; generation and audit must roll back.
	if _, err := pool.Exec(ctx, `CREATE FUNCTION refuse_test_job() RETURNS trigger AS $$ BEGIN RAISE EXCEPTION 'synthetic queue failure'; END; $$ LANGUAGE plpgsql;
 CREATE TRIGGER refuse_test_job BEFORE INSERT ON grading_jobs FOR EACH ROW EXECUTE FUNCTION refuse_test_job()`); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Regrade(ctx, RegradeInput{SubmissionID: id, Actor: "reviewer-a", Reason: "核对后重新评分"}); err == nil {
		t.Fatal("queue failure unexpectedly accepted")
	}
	var generation int64
	var audits int
	if err := pool.QueryRow(ctx, "SELECT grading_generation FROM submissions WHERE id=$1", id).Scan(&generation); err != nil || generation != 1 {
		t.Fatalf("generation=%d err=%v", generation, err)
	}
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM regrade_audit").Scan(&audits); err != nil || audits != 0 {
		t.Fatalf("failed regrade audits=%d err=%v", audits, err)
	}
	if _, err := pool.Exec(ctx, "DROP TRIGGER refuse_test_job ON grading_jobs"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Regrade(ctx, RegradeInput{SubmissionID: id, Actor: "reviewer-a", Reason: "核对后重新评分"}); err != nil {
		t.Fatal(err)
	}
	var actor, before, after string
	var from, to int64
	var oldTotal, newTotal float64
	if err := pool.QueryRow(ctx, `SELECT actor,from_generation,to_generation,before_state->>'grading_status',after_state->>'grading_status',
 (before_state->>'total_score')::float8,(after_state->>'total_score')::float8 FROM regrade_audit WHERE submission_id=$1`, id).Scan(&actor, &from, &to, &before, &after, &oldTotal, &newTotal); err != nil {
		t.Fatal(err)
	}
	if actor != "reviewer-a" || from != 1 || to != 2 || before != "done" || after != "pending" || oldTotal != 7 || newTotal != 7 {
		t.Fatalf("incorrect audit %s %d/%d %s/%s totals %g/%g", actor, from, to, before, after, oldTotal, newTotal)
	}
	if _, err := pool.Exec(ctx, "UPDATE regrade_audit SET actor='forged' WHERE submission_id=$1", id); err == nil {
		t.Fatal("audit could be rewritten")
	}
	var queued int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM grading_jobs WHERE submission_id=$1 AND generation=2 AND status='queued'", id).Scan(&queued); err != nil || queued != 1 {
		t.Fatalf("queued=%d err=%v", queued, err)
	}
}
