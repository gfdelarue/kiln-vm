package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

const tokenBytes = 32

// GenerateToken creates a new random token and returns
// the plaintext token and its SHA-256 hash.
func GenerateToken() (plain string, hash string, err error) {
	b := make([]byte, tokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", "", fmt.Errorf("generate random bytes: %w", err)
	}
	plain = "kiln_vm_" + hex.EncodeToString(b)
	hash = HashToken(plain)
	return plain, hash, nil
}

// HashToken returns the SHA-256 hex digest of a plaintext token.
func HashToken(plain string) string {
	h := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(h[:])
}
