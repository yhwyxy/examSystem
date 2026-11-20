package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/yhwyxy/examSystem/internal/auth"
	"github.com/yhwyxy/examSystem/internal/questionbank"
)

func listQuestionBankHandler(deps Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		store, ok := questionBankStore(w, deps)
		if !ok {
			return
		}
		filter, err := questionBankFilter(r)
		if err != nil {
			writeAdminError(w, http.StatusBadRequest, "INVALID_FILTER", err.Error())
			return
		}
		items, total, err := store.List(r.Context(), filter)
		if err != nil {
			writeQuestionBankError(w, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"items": items, "total": total, "limit": filter.Limit, "offset": filter.Offset})
	}
}

func createQuestionBankHandler(deps Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		store, ok := questionBankStore(w, deps)
		if !ok {
			return
		}
		var input questionbank.Input
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&input); err != nil {
			writeAdminError(w, http.StatusBadRequest, "INVALID_JSON", err.Error())
			return
		}
		if !validateQuestionBankReason(w, input.Reason) {
			return
		}
		item, err := store.Create(r.Context(), input, questionBankAudit(r, input.Reason))
		if err != nil {
			writeQuestionBankError(w, err)
			return
		}
		WriteJSON(w, http.StatusCreated, item)
	}
}

func getQuestionBankHandler(deps Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		store, ok := questionBankStore(w, deps)
		if !ok {
			return
		}
		version := 0
		if raw := r.URL.Query().Get("version"); raw != "" {
			parsed, err := strconv.Atoi(raw)
			if err != nil || parsed < 1 {
				writeAdminError(w, http.StatusBadRequest, "INVALID_VERSION", "version must be a positive integer")
				return
			}
			version = parsed
		}
		item, err := store.Get(r.Context(), chi.URLParam(r, "id"), version)
		if err != nil {
			writeQuestionBankError(w, err)
			return
		}
		WriteJSON(w, http.StatusOK, item)
	}
}

func questionBankVersionsHandler(deps Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		store, ok := questionBankStore(w, deps)
		if !ok {
			return
		}
		items, err := store.Versions(r.Context(), chi.URLParam(r, "id"))
		if err != nil {
			writeQuestionBankError(w, err)
			return
		}
		events, err := store.AuditEvents(r.Context(), chi.URLParam(r, "id"))
		if err != nil {
			writeQuestionBankError(w, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"versions": items, "audit_events": events})
	}
}

func updateQuestionBankHandler(deps Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		store, ok := questionBankStore(w, deps)
		if !ok {
			return
		}
		var body struct {
			ExpectedVersion int `json:"expected_version"`
			questionbank.Input
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&body); err != nil {
			writeAdminError(w, http.StatusBadRequest, "INVALID_JSON", err.Error())
			return
		}
		if !validateQuestionBankReason(w, body.Reason) {
			return
		}
		item, err := store.Update(r.Context(), chi.URLParam(r, "id"), body.ExpectedVersion, body.Input, questionBankAudit(r, body.Reason))
		if err != nil {
			writeQuestionBankError(w, err)
			return
		}
		WriteJSON(w, http.StatusOK, item)
	}
}

func questionBankTransitionHandler(deps Dependencies, action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		store, ok := questionBankStore(w, deps)
		if !ok {
			return
		}
		var body struct {
			Reason          string `json:"reason"`
			ExpectedVersion int    `json:"expected_version"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil {
			writeAdminError(w, http.StatusBadRequest, "INVALID_JSON", "provide a reason for the lifecycle change")
			return
		}
		body.Reason = strings.TrimSpace(body.Reason)
		if body.ExpectedVersion < 1 {
			writeAdminError(w, http.StatusBadRequest, "INVALID_VERSION", "expected_version must identify the reviewed version")
			return
		}
		if !validateQuestionBankReason(w, body.Reason) {
			return
		}
		audit := questionBankAudit(r, body.Reason)
		audit.ExpectedVersion = body.ExpectedVersion
		item, err := store.Transition(r.Context(), chi.URLParam(r, "id"), action, audit)
		if err != nil {
			writeQuestionBankError(w, err)
			return
		}
		WriteJSON(w, http.StatusOK, item)
	}
}

func exportQuestionBankHandler(deps Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		store, ok := questionBankStore(w, deps)
		if !ok {
			return
		}
		filter, err := questionBankFilter(r)
		if err != nil {
			writeAdminError(w, http.StatusBadRequest, "INVALID_FILTER", err.Error())
			return
		}
		filter.Limit = 200
		filter.Offset = 0
		var items []questionbank.Input
		for {
			page, total, err := store.List(r.Context(), filter)
			if err != nil {
				writeQuestionBankError(w, err)
				return
			}
			for _, item := range page {
				items = append(items, questionbank.Input{
					ID: item.ID, Discipline: item.Discipline, Difficulty: item.Difficulty,
					KnowledgePoints: item.KnowledgePoints, Question: item.Question,
				})
			}
			filter.Offset += len(page)
			if len(items) >= total || len(page) == 0 {
				break
			}
		}
		w.Header().Set("Content-Disposition", `attachment; filename="question-bank.json"`)
		WriteJSON(w, http.StatusOK, map[string]any{"items": items, "count": len(items), "format_version": 1})
	}
}

func importQuestionBankHandler(deps Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		store, ok := questionBankStore(w, deps)
		if !ok {
			return
		}
		var body struct {
			FormatVersion int                  `json:"format_version"`
			Items         []questionbank.Input `json:"items"`
			Reason        string               `json:"reason"`
		}
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 10<<20))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&body); err != nil {
			writeAdminError(w, http.StatusBadRequest, "INVALID_JSON", err.Error())
			return
		}
		if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
			writeAdminError(w, http.StatusBadRequest, "INVALID_JSON", "request must contain one JSON object")
			return
		}
		if body.FormatVersion != 0 && body.FormatVersion != 1 {
			writeAdminError(w, http.StatusBadRequest, "UNSUPPORTED_IMPORT_VERSION", "format_version must be 1")
			return
		}
		if !validateQuestionBankReason(w, body.Reason) {
			return
		}
		items, err := store.Import(r.Context(), body.Items, questionBankAudit(r, body.Reason))
		if err != nil {
			writeQuestionBankError(w, err)
			return
		}
		WriteJSON(w, http.StatusCreated, map[string]any{"items": items, "count": len(items)})
	}
}

func questionBankAudit(r *http.Request, reason string) questionbank.AuditInput {
	actor := auth.UsernameFromContext(r.Context())
	if actor == "" {
		actor = "local-admin"
	}
	return questionbank.AuditInput{Actor: actor, Reason: reason}
}

func validateQuestionBankReason(w http.ResponseWriter, reason string) bool {
	n := len([]rune(strings.TrimSpace(reason)))
	if n < 1 || n > 2000 {
		writeAdminError(w, http.StatusBadRequest, "INVALID_REASON", "reason must contain 1 to 2000 characters")
		return false
	}
	return true
}

func assembleQuestionBankHandler(deps Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		store, ok := questionBankStore(w, deps)
		if !ok {
			return
		}
		var body struct {
			Items []questionbank.Reference `json:"items"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
			writeAdminError(w, http.StatusBadRequest, "INVALID_JSON", err.Error())
			return
		}
		items, err := store.Assemble(r.Context(), body.Items)
		if err != nil {
			writeQuestionBankError(w, err)
			return
		}
		for i := range items {
			items[i].Question["question_bank_ref"] = map[string]any{
				"id": items[i].ID, "version": items[i].Version,
				"discipline": items[i].Discipline, "difficulty": items[i].Difficulty,
				"knowledge_points": items[i].KnowledgePoints,
			}
		}
		WriteJSON(w, http.StatusOK, map[string]any{"items": items})
	}
}

func sampleQuestionBankHandler(deps Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		store, ok := questionBankStore(w, deps)
		if !ok {
			return
		}
		var body struct {
			Count          int      `json:"count"`
			Discipline     string   `json:"discipline"`
			Difficulty     string   `json:"difficulty"`
			KnowledgePoint string   `json:"knowledge_point"`
			QuestionType   string   `json:"type"`
			Search         string   `json:"q"`
			ExcludeIDs     []string `json:"exclude_ids"`
		}
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&body); err != nil {
			writeAdminError(w, http.StatusBadRequest, "INVALID_JSON", err.Error())
			return
		}
		if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
			writeAdminError(w, http.StatusBadRequest, "INVALID_JSON", "request must contain one JSON object")
			return
		}
		items, err := store.Sample(r.Context(), questionbank.Filter{Discipline: strings.TrimSpace(body.Discipline), Difficulty: strings.TrimSpace(body.Difficulty), KnowledgePoint: strings.TrimSpace(body.KnowledgePoint), QuestionType: strings.TrimSpace(body.QuestionType), Search: strings.TrimSpace(body.Search)}, body.Count, body.ExcludeIDs)
		if err != nil {
			writeQuestionBankError(w, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"items": items, "count": len(items)})
	}
}

func questionBankFilter(r *http.Request) (questionbank.Filter, error) {
	query := r.URL.Query()
	filter := questionbank.Filter{
		Discipline:     query.Get("discipline"),
		Difficulty:     query.Get("difficulty"),
		KnowledgePoint: query.Get("knowledge_point"),
		QuestionType:   query.Get("type"),
		Status:         query.Get("status"),
		Search:         query.Get("q"),
		Limit:          50,
	}
	if raw := query.Get("limit"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > 200 {
			return filter, fmt.Errorf("limit must be between 1 and 200")
		}
		filter.Limit = value
	}
	if raw := query.Get("offset"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 0 {
			return filter, fmt.Errorf("offset must be a nonnegative integer")
		}
		filter.Offset = value
	}
	return filter, nil
}

func questionBankStore(w http.ResponseWriter, deps Dependencies) (*questionbank.Store, bool) {
	if deps.QuestionBank == nil {
		writeAdminError(w, http.StatusServiceUnavailable, "QUESTION_BANK_NOT_CONFIGURED", "question bank store is unavailable")
		return nil, false
	}
	return deps.QuestionBank, true
}

func writeQuestionBankError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, questionbank.ErrInsufficientQuestions):
		writeAdminError(w, http.StatusConflict, "INSUFFICIENT_QUESTIONS", err.Error())
	case errors.Is(err, questionbank.ErrNotFound):
		writeAdminError(w, http.StatusNotFound, "QUESTION_NOT_FOUND", "question bank item not found")
	case errors.Is(err, questionbank.ErrAlreadyExists), errors.Is(err, questionbank.ErrVersionConflict), errors.Is(err, questionbank.ErrInvalidState):
		writeAdminError(w, http.StatusConflict, "QUESTION_CONFLICT", err.Error())
	case errors.Is(err, questionbank.ErrInvalidInput):
		writeAdminError(w, http.StatusBadRequest, "INVALID_QUESTION", err.Error())
	case errors.Is(err, questionbank.ErrNotConfigured):
		writeAdminError(w, http.StatusServiceUnavailable, "QUESTION_BANK_NOT_CONFIGURED", err.Error())
	default:
		writeAdminError(w, http.StatusInternalServerError, "QUESTION_BANK_FAILED", "question bank operation failed")
	}
}
