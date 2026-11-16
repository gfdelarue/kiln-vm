package store

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/gfdelarue/kiln-vm/internal/models"
)

func (db *DB) CreateToken(t *models.Token) error {
	_, err := db.Exec(`
		INSERT INTO tokens (id, name, hash, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?)`,
		t.ID, t.Name, t.Hash, t.CreatedAt, t.ExpiresAt,
	)
	if err != nil {
		return fmt.Errorf("insert token: %w", err)
	}
	return nil
}

func (db *DB) GetTokenByHash(hash string) (*models.Token, error) {
	t := &models.Token{}
	var expiresAt sql.NullTime
	err := db.QueryRow(`
		SELECT id, name, hash, created_at, expires_at
		FROM tokens WHERE hash = ?`, hash,
	).Scan(&t.ID, &t.Name, &t.Hash, &t.CreatedAt, &expiresAt)
	if err != nil {
		return nil, fmt.Errorf("get token by hash: %w", err)
	}
	if expiresAt.Valid {
		t.ExpiresAt = &expiresAt.Time
	}
	return t, nil
}

func (db *DB) ListTokens() ([]models.Token, error) {
	rows, err := db.Query(`
		SELECT id, name, hash, created_at, expires_at
		FROM tokens ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("list tokens: %w", err)
	}
	defer rows.Close()

	var tokens []models.Token
	for rows.Next() {
		var t models.Token
		var expiresAt sql.NullTime
		if err := rows.Scan(&t.ID, &t.Name, &t.Hash, &t.CreatedAt, &expiresAt); err != nil {
			return nil, fmt.Errorf("scan token: %w", err)
		}
		if expiresAt.Valid {
			t.ExpiresAt = &expiresAt.Time
		}
		tokens = append(tokens, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate tokens: %w", err)
	}
	return tokens, nil
}

func (db *DB) DeleteToken(id string) error {
	_, err := db.Exec(`DELETE FROM tokens WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete token: %w", err)
	}
	return nil
}

func (db *DB) TokenCount() (int, error) {
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM tokens`).Scan(&count); err != nil {
		return 0, fmt.Errorf("count tokens: %w", err)
	}
	return count, nil
}

func (db *DB) DeleteExpiredTokens() error {
	_, err := db.Exec(`DELETE FROM tokens WHERE expires_at IS NOT NULL AND expires_at < ?`, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("delete expired tokens: %w", err)
	}
	return nil
}
