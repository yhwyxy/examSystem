package answersimilarity

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHTTPEmbedderBatchesAndRestoresInputOrder(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path != "/v1/embeddings" || r.Header.Get("Authorization") != "Bearer private-test-key" {
			t.Errorf("request path/auth = %q / %q", r.URL.Path, r.Header.Get("Authorization"))
		}
		var request struct {
			Model string   `json:"model"`
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if request.Model != "test-embedding-model" {
			t.Errorf("model=%q", request.Model)
		}
		data := make([]map[string]any, len(request.Input))
		for index := range request.Input {
			originalIndex := len(request.Input) - index - 1
			data[index] = map[string]any{"index": originalIndex, "embedding": []float64{float64(originalIndex + 1), 1}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	defer server.Close()

	embedder, err := NewHTTPEmbedder(server.URL+"/v1", "private-test-key", "test-embedding-model")
	if err != nil {
		t.Fatal(err)
	}
	inputs := make([]string, 33)
	for index := range inputs {
		inputs[index] = fmt.Sprintf("answer-%d", index)
	}
	vectors, err := embedder.Embed(context.Background(), inputs)
	if err != nil {
		t.Fatal(err)
	}
	if requests != 2 || len(vectors) != len(inputs) || vectors[0][0] != 1 || vectors[31][0] != 32 || vectors[32][0] != 1 {
		t.Fatalf("requests=%d vectors[0,31,32]=%v,%v,%v", requests, vectors[0], vectors[31], vectors[32])
	}
	if strings.Contains(embedder.Version(), "private-test-key") {
		t.Fatal("provider version exposed the API key")
	}
}

func TestHTTPEmbedderRejectsInvalidVectorPayload(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[0,0]}]}`))
	}))
	defer server.Close()
	embedder, err := NewHTTPEmbedder(server.URL, "key", "model")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := embedder.Embed(context.Background(), []string{"answer"}); err != ErrEmbeddingUnavailable {
		t.Fatalf("invalid zero vector err=%v", err)
	}
}
