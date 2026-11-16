package machine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gfdelarue/kiln-vm/internal/dnsprobe"
	"github.com/gfdelarue/kiln-vm/internal/execution"
	"github.com/gfdelarue/kiln-vm/internal/models"
)

func TestGatewayDNSReadinessStagesCurrentAgentOverCompatibleUploadExec(t *testing.T) {
	runtime := &gatewayDNSProbeRuntimeFake{}
	runtime.exec = func(context.Context, string, string) (*execution.BufferedResult, error) {
		return gatewayDNSExecResult(t, gatewayDNSSuccessResult(), dnsprobe.ExitOK), nil
	}
	controller := newGatewayDNSReadinessController(runtime, "/host/current/kiln-vm-agent", "probe.example.")
	loads := 0
	controller.loadAgent = func() ([]byte, error) {
		loads++
		return []byte("current-kiln-vm-agent"), nil
	}
	machine := gatewayDNSReadinessMachine()

	first, err := controller.check(context.Background(), machine, true)
	if err != nil {
		t.Fatalf("first check: %v", err)
	}
	second, err := controller.check(context.Background(), machine, true)
	if err != nil {
		t.Fatalf("second check: %v", err)
	}
	if !first.Ready || !second.Ready || first.Detail != gatewayDNSDetailReady {
		t.Fatalf("statuses = first %+v second %+v, want ready", first, second)
	}
	if len(runtime.uploads) != 1 {
		t.Fatalf("uploads = %d, want one cached machine+hash stage", len(runtime.uploads))
	}
	if loads != 1 {
		t.Fatalf("agent loads = %d, want one immutable daemon-lifetime load", loads)
	}
	upload := runtime.uploads[0]
	if upload.machineID != machine.ID || upload.mode != 0700 || upload.payload != "current-kiln-vm-agent" || upload.path != gatewayDNSProbeRuntimePath {
		t.Fatalf("upload = %+v", upload)
	}
	if len(runtime.commands) != 2 {
		t.Fatalf("exec commands = %d, want 2", len(runtime.commands))
	}
	for _, command := range runtime.commands {
		if !strings.Contains(command, upload.path) || !strings.Contains(command, "'dns-probe'") || strings.Contains(command, "/sbin/kiln-vm-agent") || strings.Contains(command, "/host/current/kiln-vm-agent") {
			t.Fatalf("exec command did not use staged current agent: %q", command)
		}
	}
}

func TestGatewayDNSReadinessCachesAgentAndBoundsStagingToStableTarget(t *testing.T) {
	runtime := &gatewayDNSProbeRuntimeFake{exec: func(context.Context, string, string) (*execution.BufferedResult, error) {
		return gatewayDNSExecResult(t, gatewayDNSSuccessResult(), dnsprobe.ExitOK), nil
	}}
	controller := newGatewayDNSReadinessController(runtime, "unused", "probe.example.")
	payload := []byte("agent-v1")
	loads := 0
	controller.loadAgent = func() ([]byte, error) {
		loads++
		return append([]byte(nil), payload...), nil
	}
	machine := gatewayDNSReadinessMachine()

	if _, err := controller.check(context.Background(), machine, true); err != nil {
		t.Fatalf("v1 check: %v", err)
	}
	payload = []byte("agent-v2")
	if _, err := controller.check(context.Background(), machine, true); err != nil {
		t.Fatalf("same-daemon check after source mutation: %v", err)
	}
	if loads != 1 || len(runtime.uploads) != 1 || runtime.uploads[0].payload != "agent-v1" {
		t.Fatalf("loads=%d uploads=%+v, want immutable cached v1 payload", loads, runtime.uploads)
	}

	// A new controller models a kiln-vmd upgrade. It loads the new immutable agent
	// once and atomically replaces the same bounded guest target.
	restarted := newGatewayDNSReadinessController(runtime, "unused", "probe.example.")
	restarted.loadAgent = func() ([]byte, error) { return append([]byte(nil), payload...), nil }
	if _, err := restarted.check(context.Background(), machine, true); err != nil {
		t.Fatalf("new-daemon v2 check: %v", err)
	}
	if len(runtime.uploads) != 2 || runtime.uploads[1].payload != "agent-v2" {
		t.Fatalf("uploads = %+v, want one replacement with v2 payload", runtime.uploads)
	}
	for _, upload := range runtime.uploads {
		if upload.path != gatewayDNSProbeRuntimePath {
			t.Fatalf("upload path = %q, want stable bounded target %q", upload.path, gatewayDNSProbeRuntimePath)
		}
	}
}

func TestGatewayDNSReadinessStagesIndependentMachinesConcurrently(t *testing.T) {
	output := gatewayDNSJSON(t, gatewayDNSSuccessResult())
	runtime := &blockingGatewayDNSProbeRuntime{
		started: make(chan string, 2),
		release: make(chan struct{}),
		output:  output,
	}
	controller := newGatewayDNSReadinessController(runtime, "unused", "probe.example.")
	var loads atomic.Int32
	controller.loadAgent = func() ([]byte, error) {
		loads.Add(1)
		return []byte("agent"), nil
	}
	first := gatewayDNSReadinessMachine()
	second := gatewayDNSReadinessMachine()
	second.ID = "machine-dns-ready-2"
	errs := make(chan error, 2)
	go func() {
		_, err := controller.check(context.Background(), first, true)
		errs <- err
	}()
	go func() {
		_, err := controller.check(context.Background(), second, true)
		errs <- err
	}()

	seen := map[string]bool{}
	for len(seen) < 2 {
		select {
		case machineID := <-runtime.started:
			seen[machineID] = true
		case <-time.After(time.Second):
			t.Fatalf("only %d independent uploads started before release; staging was serialized", len(seen))
		}
	}
	close(runtime.release)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent check: %v", err)
		}
	}
	if got := loads.Load(); got != 1 {
		t.Fatalf("concurrent agent loads = %d, want one cached/singleflight load", got)
	}
}

func TestGatewayDNSReadinessReuploadsAfterGuestRuntimeLoss(t *testing.T) {
	runtime := &gatewayDNSProbeRuntimeFake{}
	results := []*execution.BufferedResult{
		gatewayDNSExecResult(t, gatewayDNSSuccessResult(), dnsprobe.ExitOK),
		{ExitCode: 127, Stderr: "staged file was removed"},
		gatewayDNSExecResult(t, gatewayDNSSuccessResult(), dnsprobe.ExitOK),
	}
	runtime.exec = func(context.Context, string, string) (*execution.BufferedResult, error) {
		result := results[0]
		results = results[1:]
		return result, nil
	}
	controller := newGatewayDNSReadinessController(runtime, "unused", "probe.example.")
	controller.loadAgent = func() ([]byte, error) { return []byte("agent"), nil }
	machine := gatewayDNSReadinessMachine()

	if _, err := controller.check(context.Background(), machine, true); err != nil {
		t.Fatalf("initial check: %v", err)
	}
	if _, err := controller.check(context.Background(), machine, true); err != nil {
		t.Fatalf("check after staged runtime loss: %v", err)
	}
	if len(runtime.uploads) != 2 || len(runtime.commands) != 3 {
		t.Fatalf("uploads=%d execs=%d, want one bounded restage retry", len(runtime.uploads), len(runtime.commands))
	}
	for _, upload := range runtime.uploads {
		if upload.path != gatewayDNSProbeRuntimePath {
			t.Fatalf("restage path = %q, want stable target %q", upload.path, gatewayDNSProbeRuntimePath)
		}
	}
}

func TestGatewayDNSReadinessRestagesOnceAfterUnavailableOrInvalidProbe(t *testing.T) {
	tests := []struct {
		name  string
		first func() (*execution.BufferedResult, error)
	}{
		{
			name: "exec unavailable",
			first: func() (*execution.BufferedResult, error) {
				return nil, errors.New("raw guest transport failure")
			},
		},
		{
			name: "invalid structured output",
			first: func() (*execution.BufferedResult, error) {
				return &execution.BufferedResult{ExitCode: dnsprobe.ExitOK, Stdout: `{"schema_version":1,"secret":"raw-output"}`}, nil
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runtime := &gatewayDNSProbeRuntimeFake{}
			execs := 0
			runtime.exec = func(context.Context, string, string) (*execution.BufferedResult, error) {
				execs++
				if execs == 1 {
					return test.first()
				}
				return gatewayDNSExecResult(t, gatewayDNSSuccessResult(), dnsprobe.ExitOK), nil
			}
			controller := newGatewayDNSReadinessController(runtime, "unused", "probe.example.")
			controller.loadAgent = func() ([]byte, error) { return []byte("agent"), nil }

			status, err := controller.check(context.Background(), gatewayDNSReadinessMachine(), true)
			if err != nil || !status.Ready || status.Detail != gatewayDNSDetailReady {
				t.Fatalf("status=%+v error=%v, want successful bounded self-heal", status, err)
			}
			if len(runtime.uploads) != 2 || len(runtime.commands) != 2 {
				t.Fatalf("uploads=%d execs=%d, want one forced restage and one retry", len(runtime.uploads), len(runtime.commands))
			}
			if runtime.uploads[0].path != gatewayDNSProbeRuntimePath || runtime.uploads[1].path != gatewayDNSProbeRuntimePath {
				t.Fatalf("uploads = %+v, want stable atomic target", runtime.uploads)
			}
		})
	}
}

func TestGatewayDNSReadinessRetriesFailedAgentLoadWithoutCachingIt(t *testing.T) {
	runtime := &gatewayDNSProbeRuntimeFake{exec: func(context.Context, string, string) (*execution.BufferedResult, error) {
		return gatewayDNSExecResult(t, gatewayDNSSuccessResult(), dnsprobe.ExitOK), nil
	}}
	controller := newGatewayDNSReadinessController(runtime, "unused", "probe.example.")
	loads := 0
	controller.loadAgent = func() ([]byte, error) {
		loads++
		if loads == 1 {
			return nil, errors.New("raw transient read failure")
		}
		return []byte("agent"), nil
	}
	machine := gatewayDNSReadinessMachine()

	first, err := controller.check(context.Background(), machine, true)
	if !errors.Is(err, ErrGatewayDNSProbe) || first.Detail != gatewayDNSDetailProbeUnavailable {
		t.Fatalf("first status=%+v error=%v, want sanitized probe-unavailable failure", first, err)
	}
	second, err := controller.check(context.Background(), machine, true)
	if err != nil || !second.Ready {
		t.Fatalf("second status=%+v error=%v, want successful load retry", second, err)
	}
	if loads != 2 || len(runtime.uploads) != 1 {
		t.Fatalf("loads=%d uploads=%d, want failed load uncached and successful load cached", loads, len(runtime.uploads))
	}
}

func TestGatewayDNSReadinessClassifiesFailuresAndPreservesLastSuccess(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	runtime := &gatewayDNSProbeRuntimeFake{}
	results := []dnsprobe.Result{
		gatewayDNSSuccessResult(),
		gatewayDNSPathFailureResult("sensitive raw network failure"),
		gatewayDNSResolutionFailureResult("sensitive raw resolver failure"),
	}
	runtime.exec = func(context.Context, string, string) (*execution.BufferedResult, error) {
		result := results[0]
		results = results[1:]
		return gatewayDNSExecResult(t, result, result.ExitCode()), nil
	}
	controller := newGatewayDNSReadinessController(runtime, "unused", "probe.example.")
	controller.loadAgent = func() ([]byte, error) { return []byte("agent"), nil }
	controller.now = func() time.Time {
		current := now
		now = now.Add(time.Minute)
		return current
	}
	machine := gatewayDNSReadinessMachine()

	success, err := controller.check(context.Background(), machine, true)
	if err != nil {
		t.Fatalf("success check: %v", err)
	}
	path, err := controller.check(context.Background(), machine, true)
	if !errors.Is(err, ErrGatewayDNSPath) {
		t.Fatalf("path error = %v, want ErrGatewayDNSPath", err)
	}
	if path.Ready || path.PathOK || path.FailureKind != "path" || path.Transport != "udp" || path.Phase != "read" || path.Detail != gatewayDNSDetailPathFailed {
		t.Fatalf("path status = %+v", path)
	}
	if !path.LastSuccessAt.Equal(success.CheckedAt) {
		t.Fatalf("path last success = %s, want %s", path.LastSuccessAt, success.CheckedAt)
	}
	resolution, err := controller.check(context.Background(), machine, true)
	if !errors.Is(err, ErrGatewayDNSResolution) {
		t.Fatalf("resolution error = %v, want ErrGatewayDNSResolution", err)
	}
	if resolution.Ready || !resolution.PathOK || resolution.ResolutionOK || resolution.FailureKind != "resolution" || resolution.Transport != "udp" || resolution.Phase != "resolve" || resolution.RCodeName != "SERVFAIL" || resolution.Detail != gatewayDNSDetailResolutionFailed {
		t.Fatalf("resolution status = %+v", resolution)
	}
	if !resolution.LastSuccessAt.Equal(success.CheckedAt) {
		t.Fatalf("resolution last success = %s, want %s", resolution.LastSuccessAt, success.CheckedAt)
	}
	if len(runtime.uploads) != 1 || len(runtime.commands) != 3 {
		t.Fatalf("uploads=%d execs=%d, valid path/resolution results must not trigger restaging", len(runtime.uploads), len(runtime.commands))
	}
	for _, exposed := range []string{gatewayDNSReadinessStatusString(path), gatewayDNSReadinessStatusString(resolution), err.Error()} {
		for _, forbidden := range []string{"sensitive", machine.GatewayIP, "probe.example", gatewayDNSProbeRuntimeDir} {
			if strings.Contains(exposed, forbidden) {
				t.Fatalf("sanitized state exposed %q in %q", forbidden, exposed)
			}
		}
	}
}

func TestDecodeGatewayDNSProbeResultRejectsMalformedOrInconsistentOutput(t *testing.T) {
	valid := gatewayDNSSuccessResult()
	validJSON := gatewayDNSJSON(t, valid)
	tests := []struct {
		name     string
		stdout   string
		stderr   string
		exitCode int
		server   string
		probe    string
	}{
		{name: "empty", exitCode: dnsprobe.ExitOK, server: valid.Server, probe: valid.Name},
		{name: "oversized", stdout: strings.Repeat("x", gatewayDNSProbeOutputLimit+1), exitCode: dnsprobe.ExitOK, server: valid.Server, probe: valid.Name},
		{name: "unknown field", stdout: strings.TrimSuffix(strings.TrimSpace(validJSON), "}") + ",\"unexpected\":true}\n", exitCode: dnsprobe.ExitOK, server: valid.Server, probe: valid.Name},
		{name: "trailing object", stdout: validJSON + "{}\n", exitCode: dnsprobe.ExitOK, server: valid.Server, probe: valid.Name},
		{name: "stderr", stdout: validJSON, stderr: "raw shell failure", exitCode: dnsprobe.ExitOK, server: valid.Server, probe: valid.Name},
		{name: "exit mismatch", stdout: validJSON, exitCode: dnsprobe.ExitPath, server: valid.Server, probe: valid.Name},
		{name: "server mismatch", stdout: validJSON, exitCode: dnsprobe.ExitOK, server: "10.231.0.9:53", probe: valid.Name},
		{name: "name mismatch", stdout: validJSON, exitCode: dnsprobe.ExitOK, server: valid.Server, probe: "other.example."},
	}
	versionMismatch := valid
	versionMismatch.SchemaVersion++
	tests = append(tests, struct {
		name     string
		stdout   string
		stderr   string
		exitCode int
		server   string
		probe    string
	}{name: "schema mismatch", stdout: gatewayDNSJSON(t, versionMismatch), exitCode: dnsprobe.ExitOK, server: valid.Server, probe: valid.Name})

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := decodeGatewayDNSProbeResult(test.stdout, test.stderr, test.exitCode, test.server, test.probe); err == nil {
				t.Fatal("decode succeeded, want strict rejection")
			}
		})
	}
}

func TestGatewayDNSReadinessMalformedOutputIsSanitized(t *testing.T) {
	runtime := &gatewayDNSProbeRuntimeFake{exec: func(context.Context, string, string) (*execution.BufferedResult, error) {
		return &execution.BufferedResult{ExitCode: 0, Stdout: `{"schema_version":1,"secret":"raw-output"}`}, nil
	}}
	controller := newGatewayDNSReadinessController(runtime, "unused", "probe.example.")
	controller.loadAgent = func() ([]byte, error) { return []byte("agent"), nil }
	machine := gatewayDNSReadinessMachine()

	status, err := controller.check(context.Background(), machine, true)
	if !errors.Is(err, ErrGatewayDNSProbe) {
		t.Fatalf("error = %v, want ErrGatewayDNSProbe", err)
	}
	if status.FailureKind != "probe" || status.Detail != gatewayDNSDetailProbeInvalid {
		t.Fatalf("status = %+v", status)
	}
	if len(runtime.uploads) != 2 || len(runtime.commands) != 2 {
		t.Fatalf("uploads=%d execs=%d, want exactly one forced restage before invalid-output failure", len(runtime.uploads), len(runtime.commands))
	}
	exposed := gatewayDNSReadinessStatusString(status) + err.Error()
	for _, forbidden := range []string{"raw-output", machine.GatewayIP, "probe.example", gatewayDNSProbeRuntimeDir} {
		if strings.Contains(exposed, forbidden) {
			t.Fatalf("sanitized failure exposed %q in %q", forbidden, exposed)
		}
	}
}

func TestGatewayDNSReadinessStatusCacheUsesCheckOrderNotWallClock(t *testing.T) {
	controller := newGatewayDNSReadinessController(&gatewayDNSProbeRuntimeFake{}, "unused", "probe.example.")
	machineID := "machine-check-order"
	newerTime := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	olderWallClock := newerTime.Add(time.Hour)

	olderCheck := controller.beginCheck(machineID)
	newerCheck := controller.beginCheck(machineID)
	newerCandidate := gatewayDNSStatusFromResult(gatewayDNSSuccessResult(), newerTime)
	newer, err := controller.finishCheck(machineID, newerCheck, newerCandidate)
	if err != nil || !newer.Ready {
		t.Fatalf("newer check status=%+v error=%v, want ready", newer, err)
	}

	// The older check finishes later and even carries a later wall-clock time.
	// It must return, but never replace, the newer-started check's state.
	olderCandidate := gatewayDNSStatusFromResult(gatewayDNSPathFailureResult("raw old failure"), olderWallClock)
	observed, err := controller.finishCheck(machineID, olderCheck, olderCandidate)
	if err != nil || !observed.Ready || !observed.CheckedAt.Equal(newerTime) {
		t.Fatalf("older completion observed status=%+v error=%v, want cached newer ready state", observed, err)
	}
	cached, ok := controller.cachedStatus(machineID)
	if !ok || !cached.Ready || !cached.CheckedAt.Equal(newerTime) || !cached.LastSuccessAt.Equal(newerTime) {
		t.Fatalf("cached status = %+v, present=%t", cached, ok)
	}

	// A genuinely newer check still wins if the system clock moves backwards,
	// while the last-success watermark remains monotonic.
	newestCheck := controller.beginCheck(machineID)
	backwardTime := newerTime.Add(-time.Hour)
	newestCandidate := gatewayDNSStatusFromResult(gatewayDNSPathFailureResult("raw current failure"), backwardTime)
	newest, err := controller.finishCheck(machineID, newestCheck, newestCandidate)
	if !errors.Is(err, ErrGatewayDNSPath) || newest.Ready || !newest.CheckedAt.Equal(backwardTime) {
		t.Fatalf("newest status=%+v error=%v, want newest path failure", newest, err)
	}
	if !newest.LastSuccessAt.Equal(newerTime) {
		t.Fatalf("last success = %s, want monotonic %s", newest.LastSuccessAt, newerTime)
	}
}

func TestGatewayDNSReadinessLifecycleEpochRejectsStaleCompletion(t *testing.T) {
	controller := newGatewayDNSReadinessController(&gatewayDNSProbeRuntimeFake{}, "unused", "probe.example.")
	machineID := "machine-lifecycle-epoch"
	lastSuccess := time.Date(2026, 7, 21, 9, 0, 0, 0, time.UTC)

	initial := controller.beginCheck(machineID)
	if _, err := controller.finishCheck(machineID, initial, gatewayDNSStatusFromResult(gatewayDNSSuccessResult(), lastSuccess)); err != nil {
		t.Fatalf("record initial success: %v", err)
	}
	stale := controller.beginCheck(machineID)
	controller.clearMachine(machineID, false)
	fresh := controller.beginCheck(machineID)
	freshTime := lastSuccess.Add(time.Minute)
	freshStatus, err := controller.finishCheck(machineID, fresh, gatewayDNSStatusFromResult(gatewayDNSPathFailureResult("raw fresh failure"), freshTime))
	if !errors.Is(err, ErrGatewayDNSPath) || freshStatus.Ready {
		t.Fatalf("fresh status=%+v error=%v, want path failure", freshStatus, err)
	}

	staleTime := freshTime.Add(time.Minute)
	staleStatus, err := controller.finishCheck(machineID, stale, gatewayDNSStatusFromResult(gatewayDNSSuccessResult(), staleTime))
	if !errors.Is(err, ErrGatewayDNSProbe) || staleStatus.Ready || staleStatus.Detail != gatewayDNSDetailProbeUnavailable {
		t.Fatalf("stale status=%+v error=%v, want superseded probe failure", staleStatus, err)
	}
	cached, ok := controller.cachedStatus(machineID)
	if !ok || cached.Ready || !cached.CheckedAt.Equal(freshTime) || !cached.LastSuccessAt.Equal(lastSuccess) {
		t.Fatalf("cached status=%+v present=%t, stale completion replaced fresh state", cached, ok)
	}

	controller.clearMachine(machineID, true)
	if _, ok := controller.cachedStatus(machineID); ok {
		t.Fatal("deleted machine retained cached readiness status")
	}
	controller.statusMu.RLock()
	_, retained := controller.lastSuccess[machineID]
	controller.statusMu.RUnlock()
	if retained {
		t.Fatal("deleted machine retained readiness success history")
	}
}

func TestGatewayDNSReadinessClearDuringStageCannotRepublishOldGeneration(t *testing.T) {
	runtime := &blockingGatewayDNSProbeRuntime{
		started: make(chan string, 2),
		release: make(chan struct{}),
		output:  gatewayDNSJSON(t, gatewayDNSSuccessResult()),
	}
	controller := newGatewayDNSReadinessController(runtime, "unused", "probe.example.")
	controller.loadAgent = func() ([]byte, error) { return []byte("agent"), nil }
	machine := gatewayDNSReadinessMachine()
	type checkResult struct {
		status GatewayDNSReadinessStatus
		err    error
	}
	result := make(chan checkResult, 1)
	go func() {
		status, err := controller.check(context.Background(), machine, true)
		result <- checkResult{status: status, err: err}
	}()

	select {
	case <-runtime.started:
	case <-time.After(time.Second):
		t.Fatal("initial stage did not start")
	}
	controller.clearMachine(machine.ID, false)
	close(runtime.release)
	stale := <-result
	if !errors.Is(stale.err, ErrGatewayDNSProbe) || stale.status.Ready || stale.status.Detail != gatewayDNSDetailProbeUnavailable {
		t.Fatalf("stale check status=%+v error=%v", stale.status, stale.err)
	}
	if _, ok := controller.cachedStatus(machine.ID); ok {
		t.Fatal("stale in-flight check republished cached status after clear")
	}
	controller.stageMu.Lock()
	stagedAfterClear := len(controller.staged)
	controller.stageMu.Unlock()
	if stagedAfterClear != 0 {
		t.Fatalf("staged entries after stale upload = %d, want 0", stagedAfterClear)
	}

	fresh, err := controller.check(context.Background(), machine, true)
	if err != nil || !fresh.Ready {
		t.Fatalf("fresh status=%+v error=%v", fresh, err)
	}
	controller.statusMu.RLock()
	currentGeneration := controller.generations[machine.ID]
	controller.statusMu.RUnlock()
	controller.stageMu.Lock()
	staged := make([]gatewayDNSStageKey, 0, len(controller.staged))
	for key := range controller.staged {
		staged = append(staged, key)
	}
	controller.stageMu.Unlock()
	if len(staged) != 1 || staged[0].machineID != machine.ID || staged[0].generation != currentGeneration {
		t.Fatalf("fresh staged keys=%+v current generation=%d", staged, currentGeneration)
	}
}

func TestGatewayDNSReadinessCoalescesConcurrentForcedRestage(t *testing.T) {
	runtime := &coalescingGatewayDNSProbeRuntime{
		restageStarted: make(chan struct{}, 1),
		releaseRestage: make(chan struct{}),
	}
	controller := newGatewayDNSReadinessController(runtime, "unused", "probe.example.")
	token := controller.beginCheck("machine-coalesced-restage")
	key := gatewayDNSStageKey{machineID: "machine-coalesced-restage", agentHash: "hash", generation: token.generation}
	payload := []byte("agent")
	initialVersion, err := controller.ensureStaged(context.Background(), key, gatewayDNSProbeRuntimePath, payload)
	if err != nil {
		t.Fatalf("initial stage: %v", err)
	}

	versions := make(chan uint64, 2)
	errs := make(chan error, 2)
	for range 2 {
		go func() {
			version, err := controller.restage(context.Background(), key, gatewayDNSProbeRuntimePath, payload, initialVersion)
			versions <- version
			errs <- err
		}()
	}
	select {
	case <-runtime.restageStarted:
	case <-time.After(time.Second):
		t.Fatal("forced restage did not start")
	}
	close(runtime.releaseRestage)
	firstVersion, secondVersion := <-versions, <-versions
	if err := <-errs; err != nil {
		t.Fatalf("first restage: %v", err)
	}
	if err := <-errs; err != nil {
		t.Fatalf("second restage: %v", err)
	}
	if firstVersion <= initialVersion || secondVersion != firstVersion {
		t.Fatalf("restage versions=(%d,%d), initial=%d; want one shared newer version", firstVersion, secondVersion, initialVersion)
	}
	if uploads := runtime.uploadCount(); uploads != 2 {
		t.Fatalf("uploads=%d, want initial upload plus one coalesced replacement", uploads)
	}
}

func TestGatewayDNSReadinessBoundsRuntimeTimeout(t *testing.T) {
	runtime := &gatewayDNSProbeRuntimeFake{exec: func(ctx context.Context, _ string, _ string) (*execution.BufferedResult, error) {
		<-ctx.Done()
		return nil, fmt.Errorf("raw timeout detail: %w", ctx.Err())
	}}
	controller := newGatewayDNSReadinessController(runtime, "unused", "probe.example.")
	controller.loadAgent = func() ([]byte, error) { return []byte("agent"), nil }
	controller.operationTimeout = 10 * time.Millisecond

	started := time.Now()
	status, err := controller.check(context.Background(), gatewayDNSReadinessMachine(), true)
	if !errors.Is(err, ErrGatewayDNSProbe) {
		t.Fatalf("error = %v, want ErrGatewayDNSProbe", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("timeout took %s, want bounded operation", elapsed)
	}
	if status.FailureKind != "probe" || status.Detail != gatewayDNSDetailProbeUnavailable || strings.Contains(err.Error(), "raw timeout") {
		t.Fatalf("status=%+v error=%v", status, err)
	}
}

func TestGatewayDNSReadinessBypassesNonApplicableMachines(t *testing.T) {
	tests := []struct {
		name         string
		proxyEnabled bool
		machine      *models.Machine
	}{
		{name: "proxy disabled", machine: gatewayDNSReadinessMachine()},
		{name: "non firecracker", proxyEnabled: true, machine: func() *models.Machine { m := gatewayDNSReadinessMachine(); m.Backend = libvirtBackend; return m }()},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runtime := &gatewayDNSProbeRuntimeFake{}
			controller := newGatewayDNSReadinessController(runtime, "unused", "probe.example.")
			loads := 0
			controller.loadAgent = func() ([]byte, error) { loads++; return []byte("agent"), nil }
			status, err := controller.check(context.Background(), test.machine, test.proxyEnabled)
			if err != nil {
				t.Fatalf("check: %v", err)
			}
			if status.Applicable || !status.Ready || status.Detail != gatewayDNSDetailNotApplicable {
				t.Fatalf("status = %+v, want non-applicable success", status)
			}
			if loads != 0 || len(runtime.uploads) != 0 || len(runtime.commands) != 0 {
				t.Fatalf("loads=%d uploads=%d execs=%d, want no guest operation", loads, len(runtime.uploads), len(runtime.commands))
			}
		})
	}
}

func TestGatewayDNSReadinessFailsApplicableMachineWithPartialOrMalformedPublicIdentity(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*models.Machine)
	}{
		{name: "missing guest IP", mutate: func(machine *models.Machine) { machine.IP = "" }},
		{name: "missing gateway", mutate: func(machine *models.Machine) { machine.GatewayIP = "" }},
		{name: "missing TAP", mutate: func(machine *models.Machine) { machine.TAPDevice = "" }},
		{name: "malformed TAP", mutate: func(machine *models.Machine) { machine.TAPDevice = "kv01" }},
		{name: "wrong allocation members", mutate: func(machine *models.Machine) { machine.GatewayIP = "10.231.0.9" }},
		{name: "creating partial identity", mutate: func(machine *models.Machine) { machine.State = models.MachineStateCreating; machine.GatewayIP = "" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runtime := &gatewayDNSProbeRuntimeFake{}
			controller := newGatewayDNSReadinessController(runtime, "unused", "probe.example.")
			loads := 0
			controller.loadAgent = func() ([]byte, error) { loads++; return []byte("agent"), nil }
			machine := gatewayDNSReadinessMachine()
			test.mutate(machine)

			status, err := controller.check(context.Background(), machine, true)
			if !errors.Is(err, ErrGatewayDNSProbe) {
				t.Fatalf("check error = %v, want applicable provider readiness failure", err)
			}
			if !status.Applicable || status.Ready || status.FailureKind != "probe" || status.Detail != gatewayDNSDetailProbeUnavailable {
				t.Fatalf("status = %+v, want applicable probe-unavailable failure", status)
			}
			if loads != 0 || len(runtime.uploads) != 0 || len(runtime.commands) != 0 {
				t.Fatalf("loads=%d uploads=%d execs=%d, want fail before guest operation", loads, len(runtime.uploads), len(runtime.commands))
			}
		})
	}
}

type gatewayDNSProbeUpload struct {
	machineID string
	path      string
	mode      uint32
	payload   string
}

type gatewayDNSProbeRuntimeFake struct {
	uploads   []gatewayDNSProbeUpload
	commands  []string
	uploadErr error
	exec      func(context.Context, string, string) (*execution.BufferedResult, error)
}

type blockingGatewayDNSProbeRuntime struct {
	started chan string
	release chan struct{}
	output  string
}

type coalescingGatewayDNSProbeRuntime struct {
	mu             sync.Mutex
	uploads        int
	restageStarted chan struct{}
	releaseRestage chan struct{}
}

func (r *blockingGatewayDNSProbeRuntime) Upload(ctx context.Context, machineID, _ string, _ uint32, _ int64, _ io.Reader) error {
	select {
	case r.started <- machineID:
	case <-ctx.Done():
		return fmt.Errorf("wait to report blocked gateway DNS upload: %w", ctx.Err())
	}
	select {
	case <-r.release:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("wait to release blocked gateway DNS upload: %w", ctx.Err())
	}
}

func (r *blockingGatewayDNSProbeRuntime) Exec(context.Context, string, string, io.Reader) (*execution.BufferedResult, error) {
	return &execution.BufferedResult{Stdout: r.output, ExitCode: dnsprobe.ExitOK}, nil
}

func (r *coalescingGatewayDNSProbeRuntime) Upload(ctx context.Context, _, _ string, _ uint32, _ int64, _ io.Reader) error {
	r.mu.Lock()
	r.uploads++
	upload := r.uploads
	r.mu.Unlock()
	if upload == 1 {
		return nil
	}
	select {
	case r.restageStarted <- struct{}{}:
	default:
	}
	select {
	case <-r.releaseRestage:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("wait to release coalesced gateway DNS restage: %w", ctx.Err())
	}
}

func (r *coalescingGatewayDNSProbeRuntime) Exec(context.Context, string, string, io.Reader) (*execution.BufferedResult, error) {
	return nil, errors.New("unexpected exec")
}

func (r *coalescingGatewayDNSProbeRuntime) uploadCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.uploads
}

func (f *gatewayDNSProbeRuntimeFake) Upload(_ context.Context, machineID, path string, mode uint32, _ int64, reader io.Reader) error {
	payload, err := io.ReadAll(reader)
	if err != nil {
		return fmt.Errorf("read staged gateway DNS payload: %w", err)
	}
	f.uploads = append(f.uploads, gatewayDNSProbeUpload{machineID: machineID, path: path, mode: mode, payload: string(payload)})
	return f.uploadErr
}

func (f *gatewayDNSProbeRuntimeFake) Exec(ctx context.Context, machineID, command string, _ io.Reader) (*execution.BufferedResult, error) {
	f.commands = append(f.commands, command)
	if f.exec == nil {
		return nil, fmt.Errorf("unexpected exec for %s", machineID)
	}
	return f.exec(ctx, machineID, command)
}

func gatewayDNSReadinessMachine() *models.Machine {
	machine := &models.Machine{
		ID:       "machine-dns-ready",
		Backend:  firecrackerBackend,
		State:    models.MachineStateRunning,
		VSockCID: 42,
	}
	alloc, err := allocationForIndex("10.231.0.0/16", 1)
	if err != nil {
		panic(err)
	}
	alloc.OwnerID = machine.ID
	setMachinePublicAllocation(machine, alloc)
	return machine
}

func gatewayDNSSuccessResult() dnsprobe.Result {
	rcode := 0
	transport := func(name string) dnsprobe.TransportResult {
		return dnsprobe.TransportResult{Transport: name, PathOK: true, ResolutionOK: true, RCode: &rcode, RCodeName: "NOERROR", AnswerCount: 1, AddressCount: 1, DurationMS: 1}
	}
	return dnsprobe.Result{
		SchemaVersion: dnsprobe.SchemaVersion,
		Server:        "10.231.0.5:53",
		Name:          "probe.example.",
		QueryType:     "A",
		PathOK:        true,
		ResolutionOK:  true,
		UDP:           transport("udp"),
		TCP:           transport("tcp"),
	}
}

func gatewayDNSPathFailureResult(rawError string) dnsprobe.Result {
	result := gatewayDNSSuccessResult()
	result.PathOK = false
	result.ResolutionOK = false
	result.FailureKind = "path"
	result.UDP = dnsprobe.TransportResult{Transport: "udp", FailureKind: "path", Phase: "read", DurationMS: 3, Error: rawError}
	return result
}

func gatewayDNSResolutionFailureResult(rawError string) dnsprobe.Result {
	result := gatewayDNSSuccessResult()
	result.ResolutionOK = false
	result.FailureKind = "resolution"
	rcode := 2
	result.UDP = dnsprobe.TransportResult{Transport: "udp", PathOK: true, FailureKind: "resolution", Phase: "resolve", RCode: &rcode, RCodeName: "SERVFAIL", DurationMS: 2, Error: rawError}
	return result
}

func gatewayDNSExecResult(t *testing.T, result dnsprobe.Result, exitCode int) *execution.BufferedResult {
	t.Helper()
	return &execution.BufferedResult{Stdout: gatewayDNSJSON(t, result), ExitCode: exitCode}
}

func gatewayDNSJSON(t *testing.T, result dnsprobe.Result) string {
	t.Helper()
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal probe result: %v", err)
	}
	return string(data) + "\n"
}

func gatewayDNSReadinessStatusString(status GatewayDNSReadinessStatus) string {
	data, _ := json.Marshal(status)
	return string(data)
}
