package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yhwyxy/examSystem/internal/config"
)

// helper: 用默认 Dependencies 与最小 Config 构造 router
func newTestRouter(t *testing.T, deps Dependencies) http.Handler {
	t.Helper()
	if deps.Config == nil {
		deps.Config = &config.Config{
			Grading: config.GradingConfig{SyncGrading: false},
		}
		deps.Config.Server.Host = "127.0.0.1"
		deps.Config.Server.Port = 8000
	}
	return NewRouter(deps)
}

func TestHealth_OK(t *testing.T) {
	r := newTestRouter(t, Dependencies{})
	req := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("/api/health = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, `"ok":true`) {
		t.Errorf("health body unexpected: %s", body)
	}
	if !strings.Contains(body, `"time"`) {
		t.Errorf("health body missing time: %s", body)
	}
	if !strings.Contains(body, `"version"`) {
		t.Errorf("health body missing version: %s", body)
	}
}

func TestAPI_UnknownReturnsJSON404(t *testing.T) {
	r := newTestRouter(t, Dependencies{})
	req := httptest.NewRequest(http.MethodGet, "/api/no-such-path", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), `"error"`) {
		t.Errorf("404 body should be JSON {error}, got %s", rr.Body.String())
	}
	// 不回退到 exam.html
	if strings.Contains(rr.Body.String(), "<html") {
		t.Errorf("404 should NOT fallback to html, got %s", rr.Body.String())
	}
}

// 静态未知路径不回退到 exam.html: Task 1 安全要求
func TestStatic_Unknown_Returns404NotExamHTML(t *testing.T) {
	r := newTestRouter(t, Dependencies{
		StaticRoot: t.TempDir(), // 空目录, 无 exam.html
	})
	req := httptest.NewRequest(http.MethodGet, "/no-such-static", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Errorf("got %d, want 404", rr.Code)
	}
}

// CORS 中间件: 允许 origin 在白名单内 (白名单为空时通配)
func TestCORS_AllowAnyWhenEmpty(t *testing.T) {
	r := newTestRouter(t, Dependencies{
		Config: &config.Config{},
	})
	req := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	req.Header.Set("Origin", "https://example.com")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "https://example.com" {
		t.Errorf("CORS origin got %q, want https://example.com", got)
	}
	if got := rr.Header().Get("Access-Control-Allow-Headers"); !strings.Contains(got, "Authorization") {
		t.Errorf("missing Authorization in CORS headers: %s", got)
	}
	if got := rr.Header().Get("Access-Control-Allow-Methods"); !strings.Contains(got, "GET") {
		t.Errorf("missing GET in CORS methods: %s", got)
	}
	if got := rr.Header().Get("Access-Control-Allow-Credentials"); got != "false" {
		t.Errorf("credentials must be false, got %q", got)
	}
}

// CORS preflight OPTIONS -> 204
func TestCORS_PreflightReturns204(t *testing.T) {
	r := newTestRouter(t, Dependencies{Config: &config.Config{}})
	req := httptest.NewRequest(http.MethodOptions, "/api/health", nil)
	req.Header.Set("Origin", "https://example.com")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Errorf("preflight OPTIONS got %d, want 204", rr.Code)
	}
}

// reload-config: nil 回调 -> 503 (service unavailable)
func TestReloadConfig_NoHandler(t *testing.T) {
	r := newTestRouter(t, Dependencies{})
	req := httptest.NewRequest(http.MethodPost, "/api/reload-config", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("reload-config nil got %d, want 503", rr.Code)
	}
}

// reload-config: 回调返回 ok=true -> 200 + success=true
func TestReloadConfig_Ok(t *testing.T) {
	r := newTestRouter(t, Dependencies{
		ReloadConfig: func() (bool, string) {
			return true, "config reloaded; CORS change requires restart"
		},
	})
	req := httptest.NewRequest(http.MethodPost, "/api/reload-config", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, `"success":true`) {
		t.Errorf("body success not true: %s", body)
	}
	if !strings.Contains(body, "CORS change requires restart") {
		t.Errorf("body should include CORS hint message: %s", body)
	}
}

// reload-config: 回调返回 ok=false -> 409
func TestReloadConfig_Failed(t *testing.T) {
	r := newTestRouter(t, Dependencies{
		ReloadConfig: func() (bool, string) {
			return false, "validation failed"
		},
	})
	req := httptest.NewRequest(http.MethodPost, "/api/reload-config", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusConflict {
		t.Fatalf("got %d, want 409", rr.Code)
	}
}
