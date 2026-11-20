package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/yhwyxy/examSystem/internal/answersimilarity"
	"github.com/yhwyxy/examSystem/internal/scoreanomaly"
)

type reviewQueueTask struct {
	ID           string    `json:"id"`
	Kind         string    `json:"kind"`
	RunID        string    `json:"run_id"`
	PaperID      string    `json:"paper_id"`
	RoundNo      int       `json:"round_no"`
	SubmissionID *int64    `json:"submission_id,omitempty"`
	FindingID    *int64    `json:"finding_id,omitempty"`
	BatchID      *int64    `json:"batch_id,omitempty"`
	QuestionID   string    `json:"question_id,omitempty"`
	Participants string    `json:"participants"`
	Status       string    `json:"status"`
	Severity     string    `json:"severity,omitempty"`
	Description  string    `json:"description,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
}

func reviewQueueHandler(deps Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Pool == nil {
			writeAdminError(w, http.StatusServiceUnavailable, "REVIEW_QUEUE_NOT_CONFIGURED", "review queue is unavailable")
			return
		}
		limit := atoiClamp(r.URL.Query().Get("limit"), 200, 1, 500)
		tasks, err := loadReviewQueue(r.Context(), deps, limit)
		if err != nil {
			writeAdminError(w, http.StatusInternalServerError, "REVIEW_QUEUE_FAILED", "review tasks are unavailable")
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"tasks": tasks, "count": len(tasks)})
	}
}

func loadReviewQueue(ctx context.Context, deps Dependencies, limit int) ([]reviewQueueTask, error) {
	tasks := make([]reviewQueueTask, 0)
	rows, err := deps.Pool.Query(ctx, `SELECT s.id,s.run_id,s.paper_id,COALESCE(r.round_no,0),
COALESCE(NULLIF(s.paper_name,''),s.paper_id),s.name,s.employee_id,s.review_status,s.submitted_at
FROM submissions s LEFT JOIN exam_runs r ON r.id=s.run_id
WHERE s.grading_status='done' AND s.review_status NOT IN ('auto_scored','high_confidence','reviewed')
ORDER BY s.submitted_at DESC,s.id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var task reviewQueueTask
		var id int64
		var paperName, name, employeeID string
		if err := rows.Scan(&id, &task.RunID, &task.PaperID, &task.RoundNo, &paperName, &name, &employeeID, &task.Status, &task.CreatedAt); err != nil {
			rows.Close()
			return nil, err
		}
		task.ID, task.Kind, task.SubmissionID = "submission:"+formatID(id), "submission", &id
		task.Participants = name + " · " + employeeID
		task.Description = paperName
		tasks = append(tasks, task)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	if deps.AnswerSimilarity != nil {
		batchIDs, err := currentSimilarityBatchIDs(ctx, deps)
		if err != nil {
			return nil, err
		}
		if len(batchIDs) > 0 {
			findingRows, err := deps.Pool.Query(ctx, `SELECT f.id,b.id,b.run_id,r.paper_id,r.round_no,
f.question_id,f.risk_level,f.risk_score,f.shared_excerpt,f.review_state,f.created_at,f.left_name,f.right_name
FROM answer_similarity_findings f
JOIN answer_similarity_batches b ON b.id=f.batch_id
JOIN exam_runs r ON r.id=b.run_id
WHERE b.id=ANY($1) AND f.review_state='needs_review'
ORDER BY f.created_at DESC,f.id DESC LIMIT $2`, batchIDs, limit)
			if err != nil {
				return nil, err
			}
			for findingRows.Next() {
				var task reviewQueueTask
				var findingID, batchID int64
				var riskScore float64
				var sharedExcerpt, leftName, rightName string
				if err := findingRows.Scan(&findingID, &batchID, &task.RunID, &task.PaperID, &task.RoundNo,
					&task.QuestionID, &task.Severity, &riskScore, &sharedExcerpt, &task.Status, &task.CreatedAt,
					&leftName, &rightName); err != nil {
					findingRows.Close()
					return nil, err
				}
				task.ID, task.Kind = "answer_similarity:"+formatID(findingID), "answer_similarity"
				task.FindingID, task.BatchID = &findingID, &batchID
				task.Participants = leftName + " / " + rightName
				task.Description = "风险 " + strconv.FormatInt(int64(riskScore*100+0.5), 10) + "% · " + sharedExcerpt
				tasks = append(tasks, task)
			}
			if err := findingRows.Err(); err != nil {
				findingRows.Close()
				return nil, err
			}
			findingRows.Close()
		}
	}

	if deps.ScoreAnomaly != nil {
		batchIDs, err := currentAnomalyBatchIDs(ctx, deps)
		if err != nil {
			return nil, err
		}
		if len(batchIDs) > 0 {
			findingRows, err := deps.Pool.Query(ctx, `SELECT f.id,b.id,b.run_id,r.paper_id,r.round_no,
f.question_id,f.rule_code,f.severity,f.message,f.review_status,f.created_at,f.participant_names::text
FROM score_anomaly_findings f
JOIN score_anomaly_batches b ON b.id=f.batch_id
JOIN exam_runs r ON r.id=b.run_id
WHERE b.id=ANY($1) AND f.severity='blocking' AND f.review_status='open'
ORDER BY f.created_at DESC,f.id DESC LIMIT $2`, batchIDs, limit)
			if err != nil {
				return nil, err
			}
			for findingRows.Next() {
				var task reviewQueueTask
				var findingID, batchID int64
				var ruleCode, participantNames string
				if err := findingRows.Scan(&findingID, &batchID, &task.RunID, &task.PaperID, &task.RoundNo,
					&task.QuestionID, &ruleCode, &task.Severity, &task.Description, &task.Status, &task.CreatedAt, &participantNames); err != nil {
					findingRows.Close()
					return nil, err
				}
				task.ID, task.Kind = "score_anomaly:"+formatID(findingID), "score_anomaly"
				task.FindingID, task.BatchID = &findingID, &batchID
				var names []string
				if err := json.Unmarshal([]byte(participantNames), &names); err == nil {
					task.Participants = strings.Join(names, "、")
				} else {
					task.Participants = participantNames
				}
				task.Description = ruleCode + ": " + task.Description
				tasks = append(tasks, task)
			}
			if err := findingRows.Err(); err != nil {
				findingRows.Close()
				return nil, err
			}
			findingRows.Close()
		}
	}

	sort.Slice(tasks, func(i, j int) bool {
		if tasks[i].CreatedAt != tasks[j].CreatedAt {
			return tasks[i].CreatedAt.After(tasks[j].CreatedAt)
		}
		return tasks[i].ID < tasks[j].ID
	})
	if len(tasks) > limit {
		tasks = tasks[:limit]
	}
	return tasks, nil
}

func currentSimilarityBatchIDs(ctx context.Context, deps Dependencies) ([]int64, error) {
	runs, err := deps.Pool.Query(ctx, `SELECT DISTINCT b.run_id
FROM answer_similarity_findings f JOIN answer_similarity_batches b ON b.id=f.batch_id
WHERE b.status='completed' AND f.review_state='needs_review'
ORDER BY b.run_id LIMIT 100`)
	if err != nil {
		return nil, err
	}
	runIDs := make([]string, 0)
	for runs.Next() {
		var runID string
		if err := runs.Scan(&runID); err != nil {
			runs.Close()
			return nil, err
		}
		runIDs = append(runIDs, runID)
	}
	if err := runs.Err(); err != nil {
		runs.Close()
		return nil, err
	}
	runs.Close()

	batchIDs := make([]int64, 0)
	for _, runID := range runIDs {
		tx, err := deps.Pool.Begin(ctx)
		if err != nil {
			return nil, err
		}
		batch, batchErr := deps.AnswerSimilarity.CurrentCompletedBatch(ctx, tx, runID)
		_ = tx.Rollback(ctx)
		if errors.Is(batchErr, answersimilarity.ErrCurrentReportNotReady) ||
			errors.Is(batchErr, answersimilarity.ErrRunNotClosed) ||
			errors.Is(batchErr, answersimilarity.ErrRunIncomplete) ||
			errors.Is(batchErr, answersimilarity.ErrNoSubmissions) ||
			errors.Is(batchErr, answersimilarity.ErrRunNotFound) {
			continue
		}
		if batchErr != nil {
			return nil, batchErr
		}
		batchIDs = append(batchIDs, batch.ID)
	}
	return batchIDs, nil
}

func currentAnomalyBatchIDs(ctx context.Context, deps Dependencies) ([]int64, error) {
	runs, err := deps.Pool.Query(ctx, `SELECT DISTINCT b.run_id
FROM score_anomaly_findings f JOIN score_anomaly_batches b ON b.id=f.batch_id
WHERE b.status='completed' AND f.severity='blocking' AND f.review_status='open'
ORDER BY b.run_id LIMIT 100`)
	if err != nil {
		return nil, err
	}
	runIDs := make([]string, 0)
	for runs.Next() {
		var runID string
		if err := runs.Scan(&runID); err != nil {
			runs.Close()
			return nil, err
		}
		runIDs = append(runIDs, runID)
	}
	if err := runs.Err(); err != nil {
		runs.Close()
		return nil, err
	}
	runs.Close()

	batchIDs := make([]int64, 0)
	for _, runID := range runIDs {
		tx, err := deps.Pool.Begin(ctx)
		if err != nil {
			return nil, err
		}
		batch, batchErr := deps.ScoreAnomaly.CurrentCompletedBatch(ctx, tx, runID)
		_ = tx.Rollback(ctx)
		if errors.Is(batchErr, scoreanomaly.ErrSimilarityNotReady) || errors.Is(batchErr, scoreanomaly.ErrReportNotReady) || errors.Is(batchErr, scoreanomaly.ErrRunNotReady) {
			continue
		}
		if batchErr != nil {
			if errors.Is(batchErr, pgx.ErrNoRows) {
				continue
			}
			return nil, batchErr
		}
		batchIDs = append(batchIDs, batch.ID)
	}
	return batchIDs, nil
}

func formatID(id int64) string {
	return strconv.FormatInt(id, 10)
}
