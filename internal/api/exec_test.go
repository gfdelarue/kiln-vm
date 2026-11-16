package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gfdelarue/kiln-vm/internal/execution"
	"github.com/gfdelarue/kiln-vm/internal/execwire"
	"github.com/gfdelarue/kiln-vm/internal/middleware"
	"github.com/gfdelarue/kiln-vm/internal/models"
)

func TestDecodeExecRequestPreservesCommandOnlyJSON(t *testing.T) {
	body, err := json.Marshal(models.ExecRequest{Command: "printf ok"})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	req := httptest.NewRequest("POST", "/exec", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	header, err := decodeExecRequest(req)
	if err != nil {
		t.Fatalf("decodeExecRequest: %v", err)
	}
	if header.Command != "printf ok" {
		t.Fatalf("request = %+v, want command-only JSON", header)
	}
}

func TestDecodeExecRequestRejectsMissingCommand(t *testing.T) {
	req := httptest.NewRequest("POST", "/exec", strings.NewReader(`{"command":""}`))
	req.Header.Set("Content-Type", "application/json")
	if _, err := decodeExecRequest(req); err == nil || !strings.Contains(err.Error(), "command is required") {
		t.Fatalf("decodeExecRequest error = %v, want missing command", err)
	}
}

func TestExecDuplexStreamsOutputWhileRequestBodyRemainsOpenThroughMiddleware(t *testing.T) {
	handler := middleware.RequestLogger(middleware.TimeoutExceptStreaming(time.Second)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveExec(w, r, func(_ context.Context, request execution.Request) execution.Outcome {
			if err := request.Started(); err != nil {
				return execution.OutcomeFromError(err)
			}
			if _, err := io.WriteString(request.Stdout, "early"); err != nil {
				return execution.OutcomeFromError(err)
			}
			if _, err := io.ReadAll(request.Stdin); err != nil {
				return execution.OutcomeFromError(err)
			}
			return execution.Outcome{}
		})
	})))
	server := httptest.NewServer(handler)
	defer server.Close()

	bodyReader, bodyWriter := io.Pipe()
	request, err := http.NewRequest(http.MethodPost, server.URL+"/machines/m-1/exec", bodyReader)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	request.Header.Set("Content-Type", execwire.MediaType)
	request.Header.Set("Accept", execwire.MediaType)
	requestCodec := execwire.NewCodec(nil, bodyWriter)
	writeErr := make(chan error, 1)
	go func() {
		if err := requestCodec.WriteRequest(execwire.RequestFrame{
			Version: execwire.Version,
			Type:    execwire.FrameStart,
			Start:   &execwire.Start{Command: "cat"},
		}); err != nil {
			writeErr <- err
			return
		}
		writeErr <- requestCodec.WriteRequest(execwire.RequestFrame{
			Version: execwire.Version,
			Type:    execwire.FrameStdin,
			Data:    []byte("body remains open"),
		})
	}()
	client := server.Client()
	client.Timeout = 3 * time.Second
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	if err := <-writeErr; err != nil {
		t.Fatalf("write initial request: %v", err)
	}
	defer response.Body.Close()
	if response.Header.Get("Content-Type") != execwire.MediaType {
		t.Fatalf("content type = %q", response.Header.Get("Content-Type"))
	}
	responseCodec := execwire.NewCodec(response.Body, nil)
	started, err := responseCodec.ReadResponse()
	if err != nil || started.Type != execwire.FrameStarted {
		t.Fatalf("started frame = %#v, %v", started, err)
	}
	output, err := responseCodec.ReadResponse()
	if err != nil || output.Type != execwire.FrameStdout || string(output.Data) != "early" {
		t.Fatalf("output frame = %#v, %v", output, err)
	}
	if err := requestCodec.WriteRequest(execwire.RequestFrame{Version: execwire.Version, Type: execwire.FrameStdinEOF}); err != nil {
		t.Fatalf("write stdin EOF: %v", err)
	}
	terminal, err := responseCodec.ReadResponse()
	if err != nil || terminal.Type != execwire.FrameTerminal {
		t.Fatalf("terminal frame = %#v, %v", terminal, err)
	}
	_ = bodyWriter.Close()
}

func TestExecNegotiationRejectsUnsupportedRepresentationsBeforeExecution(t *testing.T) {
	var executions atomic.Int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveExec(w, r, func(context.Context, execution.Request) execution.Outcome {
			executions.Add(1)
			return execution.Outcome{}
		})
	})
	tests := []struct {
		name        string
		contentType string
		accept      string
		wantStatus  int
	}{
		{name: "unsupported content type", contentType: "text/plain", wantStatus: http.StatusUnsupportedMediaType},
		{name: "incompatible accept", contentType: execwire.MediaType, accept: "application/json", wantStatus: http.StatusNotAcceptable},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/machines/m-1/exec", strings.NewReader("{}"))
			request.Header.Set("Content-Type", test.contentType)
			if test.accept != "" {
				request.Header.Set("Accept", test.accept)
			}
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)
			if recorder.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", recorder.Code, test.wantStatus, recorder.Body.String())
			}
		})
	}
	if executions.Load() != 0 {
		t.Fatalf("executions = %d, want zero", executions.Load())
	}
}
