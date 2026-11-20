package answersimilarity

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/yhwyxy/examSystem/internal/db"
	"github.com/yhwyxy/examSystem/internal/papers"
	"github.com/yhwyxy/examSystem/internal/testutil"
)

const answerA = "测量结果显示温度升高导致反应速度增加，同时样本边缘出现蓝色条纹，说明设备校准存在明显偏差。"
const answerB = "研究发现温度升高导致反应速度增加，同时样本边缘出现蓝色条纹，导致最终结果不可信。"

type fixedSemanticEmbedder struct{ version string }

func (embedder fixedSemanticEmbedder) Version() string {
	if embedder.version == "" {
		return "test-semantic-v1"
	}
	return embedder.version
}
func (fixedSemanticEmbedder) Embed(_ context.Context, inputs []string) ([][]float64, error) {
	vectors := make([][]float64, len(inputs))
	for index, input := range inputs {
		if strings.Contains(input, "标准答案") {
			vectors[index] = []float64{0, 1}
		} else {
			vectors[index] = []float64{1, 0}
		}
	}
	return vectors, nil
}

func TestServiceQueueAnalysisAndAuditedReview(t *testing.T) {
	pool, rawPool := newSimilarityPool(t)
	defer pool()
	ctx := context.Background()
	snapshotPath, snapshotHash := writeSimilaritySnapshot(t, map[string]any{
		"questions": []any{
			map[string]any{"id": "q1", "type": "essay", "scoring_mode": "text", "question": "解释实验结果", "answer": "标准答案说明实验因素。"},
			map[string]any{"id": "q2", "type": "essay", "scoring_mode": "text", "question": "分析测量误差", "answer": "标准答案说明其他因素。"},
			map[string]any{"id": "code", "type": "short_answer", "scoring_mode": "code", "code_language": "python", "answers_by_language": map[string]any{"python": ""}, "question": "实现计算函数"},
			map[string]any{"id": "composite", "type": "composite", "question": "复合题", "subquestions": []any{
				map[string]any{"id": "part", "type": "short_answer", "scoring_mode": "text", "question": "说明设备偏差"},
			}},
		},
	})
	seedSimilarityRun(t, rawPool, snapshotPath, snapshotHash, map[string]any{
		"q1":        answerA,
		"q2":        answerA,
		"code":      map[string]any{"answer": "def calculate_value(sample_value):\n    return sample_value * 37 + 12", "language": "python"},
		"composite": map[string]any{"part": map[string]any{"answer": answerA}},
	}, map[string]any{
		"q1":        answerB,
		"q2":        answerB,
		"code":      map[string]any{"answer": "def calculate_value(sample_value): # equivalent format\n  return sample_value*37+12", "language": "python"},
		"composite": map[string]any{"part": map[string]any{"answer": answerB}},
	})

	service := NewService(rawPool, nil).WithEmbedderLoader(func(context.Context) (Embedder, error) {
		return fixedSemanticEmbedder{}, nil
	})
	first, created, err := service.Enqueue(ctx, "run-sim")
	if err != nil || !created || first.Status != "queued" || first.RuleVersion != "test-semantic-v1" {
		t.Fatalf("first enqueue batch=%+v created=%v err=%v", first, created, err)
	}
	second, created, err := service.Enqueue(ctx, "run-sim")
	if err != nil || created || second.ID != first.ID {
		t.Fatalf("duplicate enqueue batch=%+v created=%v err=%v", second, created, err)
	}
	worked, err := service.ProcessNext(ctx)
	if err != nil || !worked {
		t.Fatalf("ProcessNext worked=%v err=%v", worked, err)
	}
	currentTx, err := rawPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	currentBatch, err := service.CurrentCompletedBatch(ctx, currentTx, "run-sim")
	_ = currentTx.Rollback(ctx)
	if err != nil || currentBatch.ID != first.ID {
		t.Fatalf("current completed batch=%+v err=%v", currentBatch, err)
	}
	page, err := service.GetBatchPage(ctx, first.ID, 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	if page.Batch.Status != "completed" || page.Batch.SubmissionCount != 2 || page.Batch.QuestionCount != 4 || page.Batch.FindingCount != 4 {
		t.Fatalf("batch completion counts/status incorrect: %+v", page.Batch)
	}
	if len(page.Findings) != 4 {
		t.Fatalf("got %d findings, want text, cross-question, code, composite findings", len(page.Findings))
	}
	var textFinding, codeFinding, compositeFinding *FindingRecord
	for i := range page.Findings {
		item := &page.Findings[i]
		switch item.QuestionID {
		case "q1":
			textFinding = item
		case "code":
			codeFinding = item
		case "composite/part":
			compositeFinding = item
		}
	}
	if textFinding == nil || textFinding.ReviewState != "needs_review" || textFinding.LeftAnswer == "" || textFinding.RightAnswer == "" || textFinding.SemanticSimilarity < 0.99 {
		t.Fatalf("text finding does not retain review evidence: %+v", textFinding)
	}
	if !contains(textFinding.RelatedQuestionIDs, "q1") || !contains(textFinding.RelatedQuestionIDs, "q2") || textFinding.RiskLevel != "high" {
		t.Fatalf("cross-question evidence was not linked: %+v", textFinding)
	}
	if codeFinding == nil || compositeFinding == nil || compositeFinding.QuestionLabel == "" {
		t.Fatalf("code/composite normalization or labels missing; findings=%+v", page.Findings)
	}

	updated, err := service.Review(ctx, ReviewInput{FindingID: textFinding.ID, ExpectedRevision: 0,
		State: "confirmed_abnormal", Note: "两份答案共享非标准表述。", Actor: "reviewer-1"})
	if err != nil || updated.ReviewState != "confirmed_abnormal" || updated.ReviewRevision != 1 || len(updated.ReviewHistory) != 1 {
		t.Fatalf("review update=%+v err=%v", updated, err)
	}
	if _, err := service.Review(ctx, ReviewInput{FindingID: textFinding.ID, ExpectedRevision: 0,
		State: "normal_consistent", Note: "stale update"}); !errors.Is(err, ErrReviewRevision) {
		t.Fatalf("stale review err=%v, want revision conflict", err)
	}
	startReviews := make(chan struct{})
	var reviewWait sync.WaitGroup
	reviewWait.Add(2)
	concurrentErrors := make([]error, 2)
	for index, state := range []string{"normal_consistent", "needs_review"} {
		go func(index int, state string) {
			defer reviewWait.Done()
			<-startReviews
			_, concurrentErrors[index] = service.Review(ctx, ReviewInput{FindingID: textFinding.ID,
				ExpectedRevision: 1, State: state, Note: "并发核查记录用于验证修订冲突。", Actor: "reviewer-race"})
		}(index, state)
	}
	close(startReviews)
	reviewWait.Wait()
	concurrentSuccess, concurrentConflict := 0, 0
	for _, err := range concurrentErrors {
		switch {
		case err == nil:
			concurrentSuccess++
		case errors.Is(err, ErrReviewRevision):
			concurrentConflict++
		default:
			t.Fatalf("unexpected concurrent review error: %v", err)
		}
	}
	if concurrentSuccess != 1 || concurrentConflict != 1 {
		t.Fatalf("concurrent review success=%d revision_conflict=%d, want one each", concurrentSuccess, concurrentConflict)
	}
	var totalScore float64
	leftSubmissionID, _ := strconv.ParseInt(textFinding.LeftSubmissionID, 10, 64)
	if err := rawPool.QueryRow(ctx, `SELECT total_score FROM submissions WHERE id=$1`, leftSubmissionID).Scan(&totalScore); err != nil {
		t.Fatal(err)
	}
	if _, err := rawPool.Exec(ctx, `UPDATE submissions SET answers_json=jsonb_set(answers_json,'{q1}','"changed answer"'::jsonb) WHERE id=$1`, leftSubmissionID); err != nil {
		t.Fatal(err)
	}
	staleTx, err := rawPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.CurrentCompletedBatch(ctx, staleTx, "run-sim")
	_ = staleTx.Rollback(ctx)
	if !errors.Is(err, ErrCurrentReportNotReady) {
		t.Fatalf("stale report error=%v, want ErrCurrentReportNotReady", err)
	}
	if totalScore != 0 {
		t.Fatalf("similarity review changed the grading result: total_score=%v", totalScore)
	}
}

func contains(items []string, target string) bool {
	for _, item := range items {
		if item == target {
			return true
		}
	}
	return false
}

func TestServiceFailureIsNotReportedAsNoFindings(t *testing.T) {
	cleanup, rawPool := newSimilarityPool(t)
	defer cleanup()
	ctx := context.Background()
	snapshotPath, snapshotHash := writeSimilaritySnapshot(t, map[string]any{"questions": []any{}})
	seedSimilarityRun(t, rawPool, snapshotPath, snapshotHash,
		map[string]any{"q": answerA}, map[string]any{"q": answerB})
	service := NewService(rawPool, func(string, string) (*papers.Snapshot, error) {
		return nil, errors.New("controlled snapshot read failure")
	})
	batch, created, err := service.Enqueue(ctx, "run-sim")
	if err != nil || !created {
		t.Fatalf("enqueue batch=%+v created=%v err=%v", batch, created, err)
	}
	worked, err := service.ProcessNext(ctx)
	if err != nil || !worked {
		t.Fatalf("ProcessNext worked=%v err=%v", worked, err)
	}
	page, err := service.GetBatchPage(ctx, batch.ID, 20, 0)
	if err != nil {
		t.Fatal(err)
	}
	if page.Batch.Status != "failed" || page.Batch.ErrorCode != "SNAPSHOT_INVALID" || page.Batch.FindingCount != 0 {
		t.Fatalf("failed batch was confused with an empty successful report: %+v", page.Batch)
	}
}

func TestEnqueueRequiresClosedCompleteRun(t *testing.T) {
	cleanup, rawPool := newSimilarityPool(t)
	defer cleanup()
	snapshotPath, snapshotHash := writeSimilaritySnapshot(t, map[string]any{"questions": []any{}})
	seedSimilarityRun(t, rawPool, snapshotPath, snapshotHash, map[string]any{"q": answerA}, map[string]any{"q": answerB})
	if _, err := rawPool.Exec(context.Background(), `UPDATE exam_runs SET status='open' WHERE id='run-sim'`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := NewService(rawPool, nil).Enqueue(context.Background(), "run-sim"); !errors.Is(err, ErrRunNotClosed) {
		t.Fatalf("open run enqueue err=%v, want ErrRunNotClosed", err)
	}
	if _, err := rawPool.Exec(context.Background(), `UPDATE exam_runs SET status='closed' WHERE id='run-sim'`); err != nil {
		t.Fatal(err)
	}
	if _, err := rawPool.Exec(context.Background(), `UPDATE exam_sessions SET status='active' WHERE run_id='run-sim'`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := NewService(rawPool, nil).Enqueue(context.Background(), "run-sim"); !errors.Is(err, ErrRunIncomplete) {
		t.Fatalf("run with active session enqueue err=%v, want ErrRunIncomplete", err)
	}
}

func TestServiceReclaimsExpiredLeaseAndKeepsOneFinalReport(t *testing.T) {
	cleanup, pool := newSimilarityPool(t)
	defer cleanup()
	ctx := context.Background()
	snapshotPath, snapshotHash := writeSimilaritySnapshot(t, map[string]any{"questions": []any{
		map[string]any{"id": "choice", "type": "single_choice", "scoring_mode": "text", "question": "选择答案", "answer": "A"},
	}})
	seedSimilarityRun(t, pool, snapshotPath, snapshotHash, map[string]any{"q": answerA}, map[string]any{"q": answerB})
	service := NewService(pool, nil)
	batch, created, err := service.Enqueue(ctx, "run-sim")
	if err != nil || !created {
		t.Fatalf("enqueue batch=%+v created=%v err=%v", batch, created, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE answer_similarity_batches
SET status='running',attempt_count=1,lease_owner='stopped-worker',lease_until=now()-interval '1 second'
WHERE id=$1`, batch.ID); err != nil {
		t.Fatal(err)
	}
	worked, err := service.ProcessNext(ctx)
	if err != nil || !worked {
		t.Fatalf("recovered ProcessNext worked=%v err=%v", worked, err)
	}
	page, err := service.GetBatchPage(ctx, batch.ID, 20, 0)
	if err != nil {
		t.Fatal(err)
	}
	if page.Batch.Status != "completed" || page.Batch.AttemptCount != 2 {
		t.Fatalf("expired batch was not reclaimed exactly once: %+v", page.Batch)
	}
}

func TestServiceFailsWhenBoundAnswerVersionChanges(t *testing.T) {
	cleanup, pool := newSimilarityPool(t)
	defer cleanup()
	ctx := context.Background()
	snapshotPath, snapshotHash := writeSimilaritySnapshot(t, map[string]any{"questions": []any{}})
	seedSimilarityRun(t, pool, snapshotPath, snapshotHash, map[string]any{"q": answerA}, map[string]any{"q": answerB})
	service := NewService(pool, nil)
	batch, _, err := service.Enqueue(ctx, "run-sim")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE submissions SET answers_json='{"q":"changed after queue"}'::jsonb WHERE run_id='run-sim' AND employee_id='employee-1'`); err != nil {
		t.Fatal(err)
	}
	worked, err := service.ProcessNext(ctx)
	if err != nil || !worked {
		t.Fatalf("ProcessNext worked=%v err=%v", worked, err)
	}
	page, err := service.GetBatchPage(ctx, batch.ID, 20, 0)
	if err != nil {
		t.Fatal(err)
	}
	if page.Batch.Status != "failed" || page.Batch.ErrorCode != "ANSWER_SET_CHANGED" {
		t.Fatalf("old batch overwrote a changed answer set: %+v", page.Batch)
	}
}

func TestServiceFailsQueuedSemanticBatchWhenProviderVersionChanges(t *testing.T) {
	cleanup, pool := newSimilarityPool(t)
	defer cleanup()
	ctx := context.Background()
	snapshotPath, snapshotHash := writeSimilaritySnapshot(t, map[string]any{"questions": []any{
		map[string]any{"id": "q1", "type": "essay", "scoring_mode": "text", "question": "分析", "answer": "标准答案"},
	}})
	seedSimilarityRun(t, pool, snapshotPath, snapshotHash, map[string]any{"q1": answerA}, map[string]any{"q1": answerB})
	provider := fixedSemanticEmbedder{}
	service := NewService(pool, nil).WithEmbedderLoader(func(context.Context) (Embedder, error) { return provider, nil })
	batch, created, err := service.Enqueue(ctx, "run-sim")
	if err != nil || !created || batch.RuleVersion != "test-semantic-v1" {
		t.Fatalf("semantic batch=%+v created=%v err=%v", batch, created, err)
	}
	provider.version = "test-semantic-v2"
	worked, err := service.ProcessNext(ctx)
	if err != nil || !worked {
		t.Fatalf("ProcessNext worked=%v err=%v", worked, err)
	}
	page, err := service.GetBatchPage(ctx, batch.ID, 20, 0)
	if err != nil {
		t.Fatal(err)
	}
	if page.Batch.Status != "failed" || page.Batch.ErrorCode != "EMBEDDING_CONFIG_CHANGED" {
		t.Fatalf("batch was analyzed under a different provider version: %+v", page.Batch)
	}
}

func newSimilarityPool(t *testing.T) (func(), *pgxpool.Pool) {
	t.Helper()
	pool, cleanup := testutil.NewSchema(t)
	if _, err := db.Migrate(context.Background(), pool); err != nil {
		t.Fatalf("migrate test schema: %v", err)
	}
	return cleanup, pool
}

func writeSimilaritySnapshot(t *testing.T, doc map[string]any) (string, string) {
	t.Helper()
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "snapshot.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	return path, hex.EncodeToString(sum[:])
}

func seedSimilarityRun(t *testing.T, pool *pgxpool.Pool, snapshotPath, snapshotHash string, first, second map[string]any) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO exam_runs
(id,paper_id,round_no,public_token_hash,status,duration_minutes,snapshot_path,snapshot_hash,is_legacy,opened_at,closed_at,created_at)
VALUES ('run-sim','paper-sim',1,'public-token-hash','closed',60,$1,$2,false,now(),now(),now())`, snapshotPath, snapshotHash); err != nil {
		t.Fatal(err)
	}
	answers := []map[string]any{first, second}
	for index, value := range answers {
		employeeID := "employee-" + strconv.Itoa(index+1)
		name := "Participant " + strconv.Itoa(index+1)
		if _, err := pool.Exec(ctx, `INSERT INTO exam_sessions
(id,run_id,employee_id,name,session_token_hash,started_at,deadline_at,status,created_at,updated_at)
VALUES ($1,'run-sim',$2,$3,$4,now(),now(),'submitted',now(),now())`, "session-"+strconv.Itoa(index+1), employeeID, name, "token-"+strconv.Itoa(index+1)); err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO submissions
(name,employee_id,paper_id,run_id,answers_json,grading_status,submitted_at)
VALUES ($1,$2,'paper-sim','run-sim',$3::jsonb,'done',now())`, name, employeeID, string(encoded)); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM submissions WHERE run_id='run-sim'`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("seeded submissions=%d err=%v", count, err)
	}
}

func TestEnqueueRetriesConcurrentRunUpdateWithFreshSnapshot(t *testing.T) {
	cleanup, pool := newSimilarityPool(t)
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	path, hash := writeSimilaritySnapshot(t, map[string]any{"questions": []any{map[string]any{"id": "q", "type": "essay"}}})
	seedSimilarityRun(t, pool, path, hash, map[string]any{"q": answerA}, map[string]any{"q": answerB})
	config := pool.Config().Copy()
	appName := "enqueue-race-" + config.ConnConfig.RuntimeParams["search_path"]
	config.ConnConfig.RuntimeParams["application_name"] = appName
	enqueuePool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer enqueuePool.Close()
	writer, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback(context.Background())
	if _, err := writer.Exec(ctx, "UPDATE exam_runs SET closed_at=now() WHERE id='run-sim'"); err != nil {
		t.Fatal(err)
	}
	type result struct {
		batch   Batch
		created bool
		err     error
	}
	finished := make(chan result, 1)
	go func() {
		batch, created, err := NewService(enqueuePool, nil).Enqueue(ctx, "run-sim")
		finished <- result{batch, created, err}
	}()
	// Confirm the first repeatable-read transaction is actually waiting for
	// our uncommitted run update before committing it; no timing-only race.
	deadline := time.Now().Add(3 * time.Second)
	waiting := false
	for time.Now().Before(deadline) {
		if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity
WHERE application_name=$1 AND wait_event_type='Lock')`, appName).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !waiting {
		t.Fatal("enqueue did not reach the expected database lock")
	}
	if err := writer.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case out := <-finished:
		if out.err != nil || !out.created || out.batch.Status != "queued" {
			t.Fatalf("enqueue after concurrent run update: %+v", out)
		}
		var count int
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM answer_similarity_batches").Scan(&count); err != nil || count != 1 {
			t.Fatalf("batch count=%d err=%v", count, err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}
