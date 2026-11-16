package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gfdelarue/kiln-vm/internal/apierr"
)

func TestWriteServiceErrorResourceExhausted(t *testing.T) {
	w := httptest.NewRecorder()
	writeServiceError(w, fmt.Errorf("build image: %w", apierr.ErrResourceExhausted))

	if w.Code != http.StatusInsufficientStorage {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusInsufficientStorage, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "resource exhausted") {
		t.Fatalf("response body missing resource error: %s", w.Body.String())
	}
}

func TestWriteServiceErrorIncludesInternalDetail(t *testing.T) {
	w := httptest.NewRecorder()
	writeServiceError(w, fmt.Errorf("create vm machine: total memory limit exceeded"))

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusInternalServerError, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"error":"internal error"`) {
		t.Fatalf("response body missing internal error marker: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "create vm machine") {
		t.Fatalf("response body missing actionable detail: %s", w.Body.String())
	}
}

func TestWriteServiceErrorIncludesOperationFields(t *testing.T) {
	w := httptest.NewRecorder()
	writeServiceError(w, &apierr.OperationError{
		Code:      "vm_readiness_failed",
		StatusRef: "/vms/dev",
		Phase:     "readiness",
		Action:    "start",
		Resource:  "dev",
		Retryable: false,
		Err:       fmt.Errorf("vm dev readiness failed: %w", apierr.ErrConflict),
	})

	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusConflict, w.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	for key, want := range map[string]string{
		"code":       "vm_readiness_failed",
		"status_ref": "/vms/dev",
		"phase":      "readiness",
		"action":     "start",
		"resource":   "dev",
	} {
		if got := body[key]; got != want {
			t.Fatalf("%s = %#v, want %q; body: %s", key, got, want, w.Body.String())
		}
	}
	if body["retryable"] != false {
		t.Fatalf("retryable = %#v, want false; body: %s", body["retryable"], w.Body.String())
	}
}

func TestWriteServiceErrorIncludesOperationDiagnostics(t *testing.T) {
	w := httptest.NewRecorder()
	writeServiceError(w, &apierr.OperationError{
		Code:     "vm_create_failed",
		Phase:    "build",
		Action:   "run",
		Resource: "app",
		Diagnostics: []apierr.OperationDiagnostic{{
			Category:   "build",
			Service:    "web",
			Step:       "vercel-install",
			Machine:    "builder",
			State:      "failed",
			LogDir:     "/var/lib/kiln-vm/machines/dev/logs",
			Error:      "exit 1",
			StdoutTail: "install stdout\n",
			StderrTail: "install stderr\n",
			Retryable:  true,
			Lines:      120,
		}},
		Err: fmt.Errorf("service web build step vercel-install failed: %w", apierr.ErrConflict),
	})

	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusConflict, w.Body.String())
	}
	var body struct {
		Diagnostics []apierr.OperationDiagnostic `json:"diagnostics"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(body.Diagnostics) != 1 {
		t.Fatalf("diagnostics = %#v, want one entry; body: %s", body.Diagnostics, w.Body.String())
	}
	diagnostic := body.Diagnostics[0]
	if diagnostic.Category != "build" || diagnostic.Service != "web" || diagnostic.Step != "vercel-install" {
		t.Fatalf("diagnostic identity = %+v", diagnostic)
	}
	if diagnostic.StdoutTail != "install stdout\n" || diagnostic.StderrTail != "install stderr\n" {
		t.Fatalf("diagnostic tails = stdout %q stderr %q", diagnostic.StdoutTail, diagnostic.StderrTail)
	}
	if !diagnostic.Retryable {
		t.Fatalf("diagnostic retryable = false, want true")
	}
}

func TestWriteServiceErrorBoundsOperationDetail(t *testing.T) {
	longLog := "guest start failure: out of memory\n" + strings.Repeat("restart attempt\n", 3000) + "final useful tail\n"
	w := httptest.NewRecorder()
	writeServiceError(w, &apierr.OperationError{
		Code:     "vm_recover_failed",
		Phase:    "start",
		Action:   "recover",
		Resource: "shop",
		Err:      fmt.Errorf("%s: %w", longLog, apierr.ErrConflict),
	})

	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusConflict, w.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	detail, ok := body["detail"].(string)
	if !ok {
		t.Fatalf("detail = %#v, want string", body["detail"])
	}
	if len(detail) > maxStructuredErrorDetailBytes {
		t.Fatalf("detail length = %d, want <= %d", len(detail), maxStructuredErrorDetailBytes)
	}
	if body["detail_truncated"] != true {
		t.Fatalf("detail_truncated = %#v, want true; body: %s", body["detail_truncated"], w.Body.String())
	}
	if !strings.Contains(detail, "guest start failure") || !strings.Contains(detail, "final useful tail") || !strings.Contains(detail, "truncated") {
		t.Fatalf("bounded detail lost useful context: %q", detail)
	}
	if body["error"] != "vm_recover_failed" {
		t.Fatalf("error = %#v, want concise operation code; body: %s", body["error"], w.Body.String())
	}
	if body["detail_original_bytes"] != float64(len(fmt.Sprintf("%s: %s", longLog, apierr.ErrConflict.Error()))) {
		t.Fatalf("detail_original_bytes = %#v, want original byte count", body["detail_original_bytes"])
	}
	if w.Body.Len() > 40*1024 {
		t.Fatalf("response body length = %d, want bounded response", w.Body.Len())
	}
}

func TestWriteServiceErrorBoundsOperationDiagnosticText(t *testing.T) {
	w := httptest.NewRecorder()
	writeServiceError(w, &apierr.OperationError{
		Code:     "vm_create_failed",
		Phase:    "build",
		Action:   "run",
		Resource: "web",
		Diagnostics: []apierr.OperationDiagnostic{{
			Category:   "build",
			Service:    "web",
			Step:       "install",
			Error:      "diagnostic-error-start\n" + strings.Repeat("diagnostic-error-noise\n", 3000) + "diagnostic-error-end\n",
			StdoutTail: "stdout-start\n" + strings.Repeat("stdout-noise\n", 3000) + "stdout-end\n",
			StderrTail: "stderr-start\n" + strings.Repeat("stderr-noise\n", 3000) + "stderr-end\n",
		}},
		Err: fmt.Errorf("%s: %w", "detail-start\n"+strings.Repeat("detail-noise\n", 3000)+"detail-end\n", apierr.ErrConflict),
	})

	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusConflict, w.Body.String())
	}
	var body struct {
		Diagnostics []apierr.OperationDiagnostic `json:"diagnostics"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(body.Diagnostics) != 1 {
		t.Fatalf("diagnostics = %#v, want one entry", body.Diagnostics)
	}
	diagnostic := body.Diagnostics[0]
	if len(diagnostic.Error) > maxStructuredDiagnosticErrorBytes {
		t.Fatalf("diagnostic error not bounded: %d", len(diagnostic.Error))
	}
	if len(diagnostic.StdoutTail) > maxStructuredDiagnosticTailBytes || len(diagnostic.StderrTail) > maxStructuredDiagnosticTailBytes {
		t.Fatalf("diagnostic tails not bounded: stdout=%d stderr=%d", len(diagnostic.StdoutTail), len(diagnostic.StderrTail))
	}
	if !strings.Contains(diagnostic.Error, "diagnostic-error-start") || !strings.Contains(diagnostic.Error, "diagnostic-error-end") {
		t.Fatalf("bounded diagnostic error lost context: %q", diagnostic.Error)
	}
	if !strings.Contains(diagnostic.StdoutTail, "stdout-start") || !strings.Contains(diagnostic.StdoutTail, "stdout-end") {
		t.Fatalf("bounded stdout tail lost context: %q", diagnostic.StdoutTail)
	}
	if !strings.Contains(diagnostic.StderrTail, "stderr-start") || !strings.Contains(diagnostic.StderrTail, "stderr-end") {
		t.Fatalf("bounded stderr tail lost context: %q", diagnostic.StderrTail)
	}
	if w.Body.Len() > maxStructuredErrorResponseBytes {
		t.Fatalf("response body length = %d, want <= %d", w.Body.Len(), maxStructuredErrorResponseBytes)
	}
}

func TestDecodeOptionalJSON(t *testing.T) {
	t.Run("empty body keeps defaults", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/verify", nil)
		value := struct {
			Capability string `json:"capability"`
		}{Capability: "docker"}
		if err := decodeOptionalJSON(req, &value); err != nil {
			t.Fatalf("decode optional JSON: %v", err)
		}
		if value.Capability != "docker" {
			t.Fatalf("capability = %q, want default docker", value.Capability)
		}
	})

	t.Run("whitespace body keeps defaults", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/verify", strings.NewReader(" \n\t "))
		value := struct {
			Force bool `json:"force"`
		}{Force: true}
		if err := decodeOptionalJSON(req, &value); err != nil {
			t.Fatalf("decode optional JSON: %v", err)
		}
		if !value.Force {
			t.Fatal("force was cleared by whitespace body")
		}
	})

	t.Run("JSON body decodes", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/verify", strings.NewReader(`{"capability":"docker","force":true}`))
		value := struct {
			Capability string `json:"capability"`
			Force      bool   `json:"force"`
		}{}
		if err := decodeOptionalJSON(req, &value); err != nil {
			t.Fatalf("decode optional JSON: %v", err)
		}
		if value.Capability != "docker" || !value.Force {
			t.Fatalf("decoded value = %+v, want docker force", value)
		}
	})

	t.Run("malformed body fails", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/verify", strings.NewReader(`{"capability"`))
		var value struct {
			Capability string `json:"capability"`
		}
		if err := decodeOptionalJSON(req, &value); err == nil {
			t.Fatal("decode optional JSON succeeded, want error")
		}
	})
}
