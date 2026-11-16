package vm

import (
	"context"
	"io"

	"github.com/gfdelarue/kiln-vm/internal/execution"
)

func executeTestAdapter(
	ctx context.Context,
	request execution.Request,
	exec func(context.Context, string, io.Reader) (*execution.BufferedResult, error),
) execution.Outcome {
	result, err := exec(ctx, request.Command, request.Stdin)
	if err != nil {
		return execution.OutcomeFromError(err)
	}
	if request.Started != nil {
		if err := request.Started(); err != nil {
			return execution.OutcomeFromError(err)
		}
	}
	if request.Stdout != nil {
		if _, err := io.WriteString(request.Stdout, result.Stdout); err != nil {
			return execution.OutcomeFromError(err)
		}
	}
	if request.Stderr != nil {
		if _, err := io.WriteString(request.Stderr, result.Stderr); err != nil {
			return execution.OutcomeFromError(err)
		}
	}
	return execution.Outcome{ExitCode: result.ExitCode}
}
