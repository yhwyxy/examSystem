package settings

import (
	"strings"
	"testing"
)

func TestParseScoringMethod(t *testing.T) {
	cases := map[string]ScoringMethod{
		"local":           MethodLocal,
		"LOCAL":           MethodLocal,
		" local ":         MethodLocal,
		"remote_reranker": MethodRemoteReranker,
		"llm":             MethodLLM,
	}
	for in, want := range cases {
		got, err := ParseScoringMethod(in)
		if err != nil {
			t.Fatalf("ParseScoringMethod(%q) err=%v", in, err)
		}
		if got != want {
			t.Fatalf("ParseScoringMethod(%q)=%v want %v", in, got, want)
		}
	}
	for _, in := range []string{"", "unknown", "Remote", "llm2"} {
		if _, err := ParseScoringMethod(in); err == nil {
			t.Fatalf("ParseScoringMethod(%q) should error", in)
		}
	}
}

func TestScoringConfigValidate(t *testing.T) {
	// local 无需凭据
	if err := (&ScoringConfig{Method: MethodLocal}).Validate(); err != nil {
		t.Fatalf("local should be valid: %v", err)
	}
	// remote_reranker 缺 key -> error
	cfg := &ScoringConfig{Method: MethodRemoteReranker, RerankAPIURL: "http://x", RerankModel: "m"}
	if err := cfg.Validate(); err == nil {
		t.Fatal("remote_reranker missing api_key should error")
	}
	cfg.RerankAPIKey = "k"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("complete remote_reranker should be valid: %v", err)
	}
	// llm 缺 url -> error
	cfg = &ScoringConfig{Method: MethodLLM, LLMAPIPKey: "k", LLMModel: "m"}
	if err := cfg.Validate(); err == nil {
		t.Fatal("llm missing url should error")
	}
	cfg.LLMAPIURL = "http://x"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("complete llm should be valid: %v", err)
	}
}

func TestLoadScoringFromEnv(t *testing.T) {
	t.Setenv("SCORING_METHOD", "")
	t.Setenv("RERANK_USE_REMOTE", "")
	t.Setenv("RERANK_API_URL", "http://r")
	t.Setenv("RERANK_API_KEY", "rk")
	t.Setenv("RERANK_MODEL", "rm")
	t.Setenv("LLM_API_URL", "http://l")
	t.Setenv("LLM_API_KEY", "lk")
	t.Setenv("LLM_MODEL", "lm")

	// 默认 local
	cfg := LoadScoringFromEnv()
	if cfg.Method != MethodLocal {
		t.Fatalf("default method=%v want local", cfg.Method)
	}
	// RERANK_USE_REMOTE=true -> remote_reranker
	t.Setenv("RERANK_USE_REMOTE", "true")
	cfg = LoadScoringFromEnv()
	if cfg.Method != MethodRemoteReranker {
		t.Fatalf("RERANK_USE_REMOTE=true method=%v want remote_reranker", cfg.Method)
	}
	if cfg.RerankAPIKey != "rk" {
		t.Fatalf("rerank key not read: %q", cfg.RerankAPIKey)
	}
	// SCORING_METHOD 优先
	t.Setenv("SCORING_METHOD", "llm")
	cfg = LoadScoringFromEnv()
	if cfg.Method != MethodLLM {
		t.Fatalf("SCORING_METHOD=llm method=%v want llm", cfg.Method)
	}
	if cfg.LLMAPIPKey != "lk" || cfg.LLMAPIURL != "http://l" || cfg.LLMModel != "lm" {
		t.Fatalf("llm env not read: %+v", cfg)
	}
	// 非法 SCORING_METHOD -> 回退 RERANK_USE_REMOTE
	t.Setenv("SCORING_METHOD", "garbage")
	cfg = LoadScoringFromEnv()
	if cfg.Method != MethodRemoteReranker {
		t.Fatalf("invalid SCORING_METHOD should fall back to RERANK_USE_REMOTE, got %v", cfg.Method)
	}
}

func TestNormalizePasswordInput(t *testing.T) {
	if _, err := NormalizePasswordInput("   "); err == nil {
		t.Fatal("blank password should error")
	}
	if _, err := NormalizePasswordInput("12345"); err == nil {
		t.Fatal("short password should error")
	}
	p, err := NormalizePasswordInput("  secret1  ")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if p != "secret1" {
		t.Fatalf("normalized=%q want secret1", p)
	}
}

func TestHashPasswordRoundtrip(t *testing.T) {
	h, err := HashPassword("my-password-123")
	if err != nil {
		t.Fatalf("hash err: %v", err)
	}
	if !strings.HasPrefix(h, "$2a$") && !strings.HasPrefix(h, "$2b$") && !strings.HasPrefix(h, "$2y$") {
		t.Fatalf("hash not bcrypt: %q", h)
	}
}
