package answersimilarity

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"hash"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/yhwyxy/examSystem/internal/papers"
)

const (
	maxRunParticipants = 5_000
	maxAnswerJSONBytes = 256 << 10
	maxRunAnswerBytes  = 64 << 20
	maxStoredFindings  = 10_000
	batchMaxAttempts   = 3
)

var (
	ErrNotConfigured           = errors.New("answer similarity service is not configured")
	ErrRunNotFound             = errors.New("answer similarity run not found")
	ErrRunNotClosed            = errors.New("answer similarity requires a closed run")
	ErrRunIncomplete           = errors.New("answer similarity requires all sessions to be submitted")
	ErrNoSubmissions           = errors.New("answer similarity run has no submissions")
	ErrCurrentReportNotReady   = errors.New("answer similarity has no completed report for current answers")
	ErrRunTooLarge             = errors.New("answer similarity run exceeds processing limits")
	ErrAnswerSetChanged        = errors.New("answer similarity answer set changed")
	ErrSnapshotInvalid         = errors.New("answer similarity snapshot is invalid")
	ErrBatchNotFound           = errors.New("answer similarity batch not found")
	ErrFindingNotFound         = errors.New("answer similarity finding not found")
	ErrReviewRevision          = errors.New("answer similarity review revision conflict")
	ErrInvalidReview           = errors.New("invalid answer similarity review")
	ErrLeaseLost               = errors.New("answer similarity batch lease lost")
	ErrTooManyStoredFindings   = errors.New("answer similarity report exceeds finding limit")
	ErrEmbeddingConfigChanged  = errors.New("answer embedding configuration changed after batch creation")
	ErrEmbeddingBudgetExceeded = errors.New("answer embedding analysis budget exceeded")
)

type Batch struct {
	ID              int64      `json:"id"`
	RunID           string     `json:"run_id"`
	SnapshotHash    string     `json:"snapshot_hash"`
	AnswerSetHash   string     `json:"answer_set_hash"`
	RuleVersion     string     `json:"rule_version"`
	Status          string     `json:"status"`
	AttemptCount    int        `json:"attempt_count"`
	MaxAttempts     int        `json:"max_attempts"`
	ErrorCode       string     `json:"error_code,omitempty"`
	SubmissionCount int        `json:"submission_count"`
	QuestionCount   int        `json:"question_count"`
	FindingCount    int        `json:"finding_count"`
	CreatedAt       time.Time  `json:"created_at"`
	StartedAt       *time.Time `json:"started_at,omitempty"`
	CompletedAt     *time.Time `json:"completed_at,omitempty"`
}

type ReviewEvent struct {
	ID        int64     `json:"id"`
	Actor     string    `json:"actor"`
	FromState string    `json:"from_state"`
	ToState   string    `json:"to_state"`
	Revision  int       `json:"revision"`
	Note      string    `json:"note"`
	CreatedAt time.Time `json:"created_at"`
}

type FindingRecord struct {
	Finding
	ID             int64         `json:"id"`
	ReviewState    string        `json:"review_state"`
	ReviewRevision int           `json:"review_revision"`
	ReviewNote     string        `json:"review_note,omitempty"`
	ReviewedBy     string        `json:"reviewed_by,omitempty"`
	ReviewedAt     *time.Time    `json:"reviewed_at,omitempty"`
	ReviewHistory  []ReviewEvent `json:"review_history"`
}

type RunCandidate struct {
	RunID              string     `json:"run_id"`
	PaperID            string     `json:"paper_id"`
	RoundNo            int        `json:"round_no"`
	Status             string     `json:"status"`
	ClosedAt           *time.Time `json:"closed_at,omitempty"`
	SubmissionCount    int        `json:"submission_count"`
	ActiveSessionCount int        `json:"active_session_count"`
}

type BatchPage struct {
	Batch    Batch           `json:"batch"`
	Findings []FindingRecord `json:"findings"`
	Limit    int             `json:"limit"`
	Offset   int             `json:"offset"`
}

type SnapshotLoader func(path, expectedSHA256Hex string) (*papers.Snapshot, error)
type EmbedderLoader func(context.Context) (Embedder, error)

type embeddingBudget struct {
	remainingInputs int
	remainingBytes  int
}

func (budget *embeddingBudget) consume(inputs []string) error {
	if budget == nil || len(inputs) > budget.remainingInputs {
		return ErrEmbeddingBudgetExceeded
	}
	byteCount := 0
	for _, input := range inputs {
		byteCount += len(input)
		if byteCount > budget.remainingBytes {
			return ErrEmbeddingBudgetExceeded
		}
	}
	budget.remainingInputs -= len(inputs)
	budget.remainingBytes -= byteCount
	return nil
}

type Service struct {
	pool         *pgxpool.Pool
	loadSnapshot SnapshotLoader
	loadEmbedder EmbedderLoader
	leaseOwner   string
}

func NewService(pool *pgxpool.Pool, loader SnapshotLoader) *Service {
	if loader == nil {
		loader = papers.LoadSnapshot
	}
	ownerBytes := make([]byte, 12)
	if _, err := rand.Read(ownerBytes); err != nil {
		ownerBytes = []byte(strconv.FormatInt(time.Now().UnixNano(), 10))
	}
	return &Service{pool: pool, loadSnapshot: loader, leaseOwner: hex.EncodeToString(ownerBytes)}
}

func (s *Service) WithEmbedderLoader(loader EmbedderLoader) *Service {
	if s != nil {
		s.loadEmbedder = loader
	}
	return s
}

func (s *Service) currentEmbedder(ctx context.Context) (Embedder, string, error) {
	if s == nil || s.loadEmbedder == nil {
		return nil, RuleVersion, nil
	}
	embedder, err := s.loadEmbedder(ctx)
	if err != nil {
		return nil, "", err
	}
	if embedder == nil {
		return nil, RuleVersion, nil
	}
	version := strings.TrimSpace(embedder.Version())
	if version == "" || len(version) > 256 {
		return nil, "", ErrEmbeddingUnavailable
	}
	return embedder, version, nil
}

type runInput struct {
	id, paperID, status, snapshotPath, snapshotHash string
	roundNo                                         int
	isLegacy, hasActiveSessions                     bool
	closedAt                                        *time.Time
}

type submissionInput struct {
	id         int64
	employeeID string
	name       string
	answersRaw []byte
	answers    map[string]any
}

func (s *Service) ready() error {
	if s == nil || s.pool == nil {
		return ErrNotConfigured
	}
	return nil
}

// Enqueue freezes the closed run's immutable snapshot and answer set into an
// idempotent queued batch. New attempts use a new answer hash and never replace
// an older report.
func (s *Service) Enqueue(ctx context.Context, runID string) (Batch, bool, error) {
	for attempt := 0; ; attempt++ {
		batch, created, err := s.enqueueOnce(ctx, runID)
		var pgErr *pgconn.PgError
		if attempt >= 2 || !errors.As(err, &pgErr) || (pgErr.Code != "40001" && pgErr.Code != "40P01") {
			return batch, created, err
		}
		// A closing run or concurrent idempotent enqueue can invalidate the
		// repeatable-read snapshot. Retry the entire rolled-back transaction.
		if err := ctx.Err(); err != nil {
			return Batch{}, false, err
		}
	}
}

func (s *Service) enqueueOnce(ctx context.Context, runID string) (Batch, bool, error) {
	if err := s.ready(); err != nil {
		return Batch{}, false, err
	}
	runID = strings.TrimSpace(runID)
	if runID == "" || len(runID) > 200 {
		return Batch{}, false, ErrRunNotFound
	}
	_, ruleVersion, err := s.currentEmbedder(ctx)
	if err != nil {
		return Batch{}, false, err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return Batch{}, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	run, err := readRun(ctx, tx, runID, true)
	if err != nil {
		return Batch{}, false, err
	}
	if run.status != "closed" || run.isLegacy || run.snapshotPath == "" || len(run.snapshotHash) != 64 {
		return Batch{}, false, ErrRunNotClosed
	}
	if run.hasActiveSessions {
		return Batch{}, false, ErrRunIncomplete
	}
	submissions, answerHash, err := readSubmissions(ctx, tx, runID)
	if err != nil {
		return Batch{}, false, err
	}
	if len(submissions) == 0 {
		return Batch{}, false, ErrNoSubmissions
	}
	var batch Batch
	err = tx.QueryRow(ctx, `
INSERT INTO answer_similarity_batches
    (run_id, snapshot_hash, answer_set_hash, rule_version, status, max_attempts)
VALUES ($1,$2,$3,$4,'queued',$5)
ON CONFLICT (run_id, snapshot_hash, answer_set_hash, rule_version) DO NOTHING
RETURNING id, run_id, snapshot_hash, answer_set_hash, rule_version, status,
          attempt_count, max_attempts, coalesce(error_code,''), submission_count,
          question_count, finding_count, created_at, started_at, completed_at`,
		run.id, run.snapshotHash, answerHash, ruleVersion, batchMaxAttempts).Scan(batchScanTargets(&batch)...)
	created := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Batch{}, false, err
	}
	if !created {
		if err := tx.QueryRow(ctx, batchSelect+` WHERE run_id=$1 AND snapshot_hash=$2 AND answer_set_hash=$3 AND rule_version=$4`,
			run.id, run.snapshotHash, answerHash, ruleVersion).Scan(batchScanTargets(&batch)...); err != nil {
			return Batch{}, false, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return Batch{}, false, err
	}
	return batch, created, nil
}

// ListRuns returns recent closed runs eligible for detection or already carrying a report.
func (s *Service) ListRuns(ctx context.Context, limit int) ([]RunCandidate, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `
SELECT r.id, r.paper_id, r.round_no, r.status, r.closed_at,
       (SELECT count(*) FROM submissions s WHERE s.run_id=r.id)::int,
       (SELECT count(*) FROM exam_sessions es WHERE es.run_id=r.id AND es.status='active')::int
FROM exam_runs r
WHERE r.status='closed' AND r.is_legacy=false AND r.snapshot_path IS NOT NULL
  AND r.snapshot_hash IS NOT NULL
ORDER BY r.closed_at DESC NULLS LAST, r.created_at DESC, r.id
LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RunCandidate{}
	for rows.Next() {
		var item RunCandidate
		if err := rows.Scan(&item.RunID, &item.PaperID, &item.RoundNo, &item.Status,
			&item.ClosedAt, &item.SubmissionCount, &item.ActiveSessionCount); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *Service) ListBatches(ctx context.Context, runID string) ([]Batch, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, batchSelect+` WHERE run_id=$1 ORDER BY created_at DESC,id DESC`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Batch{}
	for rows.Next() {
		var item Batch
		if err := rows.Scan(batchScanTargets(&item)...); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// CurrentCompletedBatch returns the completed report whose frozen input still
// matches the current closed run. It shares the caller's transaction so queue
// and publication reads can verify report freshness consistently.
func (s *Service) CurrentCompletedBatch(ctx context.Context, tx pgx.Tx, runID string) (Batch, error) {
	if err := s.ready(); err != nil {
		return Batch{}, err
	}
	_, ruleVersion, err := s.currentEmbedder(ctx)
	if err != nil {
		return Batch{}, err
	}
	run, err := readRun(ctx, tx, runID, false)
	if err != nil {
		return Batch{}, err
	}
	if run.status != "closed" || run.isLegacy || run.snapshotPath == "" || len(run.snapshotHash) != 64 {
		return Batch{}, ErrRunNotClosed
	}
	if run.hasActiveSessions {
		return Batch{}, ErrRunIncomplete
	}
	submissions, answerHash, err := readSubmissions(ctx, tx, runID)
	if err != nil {
		return Batch{}, err
	}
	if len(submissions) == 0 {
		return Batch{}, ErrNoSubmissions
	}
	var batch Batch
	err = tx.QueryRow(ctx, batchSelect+` WHERE run_id=$1 AND snapshot_hash=$2 AND answer_set_hash=$3
AND rule_version=$4 AND status='completed' ORDER BY completed_at DESC,id DESC LIMIT 1`, runID,
		run.snapshotHash, answerHash, ruleVersion).Scan(batchScanTargets(&batch)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return Batch{}, ErrCurrentReportNotReady
	}
	return batch, err
}

func (s *Service) GetBatchPage(ctx context.Context, batchID int64, limit, offset int) (BatchPage, error) {
	if err := s.ready(); err != nil {
		return BatchPage{}, err
	}
	if batchID <= 0 {
		return BatchPage{}, ErrBatchNotFound
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}
	var page BatchPage
	page.Limit, page.Offset = limit, offset
	if err := s.pool.QueryRow(ctx, batchSelect+` WHERE id=$1`, batchID).Scan(batchScanTargets(&page.Batch)...); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return BatchPage{}, ErrBatchNotFound
		}
		return BatchPage{}, err
	}
	rows, err := s.pool.Query(ctx, findingSelect+` WHERE batch_id=$1 ORDER BY risk_score DESC,id LIMIT $2 OFFSET $3`, batchID, limit, offset)
	if err != nil {
		return BatchPage{}, err
	}
	defer rows.Close()
	ids := []int64{}
	for rows.Next() {
		var item FindingRecord
		if err := rows.Scan(findingScanTargets(&item)...); err != nil {
			return BatchPage{}, err
		}
		item.ReviewHistory = []ReviewEvent{}
		ids = append(ids, item.ID)
		page.Findings = append(page.Findings, item)
	}
	if err := rows.Err(); err != nil {
		return BatchPage{}, err
	}
	if len(ids) > 0 {
		historyRows, err := s.pool.Query(ctx, `SELECT id,finding_id,actor,from_state,to_state,revision,note,created_at
FROM answer_similarity_review_audit WHERE finding_id=ANY($1) ORDER BY id`, ids)
		if err != nil {
			return BatchPage{}, err
		}
		defer historyRows.Close()
		byID := make(map[int64][]ReviewEvent, len(ids))
		for historyRows.Next() {
			var id, findingID int64
			var event ReviewEvent
			if err := historyRows.Scan(&event.ID, &findingID, &event.Actor, &event.FromState,
				&event.ToState, &event.Revision, &event.Note, &event.CreatedAt); err != nil {
				return BatchPage{}, err
			}
			id = findingID
			byID[id] = append(byID[id], event)
		}
		if err := historyRows.Err(); err != nil {
			return BatchPage{}, err
		}
		for i := range page.Findings {
			page.Findings[i].ReviewHistory = byID[page.Findings[i].ID]
		}
	}
	return page, nil
}

type ReviewInput struct {
	FindingID        int64
	ExpectedRevision int
	State            string
	Note             string
	Actor            string
}

func (s *Service) Review(ctx context.Context, input ReviewInput) (FindingRecord, error) {
	if err := s.ready(); err != nil {
		return FindingRecord{}, err
	}
	input.Note = strings.TrimSpace(input.Note)
	if input.FindingID <= 0 || input.ExpectedRevision < 0 ||
		(input.State != "normal_consistent" && input.State != "needs_review" && input.State != "confirmed_abnormal") ||
		input.Note == "" || len([]rune(input.Note)) > 2000 {
		return FindingRecord{}, ErrInvalidReview
	}
	if input.Actor == "" {
		input.Actor = "admin"
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return FindingRecord{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var fromState string
	var revision int
	err = tx.QueryRow(ctx, `SELECT review_state,review_revision
FROM answer_similarity_findings WHERE id=$1 FOR UPDATE`, input.FindingID).Scan(&fromState, &revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return FindingRecord{}, ErrFindingNotFound
	}
	if err != nil {
		return FindingRecord{}, err
	}
	if revision != input.ExpectedRevision {
		return FindingRecord{}, ErrReviewRevision
	}
	if _, err := tx.Exec(ctx, `UPDATE answer_similarity_findings
SET review_state=$2,review_revision=review_revision+1,review_note=$3,reviewed_by=$4,reviewed_at=now()
WHERE id=$1`, input.FindingID, input.State, input.Note, input.Actor); err != nil {
		return FindingRecord{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO answer_similarity_review_audit
(finding_id,actor,from_state,to_state,revision,note)
VALUES ($1,$2,$3,$4,$5,$6)`, input.FindingID, input.Actor, fromState,
		input.State, revision+1, input.Note); err != nil {
		return FindingRecord{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return FindingRecord{}, err
	}
	return s.getFinding(ctx, input.FindingID)
}

func (s *Service) getFinding(ctx context.Context, findingID int64) (FindingRecord, error) {
	var item FindingRecord
	if err := s.pool.QueryRow(ctx, findingSelect+` WHERE id=$1`, findingID).Scan(findingScanTargets(&item)...); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return FindingRecord{}, ErrFindingNotFound
		}
		return FindingRecord{}, err
	}
	item.ReviewHistory = []ReviewEvent{}
	rows, err := s.pool.Query(ctx, `SELECT id,actor,from_state,to_state,revision,note,created_at
FROM answer_similarity_review_audit WHERE finding_id=$1 ORDER BY id`, findingID)
	if err != nil {
		return FindingRecord{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var event ReviewEvent
		if err := rows.Scan(&event.ID, &event.Actor, &event.FromState, &event.ToState,
			&event.Revision, &event.Note, &event.CreatedAt); err != nil {
			return FindingRecord{}, err
		}
		item.ReviewHistory = append(item.ReviewHistory, event)
	}
	return item, rows.Err()
}

// ProcessNext claims at most one durable queue item. A false result means the
// queue is idle; failed analysis is persisted as an explicit failed state.
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
	findings, questionCount, submissionCount, err := s.analyzeBatch(workCtx, *batch)
	if err != nil {
		code, retryable := failureCode(err)
		stateCtx, stateCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer stateCancel()
		return true, s.fail(stateCtx, *batch, code, retryable)
	}
	if err := s.complete(workCtx, *batch, findings, questionCount, submissionCount); err != nil {
		stateCtx, stateCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer stateCancel()
		return true, s.fail(stateCtx, *batch, "PERSISTENCE_FAILED", true)
	}
	return true, nil
}

func (s *Service) claim(ctx context.Context) (*Batch, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `UPDATE answer_similarity_batches
SET status='failed',error_code='ATTEMPTS_EXHAUSTED',lease_owner=NULL,lease_until=NULL,
    completed_at=now(),updated_at=now()
WHERE status='running' AND lease_until<=clock_timestamp() AND attempt_count>=max_attempts`); err != nil {
		return nil, err
	}
	var batch Batch
	err = tx.QueryRow(ctx, `
WITH candidate AS (
  SELECT id FROM answer_similarity_batches
  WHERE attempt_count < max_attempts AND (
    (status='queued' AND available_at<=clock_timestamp()) OR
    (status='running' AND lease_until<=clock_timestamp())
  )
  ORDER BY available_at,id
  FOR UPDATE SKIP LOCKED LIMIT 1
)
UPDATE answer_similarity_batches b
SET status='running',attempt_count=b.attempt_count+1,lease_owner=$1,
    lease_until=clock_timestamp()+interval '6 minutes',started_at=coalesce(b.started_at,now()),
    error_code=NULL,updated_at=now()
FROM candidate c WHERE b.id=c.id
RETURNING b.id,b.run_id,b.snapshot_hash,b.answer_set_hash,b.rule_version,b.status,
          b.attempt_count,b.max_attempts,coalesce(b.error_code,''),b.submission_count,
          b.question_count,b.finding_count,b.created_at,b.started_at,b.completed_at`, s.leaseOwner).Scan(batchScanTargets(&batch)...)
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

func (s *Service) analyzeBatch(ctx context.Context, batch Batch) ([]Finding, int, int, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return nil, 0, 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	run, err := readRun(ctx, tx, batch.RunID, false)
	if err != nil {
		return nil, 0, 0, err
	}
	if run.status != "closed" || run.isLegacy || run.snapshotPath == "" || run.snapshotHash != batch.SnapshotHash {
		return nil, 0, 0, ErrSnapshotInvalid
	}
	submissions, answerHash, err := readSubmissions(ctx, tx, batch.RunID)
	if err != nil {
		return nil, 0, 0, err
	}
	if answerHash != batch.AnswerSetHash {
		return nil, 0, 0, ErrAnswerSetChanged
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, 0, 0, err
	}
	embedder, currentRuleVersion, err := s.currentEmbedder(ctx)
	if err != nil {
		return nil, 0, 0, err
	}
	if currentRuleVersion != batch.RuleVersion {
		return nil, 0, 0, ErrEmbeddingConfigChanged
	}
	snapshot, err := s.loadSnapshot(run.snapshotPath, run.snapshotHash)
	if err != nil || snapshot == nil {
		return nil, 0, 0, ErrSnapshotInvalid
	}
	questions := listMaps(snapshot.Doc["questions"])
	if len(questions) == 0 {
		return nil, 0, 0, ErrSnapshotInvalid
	}
	for _, question := range questions {
		if stringField(question, "id") == "" {
			return nil, 0, 0, ErrSnapshotInvalid
		}
		if strings.EqualFold(stringField(question, "type"), "composite") || len(listMaps(question["subquestions"])) > 0 {
			subquestions := listMaps(question["subquestions"])
			if len(subquestions) == 0 {
				return nil, 0, 0, ErrSnapshotInvalid
			}
			for _, subquestion := range subquestions {
				if stringField(subquestion, "id") == "" {
					return nil, 0, 0, ErrSnapshotInvalid
				}
			}
		}
	}
	findings := make([]Finding, 0)
	questionCount := 0
	embeddingBudget := embeddingBudget{remainingInputs: maxEmbeddingInputsPerBatch, remainingBytes: maxEmbeddingTextBytes}
	for _, question := range questions {
		if strings.EqualFold(stringField(question, "type"), "composite") || len(listMaps(question["subquestions"])) > 0 {
			for _, sub := range listMaps(question["subquestions"]) {
				if len(findings) > maxStoredFindings {
					return nil, 0, 0, ErrTooManyStoredFindings
				}
				questionCount++
				id := stringField(question, "id") + "/" + stringField(sub, "id")
				label := stringField(question, "question") + " · " + stringField(sub, "question")
				analysis, err := analyzeQuestion(ctx, id, label, sub, submissions,
					stringField(question, "id"), stringField(sub, "id"), embedder, &embeddingBudget)
				if err != nil {
					return nil, 0, 0, err
				}
				findings = append(findings, analysis...)
				if len(findings) > maxStoredFindings {
					return nil, 0, 0, ErrTooManyStoredFindings
				}
			}
			continue
		}
		questionCount++
		analysis, err := analyzeQuestion(ctx, stringField(question, "id"), stringField(question, "question"),
			question, submissions, "", "", embedder, &embeddingBudget)
		if err != nil {
			return nil, 0, 0, err
		}
		findings = append(findings, analysis...)
		if len(findings) > maxStoredFindings {
			return nil, 0, 0, ErrTooManyStoredFindings
		}
	}
	RaiseCrossQuestionRisk(findings)
	return findings, questionCount, len(submissions), nil
}

func analyzeQuestion(ctx context.Context, questionID, label string, question map[string]any,
	submissions []submissionInput, parentID, childID string, embedder Embedder, embeddingBudget *embeddingBudget) ([]Finding, error) {
	questionType := stringField(question, "type")
	if strings.EqualFold(questionType, "single_choice") || strings.EqualFold(questionType, "multiple_choice") || strings.EqualFold(questionType, "true_false") {
		return nil, nil
	}
	mode := stringField(question, "scoring_mode")
	if mode == "" && (stringField(question, "code_language") != "" || len(mapField(question, "answers_by_language")) > 0) {
		mode = "code"
	}
	groups := map[string][]Response{}
	groupReferences := map[string]string{}
	for _, sub := range submissions {
		raw := sub.answers[questionID]
		if childID != "" {
			parentAnswer, ok := sub.answers[parentID].(map[string]any)
			if !ok {
				continue
			}
			raw = parentAnswer[childID]
		}
		answer, language := answerText(raw), answerLanguage(raw)
		if answer == "" {
			continue
		}
		if mode == "code" && language == "" {
			language = stringField(question, "code_language")
		}
		group := ""
		if mode == "code" {
			group = language
		}
		groups[group] = append(groups[group], Response{
			SubmissionID: strconv.FormatInt(sub.id, 10), ParticipantID: sub.employeeID,
			Name: sub.name, Answer: answer,
		})
		groupReferences[group] = referenceForQuestion(question, language)
	}
	all := []Finding{}
	groupNames := make([]string, 0, len(groups))
	for group := range groups {
		groupNames = append(groupNames, group)
	}
	sort.Strings(groupNames)
	for _, group := range groupNames {
		responses := groups[group]
		if len(responses) < 2 {
			continue
		}
		analysisQuestion := Question{ID: questionID, Type: questionType, Mode: mode,
			Language: group, ReferenceAnswer: groupReferences[group]}
		var analysis Analysis
		var err error
		if embedder == nil {
			analysis, err = Analyze(analysisQuestion, responses)
		} else {
			reference := strings.TrimSpace(groupReferences[group])
			totalRunes := 0
			for _, response := range responses {
				runeCount := utf8.RuneCountInString(response.Answer)
				if runeCount > maxAnswerRunes {
					return nil, ErrAnswerTooLong
				}
				totalRunes += runeCount
				if totalRunes > maxTotalAnswerRunes {
					return nil, ErrAnswerTooLong
				}
			}
			eligibleResponses := make([]Response, 0, len(responses))
			for _, response := range responses {
				cleaned := normalizeAnswer(response.Answer, mode, group)
				if len([]rune(cleaned)) >= 24 && len(tokenize(cleaned)) >= 4 {
					eligibleResponses = append(eligibleResponses, response)
				}
			}
			if len(eligibleResponses) < 2 {
				analysis, err = Analyze(analysisQuestion, responses)
			} else {
				inputs := make([]string, 0, len(eligibleResponses)+1)
				inputIndexes := make(map[string]int, len(eligibleResponses)+1)
				addInput := func(value string) int {
					if index, exists := inputIndexes[value]; exists {
						return index
					}
					index := len(inputs)
					inputIndexes[value] = index
					inputs = append(inputs, value)
					return index
				}
				referenceIndex := -1
				if reference != "" {
					referenceIndex = addInput(reference)
				}
				responseIndexes := make(map[string]int, len(eligibleResponses))
				for _, response := range eligibleResponses {
					responseIndexes[response.SubmissionID] = addInput(response.Answer)
				}
				if err := embeddingBudget.consume(inputs); err != nil {
					return nil, err
				}
				vectors, embedErr := embedder.Embed(ctx, inputs)
				if embedErr != nil || len(vectors) != len(inputs) {
					return nil, ErrEmbeddingUnavailable
				}
				var referenceVector []float64
				if referenceIndex >= 0 {
					referenceVector = vectors[referenceIndex]
				}
				bySubmission := make(map[string][]float64, len(eligibleResponses))
				for _, response := range eligibleResponses {
					bySubmission[response.SubmissionID] = vectors[responseIndexes[response.SubmissionID]]
				}
				analysis, err = AnalyzeWithEmbeddings(analysisQuestion, responses, bySubmission, referenceVector)
			}
		}
		if err != nil {
			return nil, err
		}
		for i := range analysis.Findings {
			analysis.Findings[i].QuestionLabel = label
		}
		all = append(all, analysis.Findings...)
	}
	return all, nil
}

func (s *Service) complete(ctx context.Context, claimed Batch, findings []Finding, questionCount, submissionCount int) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var owner, status string
	var attempts int
	if err := tx.QueryRow(ctx, `SELECT status,coalesce(lease_owner,''),attempt_count FROM answer_similarity_batches WHERE id=$1 FOR UPDATE`, claimed.ID).Scan(&status, &owner, &attempts); err != nil {
		return err
	}
	if status != "running" || owner != s.leaseOwner || attempts != claimed.AttemptCount {
		return ErrLeaseLost
	}
	if err := s.checkLeaseDeadline(ctx, tx, claimed.ID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM answer_similarity_findings WHERE batch_id=$1`, claimed.ID); err != nil {
		return err
	}
	const insertSQL = `INSERT INTO answer_similarity_findings
(batch_id,question_id,question_label,left_submission_id,right_submission_id,left_participant_id,right_participant_id,
 left_name,right_name,left_answer,right_answer,risk_level,risk_score,text_overlap,semantic_similarity,reference_similarity,
 shared_excerpt,left_excerpt,right_excerpt,reasons,common_error_evidence,related_question_ids,features)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20::jsonb,$21::jsonb,$22::jsonb,$23::jsonb)`
	queued := &pgx.Batch{}
	for _, finding := range findings {
		leftID, err := strconv.ParseInt(finding.LeftSubmissionID, 10, 64)
		if err != nil {
			return err
		}
		rightID, err := strconv.ParseInt(finding.RightSubmissionID, 10, 64)
		if err != nil {
			return err
		}
		if leftID > rightID {
			leftID, rightID = rightID, leftID
		}
		reasons, _ := json.Marshal(finding.Reasons)
		commonErrors, _ := json.Marshal(finding.CommonErrorEvidence)
		related, _ := json.Marshal(finding.RelatedQuestionIDs)
		features, _ := json.Marshal(finding.Features)
		queued.Queue(insertSQL, claimed.ID, finding.QuestionID, finding.QuestionLabel, leftID, rightID,
			finding.LeftParticipantID, finding.RightParticipantID, finding.LeftName, finding.RightName,
			finding.LeftAnswer, finding.RightAnswer, finding.RiskLevel, finding.RiskScore,
			finding.TextOverlap, finding.SemanticSimilarity, finding.ReferenceSimilarity, finding.SharedExcerpt,
			finding.LeftExcerpt, finding.RightExcerpt, string(reasons), string(commonErrors), string(related), string(features))
	}
	if len(findings) > 0 {
		br := tx.SendBatch(ctx, queued)
		for range findings {
			if _, err := br.Exec(); err != nil {
				_ = br.Close()
				return err
			}
		}
		if err := br.Close(); err != nil {
			return err
		}
	}
	tag, err := tx.Exec(ctx, `UPDATE answer_similarity_batches
SET status='completed',submission_count=$2,question_count=$3,finding_count=$4,
    lease_owner=NULL,lease_until=NULL,error_code=NULL,completed_at=now(),updated_at=now()
WHERE id=$1 AND status='running' AND lease_owner=$5 AND lease_until>clock_timestamp()`,
		claimed.ID, submissionCount, questionCount, len(findings), s.leaseOwner)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrLeaseLost
	}
	return tx.Commit(ctx)
}

func (s *Service) fail(ctx context.Context, claimed Batch, code string, retryable bool) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var attempts, maxAttempts int
	var owner, status string
	if err := tx.QueryRow(ctx, `SELECT status,coalesce(lease_owner,''),attempt_count,max_attempts
FROM answer_similarity_batches WHERE id=$1 FOR UPDATE`, claimed.ID).Scan(&status, &owner, &attempts, &maxAttempts); err != nil {
		return err
	}
	if status != "running" || owner != s.leaseOwner || attempts != claimed.AttemptCount {
		return ErrLeaseLost
	}
	if err := s.checkLeaseDeadline(ctx, tx, claimed.ID); err != nil {
		return err
	}
	var tag pgconn.CommandTag
	if retryable && attempts < maxAttempts {
		wait := time.Duration(1<<min(attempts-1, 4)) * time.Second
		tag, err = tx.Exec(ctx, `UPDATE answer_similarity_batches
SET status='queued',available_at=clock_timestamp()+$2::interval,lease_owner=NULL,lease_until=NULL,
    error_code=$3,updated_at=now() WHERE id=$1 AND lease_until>clock_timestamp()`, claimed.ID, wait.String(), code)
	} else {
		tag, err = tx.Exec(ctx, `UPDATE answer_similarity_batches
SET status='failed',lease_owner=NULL,lease_until=NULL,error_code=$2,
    completed_at=now(),updated_at=now() WHERE id=$1 AND lease_until>clock_timestamp()`, claimed.ID, code)
	}
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrLeaseLost
	}
	return tx.Commit(ctx)
}

func failureCode(err error) (string, bool) {
	switch {
	case errors.Is(err, ErrRunNotFound):
		return "RUN_MISSING", false
	case errors.Is(err, ErrRunNotClosed), errors.Is(err, ErrRunIncomplete):
		return "RUN_NOT_READY", false
	case errors.Is(err, ErrRunTooLarge):
		return "RUN_TOO_LARGE", false
	case errors.Is(err, ErrAnswerSetChanged):
		return "ANSWER_SET_CHANGED", false
	case errors.Is(err, ErrSnapshotInvalid):
		return "SNAPSHOT_INVALID", false
	case errors.Is(err, ErrAnswerTooLong):
		return "ANSWER_TOO_LARGE", false
	case errors.Is(err, ErrCandidateLimit):
		return "CANDIDATE_LIMIT", false
	case errors.Is(err, ErrTooManyStoredFindings):
		return "FINDING_LIMIT", false
	case errors.Is(err, ErrEmbeddingConfigChanged):
		return "EMBEDDING_CONFIG_CHANGED", false
	case errors.Is(err, ErrEmbeddingBudgetExceeded):
		return "EMBEDDING_BUDGET_EXCEEDED", false
	default:
		return "PROCESSING_FAILED", true
	}
}

func readRun(ctx context.Context, tx pgx.Tx, runID string, lock bool) (runInput, error) {
	query := `SELECT id,paper_id,round_no,status,coalesce(snapshot_path,''),coalesce(snapshot_hash,''),is_legacy,closed_at,
EXISTS(SELECT 1 FROM exam_sessions es WHERE es.run_id=exam_runs.id AND es.status='active')
FROM exam_runs WHERE id=$1`
	if lock {
		query += ` FOR SHARE`
	}
	var run runInput
	err := tx.QueryRow(ctx, query, runID).Scan(&run.id, &run.paperID, &run.roundNo, &run.status, &run.snapshotPath,
		&run.snapshotHash, &run.isLegacy, &run.closedAt, &run.hasActiveSessions)
	if errors.Is(err, pgx.ErrNoRows) {
		return runInput{}, ErrRunNotFound
	}
	return run, err
}

func readSubmissions(ctx context.Context, tx pgx.Tx, runID string) ([]submissionInput, string, error) {
	rows, err := tx.Query(ctx, `SELECT id,employee_id,name,answers_json FROM submissions
WHERE run_id=$1 ORDER BY id LIMIT $2`, runID, maxRunParticipants+1)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	out := make([]submissionInput, 0)
	totalBytes := 0
	for rows.Next() {
		var item submissionInput
		if err := rows.Scan(&item.id, &item.employeeID, &item.name, &item.answersRaw); err != nil {
			return nil, "", err
		}
		if len(out) >= maxRunParticipants || len(item.answersRaw) > maxAnswerJSONBytes {
			return nil, "", ErrRunTooLarge
		}
		totalBytes += len(item.answersRaw)
		if totalBytes > maxRunAnswerBytes {
			return nil, "", ErrRunTooLarge
		}
		dec := json.NewDecoder(strings.NewReader(string(item.answersRaw)))
		dec.UseNumber()
		if err := dec.Decode(&item.answers); err != nil || item.answers == nil {
			return nil, "", ErrSnapshotInvalid
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	h := sha256.New()
	for _, item := range out {
		writeHashPart(h, strconv.FormatInt(item.id, 10))
		writeHashPart(h, item.employeeID)
		writeHashPart(h, item.name)
		writeHashPart(h, string(item.answersRaw))
	}
	return out, hex.EncodeToString(h.Sum(nil)), nil
}

func writeHashPart(h hash.Hash, value string) {
	_, _ = h.Write([]byte(strconv.Itoa(len(value))))
	_, _ = h.Write([]byte{':'})
	_, _ = h.Write([]byte(value))
}

func answerText(raw any) string {
	switch value := raw.(type) {
	case string:
		return value
	case json.Number:
		return value.String()
	case float64:
		return strconv.FormatFloat(value, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(value)
	case map[string]any:
		if answer, ok := value["answer"]; ok {
			return answerText(answer)
		}
		if answer, ok := value["text"]; ok {
			return answerText(answer)
		}
	case []any:
		parts := make([]string, 0, len(value))
		for _, item := range value {
			parts = append(parts, answerText(item))
		}
		return strings.Join(parts, " ")
	}
	return ""
}

func answerLanguage(raw any) string {
	if object, ok := raw.(map[string]any); ok {
		return strings.ToLower(strings.TrimSpace(stringField(object, "language")))
	}
	return ""
}

func referenceForQuestion(question map[string]any, language string) string {
	if language != "" {
		if answers := mapField(question, "answers_by_language"); answers != nil {
			if answer, ok := answers[language]; ok {
				return answerText(answer)
			}
		}
	}
	return answerText(question["answer"])
}

func stringField(object map[string]any, key string) string {
	value, _ := object[key].(string)
	return strings.TrimSpace(value)
}

func mapField(object map[string]any, key string) map[string]any {
	value, _ := object[key].(map[string]any)
	return value
}

func listMaps(value any) []map[string]any {
	switch items := value.(type) {
	case []any:
		out := make([]map[string]any, 0, len(items))
		for _, raw := range items {
			if object, ok := raw.(map[string]any); ok {
				out = append(out, object)
			}
		}
		return out
	case []map[string]any:
		return items
	default:
		return nil
	}
}

const batchSelect = `SELECT id,run_id,snapshot_hash,answer_set_hash,rule_version,status,attempt_count,
max_attempts,coalesce(error_code,''),submission_count,question_count,finding_count,created_at,started_at,completed_at
FROM answer_similarity_batches`

func batchScanTargets(item *Batch) []any {
	return []any{&item.ID, &item.RunID, &item.SnapshotHash, &item.AnswerSetHash, &item.RuleVersion, &item.Status,
		&item.AttemptCount, &item.MaxAttempts, &item.ErrorCode, &item.SubmissionCount, &item.QuestionCount,
		&item.FindingCount, &item.CreatedAt, &item.StartedAt, &item.CompletedAt}
}

const findingSelect = `SELECT id,question_id,question_label,left_submission_id,right_submission_id,left_participant_id,right_participant_id,
left_name,right_name,left_answer,right_answer,risk_level,risk_score,text_overlap,semantic_similarity,reference_similarity,
shared_excerpt,left_excerpt,right_excerpt,reasons,common_error_evidence,related_question_ids,features,review_state,review_revision,
coalesce(review_note,''),coalesce(reviewed_by,''),reviewed_at FROM answer_similarity_findings`

func findingScanTargets(item *FindingRecord) []any {
	return []any{&item.ID, &item.QuestionID, &item.QuestionLabel, &item.LeftSubmissionID, &item.RightSubmissionID,
		&item.LeftParticipantID, &item.RightParticipantID, &item.LeftName, &item.RightName, &item.LeftAnswer,
		&item.RightAnswer, &item.RiskLevel, &item.RiskScore, &item.TextOverlap, &item.SemanticSimilarity, &item.ReferenceSimilarity,
		&item.SharedExcerpt, &item.LeftExcerpt, &item.RightExcerpt, &item.Reasons, &item.CommonErrorEvidence, &item.RelatedQuestionIDs,
		&item.Features, &item.ReviewState, &item.ReviewRevision, &item.ReviewNote, &item.ReviewedBy, &item.ReviewedAt}
}

// Called after locking the batch: PostgreSQL transaction-start time cannot
// authorize a lease that expired while this writer waited for the row lock.
func (s *Service) checkLeaseDeadline(ctx context.Context, tx pgx.Tx, id int64) error {
	var valid bool
	if err := tx.QueryRow(ctx, `SELECT coalesce(lease_until>clock_timestamp(),false) FROM answer_similarity_batches WHERE id=$1`, id).Scan(&valid); err != nil {
		return err
	}
	if !valid {
		return ErrLeaseLost
	}
	return nil
}
