package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/yhwyxy/examSystem/internal/auth"
	"github.com/yhwyxy/examSystem/internal/config"
	"github.com/yhwyxy/examSystem/internal/settings"
)

func verifyBcrypt(hash, password string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

// newSettingsRouter 构造带 Auth + Settings 的路由 (同 main.go 接线).
// admin password: 初始 config 明文 "config-pass"; 首改后走 DB bcrypt hash.
func newSettingsRouter(t *testing.T, pool *pgxpool.Pool) http.Handler {
	t.Helper()
	cfg := &config.Config{Admin: config.AdminConfig{EnableAuth: true}}
	pw := "config-pass"
	cfg.Admin.Password = &pw
	authStore := auth.NewStore(true, func() (string, error) { return pw, nil })
	ss := settings.NewStore(pool)
	authStore.SetPasswordProvider(auth.PasswordProvider{
		Check: func(ctx context.Context, password string) (bool, error) {
			doc, err := ss.LoadPasswordHash(ctx)
			if err != nil {
				return false, err
			}
			if doc != nil && doc.Hash != "" {
				return verifyBcrypt(doc.Hash, password), nil
			}
			return authStore.VerifyLegacyPassword(password)
		},
		Set: func(ctx context.Context, newPassword string) error {
			return ss.SavePasswordHash(ctx, newPassword)
		},
	})
	deps := Dependencies{
		Config:   cfg,
		Auth:     authStore,
		Settings: ss,
	}
	r := NewRouter(deps)
	return r
}

func TestSettings_RequireAuth(t *testing.T) {
	pool := testSubmPool(t)
	r := newSettingsRouter(t, pool)
	// 无 token -> 401
	req := httptest.NewRequest(http.MethodGet, "/api/admin/settings", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d want 401", rr.Code)
	}
}

func TestSettings_GetDefaultScoring(t *testing.T) {
	pool := testSubmPool(t)
	r := newSettingsRouter(t, pool)
	token := loginGetToken(t, r, "config-pass")

	req := httptest.NewRequest(http.MethodGet, "/api/admin/settings", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		PasswordConfigured bool `json:"password_configured"`
		Scoring            struct {
			Method string `json:"method"`
		} `json:"scoring"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.PasswordConfigured {
		t.Fatal("password_configured should be true (config plaintext present)")
	}
	if resp.Scoring.Method != "local" {
		t.Fatalf("default scoring method=%q want local", resp.Scoring.Method)
	}
}

func TestSettings_SaveScoring_Invalid(t *testing.T) {
	pool := testSubmPool(t)
	r := newSettingsRouter(t, pool)
	token := loginGetToken(t, r, "config-pass")

	// llm 缺 key -> 400
	body := `{"method":"llm","llm_api_url":"http://x","llm_api_key":"","llm_model":"m"}`
	req := httptest.NewRequest(http.MethodPost, "/api/admin/settings/scoring", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	// 未知 method -> 400
	req = httptest.NewRequest(http.MethodPost, "/api/admin/settings/scoring",
		strings.NewReader(`{"method":"bogus"}`))
	req.Header.Set("Authorization", "Bearer "+token)
	rr = httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestSettings_SaveScoring_Roundtrip(t *testing.T) {
	pool := testSubmPool(t)
	ctx := context.Background()
	_, _ = pool.Exec(ctx, `DELETE FROM app_settings WHERE key = 'scoring'`)
	defer pool.Exec(ctx, `DELETE FROM app_settings WHERE key = 'scoring'`)

	r := newSettingsRouter(t, pool)
	token := loginGetToken(t, r, "config-pass")

	body := `{"method":"llm","llm_api_url":"https://llm.example/v1","llm_api_key":"sk-secret-key-xyz","llm_model":"gpt-x"}`
	req := httptest.NewRequest(http.MethodPost, "/api/admin/settings/scoring", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}

	// GET 回显: method=llm, api key 打码 (非明文), url 完整保留.
	req = httptest.NewRequest(http.MethodGet, "/api/admin/settings", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr = httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Scoring struct {
			Method        string `json:"method"`
			LLMAPIURL     string `json:"llm_api_url"`
			LLMAPIPKey    string `json:"llm_api_key_masked"`
			LLMAPIPKeySet bool   `json:"llm_api_key_set"`
			LLMModel      string `json:"llm_model"`
		} `json:"scoring"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Scoring.Method != "llm" {
		t.Fatalf("method=%q want llm", resp.Scoring.Method)
	}
	if resp.Scoring.LLMAPIURL != "https://llm.example/v1" {
		t.Fatalf("llm url=%q", resp.Scoring.LLMAPIURL)
	}
	if !resp.Scoring.LLMAPIPKeySet {
		t.Fatal("llm key should be set")
	}
	if strings.Contains(resp.Scoring.LLMAPIPKey, "sk-secret-key-xyz") {
		t.Fatalf("masked key leaked plaintext: %q", resp.Scoring.LLMAPIPKey)
	}
	if resp.Scoring.LLMAPIPKey != "sk****yz" {
		t.Fatalf("masked key=%q want sk****yz", resp.Scoring.LLMAPIPKey)
	}
	if resp.Scoring.LLMModel != "gpt-x" {
		t.Fatalf("llm model=%q", resp.Scoring.LLMModel)
	}
}

func TestSettings_ChangePassword_Flow(t *testing.T) {
	pool := testSubmPool(t)
	ctx := context.Background()
	_, _ = pool.Exec(ctx, `DELETE FROM app_settings WHERE key = 'admin.password'`)
	defer pool.Exec(ctx, `DELETE FROM app_settings WHERE key = 'admin.password'`)

	r := newSettingsRouter(t, pool)
	token := loginGetToken(t, r, "config-pass")

	// 错旧密码 -> 401
	req := httptest.NewRequest(http.MethodPost, "/api/admin/settings/password",
		strings.NewReader(`{"old_password":"wrong","new_password":"new-pass-123"}`))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}

	// 弱密码 -> 400
	req = httptest.NewRequest(http.MethodPost, "/api/admin/settings/password",
		strings.NewReader(`{"old_password":"config-pass","new_password":"123"}`))
	req.Header.Set("Authorization", "Bearer "+token)
	rr = httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}

	// 正确改密 -> 200; 旧 token 失效
	req = httptest.NewRequest(http.MethodPost, "/api/admin/settings/password",
		strings.NewReader(`{"old_password":"config-pass","new_password":"new-pass-123"}`))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	// 旧 token 已失效
	req = httptest.NewRequest(http.MethodGet, "/api/admin/settings", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr = httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("old token status=%d want 401", rr.Code)
	}
	// 新密码可登录, 旧密码不行
	loginGetToken(t, r, "new-pass-123")
	req = httptest.NewRequest(http.MethodPost, "/api/admin/login", strings.NewReader(`{"password":"config-pass"}`))
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("old password login status=%d want 401", rr.Code)
	}
}

func TestSettings_ChangePassword_NoConfigPassword(t *testing.T) {
	// enable_auth=true 但 config 无密码 + DB 无 hash -> 登录/改密都不可行.
	pool := testSubmPool(t)
	ctx := context.Background()
	_, _ = pool.Exec(ctx, `DELETE FROM app_settings WHERE key = 'admin.password'`)
	defer pool.Exec(ctx, `DELETE FROM app_settings WHERE key = 'admin.password'`)

	cfg := &config.Config{Admin: config.AdminConfig{EnableAuth: true}}
	authStore := auth.NewStore(true, func() (string, error) { return "", auth.ErrInvalidCredentials })
	ss := settings.NewStore(pool)
	authStore.SetPasswordProvider(auth.PasswordProvider{
		Check: func(ctx context.Context, password string) (bool, error) {
			doc, err := ss.LoadPasswordHash(ctx)
			if err != nil {
				return false, err
			}
			if doc != nil && doc.Hash != "" {
				return verifyBcrypt(doc.Hash, password), nil
			}
			return false, nil // 无 config 明文, 拒绝
		},
		Set: func(ctx context.Context, newPassword string) error {
			return ss.SavePasswordHash(ctx, newPassword)
		},
	})
	r := NewRouter(Dependencies{Config: cfg, Auth: authStore, Settings: ss})

	// 登录失败
	req := httptest.NewRequest(http.MethodPost, "/api/admin/login", strings.NewReader(`{"password":"anything"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("login status=%d want 401", rr.Code)
	}
}
