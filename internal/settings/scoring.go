// 评分方式设置: app_settings["scoring"].
//
// 三种方式:
//   - local:         本地语义模型 (bge-reranker), worker 侧读 RERANKER_MODEL.
//   - remote_reranker: 远程 reranker API (Cohere 兼容), 需 url/api_key/model.
//   - llm:           大模型判分 API (OpenAI 兼容 chat/completions), 需 url/api_key/model.
//
// worker 启动/轮询时读同一张表, 按 method 构建对应评分引擎; API 凭据明文存 DB
// (内部系统), 前端 GET 返回时打码 (只回显是否已配置, 不回明文).
package settings

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
)

// scoringKey 是 app_settings 中评分方式的 key.
const scoringKey = "scoring"

// ScoringMethod 是评分方式枚举.
type ScoringMethod string

const (
	MethodLocal          ScoringMethod = "local"
	MethodRemoteReranker ScoringMethod = "remote_reranker"
	MethodLLM            ScoringMethod = "llm"
)

// ParseScoringMethod 解析字符串; 非法返回 error.
func ParseScoringMethod(s string) (ScoringMethod, error) {
	switch ScoringMethod(strings.TrimSpace(strings.ToLower(s))) {
	case MethodLocal:
		return MethodLocal, nil
	case MethodRemoteReranker:
		return MethodRemoteReranker, nil
	case MethodLLM:
		return MethodLLM, nil
	}
	return "", fmt.Errorf("未知评分方式: %q (可选 local/remote_reranker/llm)", s)
}

// ScoringConfig 是评分方式的完整配置 (DB 存储形态).
type ScoringConfig struct {
	Method       ScoringMethod `json:"method"`
	RerankAPIURL string        `json:"rerank_api_url,omitempty"`
	RerankAPIKey string        `json:"rerank_api_key,omitempty"`
	RerankModel  string        `json:"rerank_model,omitempty"`
	LLMAPIURL    string        `json:"llm_api_url,omitempty"`
	LLMAPIPKey   string        `json:"llm_api_key,omitempty"`
	LLMModel     string        `json:"llm_model,omitempty"`
}

// Validate 校验配置完整性: remote_reranker/llm 必须有对应 url+key+model.
func (c *ScoringConfig) Validate() error {
	switch c.Method {
	case MethodLocal:
		return nil
	case MethodRemoteReranker:
		var missing []string
		if c.RerankAPIURL == "" {
			missing = append(missing, "rerank_api_url")
		}
		if c.RerankAPIKey == "" {
			missing = append(missing, "rerank_api_key")
		}
		if c.RerankModel == "" {
			missing = append(missing, "rerank_model")
		}
		if len(missing) > 0 {
			return fmt.Errorf("远程 reranker 缺少必填项: %s", strings.Join(missing, ", "))
		}
		return nil
	case MethodLLM:
		var missing []string
		if c.LLMAPIURL == "" {
			missing = append(missing, "llm_api_url")
		}
		if c.LLMAPIPKey == "" {
			missing = append(missing, "llm_api_key")
		}
		if c.LLMModel == "" {
			missing = append(missing, "llm_model")
		}
		if len(missing) > 0 {
			return fmt.Errorf("大模型 API 缺少必填项: %s", strings.Join(missing, ", "))
		}
		return nil
	}
	return fmt.Errorf("未知评分方式: %q", c.Method)
}

// LoadScoringFromEnv 从环境变量派生默认评分配置 (与 worker 12-factor 对齐).
//
// 兼容现有 RERANK_USE_REMOTE 语义: true -> remote_reranker;
// 新增 SCORING_METHOD 显式指定 (local/remote_reranker/llm), 优先于 RERANK_USE_REMOTE.
func LoadScoringFromEnv() *ScoringConfig {
	cfg := &ScoringConfig{
		Method:       MethodLocal,
		RerankAPIURL: strings.TrimSpace(os.Getenv("RERANK_API_URL")),
		RerankAPIKey: strings.TrimSpace(os.Getenv("RERANK_API_KEY")),
		RerankModel:  strings.TrimSpace(os.Getenv("RERANK_MODEL")),
		LLMAPIURL:    strings.TrimSpace(os.Getenv("LLM_API_URL")),
		LLMAPIPKey:   strings.TrimSpace(os.Getenv("LLM_API_KEY")),
		LLMModel:     strings.TrimSpace(os.Getenv("LLM_MODEL")),
	}
	if m := strings.TrimSpace(os.Getenv("SCORING_METHOD")); m != "" {
		if parsed, err := ParseScoringMethod(m); err == nil {
			cfg.Method = parsed
			return cfg
		}
	}
	if raw := strings.TrimSpace(strings.ToLower(os.Getenv("RERANK_USE_REMOTE"))); raw == "true" {
		cfg.Method = MethodRemoteReranker
	}
	return cfg
}

// LoadScoring 读 DB 配置; 不存在或解析失败回退 LoadScoringFromEnv (env 兜底).
func (s *Store) LoadScoring(ctx context.Context) (*ScoringConfig, error) {
	raw, err := s.GetRaw(ctx, scoringKey)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return LoadScoringFromEnv(), nil
		}
		return nil, err
	}
	var cfg ScoringConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("settings: parse scoring: %w", err)
	}
	if cfg.Method == "" {
		return LoadScoringFromEnv(), nil
	}
	return &cfg, nil
}

// SaveScoring 校验后写 DB.
func (s *Store) SaveScoring(ctx context.Context, cfg *ScoringConfig) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	return s.SetRaw(ctx, scoringKey, raw)
}
