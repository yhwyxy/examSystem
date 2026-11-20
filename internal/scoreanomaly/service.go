package scoreanomaly

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"hash"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	maxRunSubmissions = 5_000
	maxRunBytes       = 64 << 20
	maxAttempts       = 3
	maxPageSize       = 100
)

var (
	ErrNotConfigured      = errors.New("score anomaly service is not configured")
	ErrRunNotFound        = errors.New("score anomaly run not found")
	ErrRunNotReady        = errors.New("score anomaly requires a closed, fully graded run")
	ErrSimilarityNotReady = errors.New("score anomaly requires a completed answer similarity report")
	ErrReportNotReady     = errors.New("score anomaly report is not complete for current scores")
	ErrBatchNotFound      = errors.New("score anomaly batch not found")
	ErrFindingNotFound    = errors.New("score anomaly finding not found")
	ErrReviewConflict     = errors.New("score anomaly review revision conflict")
	ErrInvalidReview      = errors.New("invalid score anomaly review")
	ErrInputChanged       = errors.New("score anomaly input changed after batch creation")
	ErrLeaseLost          = errors.New("score anomaly batch lease lost")
)

type Batch struct {
	ID                int64      `json:"id"`
	RunID             string     `json:"run_id"`
	SimilarityBatchID int64      `json:"similarity_batch_id"`
	SnapshotHash      string     `json:"snapshot_hash"`
	AnswerSetHash     string     `json:"answer_set_hash"`
	GradingSetHash    string     `json:"grading_set_hash"`
	RuleVersion       string     `json:"rule_version"`
	Status            string     `json:"status"`
	AttemptCount      int        `json:"attempt_count"`
	MaxAttempts       int        `json:"max_attempts"`
	ErrorCode         string     `json:"error_code,omitempty"`
	SubmissionCount   int        `json:"submission_count"`
	FindingCount      int        `json:"finding_count"`
	BlockingCount     int        `json:"blocking_count"`
	CreatedAt         time.Time  `json:"created_at"`
	StartedAt         *time.Time `json:"started_at,omitempty"`
	CompletedAt       *time.Time `json:"completed_at,omitempty"`
}

type FindingRecord struct {
	Finding
	ID                int64                 `json:"id"`
	ReviewStatus      string                `json:"review_status"`
	ReviewDisposition string                `json:"review_disposition,omitempty"`
	ReviewNote        string                `json:"review_note,omitempty"`
	ReviewedBy        string                `json:"reviewed_by,omitempty"`
	ReviewedAt        *time.Time            `json:"reviewed_at,omitempty"`
	ReviewRevision    int64                 `json:"review_revision"`
	ReviewHistory     []FindingReviewRecord `json:"review_history"`
}

type FindingReviewRecord struct {
	Actor           string    `json:"actor"`
	FromStatus      string    `json:"from_status"`
	ToStatus        string    `json:"to_status"`
	FromDisposition string    `json:"from_disposition,omitempty"`
	ToDisposition   string    `json:"to_disposition,omitempty"`
	Revision        int64     `json:"revision"`
	Note            string    `json:"note"`
	CreatedAt       time.Time `json:"created_at"`
}

type ReviewFindingInput struct {
	FindingID        int64
	ExpectedRevision int64
	Disposition      string
	Note             string
	Actor            string
}

type Page struct {
	Batch    Batch           `json:"batch"`
	Findings []FindingRecord `json:"findings"`
	Limit    int             `json:"limit"`
	Offset   int             `json:"offset"`
}

type Service struct {
	pool       *pgxpool.Pool
	leaseOwner string
}

// CurrentCompletedBatch resolves the report whose frozen input exactly matches
// the scores visible in tx. Callers that publish grades should lock the run and
// submissions in tx before using this gate.
func (s *Service) CurrentCompletedBatch(ctx context.Context, tx pgx.Tx, runID string) (Batch, error) {
	if err := s.ready(); err != nil {
		return Batch{}, err
	}
	input, err := readBoundInput(ctx, tx, runID)
	if err != nil {
		return Batch{}, err
	}
	if input.similarityBatchID == 0 {
		return Batch{}, ErrSimilarityNotReady
	}
	var batch Batch
	err = tx.QueryRow(ctx, batchSelect+` WHERE run_id=$1 AND similarity_batch_id=$2
AND snapshot_hash=$3 AND answer_set_hash=$4 AND grading_set_hash=$5 AND rule_version=$6 AND status='completed'
ORDER BY completed_at DESC,id DESC LIMIT 1`, runID, input.similarityBatchID, input.snapshotHash, input.answerHash,
		input.gradingHash, RuleVersion).Scan(batchTargets(&batch)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return Batch{}, ErrReportNotReady
	}
	return batch, err
}

func NewService(pool *pgxpool.Pool) *Service {
	owner := make([]byte, 12)
	if _, err := rand.Read(owner); err != nil {
		owner = []byte(strconv.FormatInt(time.Now().UnixNano(), 10))
	}
	return &Service{pool: pool, leaseOwner: hex.EncodeToString(owner)}
}

func (s *Service) ready() error {
	if s == nil || s.pool == nil {
		return ErrNotConfigured
	}
	return nil
}

type inputSubmission struct {
	id, employeeID, name string
	generation           int64
	answers, detail      []byte
	objective, machine   float64
	final, total         float64
	reviewStatus         string
	gradingStatus        string
}

type boundInput struct {
	snapshotHash, answerHash, gradingHash string
	similarityBatchID                     int64
	submissions                           []inputSubmission
}

func (s *Service) Enqueue(ctx context.Context, runID string) (Batch, bool, error) {
	if err := s.ready(); err != nil {
		return Batch{}, false, err
	}
	runID = strings.TrimSpace(runID)
	if runID == "" || len(runID) > 200 {
		return Batch{}, false, ErrRunNotFound
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return Batch{}, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	input, err := readBoundInput(ctx, tx, runID)
	if err != nil {
		return Batch{}, false, err
	}
	if input.similarityBatchID == 0 {
		return Batch{}, false, ErrSimilarityNotReady
	}
	var batch Batch
	err = tx.QueryRow(ctx, `
INSERT INTO score_anomaly_batches
    (run_id,similarity_batch_id,snapshot_hash,answer_set_hash,grading_set_hash,rule_version,status,max_attempts)
VALUES ($1,$2,$3,$4,$5,$6,'queued',$7)
ON CONFLICT (run_id,similarity_batch_id,grading_set_hash,rule_version) DO NOTHING
RETURNING id,run_id,similarity_batch_id,snapshot_hash,answer_set_hash,grading_set_hash,rule_version,status,
          attempt_count,max_attempts,coalesce(error_code,''),submission_count,finding_count,blocking_count,created_at,started_at,completed_at`,
		runID, input.similarityBatchID, input.snapshotHash, input.answerHash, input.gradingHash, RuleVersion, maxAttempts).
		Scan(batchTargets(&batch)...)
	created := err == nil
	if errors.Is(err, pgx.ErrNoRows) {
		err = tx.QueryRow(ctx, batchSelect+` WHERE run_id=$1 AND similarity_batch_id=$2 AND grading_set_hash=$3 AND rule_version=$4`,
			runID, input.similarityBatchID, input.gradingHash, RuleVersion).Scan(batchTargets(&batch)...)
	}
	if err != nil {
		return Batch{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Batch{}, false, err
	}
	return batch, created, nil
}

func (s *Service) ListBatches(ctx context.Context, runID string) ([]Batch, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, batchSelect+` WHERE run_id=$1 ORDER BY id DESC LIMIT 50`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]Batch, 0)
	for rows.Next() {
		var item Batch
		if err := rows.Scan(batchTargets(&item)...); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *Service) GetBatchPage(ctx context.Context, id int64, limit, offset int) (Page, error) {
	if err := s.ready(); err != nil {
		return Page{}, err
	}
	if limit <= 0 || limit > maxPageSize {
		limit = maxPageSize
	}
	if offset < 0 {
		offset = 0
	}
	var page Page
	page.Limit, page.Offset = limit, offset
	if err := s.pool.QueryRow(ctx, batchSelect+` WHERE id=$1`, id).Scan(batchTargets(&page.Batch)...); errors.Is(err, pgx.ErrNoRows) {
		return Page{}, ErrBatchNotFound
	} else if err != nil {
		return Page{}, err
	}
	page.Findings = make([]FindingRecord, 0)
	rows, err := s.pool.Query(ctx, `SELECT id,rule_code,severity,question_id,submission_ids,participant_names,message,score_gap,evidence,
review_status,coalesce(review_disposition,''),coalesce(review_note,''),coalesce(reviewed_by,''),reviewed_at,review_revision
FROM score_anomaly_findings WHERE batch_id=$1 ORDER BY CASE severity WHEN 'blocking' THEN 0 ELSE 1 END,id LIMIT $2 OFFSET $3`, id, limit, offset)
	if err != nil {
		return Page{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var item FindingRecord
		if err := rows.Scan(&item.ID, &item.RuleCode, &item.Severity, &item.QuestionID, &item.SubmissionIDs,
			&item.ParticipantNames, &item.Message, &item.ScoreGap, &item.Evidence, &item.ReviewStatus, &item.ReviewDisposition,
			&item.ReviewNote, &item.ReviewedBy, &item.ReviewedAt, &item.ReviewRevision); err != nil {
			return Page{}, err
		}
		page.Findings = append(page.Findings, item)
	}
	if err := rows.Err(); err != nil {
		return Page{}, err
	}
	if len(page.Findings) > 0 {
		ids := make([]int64, 0, len(page.Findings))
		byID := make(map[int64]int, len(page.Findings))
		for index := range page.Findings {
			id := page.Findings[index].ID
			ids = append(ids, id)
			byID[id] = index
			page.Findings[index].ReviewHistory = []FindingReviewRecord{}
		}
		auditRows, err := s.pool.Query(ctx, `SELECT finding_id,actor,from_status,to_status,coalesce(from_disposition,''),
coalesce(to_disposition,''),revision,note,created_at FROM score_anomaly_review_audit WHERE finding_id=ANY($1) ORDER BY finding_id,id`, ids)
		if err != nil {
			return Page{}, err
		}
		defer auditRows.Close()
		for auditRows.Next() {
			var findingID int64
			var record FindingReviewRecord
			if err := auditRows.Scan(&findingID, &record.Actor, &record.FromStatus, &record.ToStatus,
				&record.FromDisposition, &record.ToDisposition, &record.Revision, &record.Note, &record.CreatedAt); err != nil {
				return Page{}, err
			}
			index, ok := byID[findingID]
			if ok {
				page.Findings[index].ReviewHistory = append(page.Findings[index].ReviewHistory, record)
			}
		}
		if err := auditRows.Err(); err != nil {
			return Page{}, err
		}
	}
	return page, nil
}

func (s *Service) ReviewFinding(ctx context.Context, in ReviewFindingInput) (FindingRecord, error) {
	if err := s.ready(); err != nil {
		return FindingRecord{}, err
	}
	in.Disposition = strings.TrimSpace(in.Disposition)
	in.Note = strings.TrimSpace(in.Note)
	in.Actor = strings.TrimSpace(in.Actor)
	if in.FindingID <= 0 || in.ExpectedRevision <= 0 || (in.Disposition != "open" && in.Disposition != "false_positive" && in.Disposition != "manually_resolved") || len(in.Note) < 5 || len(in.Note) > 2000 || len(in.Actor) == 0 || len(in.Actor) > 200 {
		return FindingRecord{}, ErrInvalidReview
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return FindingRecord{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var oldStatus, oldDisposition string
	err = tx.QueryRow(ctx, `SELECT review_status,coalesce(review_disposition,'') FROM score_anomaly_findings WHERE id=$1 FOR UPDATE`, in.FindingID).Scan(&oldStatus, &oldDisposition)
	if errors.Is(err, pgx.ErrNoRows) {
		return FindingRecord{}, ErrFindingNotFound
	}
	if err != nil {
		return FindingRecord{}, err
	}
	var actualRevision int64
	if err := tx.QueryRow(ctx, `SELECT review_revision FROM score_anomaly_findings WHERE id=$1`, in.FindingID).Scan(&actualRevision); err != nil {
		return FindingRecord{}, err
	}
	if actualRevision != in.ExpectedRevision {
		return FindingRecord{}, ErrReviewConflict
	}
	newStatus, newDisposition := "resolved", in.Disposition
	if in.Disposition == "open" {
		newStatus, newDisposition = "open", ""
	}
	newRevision := actualRevision + 1
	var item FindingRecord
	err = tx.QueryRow(ctx, `UPDATE score_anomaly_findings SET review_status=$2,review_disposition=NULLIF($3,''),
review_note=CASE WHEN $2='open' THEN NULL ELSE $4 END,reviewed_by=CASE WHEN $2='open' THEN NULL ELSE $5 END,
reviewed_at=CASE WHEN $2='open' THEN NULL ELSE now() END,review_revision=$6 WHERE id=$1
RETURNING id,rule_code,severity,question_id,submission_ids,participant_names,message,score_gap,evidence,
review_status,coalesce(review_disposition,''),coalesce(review_note,''),coalesce(reviewed_by,''),reviewed_at,review_revision`,
		in.FindingID, newStatus, newDisposition, in.Note, in.Actor, newRevision).Scan(&item.ID, &item.RuleCode, &item.Severity,
		&item.QuestionID, &item.SubmissionIDs, &item.ParticipantNames, &item.Message, &item.ScoreGap, &item.Evidence,
		&item.ReviewStatus, &item.ReviewDisposition, &item.ReviewNote, &item.ReviewedBy, &item.ReviewedAt, &item.ReviewRevision)
	if err != nil {
		return FindingRecord{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO score_anomaly_review_audit
(finding_id,actor,from_status,to_status,from_disposition,to_disposition,revision,note)
VALUES ($1,$2,$3,$4,NULLIF($5,''),NULLIF($6,''),$7,$8)`, in.FindingID, in.Actor, oldStatus, newStatus,
		oldDisposition, newDisposition, newRevision, in.Note); err != nil {
		return FindingRecord{}, err
	}
	item.ReviewHistory = []FindingReviewRecord{{Actor: in.Actor, FromStatus: oldStatus, ToStatus: newStatus,
		FromDisposition: oldDisposition, ToDisposition: newDisposition, Revision: newRevision, Note: in.Note}}
	if err := tx.Commit(ctx); err != nil {
		return FindingRecord{}, err
	}
	return item, nil
}

func (s *Service) ProcessNext(ctx context.Context) (bool, error) {
	if err := s.ready(); err != nil {
		return false, err
	}
	workCtx, cancel := context.WithTimeout(ctx, 6*time.Minute)
	defer cancel()
	batch, err := s.claim(workCtx)
	if err != nil || batch == nil {
		return batch != nil, err
	}
	if batch.RuleVersion != RuleVersion {
		return true, s.fail(context.WithoutCancel(ctx), *batch, "RULE_VERSION_CHANGED", false)
	}
	findings, count, err := s.analyze(workCtx, *batch)
	if err != nil {
		code, retryable := anomalyFailureCode(err)
		return true, s.fail(context.WithoutCancel(ctx), *batch, code, retryable)
	}
	if err := s.complete(workCtx, *batch, findings, count); err != nil {
		return true, s.fail(context.WithoutCancel(ctx), *batch, "PERSISTENCE_FAILED", true)
	}
	return true, nil
}

func (s *Service) claim(ctx context.Context) (*Batch, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx, `UPDATE score_anomaly_batches SET status='failed',error_code='ATTEMPTS_EXHAUSTED',
lease_owner=NULL,lease_until=NULL,completed_at=now(),updated_at=now()
WHERE status='running' AND lease_until<=clock_timestamp() AND attempt_count>=max_attempts`)
	if err != nil {
		return nil, err
	}
	var batch Batch
	err = tx.QueryRow(ctx, `WITH candidate AS (
SELECT id FROM score_anomaly_batches WHERE attempt_count<max_attempts AND
((status='queued' AND available_at<=clock_timestamp()) OR (status='running' AND lease_until<=clock_timestamp()))
ORDER BY available_at,id FOR UPDATE SKIP LOCKED LIMIT 1)
UPDATE score_anomaly_batches b SET status='running',attempt_count=b.attempt_count+1,lease_owner=$1,
lease_until=clock_timestamp()+interval '6 minutes',started_at=coalesce(b.started_at,now()),error_code=NULL,updated_at=now()
FROM candidate c WHERE b.id=c.id
RETURNING b.id,b.run_id,b.similarity_batch_id,b.snapshot_hash,b.answer_set_hash,b.grading_set_hash,b.rule_version,b.status,
b.attempt_count,b.max_attempts,coalesce(b.error_code,''),b.submission_count,b.finding_count,b.blocking_count,b.created_at,b.started_at,b.completed_at`,
		s.leaseOwner).Scan(batchTargets(&batch)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &batch, nil
}

func (s *Service) analyze(ctx context.Context, batch Batch) ([]Finding, int, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	input, err := readBoundInput(ctx, tx, batch.RunID)
	if err != nil {
		return nil, 0, err
	}
	if input.snapshotHash != batch.SnapshotHash || input.answerHash != batch.AnswerSetHash ||
		input.gradingHash != batch.GradingSetHash || input.similarityBatchID != batch.SimilarityBatchID {
		return nil, 0, ErrInputChanged
	}
	ids := make([]int64, 0, len(input.submissions))
	submissions := make([]Submission, 0, len(input.submissions))
	submissionIndex := make(map[string]int, len(input.submissions))
	for index, item := range input.submissions {
		id, parseErr := strconv.ParseInt(item.id, 10, 64)
		if parseErr != nil {
			return nil, 0, parseErr
		}
		ids = append(ids, id)
		parsed, parseErr := parseSubmission(item)
		if parseErr != nil {
			return nil, 0, parseErr
		}
		submissions = append(submissions, parsed)
		submissionIndex[item.id] = index
	}
	historyRows, err := tx.Query(ctx, `SELECT h.submission_id,h.generation,h.grading_detail_json
FROM grading_history h JOIN submissions s ON s.id=h.submission_id
WHERE s.run_id=$1 ORDER BY h.submission_id,h.generation`, batch.RunID)
	if err != nil {
		return nil, 0, err
	}
	for historyRows.Next() {
		var id int64
		var generation int64
		var detail []byte
		if err := historyRows.Scan(&id, &generation, &detail); err != nil {
			historyRows.Close()
			return nil, 0, err
		}
		index, ok := submissionIndex[strconv.FormatInt(id, 10)]
		if !ok {
			continue
		}
		questions, parseErr := parseQuestions(detail)
		if parseErr != nil {
			historyRows.Close()
			return nil, 0, parseErr
		}
		submissions[index].History = append(submissions[index].History, History{Generation: generation, Questions: questions})
	}
	if err := historyRows.Err(); err != nil {
		historyRows.Close()
		return nil, 0, err
	}
	historyRows.Close()
	similarityRows, err := tx.Query(ctx, `SELECT question_id,left_submission_id,right_submission_id,risk_level,risk_score,
left_answer,right_answer,common_error_evidence FROM answer_similarity_findings WHERE batch_id=$1`, batch.SimilarityBatchID)
	if err != nil {
		return nil, 0, err
	}
	pairs := make([]SimilarPair, 0)
	for similarityRows.Next() {
		var item SimilarPair
		var leftID, rightID int64
		var commonErrors []byte
		if err := similarityRows.Scan(&item.QuestionID, &leftID, &rightID, &item.RiskLevel, &item.RiskScore,
			&item.LeftAnswer, &item.RightAnswer, &commonErrors); err != nil {
			similarityRows.Close()
			return nil, 0, err
		}
		if err := json.Unmarshal(commonErrors, &item.CommonErrorEvidence); err != nil {
			similarityRows.Close()
			return nil, 0, err
		}
		item.LeftSubmissionID, item.RightSubmissionID = strconv.FormatInt(leftID, 10), strconv.FormatInt(rightID, 10)
		pairs = append(pairs, item)
	}
	if err := similarityRows.Err(); err != nil {
		similarityRows.Close()
		return nil, 0, err
	}
	similarityRows.Close()
	if err := tx.Commit(ctx); err != nil {
		return nil, 0, err
	}
	findings, err := Analyze(submissions, pairs)
	if err != nil {
		return nil, 0, err
	}
	for _, submission := range submissions {
		if submission.Generation == 0 || hasGeneration(submission.History, submission.Generation) {
			continue
		}
		findings = append(findings, Finding{RuleCode: "grading_history_missing", Severity: "blocking",
			SubmissionIDs: []string{submission.ID}, ParticipantNames: []string{submission.Name},
			Message: "当前机器评分代次缺少不可变评分历史快照。", Evidence: map[string]any{"generation": submission.Generation}})
	}
	return findings, len(submissions), nil
}

func (s *Service) complete(ctx context.Context, claimed Batch, findings []Finding, submissionCount int) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var status, owner string
	var attempts int
	if err := tx.QueryRow(ctx, `SELECT status,coalesce(lease_owner,''),attempt_count FROM score_anomaly_batches WHERE id=$1 FOR UPDATE`, claimed.ID).Scan(&status, &owner, &attempts); err != nil {
		return err
	}
	if status != "running" || owner != s.leaseOwner || attempts != claimed.AttemptCount {
		return ErrLeaseLost
	}
	if err := s.checkLeaseDeadline(ctx, tx, claimed.ID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM score_anomaly_findings WHERE batch_id=$1`, claimed.ID); err != nil {
		return err
	}
	queued := &pgx.Batch{}
	blocking := 0
	for _, finding := range findings {
		ids, _ := json.Marshal(finding.SubmissionIDs)
		names, _ := json.Marshal(finding.ParticipantNames)
		evidence, _ := json.Marshal(finding.Evidence)
		queued.Queue(`INSERT INTO score_anomaly_findings
(batch_id,rule_code,severity,question_id,submission_ids,participant_names,message,score_gap,evidence)
VALUES ($1,$2,$3,$4,$5::jsonb,$6::jsonb,$7,$8,$9::jsonb)`, claimed.ID, finding.RuleCode, finding.Severity,
			finding.QuestionID, string(ids), string(names), finding.Message, finding.ScoreGap, string(evidence))
		if finding.Severity == "blocking" {
			blocking++
		}
	}
	if len(findings) > 0 {
		batchResults := tx.SendBatch(ctx, queued)
		for range findings {
			if _, err := batchResults.Exec(); err != nil {
				_ = batchResults.Close()
				return err
			}
		}
		if err := batchResults.Close(); err != nil {
			return err
		}
	}
	tag, err := tx.Exec(ctx, `UPDATE score_anomaly_batches SET status='completed',submission_count=$2,
finding_count=$3,blocking_count=$4,lease_owner=NULL,lease_until=NULL,error_code=NULL,completed_at=now(),updated_at=now()
WHERE id=$1 AND status='running' AND lease_owner=$5 AND lease_until>clock_timestamp()`, claimed.ID, submissionCount, len(findings), blocking, s.leaseOwner)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrLeaseLost
	}
	return tx.Commit(ctx)
}

func (s *Service) fail(ctx context.Context, claimed Batch, code string, retryable bool) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var status, owner string
	var attempts, max int
	if err := tx.QueryRow(ctx, `SELECT status,coalesce(lease_owner,''),attempt_count,max_attempts FROM score_anomaly_batches WHERE id=$1 FOR UPDATE`, claimed.ID).
		Scan(&status, &owner, &attempts, &max); err != nil {
		return err
	}
	if status != "running" || owner != s.leaseOwner || attempts != claimed.AttemptCount {
		return ErrLeaseLost
	}
	if err := s.checkLeaseDeadline(ctx, tx, claimed.ID); err != nil {
		return err
	}
	var tag pgconn.CommandTag
	if retryable && attempts < max {
		wait := time.Duration(1<<min(attempts-1, 4)) * time.Second
		tag, err = tx.Exec(ctx, `UPDATE score_anomaly_batches SET status='queued',available_at=clock_timestamp()+$2::interval,
lease_owner=NULL,lease_until=NULL,error_code=$3,updated_at=now() WHERE id=$1 AND lease_until>clock_timestamp()`, claimed.ID, wait.String(), code)
	} else {
		tag, err = tx.Exec(ctx, `UPDATE score_anomaly_batches SET status='failed',lease_owner=NULL,lease_until=NULL,
error_code=$2,completed_at=now(),updated_at=now() WHERE id=$1 AND lease_until>clock_timestamp()`, claimed.ID, code)
	}
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrLeaseLost
	}
	return tx.Commit(ctx)
}

func readBoundInput(ctx context.Context, tx pgx.Tx, runID string) (boundInput, error) {
	var status, snapshotHash string
	var isLegacy, active bool
	err := tx.QueryRow(ctx, `SELECT status,coalesce(snapshot_hash,''),is_legacy,
EXISTS(SELECT 1 FROM exam_sessions es WHERE es.run_id=exam_runs.id AND es.status='active')
FROM exam_runs WHERE id=$1`, runID).Scan(&status, &snapshotHash, &isLegacy, &active)
	if errors.Is(err, pgx.ErrNoRows) {
		return boundInput{}, ErrRunNotFound
	}
	if err != nil {
		return boundInput{}, err
	}
	if status != "closed" || isLegacy || len(snapshotHash) != 64 || active {
		return boundInput{}, ErrRunNotReady
	}
	rows, err := tx.Query(ctx, `SELECT id,employee_id,name,answers_json,grading_detail_json,objective_score,
subjective_score_machine,subjective_score_final,total_score,grading_generation,review_status,grading_status
FROM submissions WHERE run_id=$1 ORDER BY id LIMIT $2`, runID, maxRunSubmissions+1)
	if err != nil {
		return boundInput{}, err
	}
	input := boundInput{snapshotHash: snapshotHash, submissions: make([]inputSubmission, 0)}
	answerHasher, gradingHasher := sha256.New(), sha256.New()
	totalBytes := 0
	for rows.Next() {
		var item inputSubmission
		var id int64
		if err := rows.Scan(&id, &item.employeeID, &item.name, &item.answers, &item.detail,
			&item.objective, &item.machine, &item.final, &item.total, &item.generation, &item.reviewStatus, &item.gradingStatus); err != nil {
			rows.Close()
			return boundInput{}, err
		}
		item.id = strconv.FormatInt(id, 10)
		totalBytes += len(item.answers) + len(item.detail)
		if len(input.submissions) >= maxRunSubmissions || totalBytes > maxRunBytes {
			rows.Close()
			return boundInput{}, ErrRunNotReady
		}
		if item.gradingStatus != "done" {
			rows.Close()
			return boundInput{}, ErrRunNotReady
		}
		writeHashPart(answerHasher, item.id)
		writeHashPart(answerHasher, item.employeeID)
		writeHashPart(answerHasher, item.name)
		writeHashPart(answerHasher, string(item.answers))
		writeHashPart(gradingHasher, item.id)
		writeHashPart(gradingHasher, strconv.FormatInt(item.generation, 10))
		writeHashPart(gradingHasher, string(item.detail))
		writeHashPart(gradingHasher, strconv.FormatFloat(item.objective, 'g', -1, 64))
		writeHashPart(gradingHasher, strconv.FormatFloat(item.machine, 'g', -1, 64))
		writeHashPart(gradingHasher, strconv.FormatFloat(item.final, 'g', -1, 64))
		writeHashPart(gradingHasher, strconv.FormatFloat(item.total, 'g', -1, 64))
		writeHashPart(gradingHasher, item.reviewStatus)
		input.submissions = append(input.submissions, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return boundInput{}, err
	}
	rows.Close()
	if len(input.submissions) == 0 {
		return boundInput{}, ErrRunNotReady
	}
	input.answerHash = hex.EncodeToString(answerHasher.Sum(nil))
	input.gradingHash = hex.EncodeToString(gradingHasher.Sum(nil))
	err = tx.QueryRow(ctx, `SELECT id FROM answer_similarity_batches WHERE run_id=$1 AND snapshot_hash=$2
AND answer_set_hash=$3 AND status='completed' ORDER BY completed_at DESC,id DESC LIMIT 1`,
		runID, snapshotHash, input.answerHash).Scan(&input.similarityBatchID)
	if errors.Is(err, pgx.ErrNoRows) {
		input.similarityBatchID = 0
	} else if err != nil {
		return boundInput{}, err
	}
	return input, nil
}

func parseSubmission(item inputSubmission) (Submission, error) {
	questions, err := parseQuestions(item.detail)
	if err != nil {
		return Submission{}, err
	}
	return Submission{ID: item.id, EmployeeID: item.employeeID, Name: item.name, Generation: item.generation, Questions: questions}, nil
}

func parseQuestions(raw []byte) ([]QuestionResult, error) {
	var items []map[string]any
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, err
	}
	results := make([]QuestionResult, 0, len(items))
	for _, item := range items {
		parentID := stringValue(item["question_id"])
		if parentID == "" {
			parentID = stringValue(item["id"])
		}
		if children, ok := item["sub_results"].([]any); ok && len(children) > 0 {
			for _, child := range children {
				childMap, ok := child.(map[string]any)
				if !ok {
					return nil, errors.New("invalid composite grading detail")
				}
				childID := stringValue(childMap["question_id"])
				if childID == "" {
					childID = stringValue(childMap["id"])
				}
				if childID == "" {
					return nil, errors.New("composite grading detail is missing a question id")
				}
				if parentID != "" && !strings.Contains(childID, "/") {
					childID = parentID + "/" + childID
				}
				result, err := parseQuestionResult(childMap, childID)
				if err != nil {
					return nil, err
				}
				results = append(results, result)
			}
			continue
		}
		if parentID == "" {
			return nil, errors.New("grading detail is missing a question id")
		}
		result, err := parseQuestionResult(item, parentID)
		if err != nil {
			return nil, err
		}
		results = append(results, result)
	}
	return results, nil
}

func parseQuestionResult(item map[string]any, id string) (QuestionResult, error) {
	if raw, exists := item["detail"]; exists && raw != nil {
		detail, ok := raw.(map[string]any)
		if !ok {
			return QuestionResult{}, errors.New("invalid nested grading detail")
		}
		merged := make(map[string]any, len(detail)+len(item))
		for key, value := range detail {
			merged[key] = value
		}
		// Outer scores carry manual corrections; inner data carries machine evidence.
		for key, value := range item {
			merged[key] = value
		}
		item = merged
	}
	maxScore := numberValue(item["max_score"])
	machine := numberValue(item["machine_score"])
	if _, ok := item["machine_score"]; !ok {
		machine = numberValue(item["score"])
	}
	final := numberValue(item["final_score"])
	if _, ok := item["final_score"]; !ok {
		final = machine
	}
	result := QuestionResult{QuestionID: id, Question: stringValue(item["question"]),
		Answer: stringValue(item["student_answer"]), Machine: machine, Final: final, MaxScore: maxScore,
		Method: stringValue(item["grading_method"]), Backend: stringValue(item["scoring_backend"]),
		Model: stringValue(item["scoring_model"]), Review: stringValue(item["review_status"])}
	for _, key := range []string{"matched_points", "missed_points"} {
		if rawPoints, ok := item[key].([]any); ok {
			for _, rawPoint := range rawPoints {
				point, ok := rawPoint.(map[string]any)
				if !ok {
					return QuestionResult{}, errors.New("invalid scoring point detail")
				}
				located, _ := point["evidence_located"].(bool)
				result.Points = append(result.Points, Point{ID: stringValue(point["point_id"]), Score: numberValue(point["score"]),
					MaxScore: numberValue(point["max_score"]), Relation: stringValue(point["relation"]),
					Evidence: stringValue(point["evidence"]), EvidenceLocated: located,
					ExecutionClaimed:  stringValue(point["evidence_kind"]) == "execution_record",
					ExecutionVerified: verifiedPointExecution(item, point)})
			}
		}
	}
	if !finite(result.Machine) || !finite(result.Final) || !finite(result.MaxScore) {
		return QuestionResult{}, errors.New("grading detail contains non-finite scores")
	}
	return result, nil
}

func hasGeneration(history []History, generation int64) bool {
	for _, item := range history {
		if item.Generation == generation {
			return true
		}
	}
	return false
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}

func numberValue(value any) float64 {
	switch number := value.(type) {
	case float64:
		return number
	case json.Number:
		parsed, _ := number.Float64()
		return parsed
	case int:
		return float64(number)
	case int64:
		return float64(number)
	default:
		return 0
	}
}

func writeHashPart(h hash.Hash, value string) {
	_, _ = h.Write([]byte(strconv.Itoa(len(value))))
	_, _ = h.Write([]byte{':'})
	_, _ = h.Write([]byte(value))
}

func anomalyFailureCode(err error) (string, bool) {
	switch {
	case errors.Is(err, ErrRunNotFound):
		return "RUN_MISSING", false
	case errors.Is(err, ErrRunNotReady):
		return "RUN_NOT_READY", false
	case errors.Is(err, ErrSimilarityNotReady):
		return "SIMILARITY_NOT_READY", false
	case errors.Is(err, ErrInputChanged):
		return "INPUT_CHANGED", false
	case errors.Is(err, ErrFindingLimit):
		return "FINDING_LIMIT", false
	default:
		return "PROCESSING_FAILED", true
	}
}

func batchTargets(batch *Batch) []any {
	return []any{&batch.ID, &batch.RunID, &batch.SimilarityBatchID, &batch.SnapshotHash,
		&batch.AnswerSetHash, &batch.GradingSetHash, &batch.RuleVersion, &batch.Status,
		&batch.AttemptCount, &batch.MaxAttempts, &batch.ErrorCode, &batch.SubmissionCount,
		&batch.FindingCount, &batch.BlockingCount, &batch.CreatedAt, &batch.StartedAt, &batch.CompletedAt}
}

const batchSelect = `SELECT id,run_id,similarity_batch_id,snapshot_hash,answer_set_hash,grading_set_hash,rule_version,status,
attempt_count,max_attempts,coalesce(error_code,''),submission_count,finding_count,blocking_count,created_at,started_at,completed_at
FROM score_anomaly_batches`

// Called after locking the batch: PostgreSQL transaction-start time cannot
// authorize a lease that expired while this writer waited for the row lock.
func (s *Service) checkLeaseDeadline(ctx context.Context, tx pgx.Tx, id int64) error {
	var valid bool
	if err := tx.QueryRow(ctx, `SELECT coalesce(lease_until>clock_timestamp(),false) FROM score_anomaly_batches WHERE id=$1`, id).Scan(&valid); err != nil {
		return err
	}
	if !valid {
		return ErrLeaseLost
	}
	return nil
}
