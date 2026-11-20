package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/yhwyxy/examSystem/internal/auth"
	"github.com/yhwyxy/examSystem/internal/scoreanomaly"
)

func mountScoreAnomaly(admin chi.Router, deps Dependencies) {
	reader := admin.With(deps.Auth.RequireRole(auth.RoleReviewer))
	reviewer := admin.With(deps.Auth.RequireRole(auth.RoleReviewer))
	reader.Get("/score-anomalies/runs", scoreAnomalyRunsHandler(deps))
	reviewer.Post("/score-anomalies/runs/{runID}/batches", enqueueScoreAnomalyHandler(deps))
	reader.Get("/score-anomalies/runs/{runID}/batches", listScoreAnomalyBatchesHandler(deps))
	reader.Get("/score-anomalies/batches/{batchID}", getScoreAnomalyBatchHandler(deps))
	reviewer.Post("/score-anomalies/findings/{findingID}/review", reviewScoreAnomalyFindingHandler(deps))
}

func reviewScoreAnomalyFindingHandler(deps Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.ScoreAnomaly == nil {
			writeAdminError(w, http.StatusServiceUnavailable, "SCORE_ANOMALY_NOT_CONFIGURED", "score anomaly service unavailable")
			return
		}
		id, err := strconv.ParseInt(chi.URLParam(r, "findingID"), 10, 64)
		if err != nil || id <= 0 {
			writeAdminError(w, http.StatusBadRequest, "INVALID_FINDING_ID", "finding id must be a positive integer")
			return
		}
		var body struct {
			ExpectedRevision int64  `json:"expected_revision"`
			Disposition      string `json:"disposition"`
			Note             string `json:"note"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
			writeAdminError(w, http.StatusBadRequest, "INVALID_JSON", "review body is invalid")
			return
		}
		actor := auth.UsernameFromContext(r.Context())
		if actor == "" {
			actor = "local-admin"
		}
		item, err := deps.ScoreAnomaly.ReviewFinding(r.Context(), scoreanomaly.ReviewFindingInput{
			FindingID: id, ExpectedRevision: body.ExpectedRevision, Disposition: body.Disposition, Note: body.Note, Actor: actor,
		})
		if err != nil {
			writeScoreAnomalyError(w, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"finding": item})
	}
}

func scoreAnomalyRunsHandler(deps Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.AnswerSimilarity == nil {
			writeAdminError(w, http.StatusServiceUnavailable, "ANSWER_SIMILARITY_NOT_CONFIGURED", "run list is unavailable")
			return
		}
		runs, err := deps.AnswerSimilarity.ListRuns(r.Context(), 100)
		if err != nil {
			writeAdminError(w, http.StatusInternalServerError, "SCORE_ANOMALY_FAILED", "score anomaly runs are unavailable")
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"runs": runs})
	}
}

func enqueueScoreAnomalyHandler(deps Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.ScoreAnomaly == nil {
			writeAdminError(w, http.StatusServiceUnavailable, "SCORE_ANOMALY_NOT_CONFIGURED", "score anomaly worker unavailable")
			return
		}
		batch, created, err := deps.ScoreAnomaly.Enqueue(r.Context(), chi.URLParam(r, "runID"))
		if err != nil {
			writeScoreAnomalyError(w, err)
			return
		}
		status := http.StatusOK
		if created {
			status = http.StatusAccepted
		}
		WriteJSON(w, status, map[string]any{"batch": batch, "created": created})
	}
}

func listScoreAnomalyBatchesHandler(deps Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.ScoreAnomaly == nil {
			writeAdminError(w, http.StatusServiceUnavailable, "SCORE_ANOMALY_NOT_CONFIGURED", "score anomaly worker unavailable")
			return
		}
		batches, err := deps.ScoreAnomaly.ListBatches(r.Context(), chi.URLParam(r, "runID"))
		if err != nil {
			writeAdminError(w, http.StatusInternalServerError, "SCORE_ANOMALY_FAILED", "score anomaly batches are unavailable")
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"batches": batches})
	}
}

func getScoreAnomalyBatchHandler(deps Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.ScoreAnomaly == nil {
			writeAdminError(w, http.StatusServiceUnavailable, "SCORE_ANOMALY_NOT_CONFIGURED", "score anomaly worker unavailable")
			return
		}
		id, err := strconv.ParseInt(chi.URLParam(r, "batchID"), 10, 64)
		if err != nil || id <= 0 {
			writeAdminError(w, http.StatusBadRequest, "INVALID_BATCH_ID", "batch id must be a positive integer")
			return
		}
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		page, err := deps.ScoreAnomaly.GetBatchPage(r.Context(), id, limit, offset)
		if err != nil {
			writeScoreAnomalyError(w, err)
			return
		}
		WriteJSON(w, http.StatusOK, page)
	}
}

func writeScoreAnomalyError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, scoreanomaly.ErrRunNotFound):
		writeAdminError(w, http.StatusNotFound, "RUN_NOT_FOUND", "run not found")
	case errors.Is(err, scoreanomaly.ErrRunNotReady):
		writeAdminError(w, http.StatusConflict, "RUN_NOT_READY", "close the run and finish all grading before score anomaly analysis")
	case errors.Is(err, scoreanomaly.ErrSimilarityNotReady):
		writeAdminError(w, http.StatusConflict, "SIMILARITY_REPORT_NOT_READY", "complete answer similarity analysis for this run first")
	case errors.Is(err, scoreanomaly.ErrBatchNotFound):
		writeAdminError(w, http.StatusNotFound, "SCORE_ANOMALY_BATCH_NOT_FOUND", "score anomaly batch not found")
	case errors.Is(err, scoreanomaly.ErrFindingNotFound):
		writeAdminError(w, http.StatusNotFound, "SCORE_ANOMALY_FINDING_NOT_FOUND", "score anomaly finding not found")
	case errors.Is(err, scoreanomaly.ErrReviewConflict):
		writeAdminError(w, http.StatusConflict, "REVIEW_REVISION_CONFLICT", "finding changed; reload before saving")
	case errors.Is(err, scoreanomaly.ErrInvalidReview):
		writeAdminError(w, http.StatusBadRequest, "INVALID_REVIEW", "choose a disposition and provide a note of 5 to 2000 characters")
	default:
		writeAdminError(w, http.StatusInternalServerError, "SCORE_ANOMALY_FAILED", "score anomaly operation failed")
	}
}
