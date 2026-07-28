package grading

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yhwyxy/examSystem/internal/jobs"
	"github.com/yhwyxy/examSystem/internal/papers"
)

type GraderFunc func(question map[string]any, studentAnswer any, partial bool) (map[string]any, error)

type Service struct {
	pool    *pgxpool.Pool
	jobs    *jobs.Service
	repo    *papers.Repository
	grade   GraderFunc
}

func NewService(pool *pgxpool.Pool, jobsSvc *jobs.Service, repo *papers.Repository, grade GraderFunc) *Service {
	return &Service{
		pool:    pool,
		jobs:    jobsSvc,
		repo:    repo,
		grade:   grade,
	}
}

func (s *Service) StartWorker(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(3 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.processPendingJobs(ctx)
			}
		}
	}()
}

func (s *Service) processPendingJobs(ctx context.Context) {
	var subID, paperID, runID string
	var generation int

	err := s.pool.QueryRow(ctx, `
		SELECT id, paper_id, run_id, generation
		FROM grading_jobs
		WHERE status = 'pending'
		ORDER BY created_at ASC
		LIMIT 1
	`).Scan(&subID, &paperID, &runID, &generation)
	if err != nil {
		return
	}

	detail, err := s.gradeSubmission(ctx, paperID, runID, subID)
	if err != nil {
		s.pool.Exec(ctx, `
			UPDATE grading_jobs SET status = 'failed', updated_at = NOW()
			WHERE id = $1
		`, subID)
		return
	}

	s.pool.Exec(ctx, `
		UPDATE submissions
		SET grading_detail_json = $1,
		    grading_status = 'done',
		    review_status = 'auto_scored',
		    grading_generation = $2,
		    updated_at = NOW()
		WHERE id = $3
	`, detail, generation, subID)

	s.pool.Exec(ctx, `
		UPDATE grading_jobs
		SET status = 'done', updated_at = NOW()
		WHERE id = $1
	`, subID)
}

func (s *Service) gradeSubmission(ctx context.Context, paperID, runID, subID string) ([]byte, error) {
	snap, err := s.repo.LoadSnapshot(paperID, "")
	if err != nil {
		return nil, err
	}

	details := s.computeGrading(snap.Doc, runID, subID)
	b, _ := json.Marshal(details)
	return b, nil
}

func (s *Service) computeGrading(doc map[string]any, runID, subID string) []map[string]any {
	questions, _ := doc["questions"].([]any)
	details := make([]map[string]any, 0, len(questions))

	for _, qraw := range questions {
		question, ok := qraw.(map[string]any)
		if !ok {
			continue
		}

		// 实际项目中需要调用 objective.Grade
		// 这里用 placeholder 占位（你需要注入 objective.Grade）
		details = append(details, map[string]any{
			"question_id":        QID(question),
			"type":               question["type"],
			"student_answer":     nil,
			"reference_answer":   question["answer"],
			"score":              0.0,
			"machine_score":      0.0,
			"final_score":        0.0,
			"max_score":          MaxScoreOf(question),
			"is_correct":         false,
			"grading_method":     "objective_pending",
			"confidence":         0.0,
			"reason":             "pending",
			"review_status":      "unanswered",
			"manually_reviewed":  false,
			"detail":             map[string]any{},
		})
	}

	return details
}

func QID(q map[string]any) string {
	id, ok := q["id"]
	if !ok || id == nil {
		return ""
	}
	switch v := id.(type) {
	case string:
		return v
	case float64:
		return fmt.Sprintf("%v", v)
	default:
		return fmt.Sprintf("%v", id)
	}
}

func MaxScoreOf(q map[string]any) float64 {
	switch v := q["score"].(type) {
	case float64:
		return v
	case int:
		return float64(v)
	}
	return 0
}