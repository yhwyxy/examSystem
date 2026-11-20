package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/yhwyxy/examSystem/internal/auth"
	"github.com/yhwyxy/examSystem/internal/questionbank"
)

func TestQuestionBankSamplingAndAssemblyFence(t *testing.T) {
	pool := testSubmPool(t)
	ctx := context.Background()
	store := questionbank.NewStore(pool)
	input := func(id, discipline, difficulty, point string) questionbank.Input {
		return questionbank.Input{ID: id, Discipline: discipline, Difficulty: difficulty, KnowledgePoints: []string{point}, Question: map[string]any{"id": id, "type": "single_choice", "question": "Sampling source " + id, "score": 2, "options": []any{map[string]any{"key": "A", "text": "yes"}, map[string]any{"key": "B", "text": "no"}}, "answer": "A"}}
	}
	for i := 0; i < 8; i++ {
		id := fmt.Sprintf("sample-%d", i)
		in := input(id, "math", "easy", "algebra")
		if i == 3 {
			in.Discipline = "physics"
		}
		if i == 4 {
			in.Difficulty = "hard"
		}
		if i == 5 {
			in.KnowledgePoints = []string{"geometry"}
		}
		if _, err := store.Create(ctx, in); err != nil {
			t.Fatal(err)
		}
		if i == 6 {
			continue
		} // Drafts are never eligible.
		for _, action := range []string{"review", "enable"} {
			if _, err := store.Transition(ctx, id, action); err != nil {
				t.Fatal(err)
			}
		}
		if i == 7 {
			if _, err := store.Transition(ctx, id, "disable"); err != nil {
				t.Fatal(err)
			}
		}
	}
	authStore := auth.NewStore(true, nil)
	authStore.SetPasswordProvider(auth.PasswordProvider{Authenticate: func(_ context.Context, username, password string) (string, error) {
		if password != "test-password" {
			return "", auth.ErrInvalidCredentials
		}
		if username == "editor01" {
			return auth.RoleQuestionBankEditor, nil
		}
		if username == "publisher01" {
			return auth.RolePublisher, nil
		}
		return "", auth.ErrInvalidCredentials
	}})
	token, _, err := authStore.LoginUser(ctx, "editor01", "test-password")
	if err != nil {
		t.Fatal(err)
	}
	publisher, _, err := authStore.LoginUser(ctx, "publisher01", "test-password")
	if err != nil {
		t.Fatal(err)
	}
	router := chi.NewRouter()
	router.Route("/api", func(r chi.Router) { MountAdmin(r, Dependencies{Auth: authStore, QuestionBank: store, Pool: pool}) })
	request := func(path string, body any, tok string) *httptest.ResponseRecorder {
		t.Helper()
		raw, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
		req.Header.Set("Authorization", "Bearer "+tok)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}
	criteria := map[string]any{"count": 2, "discipline": "math", "difficulty": "easy", "knowledge_point": "algebra", "type": "single_choice", "q": "Sampling source", "exclude_ids": []string{"sample-0"}}
	if rec := request("/api/admin/question-bank/sample", criteria, publisher); rec.Code != http.StatusForbidden {
		t.Fatalf("publisher sampled: %d %s", rec.Code, rec.Body.String())
	}
	rec := request("/api/admin/question-bank/sample", criteria, token)
	var result struct {
		Items []questionbank.Item `json:"items"`
		Count int                 `json:"count"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &result) != nil || result.Count != 2 {
		t.Fatalf("sample: %d %s", rec.Code, rec.Body.String())
	}
	seen := map[string]bool{}
	for _, item := range result.Items {
		if seen[item.ID] || (item.ID != "sample-1" && item.ID != "sample-2") || item.Version != 1 || !item.Current || item.Status != "enabled" {
			t.Fatalf("invalid draw: %+v", result)
		}
		seen[item.ID] = true
	}
	refs := []questionbank.Reference{{ID: result.Items[0].ID, Version: 1}, {ID: result.Items[1].ID, Version: 1}}
	if rec := request("/api/admin/question-bank/assembly-snapshot", map[string]any{"items": refs}, token); rec.Code != http.StatusOK {
		t.Fatalf("assemble: %d %s", rec.Code, rec.Body.String())
	}
	// A later edit invalidates only the preview; it cannot silently approve v2.
	if _, err := store.Update(ctx, refs[0].ID, 1, input(refs[0].ID, "math", "easy", "algebra")); err != nil {
		t.Fatal(err)
	}
	if rec := request("/api/admin/question-bank/assembly-snapshot", map[string]any{"items": refs}, token); rec.Code != http.StatusConflict {
		t.Fatalf("stale assembly: %d %s", rec.Code, rec.Body.String())
	}
	if rec := request("/api/admin/question-bank/sample", criteria, token); rec.Code != http.StatusConflict || bytes.Contains(rec.Body.Bytes(), []byte(`"items"`)) {
		t.Fatalf("partial draw returned: %d %s", rec.Code, rec.Body.String())
	}
	for _, body := range []any{map[string]any{"count": 0}, map[string]any{"count": 201}, map[string]any{"count": "2"}, map[string]any{"count": 1, "status": "draft"}, map[string]any{"count": 1, "exclude_ids": []string{""}}} {
		if rec := request("/api/admin/question-bank/sample", body, token); rec.Code != http.StatusBadRequest {
			t.Fatalf("invalid sampling accepted: %d %s", rec.Code, rec.Body.String())
		}
	}
	// No exclusions must behave as an empty set, not SQL NULL filtering all rows.
	items, err := store.Sample(ctx, questionbank.Filter{Discipline: "math", Difficulty: "easy", KnowledgePoint: "algebra"}, 2, nil)
	if err != nil || len(items) != 2 {
		t.Fatalf("empty exclusions: %+v %v", items, err)
	}
}
