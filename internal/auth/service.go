package auth

import (
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/gfdelarue/kiln-vm/internal/models"
	"github.com/gfdelarue/kiln-vm/internal/store"
)

type Service struct {
	db *store.DB
}

func NewService(db *store.DB) *Service {
	return &Service{db: db}
}

// Bootstrap creates an initial admin token if no tokens exist.
// Returns the plaintext token, or empty string if tokens already exist.
func (s *Service) Bootstrap() (string, error) {
	count, err := s.db.TokenCount()
	if err != nil {
		return "", fmt.Errorf("count tokens: %w", err)
	}
	if count > 0 {
		return "", nil
	}

	resp, err := s.CreateToken("admin-bootstrap", nil)
	if err != nil {
		return "", fmt.Errorf("create bootstrap token: %w", err)
	}

	slog.Info("bootstrap token created", "name", "admin-bootstrap")
	return resp.PlainToken, nil
}

func (s *Service) CreateToken(name string, expiresIn *int) (*models.CreateTokenResponse, error) {
	plain, hash, err := GenerateToken()
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	t := &models.Token{
		ID:        uuid.New().String(),
		Name:      name,
		Hash:      hash,
		CreatedAt: now,
	}

	if expiresIn != nil {
		exp := now.Add(time.Duration(*expiresIn) * time.Second)
		t.ExpiresAt = &exp
	}

	if err := s.db.CreateToken(t); err != nil {
		return nil, fmt.Errorf("create token: %w", err)
	}

	return &models.CreateTokenResponse{
		Token:      *t,
		PlainToken: plain,
	}, nil
}

// Validate checks a plaintext token against the database.
func (s *Service) Validate(plain string) (*models.Token, error) {
	hash := HashToken(plain)
	t, err := s.db.GetTokenByHash(hash)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("invalid token")
		}
		return nil, fmt.Errorf("get token by hash: %w", err)
	}

	if t.ExpiresAt != nil && t.ExpiresAt.Before(time.Now().UTC()) {
		return nil, fmt.Errorf("token expired")
	}

	return t, nil
}

func (s *Service) ListTokens() ([]models.Token, error) {
	tokens, err := s.db.ListTokens()
	if err != nil {
		return nil, fmt.Errorf("list tokens: %w", err)
	}
	return tokens, nil
}

func (s *Service) DeleteToken(id string) error {
	if err := s.db.DeleteToken(id); err != nil {
		return fmt.Errorf("delete token: %w", err)
	}
	return nil
}
