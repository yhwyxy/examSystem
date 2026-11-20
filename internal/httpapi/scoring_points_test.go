package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yhwyxy/examSystem/internal/auth"
	"github.com/yhwyxy/examSystem/internal/config"
	"github.com/yhwyxy/examSystem/internal/settings"
)

func TestExtractScoringPointsReturnsCandidatesForTeacherReview(t *testing.T) {
	var providerCalls int
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		providerCalls++
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("provider path=%q", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer fake-test-key" {
			t.Errorf("provider authorization header was not configured")
		}
		var request struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode provider request: %v", err)
		}
		if request.Model != "test-model" {
			t.Errorf("provider model=%q", request.Model)
		}
		content, _ := json.Marshal(map[string]any{"points": []any{
			map[string]any{"text": "说明索引可提高查询效率", "score": 5, "accepted_expressions": []string{"加快查询"}, "key_conditions": []string{"与全表扫描相比"}, "source_evidence": "索引可以提高查询效率"},
			map[string]any{"text": "补充索引维护成本", "score": 4, "source_evidence": "维护索引没有代价"},
		}})
		providerBody, _ := json.Marshal(map[string]any{"choices": []any{
			map[string]any{"message": map[string]string{"content": string(content)}},
		}})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(providerBody)
	}))
	defer provider.Close()

	pool := testSubmPool(t)
	store := settings.NewStore(pool)
	if err := store.SaveScoring(context.Background(), &settings.ScoringConfig{
		Method: settings.MethodLLM, LLMAPIURL: provider.URL + "/v1", LLMAPIPKey: "fake-test-key", LLMModel: "test-model",
	}); err != nil {
		t.Fatal(err)
	}
	router := NewRouter(Dependencies{
		Config:   &config.Config{},
		Auth:     auth.NewStore(false, nil),
		Settings: store,
	})
	question := map[string]any{
		"id": "q1", "type": "short_answer", "question": "索引有什么作用？", "score": 10.0,
		"scoring_mode": "text", "answer": "索引可以提高查询效率。",
	}
	body, err := json.Marshal(map[string]any{"question": question})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/admin/question-bank/scoring-points/extract", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, req)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var result struct {
		Status                string `json:"status"`
		RequiresTeacherReview bool   `json:"requires_teacher_review"`
		Candidates            []struct {
			ID                  string   `json:"id"`
			AcceptedExpressions []string `json:"accepted_expressions"`
			SourceEvidence      string   `json:"source_evidence"`
		} `json:"candidates"`
		Diagnostics []string `json:"diagnostics"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Status != "candidate_requires_teacher_review" || !result.RequiresTeacherReview || len(result.Candidates) != 2 {
		t.Fatalf("candidate response = %s", response.Body.String())
	}
	if result.Candidates[0].ID != "p1" || result.Candidates[0].SourceEvidence != "索引可以提高查询效率" ||
		len(result.Candidates[0].AcceptedExpressions) != 1 {
		t.Fatalf("candidate fields = %s", response.Body.String())
	}
	diagnostics := strings.Join(result.Diagnostics, "\n")
	if !strings.Contains(diagnostics, "逐字匹配") || !strings.Contains(diagnostics, "不会自动缩放") {
		t.Fatalf("missing review diagnostics: %s", diagnostics)
	}
	if providerCalls != 1 {
		t.Fatalf("provider calls=%d", providerCalls)
	}
	var questions, versions int
	if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM question_bank_questions").Scan(&questions); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM question_bank_versions").Scan(&versions); err != nil {
		t.Fatal(err)
	}
	if questions != 0 || versions != 0 {
		t.Fatalf("extraction persisted question bank state: questions=%d versions=%d", questions, versions)
	}
}

func TestExtractScoringPointsDoesNotExposeProviderErrors(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "provider-secret-response fake-test-key", http.StatusInternalServerError)
	}))
	defer provider.Close()
	pool := testSubmPool(t)
	store := settings.NewStore(pool)
	if err := store.SaveScoring(context.Background(), &settings.ScoringConfig{
		Method: settings.MethodLLM, LLMAPIURL: provider.URL, LLMAPIPKey: "fake-test-key", LLMModel: "test-model",
	}); err != nil {
		t.Fatal(err)
	}
	router := NewRouter(Dependencies{Config: &config.Config{}, Auth: auth.NewStore(false, nil), Settings: store})
	question := map[string]any{
		"id": "q1", "type": "short_answer", "question": "索引有什么作用？", "score": 10.0,
		"scoring_mode": "text", "answer": "索引可以提高查询效率。",
	}
	body, _ := json.Marshal(map[string]any{"question": question})
	req := httptest.NewRequest(http.MethodPost, "/api/admin/question-bank/scoring-points/extract", bytes.NewReader(body))
	response := httptest.NewRecorder()
	router.ServeHTTP(response, req)
	if response.Code != http.StatusBadGateway {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "provider-secret-response") || strings.Contains(response.Body.String(), "fake-test-key") {
		t.Fatalf("provider details leaked: %s", response.Body.String())
	}
}

func TestScoringPointCandidatesDiagnoseDuplicateAndVaguePoints(t *testing.T) {
	question := map[string]any{"score": 10.0, "answer": "索引可以提高查询效率。"}
	content := `{"points":[{"text":"回答正确","score":5,"source_evidence":"索引可以提高查询效率"},{"text":"回答 正确。","score":5,"source_evidence":"索引可以提高查询效率"}]}`
	candidates, diagnostics, err := validateScoringPointCandidates(content, question)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 2 {
		t.Fatalf("candidates=%d", len(candidates))
	}
	joined := strings.Join(diagnostics, "\n")
	if !strings.Contains(joined, "描述重复") || !strings.Contains(joined, "过于笼统") {
		t.Fatalf("missing quality diagnostics: %s", joined)
	}
}

func TestScoringPointExtractionUsesOnlyTheSelectedLanguageAnswer(t *testing.T) {
	question := map[string]any{
		"score": 10.0, "code_language": "python", "answer": "generic fallback",
		"answers_by_language": map[string]any{
			"python": "Python reference answer", "java": "Java-only reference answer",
		},
	}
	selected, err := scoringPointExtractionQuestion(question)
	if err != nil {
		t.Fatal(err)
	}
	if selected["answer"] != "Python reference answer" {
		t.Fatalf("selected answer=%v", selected["answer"])
	}
	if _, exists := selected["answers_by_language"]; exists {
		t.Fatal("unselected language answers were sent to the model")
	}
	_, diagnostics, err := validateScoringPointCandidates(
		`{"points":[{"text":"Python behavior","score":10,"source_evidence":"Java-only reference answer"}]}`,
		selected,
	)
	if err != nil || !strings.Contains(strings.Join(diagnostics, " "), "逐字匹配") {
		t.Fatalf("unselected answer evidence was accepted: diagnostics=%v err=%v", diagnostics, err)
	}
	if _, err := scoringPointExtractionQuestion(map[string]any{
		"answers_by_language": map[string]any{"python": "answer"},
	}); err == nil {
		t.Fatal("missing selected code_language was accepted")
	}
}
