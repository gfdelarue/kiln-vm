package auth

import (
	"strings"
	"testing"
	"time"

	"github.com/gfdelarue/kiln-vm/internal/models"
	"github.com/gfdelarue/kiln-vm/internal/store"
)

func openTestDB(t *testing.T) *store.DB {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(dir)
	if err != nil {
		t.Fatalf("store.Open(%s): %v", dir, err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestBootstrapCreatesTokenWhenNoneExist(t *testing.T) {
	db := openTestDB(t)
	svc := NewService(db)

	token, err := svc.Bootstrap()
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	if token == "" {
		t.Fatal("expected a bootstrap token, got empty string")
	}

	if !strings.HasPrefix(token, "kiln_vm_") {
		t.Errorf("bootstrap token %q does not start with 'kiln_vm_'", token)
	}

	// Verify token count is now 1
	count, err := db.TokenCount()
	if err != nil {
		t.Fatalf("TokenCount: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 token, got %d", count)
	}
}

func TestBootstrapReturnsEmptyWhenTokensExist(t *testing.T) {
	db := openTestDB(t)
	svc := NewService(db)

	// Create a token first
	now := time.Now().UTC()
	tok := &models.Token{
		ID:        "existing-tok",
		Name:      "preexisting",
		Hash:      "somehash",
		CreatedAt: now,
	}
	if err := db.CreateToken(tok); err != nil {
		t.Fatalf("CreateToken: %v", err)
	}

	token, err := svc.Bootstrap()
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	if token != "" {
		t.Errorf("expected empty string when tokens exist, got %q", token)
	}
}

func TestValidateAcceptsValidToken(t *testing.T) {
	db := openTestDB(t)
	svc := NewService(db)

	// Create a token through the service
	resp, err := svc.CreateToken("test-token", nil)
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}

	// Validate the plaintext token
	tok, err := svc.Validate(resp.PlainToken)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}

	if tok.ID != resp.ID {
		t.Errorf("validated token ID = %q, want %q", tok.ID, resp.ID)
	}
	if tok.Name != "test-token" {
		t.Errorf("validated token Name = %q, want %q", tok.Name, "test-token")
	}
}

func TestValidateRejectsInvalidToken(t *testing.T) {
	db := openTestDB(t)
	svc := NewService(db)

	_, err := svc.Validate("kiln_vm_this_is_not_a_real_token")
	if err == nil {
		t.Fatal("expected error for invalid token, got nil")
	}
}

func TestValidateRejectsExpiredToken(t *testing.T) {
	db := openTestDB(t)
	svc := NewService(db)

	// Create a token that expires in -1 second (already expired)
	expiresIn := -1
	resp, err := svc.CreateToken("expired-token", &expiresIn)
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}

	_, err = svc.Validate(resp.PlainToken)
	if err == nil {
		t.Fatal("expected error for expired token, got nil")
	}
	if !strings.Contains(err.Error(), "expired") {
		t.Errorf("error should mention expiry, got: %v", err)
	}
}

func TestCreateTokenWithExpiry(t *testing.T) {
	db := openTestDB(t)
	svc := NewService(db)

	expiresIn := 3600 // 1 hour
	resp, err := svc.CreateToken("temp-token", &expiresIn)
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}

	if resp.PlainToken == "" {
		t.Error("PlainToken should not be empty")
	}
	if resp.ExpiresAt == nil {
		t.Fatal("ExpiresAt should be set for token with expiry")
	}
}

func TestCreateTokenWithoutExpiry(t *testing.T) {
	db := openTestDB(t)
	svc := NewService(db)

	resp, err := svc.CreateToken("permanent-token", nil)
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}

	if resp.PlainToken == "" {
		t.Error("PlainToken should not be empty")
	}
	if resp.ExpiresAt != nil {
		t.Error("ExpiresAt should be nil for token without expiry")
	}
}
