package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gfdelarue/kiln-vm/internal/execwire"
)

func TestTimeoutExceptStreamingPreservesFlusherForSSE(t *testing.T) {
	handler := TimeoutExceptStreaming(time.Second)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := w.(http.Flusher); !ok {
			t.Fatal("expected flusher to be preserved for SSE requests")
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	req := httptest.NewRequest(http.MethodGet, "/vms/dev", nil)
	req.Header.Set("Accept", "text/event-stream")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusNoContent)
	}
}

func TestTimeoutExceptStreamingWrapsNormalRequests(t *testing.T) {
	handler := TimeoutExceptStreaming(time.Second)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	req := httptest.NewRequest(http.MethodGet, "/vm-profiles", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusNoContent)
	}
}

func TestTimeoutExceptStreamingPreservesLongExecRequests(t *testing.T) {
	handler := TimeoutExceptStreaming(time.Millisecond)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := r.Context().Deadline(); ok {
			t.Fatal("exec requests must not inherit the generic request timeout")
		}
		time.Sleep(10 * time.Millisecond)
		w.WriteHeader(http.StatusNoContent)
	}))

	for _, path := range []string{"/vms/vm-1/exec"} {
		req := httptest.NewRequest(http.MethodPost, path, nil)
		req.Header.Set("Content-Type", execwire.MediaType)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		if w.Code != http.StatusNoContent {
			t.Fatalf("%s status = %d, want %d", path, w.Code, http.StatusNoContent)
		}
	}
}

func TestTimeoutExceptStreamingKeepsTimeoutForExecNearMisses(t *testing.T) {
	handler := TimeoutExceptStreaming(time.Second)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		if _, ok := r.Context().Deadline(); !ok {
			t.Errorf("%s %s unexpectedly bypassed timeout", r.Method, r.URL.Path)
		}
	}))
	for _, request := range []struct {
		method string
		path   string
	}{
		{method: http.MethodGet, path: "/vms/vm-1/exec"},
		{method: http.MethodPost, path: "/vms//exec"},
		{method: http.MethodPost, path: "/vms/vm-1/exec/extra"},
		{method: http.MethodPost, path: "/other/vm-1/exec"},
		{method: http.MethodPost, path: "/vms/vm-1/execution"},
	} {
		req := httptest.NewRequest(request.method, request.path, nil)
		handler.ServeHTTP(httptest.NewRecorder(), req)
	}
}

func TestTimeoutExceptStreamingPreservesLongProfilePrepareRequests(t *testing.T) {
	handler := TimeoutExceptStreaming(time.Millisecond)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := r.Context().Deadline(); ok {
			t.Fatal("profile prepare requests must not inherit the short request timeout")
		}
		time.Sleep(10 * time.Millisecond)
		w.WriteHeader(http.StatusNoContent)
	}))

	req := httptest.NewRequest(http.MethodPost, "/vm-profiles/windows11/prepare", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusNoContent)
	}
}

func TestTimeoutExceptStreamingPreservesVMForwardSessions(t *testing.T) {
	handler := TimeoutExceptStreaming(time.Millisecond)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := r.Context().Deadline(); ok {
			t.Fatal("vm forward sessions must not inherit the short request timeout")
		}
		time.Sleep(10 * time.Millisecond)
		w.WriteHeader(http.StatusNoContent)
	}))

	for _, upgrade := range []string{"websocket", ""} {
		req := httptest.NewRequest(http.MethodGet, "/vms/dev/forward/3000", nil)
		if upgrade != "" {
			req.Header.Set("Connection", "Upgrade")
			req.Header.Set("Upgrade", upgrade)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		if w.Code != http.StatusNoContent {
			t.Fatalf("upgrade %q status = %d, want %d", upgrade, w.Code, http.StatusNoContent)
		}
	}
}
