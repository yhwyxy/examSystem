package publication

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/yhwyxy/examSystem/internal/db"
	"github.com/yhwyxy/examSystem/internal/jobs"
	"github.com/yhwyxy/examSystem/internal/review"
	"github.com/yhwyxy/examSystem/internal/scoreanomaly"
	"github.com/yhwyxy/examSystem/internal/testutil"
)

func TestLegacyRunWithoutSnapshotIsNotEligibleForPublication(t *testing.T) {
	pool, cleanup := testutil.NewSchema(t)
	defer cleanup()
	ctx := context.Background()
	if _, err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO exam_runs(id,paper_id,round_no,status,duration_minutes,is_legacy,opened_at,closed_at,created_at)
VALUES ('legacy-no-snapshot','historical',1,'closed',60,true,now(),now(),now())`); err != nil {
		t.Fatal(err)
	}
	service := NewService(pool, scoreanomaly.NewService(pool))
	if _, created, err := service.Publish(ctx, PublishInput{RunID: "legacy-no-snapshot", Actor: "publisher"}); err != ErrRunNotReady || created {
		t.Fatalf("legacy run must be refused without a NULL scan error: created=%v err=%v", created, err)
	}
}

func TestPublishRequiresCurrentReportsResolvesBlockingAndCreatesCorrection(t *testing.T) {
	pool, cleanup := testutil.NewSchema(t)
	defer cleanup()
	if _, err := db.Migrate(context.Background(), pool); err != nil {
		t.Fatalf("migrate test schema: %v", err)
	}
	ctx := context.Background()
	ids := seedPublishRun(t, pool)
	anomalies := scoreanomaly.NewService(pool)
	service := NewService(pool, anomalies)
	if _, _, err := service.Publish(ctx, PublishInput{RunID: "publish-run", Actor: "publisher-a"}); err != ErrReportsNotReady {
		t.Fatalf("publish without reports error=%v", err)
	}
	batch, created, err := anomalies.Enqueue(ctx, "publish-run")
	if err != nil || !created {
		t.Fatalf("enqueue anomaly batch=%+v created=%v err=%v", batch, created, err)
	}
	if worked, err := anomalies.ProcessNext(ctx); err != nil || !worked {
		t.Fatalf("process anomaly worked=%v err=%v", worked, err)
	}
	if _, _, err := service.Publish(ctx, PublishInput{RunID: "publish-run", Actor: "publisher-a"}); err != ErrBlockingFindings {
		t.Fatalf("publish with open blocking findings error=%v", err)
	}
	resolveBlockingFindings(t, ctx, anomalies, batch.ID)
	first, created, err := service.Publish(ctx, PublishInput{RunID: "publish-run", Actor: "publisher-a"})
	if err != nil || !created || first.Batch.Version != 1 || len(first.Grades) != 2 || first.Batch.Actor != "publisher-a" {
		t.Fatalf("first publication=%+v created=%v err=%v", first.Batch, created, err)
	}
	duplicate, created, err := service.Publish(ctx, PublishInput{RunID: "publish-run", Actor: "publisher-a"})
	if err != nil || created || duplicate.Batch.ID != first.Batch.ID {
		t.Fatalf("duplicate publication=%+v created=%v err=%v", duplicate.Batch, created, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE score_publication_grades SET total_score=0 WHERE batch_id=$1`, first.Batch.ID); err == nil {
		t.Fatal("published grade snapshot was mutable")
	}
	var oldScore float64
	if err := pool.QueryRow(ctx, `SELECT total_score FROM score_publication_grades WHERE batch_id=$1 AND submission_id=$2`, first.Batch.ID, ids[1]).Scan(&oldScore); err != nil || oldScore != 1 {
		t.Fatalf("original published score=%v err=%v", oldScore, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE submissions SET subjective_score_final=2,total_score=2,review_status='reviewed',
grading_detail_json=jsonb_set(grading_detail_json,'{0,final_score}','2'::jsonb) WHERE id=$1`, ids[1]); err != nil {
		t.Fatal(err)
	}
	correction, created, err := anomalies.Enqueue(ctx, "publish-run")
	if err != nil || !created {
		t.Fatalf("enqueue correction anomaly=%+v created=%v err=%v", correction, created, err)
	}
	if worked, err := anomalies.ProcessNext(ctx); err != nil || !worked {
		t.Fatalf("process correction anomaly worked=%v err=%v", worked, err)
	}
	resolveBlockingFindings(t, ctx, anomalies, correction.ID)
	if _, _, err := service.Publish(ctx, PublishInput{RunID: "publish-run", Actor: "publisher-a"}); err != ErrCorrectionReason {
		t.Fatalf("correction without reason error=%v", err)
	}
	second, created, err := service.Publish(ctx, PublishInput{RunID: "publish-run", Actor: "publisher-a", CorrectionReason: "复核后更正第二位考生的得分。"})
	if err != nil || !created || second.Batch.Version != 2 || second.Batch.SupersedesBatchID == nil || *second.Batch.SupersedesBatchID != first.Batch.ID {
		t.Fatalf("correction publication=%+v created=%v err=%v", second.Batch, created, err)
	}
	if second.Grades[1].TotalScore != 2 {
		t.Fatalf("corrected grade=%+v", second.Grades[1])
	}
}

func TestPublishAndManualReviewRaceKeepsSnapshotBoundToCurrentReport(t *testing.T) {
	pool, cleanup := testutil.NewSchema(t)
	defer cleanup()
	if _, err := db.Migrate(context.Background(), pool); err != nil {
		t.Fatalf("migrate test schema: %v", err)
	}
	ctx := context.Background()
	ids := seedPublishRun(t, pool)
	anomalies := scoreanomaly.NewService(pool)
	service := NewService(pool, anomalies)
	batch, created, err := anomalies.Enqueue(ctx, "publish-run")
	if err != nil || !created {
		t.Fatalf("enqueue anomaly batch=%+v created=%v err=%v", batch, created, err)
	}
	if worked, err := anomalies.ProcessNext(ctx); err != nil || !worked {
		t.Fatalf("process anomaly worked=%v err=%v", worked, err)
	}
	resolveBlockingFindings(t, ctx, anomalies, batch.ID)

	start := make(chan struct{})
	var wait sync.WaitGroup
	wait.Add(2)
	var first Publication
	var firstCreated bool
	var publishErr error
	var reviewErr error
	go func() {
		defer wait.Done()
		<-start
		first, firstCreated, publishErr = service.Publish(ctx, PublishInput{RunID: "publish-run", Actor: "publisher-a"})
	}()
	go func() {
		defer wait.Done()
		<-start
		_, reviewErr = review.NewService(pool, nil).Apply(ctx, review.ApplyInput{
			SubmissionID: ids[1], QuestionID: "q1", NewScore: 2,
			Note: "并发发布测试中的人工改分。",
		})
	}()
	close(start)
	wait.Wait()
	if reviewErr != nil {
		t.Fatalf("manual review failed during publication race: %v", reviewErr)
	}
	if publishErr != nil && publishErr != ErrReportsNotReady {
		t.Fatalf("publish race error=%v, want success before edit or stale-report gate", publishErr)
	}
	var firstPublishedID int64
	if publishErr == nil {
		if !firstCreated || first.Batch.Version != 1 {
			t.Fatalf("initial publication=%+v created=%v", first.Batch, firstCreated)
		}
		var publishedScore float64
		if err := pool.QueryRow(ctx, `SELECT total_score FROM score_publication_grades WHERE batch_id=$1 AND submission_id=$2`, first.Batch.ID, ids[1]).Scan(&publishedScore); err != nil {
			t.Fatal(err)
		}
		if publishedScore != 1 {
			t.Fatalf("race published a score without a matching current report: %v", publishedScore)
		}
		firstPublishedID = first.Batch.ID
	}

	updatedBatch, created, err := anomalies.Enqueue(ctx, "publish-run")
	if err != nil || !created {
		t.Fatalf("enqueue report for reviewed scores batch=%+v created=%v err=%v", updatedBatch, created, err)
	}
	if worked, err := anomalies.ProcessNext(ctx); err != nil || !worked {
		t.Fatalf("process reviewed-score anomaly worked=%v err=%v", worked, err)
	}
	resolveBlockingFindings(t, ctx, anomalies, updatedBatch.ID)
	reason := "人工复核后更新发布成绩。"
	corrected, created, err := service.Publish(ctx, PublishInput{RunID: "publish-run", Actor: "publisher-a", CorrectionReason: reason})
	if err != nil || !created {
		t.Fatalf("publish reviewed scores batch=%+v created=%v err=%v", corrected.Batch, created, err)
	}
	expectedVersion := 1
	if firstPublishedID != 0 {
		expectedVersion = 2
		if corrected.Batch.SupersedesBatchID == nil || *corrected.Batch.SupersedesBatchID != firstPublishedID {
			t.Fatalf("correction did not retain the prior publication link: %+v", corrected.Batch)
		}
	}
	if corrected.Batch.Version != expectedVersion || corrected.Grades[1].TotalScore != 2 {
		t.Fatalf("corrected publication=%+v grades=%+v want version=%d score=2", corrected.Batch, corrected.Grades, expectedVersion)
	}
}

func resolveBlockingFindings(t *testing.T, ctx context.Context, service *scoreanomaly.Service, batchID int64) {
	t.Helper()
	page, err := service.GetBatchPage(ctx, batchID, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, finding := range page.Findings {
		if finding.Severity != "blocking" {
			continue
		}
		if _, err := service.ReviewFinding(ctx, scoreanomaly.ReviewFindingInput{FindingID: finding.ID,
			ExpectedRevision: finding.ReviewRevision, Disposition: "manually_resolved",
			Note: "人工核对了答题内容与评分依据，确认该提示已处理。", Actor: "reviewer-a"}); err != nil {
			t.Fatal(err)
		}
	}
}

func seedPublishRun(t *testing.T, pool *pgxpool.Pool) []int64 {
	t.Helper()
	ctx := context.Background()
	snapshotHash := fmt.Sprintf("%064x", 7)
	if _, err := pool.Exec(ctx, `INSERT INTO exam_runs
(id,paper_id,round_no,public_token_hash,status,duration_minutes,snapshot_path,snapshot_hash,is_legacy,opened_at,closed_at,created_at,max_score,passing_score)
VALUES ('publish-run','publish-paper',1,'publish-token','closed',60,'snapshot.json',$1,false,now(),now(),now(),10,5)`, snapshotHash); err != nil {
		t.Fatal(err)
	}
	answers := []string{`{"q1":"same answer"}`, `{"q1":"same answer"}`}
	details := []string{
		`[{"question_id":"q1","question":"Question","student_answer":"same answer","max_score":10,"score":10,"machine_score":10,"final_score":10,"grading_method":"llm","scoring_backend":"llm","scoring_model":"test","review_status":"high_confidence","matched_points":[{"point_id":"p1","score":10,"max_score":10,"relation":"supported","evidence":"same answer","evidence_located":true}]}]`,
		`[{"question_id":"q1","question":"Question","student_answer":"same answer","max_score":10,"score":1,"machine_score":1,"final_score":1,"grading_method":"llm","scoring_backend":"llm","scoring_model":"test","review_status":"high_confidence","matched_points":[{"point_id":"p1","score":1,"max_score":10,"relation":"supported","evidence":"same answer","evidence_located":true}]}]`,
	}
	ids := make([]int64, 0, 2)
	for i := range answers {
		var id int64
		if err := pool.QueryRow(ctx, `INSERT INTO submissions
(name,employee_id,paper_id,run_id,answers_json,grading_detail_json,objective_score,subjective_score_machine,
subjective_score_final,total_score,review_status,grading_status,grading_generation,submitted_at,graded_at)
VALUES ($1,$2,'publish-paper','publish-run',$3::jsonb,$4::jsonb,0,$5,$5,$5,'high_confidence','done',1,now(),now()) RETURNING id`,
			fmt.Sprintf("Candidate %d", i+1), fmt.Sprintf("employee-%d", i+1), answers[i], details[i], []float64{10, 1}[i]).Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
		if _, err := pool.Exec(ctx, `INSERT INTO grading_history
(submission_id,generation,grading_detail_json,subjective_score_machine,subjective_score_final,total_score,review_status)
VALUES ($1,1,$2::jsonb,$3,$3,$3,'high_confidence')`, id, details[i], []float64{10, 1}[i]); err != nil {
			t.Fatal(err)
		}
	}
	hasher := sha256.New()
	rows, err := pool.Query(ctx, `SELECT id,employee_id,name,answers_json::text FROM submissions WHERE run_id='publish-run' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id int64
		var employeeID, name, answersText string
		if err := rows.Scan(&id, &employeeID, &name, &answersText); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		writeHashPart(hasher, strconv.FormatInt(id, 10))
		writeHashPart(hasher, employeeID)
		writeHashPart(hasher, name)
		writeHashPart(hasher, answersText)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		t.Fatal(err)
	}
	rows.Close()
	answerHash := hex.EncodeToString(hasher.Sum(nil))
	var similarityID int64
	if err := pool.QueryRow(ctx, `INSERT INTO answer_similarity_batches
(run_id,snapshot_hash,answer_set_hash,rule_version,status,submission_count,question_count,completed_at)
VALUES ('publish-run',$1,$2,'lexical-overlap-v1','completed',2,1,now()) RETURNING id`, snapshotHash, answerHash).Scan(&similarityID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO answer_similarity_findings
(batch_id,question_id,left_submission_id,right_submission_id,left_participant_id,right_participant_id,left_name,right_name,
left_answer,right_answer,risk_level,risk_score,text_overlap,reference_similarity,shared_excerpt,left_excerpt,right_excerpt)
VALUES ($1,'q1',$2,$3,'employee-1','employee-2','Candidate 1','Candidate 2','same answer','same answer','high',0.99,1,0,'same answer','same answer','same answer')`,
		similarityID, ids[0], ids[1]); err != nil {
		t.Fatal(err)
	}
	return ids
}

func writeHashPart(h hash.Hash, value string) {
	_, _ = h.Write([]byte(strconv.Itoa(len(value))))
	_, _ = h.Write([]byte{':'})
	_, _ = h.Write([]byte(value))
}

func TestPublishAndRegradeRaceNeverPublishesPendingGeneration(t *testing.T) {
	pool, cleanup := testutil.NewSchema(t)
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	ids := seedPublishRun(t, pool)
	anomalies := scoreanomaly.NewService(pool)
	batch, _, err := anomalies.Enqueue(ctx, "publish-run")
	if err != nil {
		t.Fatal(err)
	}
	if worked, err := anomalies.ProcessNext(ctx); err != nil || !worked {
		t.Fatalf("process report worked=%v err=%v", worked, err)
	}
	resolveBlockingFindings(t, ctx, anomalies, batch.ID)
	service := NewService(pool, anomalies)
	reviewer := review.NewService(pool, jobs.NewRepository())
	start := make(chan struct{})
	var wait sync.WaitGroup
	wait.Add(2)
	var result Publication
	var created bool
	var publishErr, regradeErr error
	go func() {
		defer wait.Done()
		<-start
		result, created, publishErr = service.Publish(ctx, PublishInput{RunID: "publish-run", Actor: "publisher"})
	}()
	go func() {
		defer wait.Done()
		<-start
		_, regradeErr = reviewer.Regrade(ctx, review.RegradeInput{SubmissionID: ids[1], Actor: "reviewer", Reason: "核对发布与重评的并发行为"})
	}()
	close(start)
	wait.Wait()
	if regradeErr != nil {
		t.Fatal(regradeErr)
	}
	if publishErr != nil && !errors.Is(publishErr, ErrRunNotReady) {
		t.Fatalf("publish must finish before regrade or reject pending generation: %v", publishErr)
	}
	var generation int64
	var status string
	if err := pool.QueryRow(ctx, "SELECT grading_generation,grading_status FROM submissions WHERE id=$1", ids[1]).Scan(&generation, &status); err != nil {
		t.Fatal(err)
	}
	if generation != 2 || status != "pending" {
		t.Fatalf("regrade generation=%d status=%s", generation, status)
	}
	var queued int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM grading_jobs WHERE submission_id=$1 AND generation=2 AND status='queued'", ids[1]).Scan(&queued); err != nil || queued != 1 {
		t.Fatalf("new generation jobs=%d err=%v", queued, err)
	}
	if publishErr == nil {
		if !created || result.Batch.AnomalyBatchID != batch.ID || result.Grades[1].GradingGeneration != 1 || result.Grades[1].TotalScore != 1 {
			t.Fatalf("published pending/stale new generation: %+v", result)
		}
		stored, err := service.Get(ctx, result.Batch.ID)
		if err != nil || stored.Grades[1].GradingGeneration != 1 || stored.Grades[1].TotalScore != 1 {
			t.Fatalf("regrade mutated published snapshot: %+v err=%v", stored, err)
		}
	} else {
		items, err := service.List(ctx, "publish-run")
		if err != nil || len(items) != 0 {
			t.Fatalf("rejected publication left a batch: %+v err=%v", items, err)
		}
	}
	if _, created, err := service.Publish(ctx, PublishInput{RunID: "publish-run", Actor: "publisher", CorrectionReason: "重评尚未完成时不能发布。"}); !errors.Is(err, ErrRunNotReady) || created {
		t.Fatalf("pending new generation published created=%v err=%v", created, err)
	}
}

func TestConcurrentPublishersCreateOneSnapshotAndAudit(t *testing.T) {
	pool, cleanup := testutil.NewSchema(t)
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	seedPublishRun(t, pool)
	anomalies := scoreanomaly.NewService(pool)
	batch, _, err := anomalies.Enqueue(ctx, "publish-run")
	if err != nil {
		t.Fatal(err)
	}
	if worked, err := anomalies.ProcessNext(ctx); err != nil || !worked {
		t.Fatalf("process report worked=%v err=%v", worked, err)
	}
	resolveBlockingFindings(t, ctx, anomalies, batch.ID)
	service := NewService(pool, anomalies)
	type receipt struct {
		result  Publication
		created bool
		err     error
	}
	start := make(chan struct{})
	finished := make(chan receipt, 8)
	for index := 0; index < 8; index++ {
		go func(index int) {
			<-start
			result, created, err := service.Publish(ctx, PublishInput{RunID: "publish-run", Actor: fmt.Sprintf("publisher-%d", index)})
			finished <- receipt{result, created, err}
		}(index)
	}
	close(start)
	var batchID int64
	createdCount := 0
	for index := 0; index < 8; index++ {
		out := <-finished
		if out.err != nil || out.result.Batch.Version != 1 || len(out.result.Grades) != 2 {
			t.Fatalf("concurrent publication: %+v", out)
		}
		if batchID != 0 && out.result.Batch.ID != batchID {
			t.Fatalf("duplicate publication batches: %d and %d", batchID, out.result.Batch.ID)
		}
		batchID = out.result.Batch.ID
		if out.created {
			createdCount++
		}
	}
	if createdCount != 1 {
		t.Fatalf("created %d publications, want one", createdCount)
	}
	var batches, grades, audits int
	if err := pool.QueryRow(ctx, `SELECT
(SELECT count(*) FROM score_publication_batches),
(SELECT count(*) FROM score_publication_grades),
(SELECT count(*) FROM score_publication_audit)`).Scan(&batches, &grades, &audits); err != nil || batches != 1 || grades != 2 || audits != 1 {
		t.Fatalf("publication rows batches=%d grades=%d audits=%d err=%v", batches, grades, audits, err)
	}
}
