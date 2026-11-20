package publication

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/yhwyxy/examSystem/internal/scoreanomaly"
)

var (
	ErrNotConfigured       = errors.New("publication service is not configured")
	ErrRunNotFound         = errors.New("publication run not found")
	ErrRunNotReady         = errors.New("publication run is not ready")
	ErrReviewRequired      = errors.New("required manual review remains")
	ErrReportsNotReady     = errors.New("current detection reports are not complete")
	ErrBlockingFindings    = errors.New("unresolved blocking score findings remain")
	ErrCorrectionReason    = errors.New("correction reason is required")
	ErrPublicationNotFound = errors.New("published grade batch not found")
)

type Batch struct {
	ID                int64     `json:"id"`
	RunID             string    `json:"run_id"`
	PaperID           string    `json:"paper_id"`
	PaperName         string    `json:"paper_name"`
	Version           int       `json:"version"`
	SupersedesBatchID *int64    `json:"supersedes_batch_id,omitempty"`
	CorrectionReason  string    `json:"correction_reason,omitempty"`
	Actor             string    `json:"actor"`
	SnapshotHash      string    `json:"snapshot_hash"`
	AnswerSetHash     string    `json:"answer_set_hash"`
	GradingSetHash    string    `json:"grading_set_hash"`
	SimilarityBatchID int64     `json:"similarity_batch_id"`
	AnomalyBatchID    int64     `json:"anomaly_batch_id"`
	SubmissionCount   int       `json:"submission_count"`
	PublishedAt       time.Time `json:"published_at"`
}

type Grade struct {
	SubmissionID      int64   `json:"submission_id"`
	ParticipantName   string  `json:"participant_name"`
	EmployeeID        string  `json:"employee_id"`
	Department        *string `json:"department,omitempty"`
	GradingGeneration int64   `json:"grading_generation"`
	ReviewStatus      string  `json:"review_status"`
	ObjectiveScore    float64 `json:"objective_score"`
	SubjectiveMachine float64 `json:"subjective_machine_score"`
	SubjectiveFinal   float64 `json:"subjective_final_score"`
	TotalScore        float64 `json:"total_score"`
	MaxScore          float64 `json:"max_score"`
}

type Publication struct {
	Batch  Batch   `json:"batch"`
	Grades []Grade `json:"grades"`
}

type Service struct {
	pool      *pgxpool.Pool
	anomalies *scoreanomaly.Service
}

func NewService(pool *pgxpool.Pool, anomalies *scoreanomaly.Service) *Service {
	return &Service{pool: pool, anomalies: anomalies}
}

func (s *Service) ready() error {
	if s == nil || s.pool == nil || s.anomalies == nil {
		return ErrNotConfigured
	}
	return nil
}

type PublishInput struct {
	RunID            string
	Actor            string
	CorrectionReason string
}

func (s *Service) Publish(ctx context.Context, in PublishInput) (Publication, bool, error) {
	if err := s.ready(); err != nil {
		return Publication{}, false, err
	}
	in.RunID, in.Actor, in.CorrectionReason = strings.TrimSpace(in.RunID), strings.TrimSpace(in.Actor), strings.TrimSpace(in.CorrectionReason)
	if in.RunID == "" || len(in.RunID) > 200 {
		return Publication{}, false, ErrRunNotFound
	}
	if in.Actor == "" || len(in.Actor) > 200 || len(in.CorrectionReason) > 2000 {
		return Publication{}, false, ErrRunNotReady
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return Publication{}, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var status, snapshotHash, paperID, paperName string
	var legacy bool
	var maxScore *float64
	err = tx.QueryRow(ctx, `SELECT r.status,coalesce(r.snapshot_hash,''),r.is_legacy,r.max_score,r.paper_id,
COALESCE((SELECT max(NULLIF(s.paper_name,'')) FROM submissions s WHERE s.run_id=r.id),r.paper_id)
FROM exam_runs r WHERE r.id=$1 FOR UPDATE`, in.RunID).
		Scan(&status, &snapshotHash, &legacy, &maxScore, &paperID, &paperName)
	if errors.Is(err, pgx.ErrNoRows) {
		return Publication{}, false, ErrRunNotFound
	}
	if err != nil {
		return Publication{}, false, err
	}
	if status != "closed" || legacy || len(snapshotHash) != 64 || maxScore == nil || *maxScore <= 0 {
		return Publication{}, false, ErrRunNotReady
	}
	var activeSessions int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM exam_sessions WHERE run_id=$1 AND status='active'`, in.RunID).Scan(&activeSessions); err != nil {
		return Publication{}, false, err
	}
	if activeSessions != 0 {
		return Publication{}, false, ErrRunNotReady
	}
	grades := make([]Grade, 0)
	rows, err := tx.Query(ctx, `SELECT id,name,employee_id,department,grading_generation,review_status,grading_status,
objective_score,subjective_score_machine,subjective_score_final,total_score
FROM submissions WHERE run_id=$1 ORDER BY id FOR UPDATE`, in.RunID)
	if err != nil {
		return Publication{}, false, err
	}
	for rows.Next() {
		var grade Grade
		var gradingStatus string
		if err := rows.Scan(&grade.SubmissionID, &grade.ParticipantName, &grade.EmployeeID, &grade.Department,
			&grade.GradingGeneration, &grade.ReviewStatus, &gradingStatus, &grade.ObjectiveScore,
			&grade.SubjectiveMachine, &grade.SubjectiveFinal, &grade.TotalScore); err != nil {
			rows.Close()
			return Publication{}, false, err
		}
		if gradingStatus != "done" || grade.GradingGeneration <= 0 || !validGrade(grade, *maxScore) {
			rows.Close()
			return Publication{}, false, ErrRunNotReady
		}
		if requiredReview(grade.ReviewStatus) {
			rows.Close()
			return Publication{}, false, ErrReviewRequired
		}
		grade.MaxScore = *maxScore
		grades = append(grades, grade)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return Publication{}, false, err
	}
	if len(grades) == 0 {
		return Publication{}, false, ErrRunNotReady
	}
	anomalyBatch, err := s.anomalies.CurrentCompletedBatch(ctx, tx, in.RunID)
	if errors.Is(err, scoreanomaly.ErrSimilarityNotReady) || errors.Is(err, scoreanomaly.ErrReportNotReady) || errors.Is(err, scoreanomaly.ErrRunNotReady) {
		return Publication{}, false, ErrReportsNotReady
	}
	if err != nil {
		return Publication{}, false, err
	}
	blockingRows, err := tx.Query(ctx, `SELECT review_status FROM score_anomaly_findings
WHERE batch_id=$1 AND severity='blocking' ORDER BY id FOR UPDATE`, anomalyBatch.ID)
	if err != nil {
		return Publication{}, false, err
	}
	for blockingRows.Next() {
		var reviewStatus string
		if err := blockingRows.Scan(&reviewStatus); err != nil {
			blockingRows.Close()
			return Publication{}, false, err
		}
		if reviewStatus != "resolved" {
			blockingRows.Close()
			return Publication{}, false, ErrBlockingFindings
		}
	}
	if err := blockingRows.Err(); err != nil {
		blockingRows.Close()
		return Publication{}, false, err
	}
	blockingRows.Close()
	var priorID *int64
	var priorVersion int
	var priorAnomalyID int64
	var priorGradingHash string
	err = tx.QueryRow(ctx, `SELECT id,version,anomaly_batch_id,grading_set_hash FROM score_publication_batches
WHERE run_id=$1 ORDER BY version DESC LIMIT 1 FOR UPDATE`, in.RunID).Scan(&priorID, &priorVersion, &priorAnomalyID, &priorGradingHash)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Publication{}, false, err
	}
	if priorID != nil && priorAnomalyID == anomalyBatch.ID && priorGradingHash == anomalyBatch.GradingSetHash {
		result, err := loadPublication(ctx, tx, *priorID)
		if err != nil {
			return Publication{}, false, err
		}
		if err := tx.Commit(ctx); err != nil {
			return Publication{}, false, err
		}
		return result, false, nil
	}
	version := priorVersion + 1
	var supersedes any
	eventType := "published"
	correctionReason := ""
	if priorID != nil {
		if len(in.CorrectionReason) < 5 {
			return Publication{}, false, ErrCorrectionReason
		}
		supersedes, eventType, correctionReason = *priorID, "corrected", in.CorrectionReason
	}
	var result Publication
	err = tx.QueryRow(ctx, `INSERT INTO score_publication_batches
(run_id,paper_id,paper_name,version,supersedes_batch_id,correction_reason,actor,snapshot_hash,answer_set_hash,grading_set_hash,
similarity_batch_id,anomaly_batch_id,submission_count)
VALUES ($1,$2,$3,$4,$5,NULLIF($6,''),$7,$8,$9,$10,$11,$12,$13)
RETURNING id,run_id,paper_id,paper_name,version,supersedes_batch_id,coalesce(correction_reason,''),actor,snapshot_hash,answer_set_hash,grading_set_hash,
	similarity_batch_id,anomaly_batch_id,submission_count,published_at`, in.RunID, paperID, paperName, version, supersedes, correctionReason, in.Actor,
		snapshotHash, anomalyBatch.AnswerSetHash, anomalyBatch.GradingSetHash, anomalyBatch.SimilarityBatchID, anomalyBatch.ID, len(grades)).
		Scan(batchTargets(&result.Batch)...)
	if err != nil {
		return Publication{}, false, err
	}
	for _, grade := range grades {
		if _, err := tx.Exec(ctx, `INSERT INTO score_publication_grades
(batch_id,submission_id,participant_name,employee_id,department,grading_generation,review_status,objective_score,
subjective_machine,subjective_final,total_score,max_score) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
			result.Batch.ID, grade.SubmissionID, grade.ParticipantName, grade.EmployeeID, grade.Department,
			grade.GradingGeneration, grade.ReviewStatus, grade.ObjectiveScore, grade.SubjectiveMachine,
			grade.SubjectiveFinal, grade.TotalScore, grade.MaxScore); err != nil {
			return Publication{}, false, err
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO score_publication_audit(batch_id,actor,event_type,supersedes_batch_id,reason)
VALUES ($1,$2,$3,$4,NULLIF($5,''))`, result.Batch.ID, in.Actor, eventType, supersedes, correctionReason); err != nil {
		return Publication{}, false, err
	}
	result.Grades = grades
	if err := tx.Commit(ctx); err != nil {
		return Publication{}, false, err
	}
	return result, true, nil
}

func (s *Service) List(ctx context.Context, runID string) ([]Batch, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, batchSelect+` WHERE run_id=$1 ORDER BY version DESC LIMIT 100`, runID)
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

func (s *Service) Get(ctx context.Context, batchID int64) (Publication, error) {
	if err := s.ready(); err != nil {
		return Publication{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Publication{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	result, err := loadPublication(ctx, tx, batchID)
	if err != nil {
		return Publication{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Publication{}, err
	}
	return result, nil
}

func loadPublication(ctx context.Context, tx pgx.Tx, batchID int64) (Publication, error) {
	var result Publication
	err := tx.QueryRow(ctx, batchSelect+` WHERE id=$1`, batchID).Scan(batchTargets(&result.Batch)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return Publication{}, ErrPublicationNotFound
	}
	if err != nil {
		return Publication{}, err
	}
	result.Grades = make([]Grade, 0, result.Batch.SubmissionCount)
	rows, err := tx.Query(ctx, `SELECT submission_id,participant_name,employee_id,department,grading_generation,review_status,
objective_score,subjective_machine,subjective_final,total_score,max_score FROM score_publication_grades WHERE batch_id=$1 ORDER BY submission_id`, batchID)
	if err != nil {
		return Publication{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var grade Grade
		if err := rows.Scan(&grade.SubmissionID, &grade.ParticipantName, &grade.EmployeeID, &grade.Department,
			&grade.GradingGeneration, &grade.ReviewStatus, &grade.ObjectiveScore, &grade.SubjectiveMachine,
			&grade.SubjectiveFinal, &grade.TotalScore, &grade.MaxScore); err != nil {
			return Publication{}, err
		}
		result.Grades = append(result.Grades, grade)
	}
	return result, rows.Err()
}

func requiredReview(status string) bool {
	switch status {
	case "auto_scored", "high_confidence", "reviewed":
		return false
	default:
		return true
	}
}

func validGrade(grade Grade, maxScore float64) bool {
	if grade.ObjectiveScore < -0.01 || grade.SubjectiveMachine < -0.01 || grade.SubjectiveFinal < -0.01 || grade.TotalScore < -0.01 {
		return false
	}
	if grade.ObjectiveScore > maxScore+0.01 || grade.SubjectiveMachine > maxScore+0.01 || grade.SubjectiveFinal > maxScore+0.01 || grade.TotalScore > maxScore+0.01 {
		return false
	}
	return abs(grade.ObjectiveScore+grade.SubjectiveFinal-grade.TotalScore) <= 0.01
}

func abs(value float64) float64 {
	if value < 0 {
		return -value
	}
	return value
}

const batchSelect = `SELECT id,run_id,paper_id,paper_name,version,supersedes_batch_id,coalesce(correction_reason,''),actor,snapshot_hash,answer_set_hash,grading_set_hash,
similarity_batch_id,anomaly_batch_id,submission_count,published_at FROM score_publication_batches`

func batchTargets(batch *Batch) []any {
	return []any{&batch.ID, &batch.RunID, &batch.PaperID, &batch.PaperName, &batch.Version, &batch.SupersedesBatchID, &batch.CorrectionReason,
		&batch.Actor, &batch.SnapshotHash, &batch.AnswerSetHash, &batch.GradingSetHash,
		&batch.SimilarityBatchID, &batch.AnomalyBatchID, &batch.SubmissionCount, &batch.PublishedAt}
}
