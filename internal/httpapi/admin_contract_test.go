package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yhwyxy/examSystem/internal/auth"
	"github.com/yhwyxy/examSystem/internal/config"
)

// newAuthRouter 工具: 指定 enable_auth 构造 router.
func newAuthRouter(t *testing.T, enable bool, password string) http.Handler {
	t.Helper()
	secret := password
	deps := Dependencies{
		Config: &config.Config{
			Grading: config.GradingConfig{SyncGrading: false},
		},
		Auth: auth.NewStore(enable, func() (string, error) { return secret, nil }),
	}
	deps.Config.Server.Host = "127.0.0.1"
	deps.Config.Server.Port = 8000
	return NewRouter(deps)
}

// loginGetToken 拿 enable=true + 正确密码 -> token; 失败 t.Fatalf.
func loginGetToken(t *testing.T, r http.Handler, password string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/admin/login", strings.NewReader(`{"password":"`+password+`"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("login status=%d want 200; body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode login resp: %v", err)
	}
	if resp.Token == "" {
		t.Fatal("login returned empty token")
	}
	return resp.Token
}

// TestLogin_Success: enable_auth=true 下正确 password 返回 200 + 非空 token.
func TestLogin_Success(t *testing.T) {
	r := newAuthRouter(t, true, "secret-pw")
	tok := loginGetToken(t, r, "secret-pw")
	if tok == "" {
		t.Fatal("token should be non-empty")
	}
}

// TestLogin_WrongPassword: 错 password 返 401.
func TestLogin_WrongPassword(t *testing.T) {
	r := newAuthRouter(t, true, "secret-pw")
	req := httptest.NewRequest(http.MethodPost, "/api/admin/login", strings.NewReader(`{"password":"wrong"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d want 401; body=%s", rr.Code, rr.Body.String())
	}
}

// TestRequireAdmin_DisabledAllowsAll: enable_auth=false 下访问 admin 路由
// 不应 401 (deps.Papers=nil -> 503 PAPERS_NOT_CONFIGURED, 但 auth 一定放过).
func TestRequireAdmin_DisabledAllowsAll(t *testing.T) {
	r := newAuthRouter(t, false, "")
	req := httptest.NewRequest(http.MethodGet, "/api/admin/exams", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code == http.StatusUnauthorized {
		t.Fatalf("enabled=false 下不应 401, got %d; body=%s", rr.Code, rr.Body.String())
	}
}

// TestRequireAdmin_EnabledMissingBearerReturns401: enable_auth=true 无 Bearer -> 401.
func TestRequireAdmin_EnabledMissingBearerReturns401(t *testing.T) {
	r := newAuthRouter(t, true, "secret-pw")
	req := httptest.NewRequest(http.MethodGet, "/api/admin/exams", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d want 401; body=%s", rr.Code, rr.Body.String())
	}
}

// TestRequireAdmin_EnabledWithValidTokenPasses: 持 token 访问 admin 路由
// 不应 401 (deps.Papers=nil -> 503, 但 auth 一定通过).
func TestRequireAdmin_EnabledWithValidTokenPasses(t *testing.T) {
	r := newAuthRouter(t, true, "secret-pw")
	tok := loginGetToken(t, r, "secret-pw")
	req := httptest.NewRequest(http.MethodGet, "/api/admin/exams", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code == http.StatusUnauthorized {
		t.Fatalf("持有效 token 不应 401, got %d; body=%s", rr.Code, rr.Body.String())
	}
}

// TestRequireAdmin_EnabledWithInvalidTokenReturns401: 假/空 token -> 401.
func TestRequireAdmin_EnabledWithInvalidTokenReturns401(t *testing.T) {
	r := newAuthRouter(t, true, "secret-pw")
	req := httptest.NewRequest(http.MethodGet, "/api/admin/exams", nil)
	req.Header.Set("Authorization", "Bearer not-a-real-token")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d want 401; body=%s", rr.Code, rr.Body.String())
	}
}
