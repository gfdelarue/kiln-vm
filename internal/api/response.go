package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"github.com/gfdelarue/kiln-vm/internal/apierr"
)

const (
	maxStructuredErrorDetailBytes     = 16 * 1024
	maxStructuredErrorResponseBytes   = 40 * 1024
	maxStructuredDiagnosticErrorBytes = 4 * 1024
	maxStructuredDiagnosticTailBytes  = 8 * 1024
)

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func writeErrorDetail(w http.ResponseWriter, status int, msg string, detail string) {
	if detail == "" || detail == msg {
		writeError(w, status, msg)
		return
	}
	writeJSON(w, status, map[string]string{"error": msg, "detail": detail})
}

func writeServiceError(w http.ResponseWriter, err error) {
	if operation := operationError(err); operation != nil {
		writeStructuredServiceError(w, err, operation)
		return
	}
	publicMessage := err.Error()
	switch {
	case errors.Is(err, apierr.ErrNotFound):
		writeError(w, http.StatusNotFound, publicMessage)
	case errors.Is(err, apierr.ErrValidation):
		writeError(w, http.StatusBadRequest, publicMessage)
	case errors.Is(err, apierr.ErrConflict):
		writeError(w, http.StatusConflict, publicMessage)
	case errors.Is(err, apierr.ErrResourceExhausted):
		writeError(w, http.StatusInsufficientStorage, publicMessage)
	default:
		slog.Error("internal error", "error", err)
		writeErrorDetail(w, http.StatusInternalServerError, "internal error", publicMessage)
	}
}

func operationError(err error) *apierr.OperationError {
	var operation *apierr.OperationError
	if errors.As(err, &operation) {
		return operation
	}
	return nil
}

func writeStructuredServiceError(w http.ResponseWriter, err error, operation *apierr.OperationError) {
	status := http.StatusInternalServerError
	errorMessage := "internal error"
	switch {
	case errors.Is(err, apierr.ErrNotFound):
		status = http.StatusNotFound
		errorMessage = err.Error()
	case errors.Is(err, apierr.ErrValidation):
		status = http.StatusBadRequest
		errorMessage = err.Error()
	case errors.Is(err, apierr.ErrConflict):
		status = http.StatusConflict
		errorMessage = err.Error()
	case errors.Is(err, apierr.ErrResourceExhausted):
		status = http.StatusInsufficientStorage
		errorMessage = err.Error()
	default:
		if operation.Code != "" {
			errorMessage = operation.Code
		}
		slog.Error("internal error", "error", err)
	}
	if operation.Code != "" {
		errorMessage = operation.Code
	}
	detailSource := err.Error()
	detail, detailTruncated := boundedStructuredErrorText(detailSource)
	response := map[string]any{
		"error":     errorMessage,
		"detail":    detail,
		"code":      withDefault(operation.Code, errorCodeForStatus(status)),
		"phase":     operation.Phase,
		"resource":  operation.Resource,
		"action":    operation.Action,
		"retryable": operation.Retryable,
	}
	if detailTruncated {
		response["detail_truncated"] = true
		response["detail_original_bytes"] = len(detailSource)
	}
	addResponseString(response, "status_ref", operation.StatusRef)
	if len(operation.Diagnostics) > 0 {
		budget := maxStructuredErrorResponseBytes - encodedJSONLength(response) - 512
		diagnostics, truncated := boundedOperationDiagnostics(operation.Diagnostics, budget)
		if len(diagnostics) > 0 {
			response["diagnostics"] = diagnostics
		}
		if truncated {
			response["diagnostics_truncated"] = true
			response["diagnostics_original_count"] = len(operation.Diagnostics)
		}
	}
	writeJSON(w, status, response)
}

func boundedOperationDiagnostics(diagnostics []apierr.OperationDiagnostic, maxBytes int) ([]apierr.OperationDiagnostic, bool) {
	if len(diagnostics) == 0 {
		return nil, false
	}
	if maxBytes <= 0 {
		return nil, true
	}
	bounded := []apierr.OperationDiagnostic{}
	for index, diagnostic := range diagnostics {
		item := boundedOperationDiagnostic(diagnostic)
		candidate := append(append([]apierr.OperationDiagnostic(nil), bounded...), item)
		if encodedJSONLength(candidate) > maxBytes {
			if len(bounded) == 0 {
				item = tightlyBoundedOperationDiagnostic(diagnostic)
				if encodedJSONLength([]apierr.OperationDiagnostic{item}) <= maxBytes {
					return []apierr.OperationDiagnostic{item}, len(diagnostics) > 1
				}
			}
			return bounded, true
		}
		bounded = candidate
		if index == len(diagnostics)-1 {
			return bounded, false
		}
	}
	return bounded, false
}

func boundedOperationDiagnostic(diagnostic apierr.OperationDiagnostic) apierr.OperationDiagnostic {
	diagnostic.Error, _ = boundedStructuredText(diagnostic.Error, maxStructuredDiagnosticErrorBytes)
	diagnostic.StdoutTail, _ = boundedStructuredText(diagnostic.StdoutTail, maxStructuredDiagnosticTailBytes)
	diagnostic.StderrTail, _ = boundedStructuredText(diagnostic.StderrTail, maxStructuredDiagnosticTailBytes)
	return diagnostic
}

func tightlyBoundedOperationDiagnostic(diagnostic apierr.OperationDiagnostic) apierr.OperationDiagnostic {
	for _, limit := range []int{2048, 1024, 512} {
		item := diagnostic
		item.Error, _ = boundedStructuredText(item.Error, limit)
		item.StdoutTail, _ = boundedStructuredText(item.StdoutTail, limit)
		item.StderrTail, _ = boundedStructuredText(item.StderrTail, limit)
		if encodedJSONLength([]apierr.OperationDiagnostic{item}) <= maxStructuredErrorResponseBytes/2 {
			return item
		}
	}
	diagnostic.Error, _ = boundedStructuredText(diagnostic.Error, 512)
	diagnostic.StdoutTail = ""
	diagnostic.StderrTail = ""
	return diagnostic
}

func boundedStructuredErrorText(value string) (string, bool) {
	return boundedStructuredText(value, maxStructuredErrorDetailBytes)
}

func boundedStructuredText(value string, maxBytes int) (string, bool) {
	if maxBytes <= 0 {
		return "", value != ""
	}
	if len(value) <= maxBytes {
		return value, false
	}
	omission := fmt.Sprintf("\n... truncated %d bytes ...\n", len(value)-maxBytes)
	if len(omission) >= maxBytes {
		return value[:maxBytes], true
	}
	headBytes := (maxBytes - len(omission)) / 2
	tailBytes := maxBytes - len(omission) - headBytes
	return value[:headBytes] + omission + value[len(value)-tailBytes:], true
}

func encodedJSONLength(value any) int {
	data, err := json.Marshal(value)
	if err != nil {
		return maxStructuredErrorResponseBytes
	}
	return len(data)
}

func addResponseString(response map[string]any, key, value string) {
	if value != "" {
		response[key] = value
	}
}

func errorCodeForStatus(status int) string {
	switch status {
	case http.StatusNotFound:
		return "not_found"
	case http.StatusBadRequest:
		return "validation_error"
	case http.StatusConflict:
		return "conflict"
	case http.StatusInsufficientStorage:
		return "resource_exhausted"
	default:
		return "internal_error"
	}
}

func withDefault(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}

func decodeJSON(r *http.Request, v any) error {
	return decodeJSONLimit(r, v, 1<<20)
}

func decodeOptionalJSON(r *http.Request, v any) error {
	return decodeOptionalJSONLimit(r, v, 1<<20)
}

func decodeOptionalJSONLimit(r *http.Request, v any, limit int64) error {
	if r.Body == nil || r.Body == http.NoBody {
		return nil
	}
	defer r.Body.Close()
	r.Body = http.MaxBytesReader(nil, r.Body, limit)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		return fmt.Errorf("decode request: %w", err)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("decode request: %w", err)
	}
	return nil
}

func decodeJSONLimit(r *http.Request, v any, limit int64) error {
	defer r.Body.Close()
	r.Body = http.MaxBytesReader(nil, r.Body, limit)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		return fmt.Errorf("decode request: %w", err)
	}
	return nil
}
