package guestexec

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/gfdelarue/kiln-vm/internal/execution"
)

func TestSupervisorStreamsAndReturnsNonzeroExit(t *testing.T) {
	var stdout, stderr bytes.Buffer
	started := false
	outcome := (Supervisor{}).Execute(context.Background(), execution.Request{
		Command: "printf out; printf err >&2; exit 7",
		Stdout:  &stdout,
		Stderr:  &stderr,
		Started: func() error {
			started = true
			return nil
		},
	})
	if !started || !strings.HasPrefix(stdout.String(), "out") || stderr.String() != "err" || outcome.ExitCode != 7 || outcome.Category != execution.CategoryNone {
		t.Fatalf("started=%v stdout=%q stderr=%q outcome=%#v", started, stdout.String(), stderr.String(), outcome)
	}
}

func TestSupervisorCancellationRunsTrapAndReapsProcessGroup(t *testing.T) {
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "pid")
	childPIDPath := filepath.Join(dir, "child-pid")
	cleanupPath := filepath.Join(dir, "cleaned")
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	done := make(chan execution.Outcome, 1)
	go func() {
		done <- (Supervisor{}).Execute(ctx, execution.Request{
			Command: fmt.Sprintf(
				"trap 'printf cleaned > %s; exit 0' TERM; printf %%s $$ > %s; "+
					"(trap '' TERM; while :; do sleep 1; done) & child=$!; printf %%s \"$child\" > %s; wait \"$child\"",
				shellQuote(cleanupPath),
				shellQuote(pidPath),
				shellQuote(childPIDPath),
			),
			Started: func() error {
				close(started)
				return nil
			},
		})
	}()
	<-started
	var pid, childPID int
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		body, err := os.ReadFile(pidPath)
		if err == nil {
			pid, _ = strconv.Atoi(strings.TrimSpace(string(body)))
			if pid > 1 {
				childBody, childErr := os.ReadFile(childPIDPath)
				if childErr == nil {
					childPID, _ = strconv.Atoi(strings.TrimSpace(string(childBody)))
					if childPID > 1 {
						break
					}
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	outcome := <-done
	if outcome.Category != execution.CategoryCancelled {
		t.Fatalf("outcome = %#v", outcome)
	}
	if body, err := os.ReadFile(cleanupPath); err != nil || string(body) != "cleaned" {
		t.Fatalf("cleanup marker = %q, %v", body, err)
	}
	for _, process := range []struct {
		name string
		pid  int
	}{{name: "process", pid: pid}, {name: "descendant", pid: childPID}} {
		deadline := time.Now().Add(time.Second)
		for process.pid > 1 && time.Now().Before(deadline) && !errors.Is(unix.Kill(process.pid, 0), unix.ESRCH) {
			time.Sleep(10 * time.Millisecond)
		}
		if process.pid > 1 && !errors.Is(unix.Kill(process.pid, 0), unix.ESRCH) {
			t.Fatalf("%s %d is still alive", process.name, process.pid)
		}
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) {
	return 0, errors.New("sink failed")
}

func TestSupervisorOutputSinkFailureCancelsExecution(t *testing.T) {
	dir := t.TempDir()
	startedPath := filepath.Join(dir, "started")
	cleanupPath := filepath.Join(dir, "cleaned")
	outcome := (Supervisor{TerminationGrace: 100 * time.Millisecond}).Execute(context.Background(), execution.Request{
		Command: fmt.Sprintf(
			"trap 'printf cleaned > %s; exit 0' TERM; printf started > %s; printf output; while :; do :; done",
			shellQuote(cleanupPath),
			shellQuote(startedPath),
		),
		Stdout: failingWriter{},
		Started: func() error {
			deadline := time.Now().Add(time.Second)
			for time.Now().Before(deadline) {
				if _, err := os.Stat(startedPath); err == nil {
					return nil
				}
				time.Sleep(time.Millisecond)
			}
			return errors.New("command did not start")
		},
	})
	if outcome.Category != execution.CategoryInternal || !strings.Contains(outcome.Message, "sink failed") {
		t.Fatalf("outcome = %#v", outcome)
	}
	if body, err := os.ReadFile(cleanupPath); err != nil || string(body) != "cleaned" {
		t.Fatalf("cleanup marker = %q, %v", body, err)
	}
}
