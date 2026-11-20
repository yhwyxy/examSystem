package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/yhwyxy/examSystem/internal/answersimilarity"
	"github.com/yhwyxy/examSystem/internal/auth"
	"github.com/yhwyxy/examSystem/internal/db"
	"github.com/yhwyxy/examSystem/internal/testutil"
)

func TestAnswerSimilarityAdminRoutesRequireAdminAndListRuns(t *testing.T) {
	pool, _ := testutil.NewSchema(t)
	if _, err := db.Migrate(context.Background(), pool); err != nil {
		t.Fatalf("migrate test schema: %v", err)
	}
	store := auth.NewStore(true, func() (string, error) { return "admin-password", nil })
	token, err := store.Login(context.Background(), "admin-password")
	if err != nil {
		t.Fatalf("admin login: %v", err)
	}
	router := chi.NewRouter()
	router.Route("/api", func(api chi.Router) {
		MountAdmin(api, Dependencies{Auth: store, AnswerSimilarity: answersimilarity.NewService(pool, nil)})
	})

	unauthorized := httptest.NewRecorder()
	router.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/api/admin/answer-similarity/runs", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status=%d body=%s", unauthorized.Code, unauthorized.Body.String())
	}

	request := httptest.NewRequest(http.MethodGet, "/api/admin/answer-similarity/runs", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Body.String() != "{\"runs\":[]}\n" {
		t.Fatalf("authorized status=%d body=%s", response.Code, response.Body.String())
	}
}
