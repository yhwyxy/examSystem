package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestLogin_DisabledReturnsErrAuthDisabled(t *testing.T) {
	s := NewStore(false, func() (string, error) { return "secret", nil })
	tok, err := s.Login(context.Background(), "secret")
	if err != ErrAuthDisabled {
		t.Fatalf("err = %v, want ErrAuthDisabled", err)
	}
	if tok != "" {
		t.Fatalf("token should be empty when disabled")
	}
}

func TestLogin_CorrectPasswordReturnsToken(t *testing.T) {
	s := NewStore(true, func() (string, error) { return "correct-horse", nil })
	tok, err := s.Login(context.Background(), "correct-horse")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if tok == "" {
		t.Fatalf("token empty")
	}
	if err := s.VerifyToken(tok); err != nil {
		t.Fatalf("verify failed: %v", err)
	}
}

func TestLogin_WrongPasswordReturnsInvalidCredentials(t *testing.T) {
	s := NewStore(true, func() (string, error) { return "correct-horse", nil })
	if _, err := s.Login(context.Background(), "wrong"); err != ErrInvalidCredentials {
		t.Fatalf("err = %v, want ErrInvalidCredentials", err)
	}
}

func TestVerifyToken_DisabledReturnsErrAuthDisabled(t *testing.T) {
	s := NewStore(false, nil)
	if err := s.VerifyToken("any"); err != ErrAuthDisabled {
		t.Fatalf("err = %v, want ErrAuthDisabled", err)
	}
}

func TestVerifyToken_UnknownTokenReturnsErrInvalidToken(t *testing.T) {
	s := NewStore(true, func() (string, error) { return "x", nil })
	if err := s.VerifyToken("unknown"); err != ErrInvalidToken {
		t.Fatalf("err = %v, want ErrInvalidToken", err)
	}
}

func TestVerifyToken_EmptyTokenReturnsErrInvalidToken(t *testing.T) {
	s := NewStore(true, func() (string, error) { return "x", nil })
	if err := s.VerifyToken(""); err != ErrInvalidToken {
		t.Fatalf("err = %v, want ErrInvalidToken", err)
	}
}

func TestRequireAdmin_DisabledStorePassesRequest(t *testing.T) {
	s := NewStore(false, nil)
	called := false
	h := s.RequireAdmin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if !called {
		t.Fatalf("handler not called when auth disabled")
	}
}

func TestRequireAdminProvidesAuthenticatedUsernameForAudit(t *testing.T) {
	s := NewStore(true, func() (string, error) { return "secret", nil })
	token, err := s.Login(context.Background(), "secret")
	if err != nil {
		t.Fatal(err)
	}
	var username string
	h := s.RequireAdmin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		username = UsernameFromContext(r.Context())
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp := httptest.NewRecorder()
	h.ServeHTTP(resp, req)
	if resp.Code != http.StatusNoContent || username != "admin" {
		t.Fatalf("status=%d username=%q", resp.Code, username)
	}
}

func TestRequireAdmin_EnabledMissingBearerReturns401(t *testing.T) {
	s := NewStore(true, func() (string, error) { return "x", nil })
	h := s.RequireAdmin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestLoginUserRecordsRoleAndRequireRoleRejectsOtherRoles(t *testing.T) {
	s := NewStore(true, nil)
	s.SetPasswordProvider(PasswordProvider{Authenticate: func(_ context.Context, username, password string) (string, error) {
		if username != "reviewer01" || password != "review-password" {
			return "", ErrInvalidCredentials
		}
		return RoleReviewer, nil
	}})
	token, role, err := s.LoginUser(context.Background(), "reviewer01", "review-password")
	if err != nil || role != RoleReviewer {
		t.Fatalf("login role=%q err=%v", role, err)
	}

	called := false
	h := s.RequireRole(RolePublisher)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		if got := RoleFromContext(r.Context()); got != RoleReviewer {
			t.Errorf("context role=%q, want reviewer", got)
		}
	}))
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	h.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden || called {
		t.Fatalf("status=%d called=%v, want forbidden", response.Code, called)
	}

	allowed := s.RequireRole(RoleReviewer)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	}))
	response = httptest.NewRecorder()
	allowed.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent || !called {
		t.Fatalf("allowed status=%d called=%v", response.Code, called)
	}
	s.InvalidateUser("reviewer01")
	if err := s.VerifyToken(token); err != ErrInvalidToken {
		t.Fatalf("invalidated user token err=%v, want ErrInvalidToken", err)
	}
}

// inmemProvider 用内存 map 模拟 PasswordProvider (DB bcrypt 存储测试).
type inmemProvider struct {
	mu       map[string]string
	setCalls int
}

func (p *inmemProvider) Check(ctx context.Context, pw string) (bool, error) {
	want, ok := p.mu["pw"]
	return ok && want == pw, nil
}
func (p *inmemProvider) Set(ctx context.Context, newPw string) error {
	p.mu["pw"] = newPw
	p.setCalls++
	return nil
}

func TestChangePassword_SuccessInvalidatesTokens(t *testing.T) {
	p := &inmemProvider{mu: map[string]string{"pw": "oldpass"}}
	s := NewStore(true, nil)
	s.SetPasswordProvider(PasswordProvider{Check: p.Check, Set: p.Set})
	tok, err := s.Login(context.Background(), "oldpass")
	if err != nil {
		t.Fatalf("login err: %v", err)
	}
	if err := s.ChangePassword(context.Background(), "oldpass", "newpass123"); err != nil {
		t.Fatalf("change err: %v", err)
	}
	if p.setCalls != 1 {
		t.Fatalf("setCalls=%d want 1", p.setCalls)
	}
	// 旧 token 已失效, 需重新登录
	if err := s.VerifyToken(tok); err == nil {
		t.Fatal("old token should be invalidated after password change")
	}
	if _, err := s.Login(context.Background(), "oldpass"); err != ErrInvalidCredentials {
		t.Fatalf("old password should no longer work, got %v", err)
	}
	if tok2, err := s.Login(context.Background(), "newpass123"); err != nil {
		t.Fatalf("new password login err: %v", err)
	} else if tok2 == "" {
		t.Fatal("new token empty")
	}
}

func TestChangePassword_WrongOldPassword(t *testing.T) {
	p := &inmemProvider{mu: map[string]string{"pw": "oldpass"}}
	s := NewStore(true, nil)
	s.SetPasswordProvider(PasswordProvider{Check: p.Check, Set: p.Set})
	err := s.ChangePassword(context.Background(), "wrong", "newpass123")
	if err != ErrInvalidCredentials {
		t.Fatalf("err = %v, want ErrInvalidCredentials", err)
	}
	if p.setCalls != 0 {
		t.Fatal("Set should not be called on wrong old password")
	}
}

func TestChangePassword_WeakPassword(t *testing.T) {
	p := &inmemProvider{mu: map[string]string{"pw": "oldpass"}}
	s := NewStore(true, nil)
	s.SetPasswordProvider(PasswordProvider{Check: p.Check, Set: p.Set})
	if err := s.ChangePassword(context.Background(), "oldpass", "123"); err != ErrWeakPassword {
		t.Fatalf("short password err = %v, want ErrWeakPassword", err)
	}
	// 新旧相同 -> ErrWeakPassword
	if err := s.ChangePassword(context.Background(), "oldpass", "oldpass"); err != ErrWeakPassword {
		t.Fatalf("same password err = %v, want ErrWeakPassword", err)
	}
}

func TestChangePassword_NoProvider(t *testing.T) {
	s := NewStore(true, func() (string, error) { return "configpass", nil })
	err := s.ChangePassword(context.Background(), "configpass", "newpass123")
	if err != ErrPasswordStoreNotConfigured {
		t.Fatalf("err = %v, want ErrPasswordStoreNotConfigured", err)
	}
}

func TestLogin_FallsBackToSecretFn(t *testing.T) {
	// 未注入 provider 时 Login 走 secretFn (config 明文).
	s := NewStore(true, func() (string, error) { return "configpass", nil })
	if _, err := s.Login(context.Background(), "configpass"); err != nil {
		t.Fatalf("login via secretFn err: %v", err)
	}
	if _, err := s.Login(context.Background(), "wrong"); err != ErrInvalidCredentials {
		t.Fatalf("wrong via secretFn err = %v, want ErrInvalidCredentials", err)
	}
}
