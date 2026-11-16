package execution

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

const (
	MaxCommandBytes       = 1 << 20
	MaxDataBytes          = 32 << 10
	MaxEncodedFrameBytes  = 2 << 20
	ResponseQueueCapacity = 16
	MaxCaptureBytes       = 16 << 20
	MaxDiagnosticBytes    = 8 << 10
)

type Category string

const (
	CategoryNone                 Category = ""
	CategoryCancelled            Category = "cancelled"
	CategoryDeadlineExceeded     Category = "deadline_exceeded"
	CategoryProtocolError        Category = "protocol_error"
	CategoryOutputLimitExceeded  Category = "output_limit_exceeded"
	CategoryRuntimeIncompatible  Category = "runtime_incompatible"
	CategoryTransportUnavailable Category = "transport_unavailable"
	CategoryTransportLost        Category = "transport_lost"
	CategoryInternal             Category = "internal"
)

type Request struct {
	Command string
	Stdin   io.Reader
	Stdout  io.Writer
	Stderr  io.Writer
	Started func() error
}

type Outcome struct {
	ExitCode  int
	Category  Category
	Message   string
	Retryable bool
	Ambiguous bool
	Cause     error
}

func (o Outcome) Succeeded() bool {
	return o.Category == CategoryNone && o.ExitCode == 0
}

type Runner interface {
	Execute(context.Context, Request) Outcome
}

type RunnerFunc func(context.Context, Request) Outcome

func (f RunnerFunc) Execute(ctx context.Context, request Request) Outcome {
	return f(ctx, request)
}

type Error struct {
	Category  Category
	Message   string
	Retryable bool
	Ambiguous bool
	Cause     error
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	if e.Message != "" {
		return e.Message
	}
	if e.Cause != nil {
		return e.Cause.Error()
	}
	return string(e.Category)
}

func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

func OutcomeFromError(err error) Outcome {
	if err == nil {
		return Outcome{}
	}
	category := CategoryInternal
	switch {
	case errors.Is(err, context.Canceled):
		category = CategoryCancelled
	case errors.Is(err, context.DeadlineExceeded):
		category = CategoryDeadlineExceeded
	case errors.Is(err, ErrOutputLimitExceeded):
		category = CategoryOutputLimitExceeded
	}
	var execErr *Error
	if errors.As(err, &execErr) {
		category = execErr.Category
		return Outcome{
			Category:  category,
			Message:   Diagnostic(execErr.Error()),
			Retryable: execErr.Retryable,
			Ambiguous: execErr.Ambiguous,
			Cause:     err,
		}
	}
	return Outcome{Category: category, Message: Diagnostic(err.Error()), Cause: err}
}

func ErrorFromOutcome(outcome Outcome) error {
	if outcome.Category == CategoryNone {
		return nil
	}
	cause := outcome.Cause
	switch outcome.Category {
	case CategoryCancelled:
		if cause == nil {
			cause = context.Canceled
		}
	case CategoryDeadlineExceeded:
		if cause == nil {
			cause = context.DeadlineExceeded
		}
	case CategoryOutputLimitExceeded:
		if cause == nil {
			cause = ErrOutputLimitExceeded
		}
	}
	return &Error{
		Category:  outcome.Category,
		Message:   Diagnostic(outcome.Message),
		Retryable: outcome.Retryable,
		Ambiguous: outcome.Ambiguous,
		Cause:     cause,
	}
}

type BufferedResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

var ErrOutputLimitExceeded = errors.New("exec output capture limit exceeded")

type limitedBuffer struct {
	buffer bytes.Buffer
	limit  int
}

func (w *limitedBuffer) Write(p []byte) (int, error) {
	if len(p) > w.limit-w.buffer.Len() {
		remaining := w.limit - w.buffer.Len()
		if remaining > 0 {
			_, _ = w.buffer.Write(p[:remaining])
		}
		return remaining, ErrOutputLimitExceeded
	}
	n, _ := w.buffer.Write(p)
	return n, nil
}

func (w *limitedBuffer) String() string {
	return w.buffer.String()
}

func RunBuffered(ctx context.Context, runner Runner, command string, stdin io.Reader) (*BufferedResult, error) {
	stdout := &limitedBuffer{limit: MaxCaptureBytes}
	stderr := &limitedBuffer{limit: MaxCaptureBytes}
	outcome := runner.Execute(ctx, Request{
		Command: command,
		Stdin:   stdin,
		Stdout:  stdout,
		Stderr:  stderr,
	})
	if err := ErrorFromOutcome(outcome); err != nil {
		return nil, err
	}
	return &BufferedResult{
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		ExitCode: outcome.ExitCode,
	}, nil
}

func ValidateCommand(command string) error {
	if strings.TrimSpace(command) == "" {
		return errors.New("command is required")
	}
	if len(command) > MaxCommandBytes {
		return fmt.Errorf("command exceeds %d bytes", MaxCommandBytes)
	}
	if !utf8.ValidString(command) {
		return errors.New("command must be valid UTF-8")
	}
	return nil
}

func Diagnostic(message string) string {
	message = strings.ToValidUTF8(message, "\uFFFD")
	if len(message) <= MaxDiagnosticBytes {
		return message
	}
	return message[:MaxDiagnosticBytes]
}
