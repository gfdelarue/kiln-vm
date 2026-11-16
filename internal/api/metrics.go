package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/gfdelarue/kiln-vm/internal/metrics"
)

type MetricsHandler struct {
	service *metrics.Service
}

func NewMetricsHandler(service *metrics.Service) *MetricsHandler {
	return &MetricsHandler{service: service}
}

func (h *MetricsHandler) Summary(w http.ResponseWriter, r *http.Request) {
	summary, err := h.service.Summary(r.Context())
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, summary)
}

func (h *MetricsHandler) NodeCapacity(w http.ResponseWriter, r *http.Request) {
	capacity, err := h.service.NodeCapacity(r.Context())
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, capacity)
}

func (h *MetricsHandler) NodeHealth(w http.ResponseWriter, r *http.Request) {
	health, err := h.service.NodeHealth(r.Context())
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, health)
}

func (h *MetricsHandler) VM(w http.ResponseWriter, r *http.Request) {
	metrics, err := h.service.VMMetrics(r.Context(), chi.URLParam(r, "ref"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, metrics)
}
