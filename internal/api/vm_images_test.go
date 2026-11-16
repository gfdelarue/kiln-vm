package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/gfdelarue/kiln-vm/internal/models"
	vmservice "github.com/gfdelarue/kiln-vm/internal/vm"
)

func setupVMImageRouter(service *vmservice.Service) chi.Router {
	h := NewVMHandler(service)
	r := chi.NewRouter()
	r.Post("/vms", h.Create)
	r.Get("/vms", h.List)
	r.Post("/vm-images", h.CreateImage)
	r.Get("/vm-images", h.ListImages)
	r.Get("/vm-images/{ref}", h.GetImage)
	r.Delete("/vm-images/{ref}", h.DeleteImage)
	return r
}

func serveVMImageRequest(t *testing.T, router http.Handler, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w
}

func TestVMImageEndpointsMapErrorsToStatus(t *testing.T) {
	svc, machines := setupVMUploadTestService(t)
	machines.machine.Backend = "firecracker"
	router := setupVMImageRouter(svc)

	tests := []struct {
		name   string
		method string
		target string
		body   string
		status int
	}{
		{"malformed body", http.MethodPost, "/vm-images", `{`, http.StatusBadRequest},
		{"missing source", http.MethodPost, "/vm-images", `{"name":"env-image"}`, http.StatusBadRequest},
		{"both sources", http.MethodPost, "/vm-images", `{"name":"env-image","from_vm":"ready","dockerfile":"FROM alpine"}`, http.StatusBadRequest},
		{"invalid labels", http.MethodPost, "/vm-images", `{"name":"env-image","from_vm":"ready","labels":{"Bad":"v"}}`, http.StatusBadRequest},
		{"running source VM", http.MethodPost, "/vm-images", `{"name":"env-image","from_vm":"ready"}`, http.StatusConflict},
		{"bad label selector", http.MethodGet, "/vm-images?label=Bad", ``, http.StatusBadRequest},
		{"missing image", http.MethodGet, "/vm-images/missing", ``, http.StatusNotFound},
		{"delete missing image", http.MethodDelete, "/vm-images/missing", ``, http.StatusNotFound},
		{"vm with image and profile", http.MethodPost, "/vms", `{"name":"new-vm","image":"env-image","profile":"alpine"}`, http.StatusBadRequest},
		{"vm with unknown image", http.MethodPost, "/vms", `{"name":"new-vm","image":"env-image"}`, http.StatusBadRequest},
		{"vm with invalid labels", http.MethodPost, "/vms", `{"name":"new-vm","labels":{"has space":"v"}}`, http.StatusBadRequest},
		{"vm list bad selector", http.MethodGet, "/vms?label=Bad%3Dv", ``, http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := serveVMImageRequest(t, router, tt.method, tt.target, tt.body)
			if w.Code != tt.status {
				t.Fatalf("status = %d, want %d; body: %s", w.Code, tt.status, w.Body.String())
			}
		})
	}
}

func TestVMImageListReturnsEmptyArray(t *testing.T) {
	svc, _ := setupVMUploadTestService(t)
	w := serveVMImageRequest(t, setupVMImageRouter(svc), http.MethodGet, "/vm-images?label=owner%3Dkiln-run", "")
	if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != "[]" {
		t.Fatalf("GET /vm-images = %d %s, want 200 []", w.Code, w.Body.String())
	}
}

func TestVMListFiltersByLabelAndOmitsEmptyLabels(t *testing.T) {
	svc, _ := setupVMUploadTestService(t)
	router := setupVMImageRouter(svc)

	w := serveVMImageRequest(t, router, http.MethodGet, "/vms", "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /vms = %d %s", w.Code, w.Body.String())
	}
	var raw []map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode VMs: %v", err)
	}
	if len(raw) != 1 {
		t.Fatalf("VMs = %s", w.Body.String())
	}
	for _, key := range []string{"labels", "image"} {
		if _, ok := raw[0][key]; ok {
			t.Fatalf("VM without %s still returned the field: %s", key, w.Body.String())
		}
	}

	w = serveVMImageRequest(t, router, http.MethodGet, "/vms?label=owner&label=env%3Dx", "")
	var filtered []models.VM
	if err := json.Unmarshal(w.Body.Bytes(), &filtered); err != nil || w.Code != http.StatusOK || len(filtered) != 0 {
		t.Fatalf("GET /vms?label=owner = %d %s (%v), want empty", w.Code, w.Body.String(), err)
	}
}
