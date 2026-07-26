package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestLogin_DisabledReturnsErrAuthDisabled(t *testing.T) {
	s := NewStore(false, func() (string, error) { return "secret", nil })
	tok, err := s.Login("secret")
	if err != ErrAuthDisabled {
		t.Fatalf("err = %v, want ErrAuthDisabled", err)
	}
	if tok != "" {
		t.Fatalf("token should be empty when disabled")
	}
}

func TestLogin_CorrectPasswordReturnsToken(t *testing.T) {
	s := NewStore(true, func() (string, error) { return "correct-horse", nil })
	tok, err := s.Login("correct-horse")
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
	if _, err := s.Login("wrong"); err != ErrInvalidCredentials {
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

func TestRequireAdmin_EnabledMissingBearerReturns401(t *testing.T) {
	s := NewStore(true, func() (string, error) { return "x", nil })
	h := s.RequireAdmin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}
