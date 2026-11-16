package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"sync"

	"github.com/gfdelarue/kiln-vm/internal/execution"
	"github.com/gfdelarue/kiln-vm/internal/execwire"
	"github.com/gfdelarue/kiln-vm/internal/models"
)

func decodeExecRequest(r *http.Request) (models.ExecRequest, error) {
	contentType := r.Header.Get("Content-Type")
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil && contentType != "" {
		return models.ExecRequest{}, fmt.Errorf("parse content type: %w", err)
	}
	if mediaType != "" && mediaType != "application/json" {
		return models.ExecRequest{}, fmt.Errorf("unsupported exec content type %q", contentType)
	}
	var req models.ExecRequest
	if err := decodeJSON(r, &req); err != nil {
		return models.ExecRequest{}, err
	}
	if err := execution.ValidateCommand(req.Command); err != nil {
		return models.ExecRequest{}, fmt.Errorf("validate exec command: %w", err)
	}
	return req, nil
}

func serveExec(
	w http.ResponseWriter,
	r *http.Request,
	run func(context.Context, execution.Request) execution.Outcome,
) {
	defer r.Body.Close()
	if strings.TrimSpace(r.Header.Get("Content-Type")) == execwire.MediaType {
		serveExecDuplex(w, r, run)
		return
	}
	if strings.Contains(r.Header.Get("Accept"), "application/vnd.kiln-vm.exec-duplex") {
		writeError(w, http.StatusUnsupportedMediaType, "exec duplex requires Content-Type "+execwire.MediaType)
		return
	}
	request, err := decodeExecRequest(r)
	if err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), "unsupported exec content type") {
			status = http.StatusUnsupportedMediaType
		}
		writeError(w, status, err.Error())
		return
	}
	result, err := execution.RunBuffered(r.Context(), execution.RunnerFunc(func(ctx context.Context, request execution.Request) execution.Outcome {
		return run(ctx, request)
	}), request.Command, nil)
	if err != nil {
		writeExecOutcomeError(w, execution.OutcomeFromError(err))
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func serveExecDuplex(
	w http.ResponseWriter,
	r *http.Request,
	run func(context.Context, execution.Request) execution.Outcome,
) {
	if strings.TrimSpace(r.Header.Get("Accept")) != execwire.MediaType {
		writeError(w, http.StatusNotAcceptable, "exec duplex requires Accept "+execwire.MediaType)
		return
	}
	if err := http.NewResponseController(w).EnableFullDuplex(); err != nil {
		writeError(w, http.StatusInternalServerError, "HTTP full duplex is unavailable")
		return
	}
	codec := execwire.NewCodec(r.Body, w)
	start, err := codec.ReadRequest()
	if err != nil {
		writeError(w, http.StatusBadRequest, execution.Diagnostic(err.Error()))
		return
	}
	if start.Type != execwire.FrameStart || start.Start == nil {
		writeError(w, http.StatusBadRequest, "expected one exec start frame")
		return
	}

	ctx, cancel := context.WithCancelCause(r.Context())
	defer cancel(nil)
	writer := newHTTPExecResponseWriter(cancel, codec, w)
	defer writer.stop()
	stdinReader, stdinWriter := io.Pipe()
	runDone := make(chan struct{})
	go readHTTPExecInput(ctx, cancel, runDone, codec, stdinWriter)

	started := false
	outcome := run(ctx, execution.Request{
		Command: start.Start.Command,
		Stdin:   stdinReader,
		Stdout:  httpExecStreamWriter{writer: writer, frameType: execwire.FrameStdout},
		Stderr:  httpExecStreamWriter{writer: writer, frameType: execwire.FrameStderr},
		Started: func() error {
			w.Header().Set("Content-Type", execwire.MediaType)
			started = true
			return writer.write(execwire.ResponseFrame{Version: execwire.Version, Type: execwire.FrameStarted})
		},
	})
	close(runDone)
	_ = stdinReader.Close()
	_ = stdinWriter.Close()
	if !started {
		writeExecOutcomeError(w, outcome)
		return
	}
	_ = writer.write(execwire.ResponseFrame{
		Version: execwire.Version,
		Type:    execwire.FrameTerminal,
		Terminal: &execwire.Terminal{
			ExitCode:  outcome.ExitCode,
			Category:  outcome.Category,
			Message:   execution.Diagnostic(outcome.Message),
			Retryable: outcome.Retryable,
			Ambiguous: outcome.Ambiguous,
		},
	})
}

func readHTTPExecInput(
	ctx context.Context,
	cancel context.CancelCauseFunc,
	runDone <-chan struct{},
	codec *execwire.Codec,
	stdin *io.PipeWriter,
) {
	defer stdin.Close()
	stdinEOF := false
	for {
		frame, err := codec.ReadRequest()
		if channelClosed(runDone) {
			return
		}
		if err != nil {
			cause := error(context.Canceled)
			if !errors.Is(err, io.EOF) {
				cause = &execution.Error{
					Category: execution.CategoryProtocolError,
					Message:  execution.Diagnostic(err.Error()),
					Cause:    err,
				}
			}
			_ = stdin.CloseWithError(cause)
			cancel(cause)
			return
		}
		switch frame.Type {
		case execwire.FrameStdin:
			if stdinEOF {
				err := &execution.Error{Category: execution.CategoryProtocolError, Message: "stdin received after EOF"}
				_ = stdin.CloseWithError(err)
				cancel(err)
				return
			}
			if len(frame.Data) > 0 {
				if _, err := stdin.Write(frame.Data); err != nil && ctx.Err() == nil {
					cancel(err)
					return
				}
			}
		case execwire.FrameStdinEOF:
			if stdinEOF {
				err := &execution.Error{Category: execution.CategoryProtocolError, Message: "duplicate stdin EOF"}
				_ = stdin.CloseWithError(err)
				cancel(err)
				return
			}
			stdinEOF = true
			_ = stdin.Close()
		default:
			err := &execution.Error{Category: execution.CategoryProtocolError, Message: fmt.Sprintf("unexpected exec request frame %q", frame.Type)}
			_ = stdin.CloseWithError(err)
			cancel(err)
			return
		}
	}
}

func channelClosed(done <-chan struct{}) bool {
	select {
	case <-done:
		return true
	default:
		return false
	}
}

type httpExecResponseWriter struct {
	queue chan httpExecResponseWrite
	stopC chan struct{}
	done  chan struct{}
	once  sync.Once
}

type httpExecResponseWrite struct {
	frame execwire.ResponseFrame
	done  chan error
}

func newHTTPExecResponseWriter(
	cancel context.CancelCauseFunc,
	codec *execwire.Codec,
	response http.ResponseWriter,
) *httpExecResponseWriter {
	writer := &httpExecResponseWriter{
		queue: make(chan httpExecResponseWrite, execution.ResponseQueueCapacity),
		stopC: make(chan struct{}),
		done:  make(chan struct{}),
	}
	controller := http.NewResponseController(response)
	go func() {
		defer close(writer.done)
		for {
			select {
			case <-writer.stopC:
				return
			case item := <-writer.queue:
				err := codec.WriteResponse(item.frame)
				if err == nil {
					err = controller.Flush()
				}
				item.done <- err
				if err != nil {
					cancel(err)
					return
				}
			}
		}
	}()
	return writer
}

func (w *httpExecResponseWriter) write(frame execwire.ResponseFrame) error {
	item := httpExecResponseWrite{frame: frame, done: make(chan error, 1)}
	select {
	case <-w.done:
		return io.ErrClosedPipe
	case w.queue <- item:
	}
	select {
	case <-w.done:
		return io.ErrClosedPipe
	case err := <-item.done:
		return err
	}
}

func (w *httpExecResponseWriter) stop() {
	w.once.Do(func() { close(w.stopC) })
	<-w.done
}

type httpExecStreamWriter struct {
	writer    *httpExecResponseWriter
	frameType string
}

func (w httpExecStreamWriter) Write(data []byte) (int, error) {
	written := 0
	for len(data) > 0 {
		size := min(len(data), execution.MaxDataBytes)
		if err := w.writer.write(execwire.ResponseFrame{
			Version: execwire.Version,
			Type:    w.frameType,
			Data:    append([]byte(nil), data[:size]...),
		}); err != nil {
			return written, err
		}
		written += size
		data = data[size:]
	}
	return written, nil
}

func writeExecOutcomeError(w http.ResponseWriter, outcome execution.Outcome) {
	if outcome.Cause != nil && outcome.Category == execution.CategoryInternal {
		writeServiceError(w, outcome.Cause)
		return
	}
	status := http.StatusInternalServerError
	code := "exec_internal"
	switch outcome.Category {
	case execution.CategoryRuntimeIncompatible:
		status = http.StatusConflict
		code = "exec_runtime_incompatible"
	case execution.CategoryTransportUnavailable:
		status = http.StatusServiceUnavailable
		code = "exec_transport_unavailable"
	case execution.CategoryTransportLost:
		status = http.StatusBadGateway
		code = "exec_transport_lost"
	case execution.CategoryOutputLimitExceeded:
		status = http.StatusUnprocessableEntity
		code = "exec_output_limit_exceeded"
	case execution.CategoryProtocolError:
		status = http.StatusBadRequest
		code = "exec_protocol_error"
	case execution.CategoryDeadlineExceeded:
		status = http.StatusGatewayTimeout
		code = "exec_deadline_exceeded"
	case execution.CategoryCancelled:
		status = http.StatusConflict
		code = "exec_cancelled"
	case execution.CategoryNone:
		if outcome.ExitCode == 0 {
			writeJSON(w, http.StatusOK, &execution.BufferedResult{})
			return
		}
	}
	message := execution.Diagnostic(outcome.Message)
	if message == "" {
		message = code
	}
	writeJSON(w, status, map[string]any{
		"error":     message,
		"code":      code,
		"retryable": outcome.Retryable,
		"ambiguous": outcome.Ambiguous,
	})
}
