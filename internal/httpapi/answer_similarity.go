package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/yhwyxy/examSystem/internal/answersimilarity"
	"github.com/yhwyxy/examSystem/internal/auth"
)

func mountAnswerSimilarity(admin chi.Router, deps Dependencies) {
	reader := admin.With(deps.Auth.RequireRole(auth.RoleReviewer))
	reviewer := admin.With(deps.Auth.RequireRole(auth.RoleReviewer))
	reader.Get("/answer-similarity/runs", answerSimilarityRunsHandler(deps))
	reviewer.Post("/answer-similarity/runs/{runID}/batches", enqueueAnswerSimilarityHandler(deps))
	reader.Get("/answer-similarity/runs/{runID}/batches", listAnswerSimilarityBatchesHandler(deps))
	reader.Get("/answer-similarity/batches/{batchID}", answerSimilarityBatchHandler(deps))
	reviewer.Patch("/answer-similarity/findings/{findingID}/review", reviewAnswerSimilarityHandler(deps))
}

func answerSimilarityRunsHandler(deps Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.AnswerSimilarity == nil {
			writeAdminError(w, http.StatusServiceUnavailable, "ANSWER_SIMILARITY_NOT_CONFIGURED", "similarity worker unavailable")
			return
		}
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		items, err := deps.AnswerSimilarity.ListRuns(r.Context(), limit)
		if err != nil {
			writeAdminError(w, http.StatusInternalServerError, "ANSWER_SIMILARITY_QUERY_FAILED", "eligible runs unavailable")
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"runs": items})
	}
}

func enqueueAnswerSimilarityHandler(deps Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.AnswerSimilarity == nil {
			writeAdminError(w, http.StatusServiceUnavailable, "ANSWER_SIMILARITY_NOT_CONFIGURED", "similarity worker unavailable")
			return
		}
		batch, created, err := deps.AnswerSimilarity.Enqueue(r.Context(), chi.URLParam(r, "runID"))
		if err != nil {
			writeSimilarityServiceError(w, err)
			return
		}
		status := http.StatusOK
		if created {
			status = http.StatusAccepted
		}
		WriteJSON(w, status, map[string]any{"batch": batch, "created": created})
	}
}

func listAnswerSimilarityBatchesHandler(deps Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.AnswerSimilarity == nil {
			writeAdminError(w, http.StatusServiceUnavailable, "ANSWER_SIMILARITY_NOT_CONFIGURED", "similarity worker unavailable")
			return
		}
		items, err := deps.AnswerSimilarity.ListBatches(r.Context(), chi.URLParam(r, "runID"))
		if err != nil {
			writeAdminError(w, http.StatusInternalServerError, "ANSWER_SIMILARITY_QUERY_FAILED", "similarity batches unavailable")
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"batches": items})
	}
}

func answerSimilarityBatchHandler(deps Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.AnswerSimilarity == nil {
			writeAdminError(w, http.StatusServiceUnavailable, "ANSWER_SIMILARITY_NOT_CONFIGURED", "similarity worker unavailable")
			return
		}
		batchID, err := strconv.ParseInt(chi.URLParam(r, "batchID"), 10, 64)
		if err != nil || batchID <= 0 {
			writeAdminError(w, http.StatusBadRequest, "INVALID_BATCH_ID", "batch id must be a positive integer")
			return
		}
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		page, err := deps.AnswerSimilarity.GetBatchPage(r.Context(), batchID, limit, offset)
		if err != nil {
			writeSimilarityServiceError(w, err)
			return
		}
		WriteJSON(w, http.StatusOK, page)
	}
}

func reviewAnswerSimilarityHandler(deps Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.AnswerSimilarity == nil {
			writeAdminError(w, http.StatusServiceUnavailable, "ANSWER_SIMILARITY_NOT_CONFIGURED", "similarity worker unavailable")
			return
		}
		findingID, err := strconv.ParseInt(chi.URLParam(r, "findingID"), 10, 64)
		if err != nil || findingID <= 0 {
			writeAdminError(w, http.StatusBadRequest, "INVALID_FINDING_ID", "finding id must be a positive integer")
			return
		}
		var body struct {
			ExpectedRevision int    `json:"expected_revision"`
			State            string `json:"state"`
			Note             string `json:"note"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&body); err != nil {
			writeAdminError(w, http.StatusBadRequest, "INVALID_JSON", "review body must be valid JSON")
			return
		}
		actor := auth.UsernameFromContext(r.Context())
		if actor == "" {
			actor = "local-admin"
		}
		finding, err := deps.AnswerSimilarity.Review(r.Context(), answersimilarity.ReviewInput{
			FindingID: findingID, ExpectedRevision: body.ExpectedRevision,
			State: body.State, Note: body.Note, Actor: actor,
		})
		if err != nil {
			writeSimilarityServiceError(w, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"finding": finding})
	}
}

func writeSimilarityServiceError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, answersimilarity.ErrRunNotFound):
		writeAdminError(w, http.StatusNotFound, "RUN_NOT_FOUND", "run not found")
	case errors.Is(err, answersimilarity.ErrRunNotClosed):
		writeAdminError(w, http.StatusConflict, "RUN_NOT_CLOSED", "similarity checks require a closed run with a valid snapshot")
	case errors.Is(err, answersimilarity.ErrRunIncomplete):
		writeAdminError(w, http.StatusConflict, "RUN_HAS_ACTIVE_SESSIONS", "submit or close every session before checking similarity")
	case errors.Is(err, answersimilarity.ErrNoSubmissions):
		writeAdminError(w, http.StatusConflict, "RUN_HAS_NO_SUBMISSIONS", "the closed run has no submitted answers")
	case errors.Is(err, answersimilarity.ErrRunTooLarge):
		writeAdminError(w, http.StatusUnprocessableEntity, "RUN_TOO_LARGE", "this run exceeds the similarity analysis limits")
	case errors.Is(err, answersimilarity.ErrEmbeddingUnavailable):
		writeAdminError(w, http.StatusServiceUnavailable, "EMBEDDING_PROVIDER_UNAVAILABLE", "the configured semantic embedding provider is unavailable")
	case errors.Is(err, answersimilarity.ErrBatchNotFound), errors.Is(err, answersimilarity.ErrFindingNotFound):
		writeAdminError(w, http.StatusNotFound, "SIMILARITY_RECORD_NOT_FOUND", "similarity record not found")
	case errors.Is(err, answersimilarity.ErrReviewRevision):
		writeAdminError(w, http.StatusConflict, "REVIEW_REVISION_CONFLICT", "finding changed; reload it before reviewing")
	case errors.Is(err, answersimilarity.ErrInvalidReview):
		writeAdminError(w, http.StatusBadRequest, "INVALID_REVIEW", "choose a review state and provide a note of at most 2000 characters")
	default:
		writeAdminError(w, http.StatusInternalServerError, "ANSWER_SIMILARITY_FAILED", "similarity operation failed")
	}
}
