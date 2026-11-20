package answersimilarity

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestDetectionLeaseAttemptFence(t *testing.T) {
	cleanup, pool := newSimilarityPool(t)
	defer cleanup()
	path, hash := writeSimilaritySnapshot(t, map[string]any{"questions": []any{}})
	seedSimilarityRun(t, pool, path, hash, map[string]any{"q": answerA}, map[string]any{"q": answerB})
	service := NewService(pool, nil)
	ctx := context.Background()
	if _, _, err := service.Enqueue(ctx, "run-sim"); err != nil {
		t.Fatal(err)
	}
	claim, err := service.claim(ctx)
	if err != nil || claim == nil {
		t.Fatalf("claim: %v", err)
	}
	old := *claim
	if _, err := pool.Exec(ctx, `UPDATE answer_similarity_batches SET lease_until=clock_timestamp()-interval '1 second' WHERE id=$1`, claim.ID); err != nil {
		t.Fatal(err)
	}
	// Still owned by this process, but expired writes must fail before reclaim.
	if err := service.complete(ctx, *claim, nil, 0, 2); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("expired complete: %v", err)
	}
	for _, retry := range []bool{true, false} {
		if err := service.fail(ctx, *claim, "STALE", retry); !errors.Is(err, ErrLeaseLost) {
			t.Fatalf("expired fail: %v", err)
		}
	}
	claim, err = service.claim(ctx)
	if err != nil || claim == nil || claim.AttemptCount != old.AttemptCount+1 {
		t.Fatalf("same-process reclaim: %+v %v", claim, err)
	}
	// An unexpired second lease with the same owner must reject attempt one.
	fresh := *claim
	claim = &old
	if err := service.complete(ctx, *claim, nil, 0, 2); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("old attempt completed fresh lease: %v", err)
	}
	if err := service.fail(ctx, old, "STALE", true); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("old attempt requeued fresh lease: %v", err)
	}
	claim = &fresh
	if err := service.complete(ctx, *claim, nil, 0, 2); err != nil {
		t.Fatalf("fresh complete: %v", err)
	}
	if err := service.fail(ctx, old, "STALE", false); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("old attempt overwrote completed result: %v", err)
	}
	page, err := service.GetBatchPage(ctx, fresh.ID, 100, 0)
	if err != nil || page.Batch.Status != "completed" || page.Batch.AttemptCount != 2 || page.Batch.ErrorCode != "" {
		t.Fatalf("final report: %+v %v", page, err)
	}
}
func TestDetectionLeaseExpiresWhileWaitingForLock(t *testing.T) {
	cleanup, pool := newSimilarityPool(t)
	defer cleanup()
	path, hash := writeSimilaritySnapshot(t, map[string]any{"questions": []any{}})
	seedSimilarityRun(t, pool, path, hash, map[string]any{"q": answerA}, map[string]any{"q": answerB})
	service := NewService(pool, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, _, err := service.Enqueue(ctx, "run-sim"); err != nil {
		t.Fatal(err)
	}
	claim, err := service.claim(ctx)
	if err != nil || claim == nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE answer_similarity_batches SET lease_until=clock_timestamp()+interval '2 seconds' WHERE id=$1`, claim.ID); err != nil {
		t.Fatal(err)
	}
	lock, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Rollback(ctx)
	var blocker int
	if err := lock.QueryRow(ctx, `SELECT pg_backend_pid() FROM answer_similarity_batches WHERE id=$1 FOR UPDATE`, claim.ID).Scan(&blocker); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- service.complete(ctx, *claim, nil, 0, 2) }()
	// Observe the real lock wait, then hold it past the lease deadline. The
	// writer's transaction started while valid; transaction now() would be stale.
	for {
		var waiting bool
		if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)))`, blocker).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("writer did not wait: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	var valid bool
	if err := pool.QueryRow(ctx, `SELECT lease_until>clock_timestamp() FROM answer_similarity_batches WHERE id=$1`, claim.ID).Scan(&valid); err != nil || !valid {
		t.Fatalf("lease was not live when writer blocked: %v %v", valid, err)
	}
	time.Sleep(2100 * time.Millisecond)
	if err := lock.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("expired waiting writer accepted: %v", err)
	}
	page, err := service.GetBatchPage(ctx, claim.ID, 100, 0)
	if err != nil || page.Batch.Status != "running" || page.Batch.FindingCount != 0 {
		t.Fatalf("expired writer altered result: %+v %v", page, err)
	}
}

func TestDetectionLeaseExpiresDuringPersistence(t *testing.T) {
	cleanup, pool := newSimilarityPool(t)
	defer cleanup()
	path, hash := writeSimilaritySnapshot(t, map[string]any{"questions": []any{}})
	seedSimilarityRun(t, pool, path, hash, map[string]any{"q": answerA}, map[string]any{"q": answerB})
	service := NewService(pool, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, _, err := service.Enqueue(ctx, "run-sim"); err != nil {
		t.Fatal(err)
	}
	claim, err := service.claim(ctx)
	if err != nil || claim == nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE answer_similarity_batches SET lease_until=clock_timestamp()+interval '2 seconds' WHERE id=$1`, claim.ID); err != nil {
		t.Fatal(err)
	}
	lock, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Rollback(ctx)
	if _, err := lock.Exec(ctx, `LOCK TABLE answer_similarity_findings IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	var blocker int
	if err := lock.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&blocker); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- service.complete(ctx, *claim, nil, 0, 2) }()
	for {
		var waiting bool
		if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)))`, blocker).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("persistence did not wait: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	var valid bool
	if err := pool.QueryRow(ctx, `SELECT lease_until>clock_timestamp() FROM answer_similarity_batches WHERE id=$1`, claim.ID).Scan(&valid); err != nil || !valid {
		t.Fatalf("not a live claim when persistence blocked: %v %v", valid, err)
	}
	time.Sleep(2100 * time.Millisecond)
	if err := lock.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("expired persistence committed: %v", err)
	}
	page, err := service.GetBatchPage(ctx, claim.ID, 100, 0)
	if err != nil || page.Batch.Status != "running" || page.Batch.FindingCount != 0 {
		t.Fatalf("partial stale report: %+v %v", page, err)
	}
}
