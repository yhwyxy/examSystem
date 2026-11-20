package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/yhwyxy/examSystem/internal/auth"
	"github.com/yhwyxy/examSystem/internal/publication"
)

func mountScorePublication(admin chi.Router, deps Dependencies) {
	reader := admin.With(deps.Auth.RequireRole(auth.RoleReviewer, auth.RolePublisher))
	publisher := admin.With(deps.Auth.RequireRole(auth.RolePublisher))
	reader.Get("/publications/runs", publicationRunsHandler(deps))
	reader.Get("/publications/runs/{runID}/batches", listPublicationBatchesHandler(deps))
	publisher.Post("/publications/runs/{runID}/batches", publishRunHandler(deps))
	reader.Get("/publications/batches/{batchID}", getPublicationHandler(deps))
}

func publicationRunsHandler(deps Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.AnswerSimilarity == nil {
			writeAdminError(w, http.StatusServiceUnavailable, "ANSWER_SIMILARITY_NOT_CONFIGURED", "closed run list is unavailable")
			return
		}
		items, err := deps.AnswerSimilarity.ListRuns(r.Context(), 100)
		if err != nil {
			writeAdminError(w, http.StatusInternalServerError, "PUBLICATION_FAILED", "closed runs are unavailable")
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"runs": items})
	}
}

func listPublicationBatchesHandler(deps Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Publication == nil {
			writeAdminError(w, http.StatusServiceUnavailable, "PUBLICATION_NOT_CONFIGURED", "publication service unavailable")
			return
		}
		items, err := deps.Publication.List(r.Context(), chi.URLParam(r, "runID"))
		if err != nil {
			writePublicationError(w, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"batches": items})
	}
}

func publishRunHandler(deps Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Publication == nil {
			writeAdminError(w, http.StatusServiceUnavailable, "PUBLICATION_NOT_CONFIGURED", "publication service unavailable")
			return
		}
		var body struct {
			CorrectionReason string `json:"correction_reason"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
			writeAdminError(w, http.StatusBadRequest, "INVALID_JSON", "publication request is invalid")
			return
		}
		actor := auth.UsernameFromContext(r.Context())
		if actor == "" {
			actor = "local-admin"
		}
		result, created, err := deps.Publication.Publish(r.Context(), publication.PublishInput{
			RunID: chi.URLParam(r, "runID"), Actor: actor, CorrectionReason: body.CorrectionReason,
		})
		if err != nil {
			writePublicationError(w, err)
			return
		}
		status := http.StatusOK
		if created {
			status = http.StatusCreated
		}
		WriteJSON(w, status, map[string]any{"publication": result, "created": created})
	}
}

func getPublicationHandler(deps Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Publication == nil {
			writeAdminError(w, http.StatusServiceUnavailable, "PUBLICATION_NOT_CONFIGURED", "publication service unavailable")
			return
		}
		id, err := strconv.ParseInt(chi.URLParam(r, "batchID"), 10, 64)
		if err != nil || id <= 0 {
			writeAdminError(w, http.StatusBadRequest, "INVALID_BATCH_ID", "batch id must be a positive integer")
			return
		}
		item, err := deps.Publication.Get(r.Context(), id)
		if err != nil {
			writePublicationError(w, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"publication": item})
	}
}

func writePublicationError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, publication.ErrRunNotFound), errors.Is(err, publication.ErrPublicationNotFound):
		writeAdminError(w, http.StatusNotFound, "PUBLICATION_NOT_FOUND", "published score data not found")
	case errors.Is(err, publication.ErrRunNotReady):
		writeAdminError(w, http.StatusConflict, "PUBLICATION_RUN_NOT_READY", "run is open, incomplete, or contains invalid scores")
	case errors.Is(err, publication.ErrReviewRequired):
		writeAdminError(w, http.StatusConflict, "REVIEW_REQUIRED", "required manual review remains")
	case errors.Is(err, publication.ErrReportsNotReady):
		writeAdminError(w, http.StatusConflict, "DETECTION_REPORTS_NOT_READY", "current answer similarity and score anomaly reports must be complete")
	case errors.Is(err, publication.ErrBlockingFindings):
		writeAdminError(w, http.StatusConflict, "BLOCKING_FINDINGS_OPEN", "resolve blocking score anomaly findings before publication")
	case errors.Is(err, publication.ErrCorrectionReason):
		writeAdminError(w, http.StatusConflict, "CORRECTION_REASON_REQUIRED", "a correction reason of at least five characters is required")
	default:
		writeAdminError(w, http.StatusInternalServerError, "PUBLICATION_FAILED", "score publication failed")
	}
}
