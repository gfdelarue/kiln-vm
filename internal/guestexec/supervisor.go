package guestexec

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/gfdelarue/kiln-vm/internal/execution"
)

const (
	MaxInlineShellCommandBytes = 32 * 1024
	TerminationGrace           = 2 * time.Second
)

type Supervisor struct {
	TerminationGrace time.Duration
}

func (s Supervisor) Execute(ctx context.Context, request execution.Request) execution.Outcome {
	if err := execution.ValidateCommand(request.Command); err != nil {
		return execution.OutcomeFromError(&execution.Error{
			Category: execution.CategoryProtocolError,
			Message:  err.Error(),
			Cause:    err,
		})
	}
	if ctx == nil {
		ctx = context.Background()
	}
	sessionCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	cmd, cleanup, err := shellCommand(request.Command)
	if err != nil {
		return execution.OutcomeFromError(err)
	}
	defer cleanup()
	cmd.SysProcAttr = &unix.SysProcAttr{Setpgid: true}
	terminationDispatched := make(chan struct{})
	signalTermination := sync.OnceFunc(func() {
		if cmd.Process != nil {
			_ = unix.Kill(-cmd.Process.Pid, unix.SIGTERM)
		}
		close(terminationDispatched)
	})

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return execution.OutcomeFromError(fmt.Errorf("open command stdin: %w", err))
	}
	cmd.Stdout = cancellingWriter{
		name:                  "stdout",
		destination:           request.Stdout,
		cancel:                cancel,
		terminationDispatched: terminationDispatched,
	}
	cmd.Stderr = cancellingWriter{
		name:                  "stderr",
		destination:           request.Stderr,
		cancel:                cancel,
		terminationDispatched: terminationDispatched,
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return execution.OutcomeFromError(fmt.Errorf("start command: %w", err))
	}

	if request.Started != nil {
		if err := request.Started(); err != nil {
			cancel(fmt.Errorf("publish started notification: %w", err))
		}
	}

	closeInput := sync.OnceFunc(func() { _ = stdin.Close() })
	if request.Stdin == nil {
		closeInput()
	} else {
		go func() {
			_, copyErr := io.Copy(stdin, request.Stdin)
			closeInput()
			if copyErr != nil && sessionCtx.Err() == nil {
				cancel(fmt.Errorf("copy command stdin: %w", copyErr))
			}
		}()
	}

	waitDone := make(chan error, 1)
	go func() {
		waitDone <- cmd.Wait()
	}()

	var waitErr error
	select {
	case waitErr = <-waitDone:
		if context.Cause(sessionCtx) != nil && processGroupAlive(cmd.Process.Pid) {
			completedWait := make(chan error, 1)
			completedWait <- waitErr
			waitErr = s.terminateProcessGroup(cmd, completedWait, signalTermination)
		}
	case <-sessionCtx.Done():
		closeInput()
		closeReader(request.Stdin)
		waitErr = s.terminateProcessGroup(cmd, waitDone, signalTermination)
	}

	if cause := context.Cause(sessionCtx); cause != nil {
		return execution.OutcomeFromError(cause)
	}
	if waitErr == nil {
		return execution.Outcome{}
	}
	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) {
		return execution.Outcome{ExitCode: exitErr.ExitCode()}
	}
	return execution.OutcomeFromError(fmt.Errorf("wait for command: %w", waitErr))
}

type cancellingWriter struct {
	name                  string
	destination           io.Writer
	cancel                context.CancelCauseFunc
	terminationDispatched <-chan struct{}
}

func (w cancellingWriter) Write(data []byte) (int, error) {
	destination := w.destination
	if destination == nil {
		destination = io.Discard
	}
	n, err := destination.Write(data)
	if err != nil {
		err = fmt.Errorf("copy command %s: %w", w.name, err)
		w.cancel(err)
		if w.terminationDispatched != nil {
			<-w.terminationDispatched
		}
	}
	return n, err
}

func (s Supervisor) terminateProcessGroup(cmd *exec.Cmd, waitDone <-chan error, signalTermination func()) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	signalTermination()
	grace := s.TerminationGrace
	if grace <= 0 {
		grace = TerminationGrace
	}
	timer := time.NewTimer(grace)
	defer timer.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	var waitErr error
	waited := false
	for {
		if waited && !processGroupAlive(cmd.Process.Pid) {
			return waitErr
		}
		select {
		case waitErr = <-waitDone:
			waited = true
		case <-ticker.C:
		case <-timer.C:
			_ = unix.Kill(-cmd.Process.Pid, unix.SIGKILL)
			if !waited {
				_ = cmd.Process.Kill()
				waitErr = <-waitDone
			}
			return waitErr
		}
	}
}

func processGroupAlive(processGroupID int) bool {
	err := unix.Kill(-processGroupID, 0)
	return err == nil || errors.Is(err, unix.EPERM)
}

func closeReader(reader io.Reader) {
	if closer, ok := reader.(io.Closer); ok {
		_ = closer.Close()
	}
}

func shellCommand(command string) (*exec.Cmd, func(), error) {
	if len(command) <= MaxInlineShellCommandBytes {
		return exec.Command("/bin/sh", "-lc", command), func() {}, nil
	}
	file, err := os.CreateTemp("", "kiln-vm-agent-exec-*.sh")
	if err != nil {
		return nil, func() {}, fmt.Errorf("stage exec command: %w", err)
	}
	path := file.Name()
	cleanup := func() { _ = os.Remove(path) }
	if _, err := file.WriteString(command); err != nil {
		_ = file.Close()
		cleanup()
		return nil, func() {}, fmt.Errorf("write exec command: %w", err)
	}
	if err := file.Close(); err != nil {
		cleanup()
		return nil, func() {}, fmt.Errorf("close exec command: %w", err)
	}
	return exec.Command("/bin/sh", "-lc", ". "+shellQuote(path)), cleanup, nil
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}
