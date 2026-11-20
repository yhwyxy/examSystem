package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestQuestionStatisticsQualifyCompositeChildren(t *testing.T) {
	pool := testSubmPool(t)
	id := setupSubmData(t, pool)
	ctx := context.Background()
	detail := `[
 {"question_id":"part","max_score":2,"machine_score":2,"final_score":2},
 {"question_id":"parent-a","sub_results":[{"question_id":"part","max_score":4,"machine_score":1,"final_score":2}]},
 {"id":"parent-b","sub_results":[{"id":"part","max_score":4,"machine_score":4,"final_score":3}]},
 {"question_id":"parent-c","sub_results":[{"question_id":"parent-c/part","max_score":4,"machine_score":0,"final_score":1}]},
 {"question_id":"scalar-container","sub_results":"legacy","max_score":2,"machine_score":1,"final_score":1},
 {"question_id":"null-container","sub_results":null,"max_score":2,"machine_score":1,"final_score":1},
 {"question_id":"empty-container","sub_results":[],"max_score":2,"machine_score":1,"final_score":2},
 {"question_id":"","id":"fallback-id","max_score":2,"machine_score":1,"final_score":1},
 {"sub_results":[{"question_id":"unknown-parent-part","max_score":4,"machine_score":4,"final_score":4}]}
 ]`
	if _, err := pool.Exec(ctx, `UPDATE submissions SET reviewed_at=now(),grading_detail_json=$1 WHERE id=$2`, []byte(detail), id); err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	newSubmRouter(t, pool, nil).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/admin/stats/breakdown?run_id=run-1", nil))
	var result statisticsBreakdownResponse
	if recorder.Code != http.StatusOK || json.Unmarshal(recorder.Body.Bytes(), &result) != nil {
		t.Fatalf("statistics: %d %s", recorder.Code, recorder.Body.String())
	}
	want := map[string][2]float64{"part": {1, 1}, "parent-a/part": {.25, .5}, "parent-b/part": {1, .75}, "parent-c/part": {0, .25}, "empty-container": {.5, 1}, "scalar-container": {.5, .5}, "null-container": {.5, .5}, "fallback-id": {.5, .5}}
	if len(result.ByQuestion) != len(want) {
		t.Fatalf("merged or missing questions: %+v", result.ByQuestion)
	}
	for _, item := range result.ByQuestion {
		rates, ok := want[item.QuestionID]
		if !ok || item.AttemptCount != 1 || item.ReviewedCount != 1 || item.MachineScoreRate == nil || *item.MachineScoreRate != rates[0] || item.ReviewedScoreRate == nil || *item.ReviewedScoreRate != rates[1] {
			t.Fatalf("incorrect independent rate: %+v", item)
		}
	}
	// Incomplete manual review must keep machine rates without counting final
	// rates as reviewed, including every composite child.
	if _, err := pool.Exec(ctx, `UPDATE submissions SET review_status='partial_manual' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	recorder = httptest.NewRecorder()
	newSubmRouter(t, pool, nil).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/admin/stats/breakdown?run_id=run-1", nil))
	if recorder.Code != http.StatusOK || json.Unmarshal(recorder.Body.Bytes(), &result) != nil {
		t.Fatalf("partial review statistics: %d %s", recorder.Code, recorder.Body.String())
	}
	for _, item := range result.ByQuestion {
		if item.AttemptCount != 1 || item.ReviewedCount != 0 || item.ReviewedScoreRate != nil {
			t.Fatalf("partial review counted: %+v", item)
		}
	}
}

func TestStatisticsSnapshotSurvivesConcurrentPublication(t *testing.T) {
	pool := testSubmPool(t)
	id := setupSubmData(t, pool)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `UPDATE submissions SET reviewed_at=now() WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	lock, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Rollback(ctx)
	if _, err := lock.Exec(ctx, `LOCK TABLE score_publication_batches IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	var blocker int
	if err := lock.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&blocker); err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	done := make(chan struct{})
	router := newSubmRouter(t, pool, nil)
	go func() {
		defer close(done)
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/admin/stats/breakdown?run_id=run-1", nil).WithContext(ctx))
	}()
	for {
		var waiting bool
		if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)))`, blocker).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case <-done:
			t.Fatalf("report did not wait: %d %s", recorder.Code, recorder.Body.String())
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	// Main and question aggregates have already been read. Commit a new final
	// score and its immutable published version while the last query waits.
	if _, err := lock.Exec(ctx, `UPDATE submissions SET subjective_score_final=5,total_score=9 WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	var similarity, anomaly, published int64
	if err := lock.QueryRow(ctx, `INSERT INTO answer_similarity_batches(run_id,snapshot_hash,answer_set_hash,rule_version,status)
 VALUES ('run-1',repeat('a',64),repeat('b',64),'lexical-overlap-v1','completed') RETURNING id`).Scan(&similarity); err != nil {
		t.Fatal(err)
	}
	if err := lock.QueryRow(ctx, `INSERT INTO score_anomaly_batches(run_id,similarity_batch_id,snapshot_hash,answer_set_hash,grading_set_hash,rule_version,status)
 VALUES ('run-1',$1,repeat('a',64),repeat('b',64),repeat('c',64),'score-consistency-v2','completed') RETURNING id`, similarity).Scan(&anomaly); err != nil {
		t.Fatal(err)
	}
	if err := lock.QueryRow(ctx, `INSERT INTO score_publication_batches(run_id,paper_id,paper_name,version,actor,snapshot_hash,answer_set_hash,grading_set_hash,similarity_batch_id,anomaly_batch_id,submission_count)
 VALUES ('run-1','smoke-1','专业1',1,'synthetic-publisher',repeat('a',64),repeat('b',64),repeat('c',64),$1,$2,1) RETURNING id`, similarity, anomaly).Scan(&published); err != nil {
		t.Fatal(err)
	}
	if _, err := lock.Exec(ctx, `INSERT INTO score_publication_grades(batch_id,submission_id,participant_name,employee_id,department,grading_generation,review_status,objective_score,subjective_machine,subjective_final,total_score,max_score)
 VALUES ($1,$2,'tester','emp-1','部门1',1,'reviewed',4,2,5,9,10)`, published, id); err != nil {
		t.Fatal(err)
	}
	if err := lock.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	var result statisticsBreakdownResponse
	if recorder.Code != http.StatusOK || json.Unmarshal(recorder.Body.Bytes(), &result) != nil {
		t.Fatalf("report: %d %s", recorder.Code, recorder.Body.String())
	}
	if len(result.ByRound) != 1 || result.ByRound[0].ReviewedFinal.Average == nil || *result.ByRound[0].ReviewedFinal.Average != 7 || result.ByRound[0].PublishedReady {
		t.Fatalf("mixed old aggregates with new publication: %+v", result.ByRound)
	}
	recorder = httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/admin/stats/breakdown?run_id=run-1", nil))
	if recorder.Code != http.StatusOK || json.Unmarshal(recorder.Body.Bytes(), &result) != nil {
		t.Fatalf("new report: %d %s", recorder.Code, recorder.Body.String())
	}
	if len(result.ByRound) != 1 || result.ByRound[0].ReviewedFinal.Average == nil || *result.ByRound[0].ReviewedFinal.Average != 9 || !result.ByRound[0].PublishedReady || result.ByRound[0].Published.Average == nil || *result.ByRound[0].Published.Average != 9 {
		t.Fatalf("new snapshot not visible: %+v", result.ByRound)
	}
}
