package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/gfdelarue/kiln-vm/internal/auth"
	"github.com/gfdelarue/kiln-vm/internal/models"
)

type AuthHandler struct {
	authService *auth.Service
}

func NewAuthHandler(authService *auth.Service) *AuthHandler {
	return &AuthHandler{authService: authService}
}

func (h *AuthHandler) CreateToken(w http.ResponseWriter, r *http.Request) {
	var req models.CreateTokenRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}

	resp, err := h.authService.CreateToken(req.Name, req.ExpiresIn)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create token")
		return
	}

	writeJSON(w, http.StatusCreated, resp)
}

func (h *AuthHandler) ListTokens(w http.ResponseWriter, r *http.Request) {
	tokens, err := h.authService.ListTokens()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list tokens")
		return
	}
	if tokens == nil {
		tokens = []models.Token{}
	}
	writeJSON(w, http.StatusOK, tokens)
}

func (h *AuthHandler) DeleteToken(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "token id is required")
		return
	}

	if err := h.authService.DeleteToken(id); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to delete token")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
