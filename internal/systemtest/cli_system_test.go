//go:build system

package systemtest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gfdelarue/kiln-vm/internal/models"
	"github.com/gfdelarue/kiln-vm/internal/refid"
)

func TestCLIPersistentVMWorkflow(t *testing.T) {
	client := requireClient(t)
	cleanupResourcesByPrefix(t, client)

	xdgConfigHome := t.TempDir()
	stdout, stderr, err := runCLI(t, xdgConfigHome, "login", "--api-url", os.Getenv("KILN_VM_API_URL"), "--token", os.Getenv("KILN_VM_TOKEN"))
	if err != nil {
		t.Fatalf("kiln-vm login failed: %v stdout=%q stderr=%q", err, stdout, stderr)
	}
	if !strings.Contains(stdout, filepath.Join(xdgConfigHome, "kiln-vm", "config.json")) {
		t.Fatalf("login did not write inside test XDG_CONFIG_HOME: stdout=%q", stdout)
	}

	stdout, stderr, err = runCLI(t, xdgConfigHome, "profiles", "--json")
	if err != nil {
		t.Fatalf("kiln-vm profiles failed: %v stdout=%q stderr=%q", err, stdout, stderr)
	}
	if !strings.Contains(stdout, `"alpine"`) {
		t.Fatalf("profiles missing alpine: stdout=%q", stdout)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	profileImagesBefore := snapshotVMProfileImages(t, client, ctx)
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		cleanupNewVMProfileImages(t, client, cleanupCtx, profileImagesBefore)
	})

	stdout, stderr, err = runCLI(t, xdgConfigHome, "images", "ensure", "alpine", "--disk", "128M", "--json")
	if err != nil {
		t.Fatalf("kiln-vm images ensure failed: %v stdout=%q stderr=%q", err, stdout, stderr)
	}
	var profileImage models.VMProfileImage
	if err := json.Unmarshal([]byte(stdout), &profileImage); err != nil {
		t.Fatalf("parse VM profile image JSON %q: %v", stdout, err)
	}
	if profileImage.Profile != "alpine" || profileImage.ImageID == "" {
		t.Fatalf("unexpected VM profile image: %+v", profileImage)
	}
	stdout, stderr, err = runCLI(t, xdgConfigHome, "images", "list")
	if err != nil {
		t.Fatalf("kiln-vm images list failed: %v stdout=%q stderr=%q", err, stdout, stderr)
	}
	profileImages, err := client.listVMProfileImages(ctx)
	if err != nil {
		t.Fatalf("list VM profile images for abbreviation: %v", err)
	}
	profileImageIDs := make([]string, 0, len(profileImages))
	for _, image := range profileImages {
		profileImageIDs = append(profileImageIDs, image.ID)
	}
	profileImageRef := refid.AbbreviateAll(profileImageIDs, refid.DefaultDisplayLength)[profileImage.ID]
	if !strings.Contains(stdout, profileImageRef) {
		t.Fatalf("images list missing ensured image %s/%s: stdout=%q", profileImage.ID, profileImageRef, stdout)
	}
	if strings.Contains(stdout, profileImage.ID) {
		t.Fatalf("images list printed full profile image ID without --wide: stdout=%q", stdout)
	}
	stdout, stderr, err = runCLI(t, xdgConfigHome, "images", "list", "--wide")
	if err != nil {
		t.Fatalf("kiln-vm images list --wide failed: %v stdout=%q stderr=%q", err, stdout, stderr)
	}
	if !strings.Contains(stdout, profileImage.ID) {
		t.Fatalf("images list --wide missing ensured image %s: stdout=%q", profileImage.ID, stdout)
	}

	name := uniqueName("cli-vm")
	stdout, stderr, err = runCLI(t, xdgConfigHome,
		"create", name,
		"--profile", "alpine",
		"--disk", "128M",
		"--cpu", "1",
		"--memory-mb", "512",
		"--power", "on_demand",
		"--json",
	)
	if err != nil {
		t.Fatalf("kiln-vm create failed: %v stdout=%q stderr=%q", err, stdout, stderr)
	}
	var vm models.VM
	if err := json.Unmarshal([]byte(stdout), &vm); err != nil {
		t.Fatalf("parse VM JSON %q: %v", stdout, err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := client.deleteVM(cleanupCtx, name); err != nil {
			var statusErr *apiStatusError
			if !errors.As(err, &statusErr) || statusErr.StatusCode != http.StatusNotFound {
				t.Logf("cleanup VM %s: %v", name, err)
			}
		}
	})

	if _, err := client.waitForVMState(ctx, name, models.MachineStateRunning); err != nil {
		t.Fatalf("wait for VM running: %v", err)
	}
	t.Run("streams output before completion", func(t *testing.T) {
		assertCLIExecStreamsBeforeCompletion(t, xdgConfigHome, name)
	})
	t.Run("runs beyond the generic request timeout", func(t *testing.T) {
		assertCLIExecRunsBeyondGenericTimeout(t, xdgConfigHome, name)
	})
	t.Run("cancels the exact guest execution", func(t *testing.T) {
		assertCLIExecCancellationCleansGuest(t, xdgConfigHome, name)
	})

	stdinPayload := strings.Repeat("kiln-vm-exec-stdin-payload\n", 8192)
	stdinDigest := sha256.Sum256([]byte(stdinPayload))
	stdout, stderr, err = runCLIWithStdin(t, xdgConfigHome, strings.NewReader(stdinPayload),
		"exec", name, "--", "sh", "-lc", "tee /tmp/kiln-vm-exec-stdin | wc -c; sha256sum /tmp/kiln-vm-exec-stdin | cut -d' ' -f1; rm -f /tmp/kiln-vm-exec-stdin",
	)
	if err != nil {
		t.Fatalf("kiln-vm exec stdin failed: %v stdout=%q stderr=%q", err, stdout, stderr)
	}
	wantStdinSummary := fmt.Sprintf("%d\n%x\n", len(stdinPayload), stdinDigest)
	if stdout != wantStdinSummary {
		t.Fatalf("kiln-vm exec stdin summary = %q, want %q; stderr=%q", stdout, wantStdinSummary, stderr)
	}
	stdout, stderr, err = runCLI(t, xdgConfigHome, "exec", name, "--", "sh", "-lc", "printf cli-vm-ok >/root/cli-vm.txt && sync")
	if err != nil {
		t.Fatalf("kiln-vm exec write failed: %v stdout=%q stderr=%q", err, stdout, stderr)
	}

	stdout, stderr, err = runCLI(t, xdgConfigHome, "stop", name)
	if err != nil {
		t.Fatalf("kiln-vm stop failed: %v stdout=%q stderr=%q", err, stdout, stderr)
	}
	if _, err := client.waitForVMState(ctx, name, models.MachineStateStopped); err != nil {
		t.Fatalf("wait for VM stopped: %v", err)
	}

	stdout, stderr, err = runCLI(t, xdgConfigHome, "exec", name, "--", "cat", "/root/cli-vm.txt")
	if err != nil {
		t.Fatalf("kiln-vm exec wake failed: %v stdout=%q stderr=%q", err, stdout, stderr)
	}
	if !strings.Contains(stdout, "cli-vm-ok") {
		t.Fatalf("exec output missing marker: stdout=%q stderr=%q", stdout, stderr)
	}

	stdout, stderr, err = runCLI(t, xdgConfigHome, "delete", name)
	if err != nil {
		t.Fatalf("kiln-vm delete failed: %v stdout=%q stderr=%q", err, stdout, stderr)
	}
}

func assertCLIExecRunsBeyondGenericTimeout(t *testing.T, xdgConfigHome, vmName string) {
	t.Helper()

	startedAt := time.Now()
	stdout, stderr, err := runCLI(
		t,
		xdgConfigHome,
		"exec", vmName, "--", "sh", "-lc",
		"printf 'kiln-vm-exec-long-start\\n'; sleep 62; printf 'kiln-vm-exec-long-done\\n'",
	)
	elapsed := time.Since(startedAt)
	if err != nil {
		t.Fatalf("long-running exec failed after %s: %v stdout=%q stderr=%q", elapsed, err, stdout, stderr)
	}
	if elapsed < 60*time.Second {
		t.Fatalf("long-running exec completed in %s, want at least 60s", elapsed)
	}
	if stdout != "kiln-vm-exec-long-start\nkiln-exec-long-done\n" || stderr != "" {
		t.Fatalf("long-running exec output: stdout=%q stderr=%q", stdout, stderr)
	}
}

func assertCLIExecCancellationCleansGuest(t *testing.T, xdgConfigHome, vmName string) {
	t.Helper()

	base := fmt.Sprintf("/tmp/kiln-vm-exec-cancel-%d", time.Now().UnixNano())
	pidPath := base + ".pid"
	residuePath := base + ".credential"
	cleanupPath := base + ".cleaned"
	command := strings.Join([]string{
		"set -eu",
		"watchdog=",
		"cleanup() {",
		"  if [ -n \"$watchdog\" ]; then kill \"$watchdog\" >/dev/null 2>&1 || true; fi",
		"  rm -f " + shellQuote(pidPath) + " " + shellQuote(residuePath),
		"  printf cleaned > " + shellQuote(cleanupPath),
		"  exit 0",
		"}",
		"trap cleanup TERM INT",
		"printf '%s\\n' \"$$\" > " + shellQuote(pidPath),
		"printf credential > " + shellQuote(residuePath),
		"(sleep 8; kill -TERM \"$$\") &",
		"watchdog=$!",
		"while :; do sleep 1; done",
	}, "\n")

	cmd := newCLICommand(t, xdgConfigHome, "exec", vmName, "--", "sh", "-lc", command)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start cancellable exec: %v", err)
	}
	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()

	var guestPID int
	startDeadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(startDeadline) {
		pidOutput, _, err := runCLI(t, xdgConfigHome, "exec", vmName, "--", "cat", pidPath)
		if err == nil {
			pid, parseErr := strconv.Atoi(strings.TrimSpace(pidOutput))
			if parseErr == nil && pid > 1 {
				guestPID = pid
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if guestPID == 0 {
		_ = cmd.Process.Kill()
		<-waitDone
		t.Fatalf("cancellable guest exec did not publish its PID: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	t.Cleanup(func() {
		_, _, _ = runCLI(t, xdgConfigHome, "exec", vmName, "--", "rm", "-f", pidPath, residuePath, cleanupPath)
	})

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal kiln-vm CLI: %v", err)
	}
	var waitErr error
	select {
	case waitErr = <-waitDone:
	case <-time.After(3 * time.Second):
		_ = cmd.Process.Kill()
		waitErr = <-waitDone
		t.Fatalf("kiln-vm CLI did not retain normal SIGTERM behavior: %v", waitErr)
	}
	waitStatus, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
	if waitErr == nil || !ok || !waitStatus.Signaled() || waitStatus.Signal() != syscall.SIGTERM {
		t.Fatalf("kiln-vm CLI wait status = %v (%v), want SIGTERM; stdout=%q stderr=%q", waitStatus, waitErr, stdout.String(), stderr.String())
	}

	cleaned := false
	cleanupDeadline := time.Now().Add(3 * time.Second)
	checkCommand := fmt.Sprintf(
		"if kill -0 %d 2>/dev/null || test -e %s || test ! -e %s; then exit 1; fi",
		guestPID,
		shellQuote(residuePath),
		shellQuote(cleanupPath),
	)
	for time.Now().Before(cleanupDeadline) {
		if _, _, err := runCLI(t, xdgConfigHome, "exec", vmName, "--", "sh", "-lc", checkCommand); err == nil {
			cleaned = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if cleaned {
		return
	}

	_, _, _ = runCLI(
		t,
		xdgConfigHome,
		"exec", vmName, "--", "sh", "-lc",
		fmt.Sprintf("kill -TERM %d 2>/dev/null || true", guestPID),
	)
	time.Sleep(time.Second)
	t.Errorf("SIGTERM ended the kiln-vm client but left guest PID %d or cleanup-sensitive residue running", guestPID)
}

func assertCLIExecStreamsBeforeCompletion(t *testing.T, xdgConfigHome, vmName string) {
	t.Helper()

	cmd := newCLICommand(
		t,
		xdgConfigHome,
		"exec", vmName, "--", "sh", "-lc",
		"printf 'kiln-vm-exec-stream-start\\n'; sleep 4; printf 'kiln-vm-exec-stream-done\\n'",
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("open streaming exec stdout: %v", err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start streaming exec: %v", err)
	}

	var output bytes.Buffer
	firstOutput := make(chan struct{})
	outputDone := make(chan error, 1)
	go func() {
		buffer := make([]byte, 4096)
		signaled := false
		for {
			n, readErr := stdout.Read(buffer)
			if n > 0 {
				_, _ = output.Write(buffer[:n])
				if !signaled && strings.Contains(output.String(), "kiln-vm-exec-stream-start\n") {
					close(firstOutput)
					signaled = true
				}
			}
			if readErr != nil {
				outputDone <- readErr
				return
			}
		}
	}()
	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()

	select {
	case <-firstOutput:
	case waitErr := <-waitDone:
		<-outputDone
		t.Fatalf("streaming exec exited before exposing its first marker: %v stdout=%q stderr=%q", waitErr, output.String(), stderr.String())
	case <-time.After(2 * time.Second):
		_ = cmd.Process.Kill()
		waitErr := <-waitDone
		<-outputDone
		t.Fatalf("streaming exec exposed no stdout before command completion: %v stdout=%q stderr=%q", waitErr, output.String(), stderr.String())
	}

	select {
	case waitErr := <-waitDone:
		<-outputDone
		t.Fatalf("streaming exec completed before its four-second workload: %v stdout=%q stderr=%q", waitErr, output.String(), stderr.String())
	default:
	}

	select {
	case waitErr := <-waitDone:
		readErr := <-outputDone
		if waitErr != nil {
			t.Fatalf("streaming exec failed: %v stdout=%q stderr=%q", waitErr, output.String(), stderr.String())
		}
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			t.Fatalf("read streaming exec stdout: %v", readErr)
		}
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		<-waitDone
		<-outputDone
		t.Fatal("streaming exec did not complete")
	}
	if got := output.String(); !strings.Contains(got, "kiln-vm-exec-stream-done\n") {
		t.Fatalf("streaming exec stdout = %q, want completion marker", got)
	}
	if stderr.Len() != 0 {
		t.Fatalf("streaming exec stderr = %q, want empty", stderr.String())
	}
}

func TestCLIConfigFailuresStayInsideTestConfigHome(t *testing.T) {
	requireClient(t)

	t.Run("missing config", func(t *testing.T) {
		stdout, stderr, err := runCLI(t, t.TempDir(), "list")
		if err == nil {
			t.Fatalf("expected missing config to fail: stdout=%q stderr=%q", stdout, stderr)
		}
		if !strings.Contains(stderr, "API URL not configured") {
			t.Fatalf("expected missing API URL error, stdout=%q stderr=%q", stdout, stderr)
		}
	})

	t.Run("malformed config", func(t *testing.T) {
		xdgConfigHome := t.TempDir()
		writeMalformedCLIConfig(t, xdgConfigHome)
		stdout, stderr, err := runCLI(t, xdgConfigHome, "list")
		if err == nil {
			t.Fatalf("expected malformed config to fail: stdout=%q stderr=%q", stdout, stderr)
		}
		if !strings.Contains(stderr, "parse config") {
			t.Fatalf("expected parse config error, stdout=%q stderr=%q", stdout, stderr)
		}
	})

	t.Run("bad API URL", func(t *testing.T) {
		xdgConfigHome := t.TempDir()
		stdout, stderr, err := runCLI(t, xdgConfigHome, "login", "--api-url", "http://127.0.0.1:1", "--token", os.Getenv("KILN_VM_TOKEN"))
		if err != nil {
			t.Fatalf("login failed: %v stdout=%q stderr=%q", err, stdout, stderr)
		}
		stdout, stderr, err = runCLI(t, xdgConfigHome, "list")
		if err == nil {
			t.Fatalf("expected bad API URL to fail: stdout=%q stderr=%q", stdout, stderr)
		}
		if !strings.Contains(stderr, "request failed") {
			t.Fatalf("expected request failed error, stdout=%q stderr=%q", stdout, stderr)
		}
	})

	t.Run("bad token", func(t *testing.T) {
		xdgConfigHome := t.TempDir()
		stdout, stderr, err := runCLI(t, xdgConfigHome, "login", "--api-url", os.Getenv("KILN_VM_API_URL"), "--token", "kiln_vm_invalid_systemtest_token")
		if err != nil {
			t.Fatalf("login failed: %v stdout=%q stderr=%q", err, stdout, stderr)
		}
		stdout, stderr, err = runCLI(t, xdgConfigHome, "list")
		if err == nil {
			t.Fatalf("expected bad token to fail: stdout=%q stderr=%q", stdout, stderr)
		}
		if !strings.Contains(stderr, "API error") {
			t.Fatalf("expected API error, stdout=%q stderr=%q", stdout, stderr)
		}
	})
}
