package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/gfdelarue/kiln-vm/internal/models"
	vmservice "github.com/gfdelarue/kiln-vm/internal/vm"
)

const maxVMImageRequestBytes = 128 << 20

func (h *VMHandler) CreateImage(w http.ResponseWriter, r *http.Request) {
	var req models.CreateVMImageRequest
	if err := decodeJSONLimit(r, &req, maxVMImageRequestBytes); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	image, err := h.service.CreateImage(r.Context(), &req)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, image)
}

func (h *VMHandler) ListImages(w http.ResponseWriter, r *http.Request) {
	selectors, err := vmservice.ParseLabelSelectors(r.URL.Query()["label"])
	if err != nil {
		writeServiceError(w, err)
		return
	}
	images, err := h.service.ListImages(selectors...)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, images)
}

func (h *VMHandler) GetImage(w http.ResponseWriter, r *http.Request) {
	image, err := h.service.GetImage(chi.URLParam(r, "ref"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, image)
}

func (h *VMHandler) DeleteImage(w http.ResponseWriter, r *http.Request) {
	if err := h.service.DeleteImage(chi.URLParam(r, "ref")); err != nil {
		writeServiceError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
