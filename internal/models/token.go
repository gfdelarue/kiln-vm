package models

import "time"

type Token struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Hash      string     `json:"-"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

type CreateTokenRequest struct {
	Name      string `json:"name"`
	ExpiresIn *int   `json:"expires_in,omitempty"` // seconds
}

type CreateTokenResponse struct {
	Token
	PlainToken string `json:"token"`
}
