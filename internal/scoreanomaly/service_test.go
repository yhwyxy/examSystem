package scoreanomaly

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/yhwyxy/examSystem/internal/db"
	"github.com/yhwyxy/examSystem/internal/testutil"
)

func TestQueuedOldRuleBatchIsRejectedAfterRuleUpgrade(t *testing.T) {
	cleanup, pool := newAnomalyPool(t)
	defer cleanup()
	ctx := context.Background()
	seedAnomalyRun(t, pool)
	service := NewService(pool)
	batch, _, err := service.Enqueue(ctx, "run-anomaly")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE score_anomaly_batches SET rule_version='score-consistency-v1' WHERE id=$1`, batch.ID); err != nil {
		t.Fatal(err)
	}
	worked, err := service.ProcessNext(ctx)
	if err != nil || !worked {
		t.Fatalf("process old batch worked=%v err=%v", worked, err)
	}
	page, err := service.GetBatchPage(ctx, batch.ID, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if page.Batch.Status != "failed" || page.Batch.ErrorCode != "RULE_VERSION_CHANGED" || len(page.Findings) != 0 {
		t.Fatalf("old task was run under new rules: %+v", page)
	}
	current, created, err := service.Enqueue(ctx, "run-anomaly")
	if err != nil || !created || current.ID == batch.ID || current.RuleVersion != RuleVersion {
		t.Fatalf("new current report could not be created: %+v created=%v err=%v", current, created, err)
	}
}

func TestServicePersistsVersionBoundAnomalyReport(t *testing.T) {
	cleanup, pool := newAnomalyPool(t)
	defer cleanup()
	ctx := context.Background()
	firstID, secondID, similarityID := seedAnomalyRun(t, pool)
	service := NewService(pool)
	batch, created, err := service.Enqueue(ctx, "run-anomaly")
	if err != nil || !created || batch.SimilarityBatchID != similarityID || batch.Status != "queued" {
		t.Fatalf("enqueue batch=%+v created=%v err=%v", batch, created, err)
	}
	duplicate, created, err := service.Enqueue(ctx, "run-anomaly")
	if err != nil || created || duplicate.ID != batch.ID {
		t.Fatalf("duplicate enqueue batch=%+v created=%v err=%v", duplicate, created, err)
	}
	worked, err := service.ProcessNext(ctx)
	if err != nil || !worked {
		t.Fatalf("ProcessNext worked=%v err=%v", worked, err)
	}
	page, err := service.GetBatchPage(ctx, batch.ID, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if page.Batch.Status != "completed" || page.Batch.SubmissionCount != 2 || page.Batch.FindingCount == 0 || page.Batch.BlockingCount == 0 {
		t.Fatalf("report status or counts are wrong: %+v", page.Batch)
	}
	codes := map[string]bool{}
	for _, finding := range page.Findings {
		codes[finding.RuleCode] = true
		if finding.RuleCode == "identical_answer_score_gap" &&
			(len(finding.SubmissionIDs) != 2 || finding.SubmissionIDs[0] != strconv.FormatInt(firstID, 10) || finding.SubmissionIDs[1] != strconv.FormatInt(secondID, 10)) {
			t.Fatalf("pair finding lost submission identities: %+v", finding)
		}
		if finding.RuleCode == "regrade_score_drift" &&
			(finding.Evidence["previous_generation"] != float64(1) || finding.Evidence["current_generation"] != float64(2)) {
			t.Fatalf("regrade report lost generation evidence: %+v", finding)
		}
	}
	for _, code := range []string{"identical_answer_score_gap", "shared_error_score_disagreement", "regrade_score_drift"} {
		if !codes[code] {
			t.Errorf("missing %s finding: %+v", code, page.Findings)
		}
	}
	var currentGrade float64
	if err := pool.QueryRow(ctx, `SELECT total_score FROM submissions WHERE id=$1`, firstID).Scan(&currentGrade); err != nil || currentGrade != 11 {
		t.Fatalf("anomaly report changed the grade: score=%v err=%v", currentGrade, err)
	}
}

func TestServiceFailsWhenCurrentScoringGenerationChanges(t *testing.T) {
	cleanup, pool := newAnomalyPool(t)
	defer cleanup()
	ctx := context.Background()
	firstID, _, _ := seedAnomalyRun(t, pool)
	service := NewService(pool)
	batch, _, err := service.Enqueue(ctx, "run-anomaly")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE submissions SET grading_detail_json='[]'::jsonb WHERE id=$1`, firstID); err != nil {
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
	if page.Batch.Status != "failed" || page.Batch.ErrorCode != "INPUT_CHANGED" || page.Batch.FindingCount != 0 {
		t.Fatalf("stale scoring report was not rejected: %+v", page.Batch)
	}
}

func TestEnqueueRequiresCompletedSimilarityReport(t *testing.T) {
	cleanup, pool := newAnomalyPool(t)
	defer cleanup()
	seedAnomalyRun(t, pool)
	if _, err := pool.Exec(context.Background(), `DELETE FROM answer_similarity_batches WHERE run_id='run-anomaly'`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := NewService(pool).Enqueue(context.Background(), "run-anomaly"); err != ErrSimilarityNotReady {
		t.Fatalf("enqueue without P3 report err=%v", err)
	}
}

func TestFindingReviewUsesRevisionAndAuditsEveryChange(t *testing.T) {
	cleanup, pool := newAnomalyPool(t)
	defer cleanup()
	ctx := context.Background()
	seedAnomalyRun(t, pool)
	service := NewService(pool)
	batch, _, err := service.Enqueue(ctx, "run-anomaly")
	if err != nil {
		t.Fatal(err)
	}
	if worked, err := service.ProcessNext(ctx); err != nil || !worked {
		t.Fatalf("ProcessNext worked=%v err=%v", worked, err)
	}
	page, err := service.GetBatchPage(ctx, batch.ID, 100, 0)
	if err != nil || len(page.Findings) == 0 {
		t.Fatalf("findings=%d err=%v", len(page.Findings), err)
	}
	findingID := page.Findings[0].ID
	resolved, err := service.ReviewFinding(ctx, ReviewFindingInput{FindingID: findingID, ExpectedRevision: 1,
		Disposition: "false_positive", Note: "已核对答案与评分证据，规则提示属于可接受差异。", Actor: "reviewer-a"})
	if err != nil || resolved.ReviewStatus != "resolved" || resolved.ReviewRevision != 2 || resolved.ReviewedBy != "reviewer-a" {
		t.Fatalf("resolved finding=%+v err=%v", resolved, err)
	}
	if _, err := service.ReviewFinding(ctx, ReviewFindingInput{FindingID: findingID, ExpectedRevision: 1,
		Disposition: "manually_resolved", Note: "stale update", Actor: "reviewer-b"}); err != ErrReviewConflict {
		t.Fatalf("stale review error=%v", err)
	}
	page, err = service.GetBatchPage(ctx, batch.ID, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	var reviewed FindingRecord
	for _, item := range page.Findings {
		if item.ID == findingID {
			reviewed = item
		}
	}
	if reviewed.ReviewStatus != "resolved" || reviewed.ReviewDisposition != "false_positive" || len(reviewed.ReviewHistory) != 1 || reviewed.ReviewHistory[0].Actor != "reviewer-a" {
		t.Fatalf("review state or audit not returned: %+v", reviewed)
	}
	reopened, err := service.ReviewFinding(ctx, ReviewFindingInput{FindingID: findingID, ExpectedRevision: 2,
		Disposition: "open", Note: "补充证据后需要重新核查。", Actor: "reviewer-b"})
	if err != nil || reopened.ReviewStatus != "open" || reopened.ReviewRevision != 3 {
		t.Fatalf("reopened finding=%+v err=%v", reopened, err)
	}
}

func newAnomalyPool(t *testing.T) (func(), *pgxpool.Pool) {
	t.Helper()
	pool, cleanup := testutil.NewSchema(t)
	if _, err := db.Migrate(context.Background(), pool); err != nil {
		t.Fatalf("migrate test schema: %v", err)
	}
	return cleanup, pool
}

func seedAnomalyRun(t *testing.T, pool *pgxpool.Pool) (int64, int64, int64) {
	t.Helper()
	ctx := context.Background()
	snapshotHash := stringsOf("a", 64)
	if _, err := pool.Exec(ctx, `INSERT INTO exam_runs
(id,paper_id,round_no,public_token_hash,status,duration_minutes,snapshot_path,snapshot_hash,is_legacy,opened_at,closed_at,created_at)
VALUES ('run-anomaly','paper-anomaly',1,'token-anomaly','closed',60,'snapshot.json',$1,false,now(),now(),now())`, snapshotHash); err != nil {
		t.Fatal(err)
	}
	answers := []map[string]string{
		{"q1": "同一份完全相同的答案", "q2": "设备误差导致数值 17 偏离标准"},
		{"q1": "同一份完全相同的答案。", "q2": "校准误差使数值 17 远离标准"},
	}
	ids := make([]int64, 0, 2)
	answerTexts := []string{"同一份完全相同的答案", "设备误差导致数值 17 偏离标准"}
	for index, answers := range answers {
		encoded, err := json.Marshal(answers)
		if err != nil {
			t.Fatal(err)
		}
		employeeID := "employee-" + strconv.Itoa(index+1)
		name := "考生" + strconv.Itoa(index+1)
		if _, err := pool.Exec(ctx, `INSERT INTO exam_sessions
(id,run_id,employee_id,name,session_token_hash,started_at,deadline_at,status,created_at,updated_at)
VALUES ($1,'run-anomaly',$2,$3,$4,now(),now(),'submitted',now(),now())`, "session-"+strconv.Itoa(index+1), employeeID, name, "token-"+strconv.Itoa(index+1)); err != nil {
			t.Fatal(err)
		}
		generation := int64(1)
		if index == 0 {
			generation = 2
		}
		q1Score, q2Score := 8.0, 2.0
		if index == 0 {
			q1Score, q2Score = 3, 8
		}
		detail := []map[string]any{
			testQuestionDetail("q1", answers["q1"], q1Score, 10),
			testQuestionDetail("q2", answers["q2"], q2Score, 10),
		}
		detailJSON, _ := json.Marshal(detail)
		var id int64
		if err := pool.QueryRow(ctx, `INSERT INTO submissions
(name,employee_id,paper_id,run_id,answers_json,grading_detail_json,subjective_score_machine,subjective_score_final,total_score,
 grading_status,review_status,grading_generation,submitted_at)
VALUES ($1,$2,'paper-anomaly','run-anomaly',$3::jsonb,$4::jsonb,$5,$5,$6,'done','auto_scored',$7,now()) RETURNING id`,
			name, employeeID, string(encoded), string(detailJSON), q1Score+q2Score, q1Score+q2Score, generation).Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
		if generation == 2 {
			previous := []map[string]any{testQuestionDetail("q1", answerTexts[0], 2, 10), testQuestionDetail("q2", answerTexts[1], 1, 10)}
			previousJSON, _ := json.Marshal(previous)
			if _, err := pool.Exec(ctx, `INSERT INTO grading_history
(submission_id,generation,grading_detail_json,subjective_score_machine,subjective_score_final,total_score,review_status)
VALUES ($1,1,$2::jsonb,3,3,11,'auto_scored')`, id, string(previousJSON)); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := pool.Exec(ctx, `INSERT INTO grading_history
(submission_id,generation,grading_detail_json,subjective_score_machine,subjective_score_final,total_score,review_status)
VALUES ($1,$2,$3::jsonb,$4,$4,$5,'auto_scored')`, id, generation, string(detailJSON), q1Score+q2Score, q1Score+q2Score); err != nil {
			t.Fatal(err)
		}
	}
	// Hash exactly what the services read from jsonb, including its canonical form.
	answerHasher := sha256.New()
	for _, id := range ids {
		var employeeID, name string
		var raw []byte
		if err := pool.QueryRow(ctx, `SELECT employee_id,name,answers_json FROM submissions WHERE id=$1`, id).Scan(&employeeID, &name, &raw); err != nil {
			t.Fatal(err)
		}
		writeTestHashPart(answerHasher, strconv.FormatInt(id, 10))
		writeTestHashPart(answerHasher, employeeID)
		writeTestHashPart(answerHasher, name)
		writeTestHashPart(answerHasher, string(raw))
	}
	answerHash := hex.EncodeToString(answerHasher.Sum(nil))
	var similarityID int64
	if err := pool.QueryRow(ctx, `INSERT INTO answer_similarity_batches
(run_id,snapshot_hash,answer_set_hash,rule_version,status,submission_count,question_count,finding_count,completed_at)
VALUES ('run-anomaly',$1,$2,'lexical-overlap-v1','completed',2,2,2,now()) RETURNING id`,
		snapshotHash, answerHash).Scan(&similarityID); err != nil {
		t.Fatal(err)
	}
	for _, qid := range []string{"q1", "q2"} {
		leftAnswer, rightAnswer := answers[0][qid], answers[1][qid]
		common := []string(nil)
		if qid == "q2" {
			common = []string{"17"}
		}
		commonJSON, _ := json.Marshal(common)
		if _, err := pool.Exec(ctx, `INSERT INTO answer_similarity_findings
(batch_id,question_id,left_submission_id,right_submission_id,left_participant_id,right_participant_id,left_name,right_name,
 left_answer,right_answer,risk_level,risk_score,text_overlap,reference_similarity,shared_excerpt,left_excerpt,right_excerpt,common_error_evidence)
VALUES ($1,$2,$3,$4,'employee-1','employee-2','考生1','考生2',$5,$6,'high',0.95,0.9,0,'','', '',$7::jsonb)`,
			similarityID, qid, ids[0], ids[1], leftAnswer, rightAnswer, string(commonJSON)); err != nil {
			t.Fatalf("seed similarity finding %s: %v", qid, err)
		}
	}
	return ids[0], ids[1], similarityID
}

func testQuestionDetail(id, answer string, score, max float64) map[string]any {
	return map[string]any{"question_id": id, "question": "问题 " + id, "student_answer": answer,
		"machine_score": score, "final_score": score, "max_score": max, "grading_method": "subjective_scoring:judge",
		"scoring_backend": "llm", "scoring_model": "test-model", "review_status": "high_confidence",
		"matched_points": []map[string]any{{"point_id": "p1", "score": score, "max_score": max,
			"relation": "supported", "evidence": answer, "evidence_located": true}}}
}

func stringsOf(value string, count int) string {
	result := ""
	for range count {
		result += value
	}
	return result
}

func writeTestHashPart(h interface{ Write([]byte) (int, error) }, value string) {
	_, _ = h.Write([]byte(strconv.Itoa(len(value))))
	_, _ = h.Write([]byte{':'})
	_, _ = h.Write([]byte(value))
}
