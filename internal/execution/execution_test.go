package execution

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

type runnerFunc func(context.Context, Request) Outcome

func (f runnerFunc) Execute(ctx context.Context, request Request) Outcome {
	return f(ctx, request)
}

func TestRunBufferedCapturesCanonicalExecution(t *testing.T) {
	result, err := RunBuffered(context.Background(), runnerFunc(func(_ context.Context, request Request) Outcome {
		_, _ = io.WriteString(request.Stdout, "out")
		_, _ = io.WriteString(request.Stderr, "err")
		return Outcome{ExitCode: 7}
	}), "exit 7", nil)
	if err != nil {
		t.Fatalf("RunBuffered: %v", err)
	}
	if result.Stdout != "out" || result.Stderr != "err" || result.ExitCode != 7 {
		t.Fatalf("result = %#v", result)
	}
}

func TestRunBufferedRejectsUnboundedCapture(t *testing.T) {
	_, err := RunBuffered(context.Background(), runnerFunc(func(_ context.Context, request Request) Outcome {
		_, writeErr := io.WriteString(request.Stdout, strings.Repeat("x", MaxCaptureBytes+1))
		return OutcomeFromError(writeErr)
	}), "true", nil)
	if !errors.Is(err, ErrOutputLimitExceeded) {
		t.Fatalf("error = %v, want output limit", err)
	}
}

func TestErrorFromOutcomePreservesCancellation(t *testing.T) {
	if err := ErrorFromOutcome(Outcome{Category: CategoryCancelled}); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}
