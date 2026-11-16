package auth

import (
	"strings"
	"testing"
)

func TestGenerateTokenReturnsValidPrefixAndHash(t *testing.T) {
	plain, hash, err := GenerateToken()
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}

	if !strings.HasPrefix(plain, "kiln_vm_") {
		t.Errorf("plain token %q does not start with 'kiln_vm_'", plain)
	}

	if want := len("kiln_vm_") + 64; len(plain) != want {
		t.Errorf("plain token length = %d, want %d", len(plain), want)
	}

	if hash == "" {
		t.Error("hash should not be empty")
	}

	// SHA-256 hex digest is 64 chars
	if len(hash) != 64 {
		t.Errorf("hash length = %d, want 64", len(hash))
	}
}

func TestHashTokenIsDeterministic(t *testing.T) {
	input := "kiln_vm_abcdef1234567890"
	h1 := HashToken(input)
	h2 := HashToken(input)

	if h1 != h2 {
		t.Errorf("HashToken is not deterministic: %q != %q", h1, h2)
	}
}

func TestDifferentTokensProduceDifferentHashes(t *testing.T) {
	plain1, hash1, err := GenerateToken()
	if err != nil {
		t.Fatalf("GenerateToken (1): %v", err)
	}

	plain2, hash2, err := GenerateToken()
	if err != nil {
		t.Fatalf("GenerateToken (2): %v", err)
	}

	if plain1 == plain2 {
		t.Error("two generated tokens should not be identical")
	}

	if hash1 == hash2 {
		t.Error("hashes of two different tokens should differ")
	}
}

func TestHashTokenMatchesGenerateToken(t *testing.T) {
	plain, hash, err := GenerateToken()
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}

	recomputed := HashToken(plain)
	if recomputed != hash {
		t.Errorf("HashToken(plain) = %q, but GenerateToken returned hash %q", recomputed, hash)
	}
}
