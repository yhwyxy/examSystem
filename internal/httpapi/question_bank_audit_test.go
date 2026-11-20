package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/yhwyxy/examSystem/internal/auth"
	"github.com/yhwyxy/examSystem/internal/questionbank"
)

func TestQuestionBankNamedAuditAndVersionFence(t *testing.T) {
	pool := testSubmPool(t)
	ctx := context.Background()
	store := questionbank.NewStore(pool)
	// Legacy direct callers and generated bank IDs must remain usable without
	// inventing an authenticated identity or a supplied explanation.
	legacy, err := store.Create(ctx, questionbank.Input{Discipline: "math", Difficulty: "easy", KnowledgePoints: []string{"arithmetic"}, Question: map[string]any{
		"type": "single_choice", "question": "Legacy local source", "score": 2,
		"options": []any{map[string]any{"key": "A", "text": "two"}, map[string]any{"key": "B", "text": "three"}}, "answer": "A",
	}})
	if err != nil {
		t.Fatal(err)
	}
	legacyEvents, err := store.AuditEvents(ctx, legacy.ID)
	if err != nil || len(legacyEvents) != 1 || legacyEvents[0].Actor != nil || legacyEvents[0].Reason != nil {
		t.Fatalf("legacy attribution: %+v %v", legacyEvents, err)
	}
	authStore := auth.NewStore(true, nil)
	authStore.SetPasswordProvider(auth.PasswordProvider{Authenticate: func(_ context.Context, username, password string) (string, error) {
		if username == "editor01" && password == "test-password" {
			return auth.RoleQuestionBankEditor, nil
		}
		return "", auth.ErrInvalidCredentials
	}})
	token, _, err := authStore.LoginUser(ctx, "editor01", "test-password")
	if err != nil {
		t.Fatal(err)
	}
	router := chi.NewRouter()
	router.Route("/api", func(r chi.Router) { MountAdmin(r, Dependencies{Auth: authStore, Pool: pool, QuestionBank: store}) })
	request := func(method, path string, body any) *httptest.ResponseRecorder {
		t.Helper()
		raw, _ := json.Marshal(body)
		req := httptest.NewRequest(method, path, bytes.NewReader(raw))
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}
	input := map[string]any{"id": "audit-bank", "discipline": "math", "difficulty": "easy", "knowledge_points": []string{"arithmetic"}, "reason": "initial reviewed source", "actor": "publisher-forged", "question": map[string]any{"id": "audit-bank", "type": "single_choice", "question": "One plus one?", "score": 2, "options": []any{map[string]any{"key": "A", "text": "two"}, map[string]any{"key": "B", "text": "three"}}, "answer": "A"}}
	if rec := request(http.MethodPost, "/api/admin/question-bank", input); rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	// Missing rationale and stale/unidentified review versions cannot approve content.
	for _, body := range []any{nil, map[string]any{"expected_version": 1}, map[string]any{"reason": "confirmed"}} {
		if rec := request(http.MethodPost, "/api/admin/question-bank/audit-bank/review", body); rec.Code != http.StatusBadRequest {
			t.Fatalf("missing confirmation: %d %s", rec.Code, rec.Body.String())
		}
	}
	input["expected_version"] = 1
	input["reason"] = "revise answer explanation"
	input["question"].(map[string]any)["question"] = "Revised: one plus one?"
	if rec := request(http.MethodPut, "/api/admin/question-bank/audit-bank", input); rec.Code != http.StatusOK {
		t.Fatalf("update: %d %s", rec.Code, rec.Body.String())
	}
	if rec := request(http.MethodPost, "/api/admin/question-bank/audit-bank/review", map[string]any{"expected_version": 1, "reason": "confirmed old content"}); rec.Code != http.StatusConflict {
		t.Fatalf("stale review: %d %s", rec.Code, rec.Body.String())
	}
	// An actual database audit failure must roll back the lifecycle change.
	if _, err := pool.Exec(ctx, `CREATE FUNCTION reject_bank_test() RETURNS trigger AS $$ BEGIN RAISE EXCEPTION 'test audit failure'; END; $$ LANGUAGE plpgsql;
 CREATE TRIGGER reject_bank_test BEFORE INSERT ON question_bank_audit FOR EACH ROW EXECUTE FUNCTION reject_bank_test()`); err != nil {
		t.Fatal(err)
	}
	if rec := request(http.MethodPost, "/api/admin/question-bank/audit-bank/review", map[string]any{"expected_version": 2, "reason": "confirmed current rubric"}); rec.Code != http.StatusInternalServerError {
		t.Fatalf("audit failure: %d %s", rec.Code, rec.Body.String())
	}
	item, err := store.Get(ctx, "audit-bank", 0)
	if err != nil || item.Status != "draft" || item.ReviewedAt != nil {
		t.Fatalf("audit failure committed review: %+v %v", item, err)
	}
	if _, err := pool.Exec(ctx, `DROP TRIGGER reject_bank_test ON question_bank_audit; DROP FUNCTION reject_bank_test()`); err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"review", "enable", "disable", "enable"} {
		if rec := request(http.MethodPost, "/api/admin/question-bank/audit-bank/"+action, map[string]any{"expected_version": 2, "reason": "confirmed current rubric", "actor": "forged"}); rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", action, rec.Code, rec.Body.String())
		}
	}
	events, err := store.AuditEvents(ctx, "audit-bank")
	if err != nil || len(events) != 6 {
		t.Fatalf("events: %+v %v", events, err)
	}
	for _, event := range events {
		if event.Actor == nil || *event.Actor != "editor01" || event.Reason == nil {
			t.Fatalf("forged or missing attribution: %+v", event)
		}
	}
	if events[0].Before != nil || events[1].Version != 2 || !bytes.Contains(events[1].Before, []byte(`"version": 1`)) || !bytes.Contains(events[1].After, []byte(`Revised:`)) {
		t.Fatalf("version snapshots: %+v", events)
	}
	for _, sql := range []string{`UPDATE question_bank_audit SET actor='forged'`, `DELETE FROM question_bank_audit`} {
		if _, err := pool.Exec(ctx, sql); err == nil {
			t.Fatal("audit rewrite allowed")
		}
	}
	rec := request(http.MethodGet, "/api/admin/question-bank/audit-bank/versions", nil)
	if rec.Code != http.StatusOK || !bytes.Contains(rec.Body.Bytes(), []byte(`"audit_events"`)) || !bytes.Contains(rec.Body.Bytes(), []byte(`editor01`)) {
		t.Fatalf("history: %d %s", rec.Code, rec.Body.String())
	}
}
