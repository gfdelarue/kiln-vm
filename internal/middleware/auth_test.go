package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gfdelarue/kiln-vm/internal/auth"
	"github.com/gfdelarue/kiln-vm/internal/store"
)

func setupAuthTest(t *testing.T) (*auth.Service, string) {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(dir)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	svc := auth.NewService(db)
	resp, err := svc.CreateToken("test-token", nil)
	if err != nil {
		t.Fatalf("create token: %v", err)
	}

	return svc, resp.PlainToken
}

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
}

func TestBearerAuth_MissingHeader(t *testing.T) {
	svc, _ := setupAuthTest(t)
	handler := BearerAuth(svc)(okHandler())

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", w.Code, http.StatusUnauthorized)
	}
}

func TestBearerAuth_BadFormat(t *testing.T) {
	svc, _ := setupAuthTest(t)
	handler := BearerAuth(svc)(okHandler())

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Basic abc123")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", w.Code, http.StatusUnauthorized)
	}
}

func TestBearerAuth_InvalidToken(t *testing.T) {
	svc, _ := setupAuthTest(t)
	handler := BearerAuth(svc)(okHandler())

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer invalid-token-value")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", w.Code, http.StatusUnauthorized)
	}
}

func TestBearerAuth_ExpiredToken(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(dir)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	svc := auth.NewService(db)
	expiresIn := -1 // already expired
	resp, err := svc.CreateToken("expired-token", &expiresIn)
	if err != nil {
		t.Fatalf("create token: %v", err)
	}

	handler := BearerAuth(svc)(okHandler())

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+resp.PlainToken)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", w.Code, http.StatusUnauthorized)
	}
}

func TestBearerAuth_ValidToken(t *testing.T) {
	svc, token := setupAuthTest(t)

	var reachedHandler bool
	handler := BearerAuth(svc)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reachedHandler = true
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}
	if !reachedHandler {
		t.Error("expected authenticated request to reach the handler")
	}
}
