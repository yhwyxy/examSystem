package answersimilarity

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	embeddingRequestBatchSize  = 32
	embeddingMaxResponseBytes  = 8 << 20
	embeddingMaxDimensions     = 8192
	maxEmbeddingInputsPerBatch = 25_000
	maxEmbeddingTextBytes      = 8 << 20
)

var ErrEmbeddingUnavailable = errors.New("answer embedding provider is unavailable")

// Embedder produces one dense semantic vector for each input, in input order.
type Embedder interface {
	Version() string
	Embed(context.Context, []string) ([][]float64, error)
}

type HTTPEmbedder struct {
	endpoint string
	apiKey   string
	model    string
	version  string
	client   *http.Client
}

func NewHTTPEmbedder(rawURL, apiKey, model string) (*HTTPEmbedder, error) {
	rawURL, apiKey, model = strings.TrimSpace(rawURL), strings.TrimSpace(apiKey), strings.TrimSpace(model)
	if rawURL == "" || apiKey == "" || model == "" || len(rawURL) > 2048 || len(apiKey) > 4096 || len(model) > 200 {
		return nil, ErrEmbeddingUnavailable
	}
	parsed, err := url.Parse(rawURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
		return nil, ErrEmbeddingUnavailable
	}
	if !strings.HasSuffix(strings.TrimRight(parsed.Path, "/"), "/embeddings") && strings.TrimRight(parsed.Path, "/") != "embeddings" {
		parsed.Path = strings.TrimRight(parsed.Path, "/") + "/embeddings"
	}
	endpoint := parsed.String()
	fingerprint := sha256.Sum256([]byte(endpoint + "\x00" + model))
	version := fmt.Sprintf("hybrid-semantic-v1:%s:%s", model, hex.EncodeToString(fingerprint[:6]))
	return &HTTPEmbedder{
		endpoint: endpoint,
		apiKey:   apiKey,
		model:    model,
		version:  version,
		client:   &http.Client{Timeout: 45 * time.Second},
	}, nil
}

func (e *HTTPEmbedder) Version() string {
	if e == nil {
		return ""
	}
	return e.version
}

func (e *HTTPEmbedder) Embed(ctx context.Context, inputs []string) ([][]float64, error) {
	if e == nil || len(inputs) == 0 {
		return nil, ErrEmbeddingUnavailable
	}
	if len(inputs) > maxEmbeddingInputsPerBatch {
		return nil, ErrEmbeddingBudgetExceeded
	}
	totalBytes := 0
	for _, input := range inputs {
		totalBytes += len(input)
		if totalBytes > maxEmbeddingTextBytes {
			return nil, ErrEmbeddingBudgetExceeded
		}
	}
	result := make([][]float64, len(inputs))
	dimensions := 0
	for start := 0; start < len(inputs); start += embeddingRequestBatchSize {
		end := min(start+embeddingRequestBatchSize, len(inputs))
		vectors, err := e.embedChunk(ctx, inputs[start:end])
		if err != nil {
			return nil, err
		}
		for offset, vector := range vectors {
			if dimensions == 0 {
				dimensions = len(vector)
			}
			if len(vector) != dimensions {
				return nil, ErrEmbeddingUnavailable
			}
			result[start+offset] = vector
		}
	}
	return result, nil
}

func (e *HTTPEmbedder) embedChunk(ctx context.Context, inputs []string) ([][]float64, error) {
	body, err := json.Marshal(map[string]any{"model": e.model, "input": inputs})
	if err != nil {
		return nil, ErrEmbeddingUnavailable
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, ErrEmbeddingUnavailable
	}
	req.Header.Set("Authorization", "Bearer "+e.apiKey)
	req.Header.Set("Content-Type", "application/json")
	response, err := e.client.Do(req)
	if err != nil {
		return nil, ErrEmbeddingUnavailable
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, ErrEmbeddingUnavailable
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, embeddingMaxResponseBytes+1))
	if err != nil || len(raw) > embeddingMaxResponseBytes {
		return nil, ErrEmbeddingUnavailable
	}
	var payload struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float64 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil || len(payload.Data) != len(inputs) {
		return nil, ErrEmbeddingUnavailable
	}
	vectors := make([][]float64, len(inputs))
	seen := make([]bool, len(inputs))
	for _, item := range payload.Data {
		if item.Index < 0 || item.Index >= len(inputs) || seen[item.Index] || len(item.Embedding) == 0 || len(item.Embedding) > embeddingMaxDimensions {
			return nil, ErrEmbeddingUnavailable
		}
		norm := 0.0
		for _, value := range item.Embedding {
			if math.IsNaN(value) || math.IsInf(value, 0) {
				return nil, ErrEmbeddingUnavailable
			}
			norm += value * value
		}
		if norm < 1e-24 {
			return nil, ErrEmbeddingUnavailable
		}
		seen[item.Index] = true
		vectors[item.Index] = item.Embedding
	}
	for _, present := range seen {
		if !present {
			return nil, ErrEmbeddingUnavailable
		}
	}
	return vectors, nil
}
