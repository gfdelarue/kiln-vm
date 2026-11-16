package api

import (
	"encoding/json"
	"net/http"

	"github.com/gfdelarue/kiln-vm/internal/disk"
	"github.com/gfdelarue/kiln-vm/internal/models"
)

type DiskHandler struct {
	service *disk.Service
}

func NewDiskHandler(service *disk.Service) *DiskHandler {
	return &DiskHandler{service: service}
}

func (h *DiskHandler) Usage(w http.ResponseWriter, r *http.Request) {
	report, err := h.service.Usage()
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, report)
}

func (h *DiskHandler) Cleanup(w http.ResponseWriter, r *http.Request) {
	var req models.CleanupRequest
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON")
			return
		}
	}
	report, err := h.service.Cleanup(req.DryRun)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, report)
}
