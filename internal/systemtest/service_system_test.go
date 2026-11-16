//go:build system && service

package systemtest

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gfdelarue/kiln-vm/internal/models"
)

const daemonSystemtestDropin = "/run/systemd/system/kiln-vmd.service.d/50-kiln-vm-systemtest.conf"

func TestServiceRestartStopsRunningVMsAndCleansTap(t *testing.T) {
	client := requireClient(t)
	cleanupResourcesByPrefix(t, client)
	requireDaemonRestartSupport(t, client)

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	withSystemProfileImageCleanup(t, client, ctx)

	name := uniqueName("service-restart")
	vm, err := client.createVM(ctx, models.CreateVMRequest{
		Name:     name,
		Profile:  "alpine",
		CPU:      &models.CPUConfig{VCPUs: 1},
		MemoryMB: 512,
		Power: &models.VMPowerPolicy{
			Mode: models.VMPowerModeManual,
		},
	})
	if err != nil {
		t.Fatalf("create VM: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if current, err := client.getVM(cleanupCtx, vm.ID); err == nil && current.State != models.MachineStateStopped {
			_ = client.stopVM(cleanupCtx, vm.ID)
			_, _ = client.waitForVMState(cleanupCtx, vm.ID, models.MachineStateStopped)
		}
		if err := client.deleteVM(cleanupCtx, vm.ID); err != nil && !isAPIStatus(err, 404) {
			t.Logf("cleanup VM %s: %v", vm.ID, err)
		}
	})

	vm, err = client.waitForVMState(ctx, vm.ID, models.MachineStateRunning)
	if err != nil {
		t.Fatalf("wait for running: %v", err)
	}
	tap := vm.TAPDevice
	if tap == "" {
		t.Fatalf("running VM has no TAP device: %+v", vm)
	}

	restartDaemon(t, client)

	if _, err := client.waitForVMState(ctx, vm.ID, models.MachineStateStopped); err != nil {
		t.Fatalf("wait for stopped after restart: %v", err)
	}
	if err := waitForNoTap(ctx, tap); err != nil {
		t.Fatal(err)
	}
}

func TestServiceVMPowerAlwaysAfterRestartPreservesDisk(t *testing.T) {
	client := requireClient(t)
	cleanupResourcesByPrefix(t, client)
	requireDaemonRestartSupport(t, client)

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	profileImagesBefore := snapshotVMProfileImages(t, client, ctx)
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		cleanupNewVMProfileImages(t, client, cleanupCtx, profileImagesBefore)
	})

	name := uniqueName("vm-service-power-always")
	vm, err := client.createVM(ctx, models.CreateVMRequest{
		Name:       name,
		Profile:    "alpine",
		DiskSizeMB: 128,
		CPU:        &models.CPUConfig{VCPUs: 1},
		MemoryMB:   512,
		Power: &models.VMPowerPolicy{
			Mode: models.VMPowerModeAlways,
		},
	})
	if err != nil {
		t.Fatalf("create VM: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := client.deleteVM(cleanupCtx, name); err != nil {
			t.Logf("cleanup VM %s: %v", name, err)
		}
	})

	vm, err = client.waitForVMState(ctx, name, models.MachineStateRunning)
	if err != nil {
		t.Fatalf("wait for VM running: %v", err)
	}
	if vm.Power.LastStartedAt == nil {
		t.Fatalf("VM power state missing last_started_at before restart: %+v", vm.Power)
	}
	lastStartedBeforeRestart := *vm.Power.LastStartedAt
	if _, err := client.execVM(ctx, name, "printf service-power-always-ok >/root/service-power-always.txt && sync"); err != nil {
		t.Fatalf("write service restart marker: %v", err)
	}

	restartDaemon(t, client)

	vm, err = client.waitForVMState(ctx, name, models.MachineStateRunning)
	if err != nil {
		t.Fatalf("wait for power-always VM running after restart: %v", err)
	}
	if vm.Power.LastStartedAt == nil || !vm.Power.LastStartedAt.After(lastStartedBeforeRestart) {
		t.Fatalf("VM last_started_at did not advance across daemon restart: before=%s after=%v", lastStartedBeforeRestart.Format(time.RFC3339Nano), vm.Power.LastStartedAt)
	}
	result, err := client.execVM(ctx, name, "cat /root/service-power-always.txt")
	if err != nil {
		t.Fatalf("read service restart marker: %v", err)
	}
	if result.ExitCode != 0 || !strings.Contains(result.Stdout, "service-power-always-ok") {
		t.Fatalf("service restart marker mismatch: exit=%d stdout=%q stderr=%q", result.ExitCode, result.Stdout, result.Stderr)
	}
}

func TestServiceMachineBootFailureDiagnostics(t *testing.T) {
	client := requireClient(t)
	cleanupResourcesByPrefix(t, client)
	requireDaemonRestartSupport(t, client)

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	profileImagesBefore := snapshotVMProfileImages(t, client, ctx)
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		cleanupNewVMProfileImages(t, client, cleanupCtx, profileImagesBefore)
	})

	name := uniqueName("service-boot-diagnostics")
	badKernelPath := filepath.Join(os.TempDir(), name+"-missing-vmlinux")
	_ = os.Remove(badKernelPath)
	withDaemonKernelPathOverride(t, client, badKernelPath)

	vm, err := client.createVM(ctx, models.CreateVMRequest{
		Name:       name,
		Profile:    "alpine",
		DiskSizeMB: 128,
		CPU:        &models.CPUConfig{VCPUs: 1},
		MemoryMB:   512,
		Power: &models.VMPowerPolicy{
			Mode: models.VMPowerModeOnDemand,
		},
	})
	if err != nil {
		t.Fatalf("create VM with bad kernel path: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := client.deleteVM(cleanupCtx, name); err != nil {
			t.Logf("cleanup VM %s: %v", name, err)
		}
	})

	vm, err = waitForVMErrorDiagnostics(ctx, client, name)
	if err != nil {
		t.Fatalf("wait for VM boot diagnostics: %v", err)
	}
	if len(vm.Diagnostics) == 0 {
		t.Fatalf("VM missing diagnostics: %+v", vm)
	}
	diag := vm.Diagnostics[0]
	if diag.Category != "machine_boot" || diag.Backend != "firecracker" || diag.KernelPath != badKernelPath || diag.KernelType != "missing" || diag.LogPath == "" {
		t.Fatalf("diagnostic does not identify bad kernel boot failure: %+v", diag)
	}
	if diag.TAPDevice != "" {
		if err := waitForNoTap(ctx, diag.TAPDevice); err != nil {
			t.Fatalf("tap device still exists after failed boot: %v", err)
		}
	}
	for _, path := range []string{diag.SocketPath, diag.VSockPath} {
		if path == "" {
			continue
		}
		if _, err := os.Stat(path); err == nil {
			t.Fatalf("runtime path still exists after failed boot: %s", path)
		} else if !os.IsNotExist(err) {
			t.Fatalf("stat runtime path %s: %v", path, err)
		}
	}

	_, err = client.execVM(ctx, name, "true")
	if err == nil {
		t.Fatal("exec VM with bad kernel path succeeded, want diagnostic API error")
	}
	statusErr, ok := err.(*apiStatusError)
	if !ok {
		t.Fatalf("exec error = %T %v, want apiStatusError", err, err)
	}
	var body struct {
		Code      string           `json:"code"`
		Phase     string           `json:"phase"`
		Resource  string           `json:"resource"`
		Retryable bool             `json:"retryable"`
		Raw       []map[string]any `json:"diagnostics"`
	}
	if err := json.Unmarshal([]byte(statusErr.Body), &body); err != nil {
		t.Fatalf("decode diagnostic API error: %v\n%s", err, statusErr.Body)
	}
	if body.Code != "vm_boot_failed" || body.Phase == "" || body.Resource != name || len(body.Raw) == 0 {
		t.Fatalf("diagnostic API error body missing fields: %s", statusErr.Body)
	}
	if body.Retryable {
		t.Fatalf("diagnostic API retryable = true, want false: %s", statusErr.Body)
	}
	if got := fmt.Sprint(body.Raw[0]["kernel_type"]); got != "missing" {
		t.Fatalf("diagnostic API kernel_type = %q, body: %s", got, statusErr.Body)
	}
}

func waitForVMErrorDiagnostics(ctx context.Context, client *apiClient, ref string) (*models.VM, error) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		vm, err := client.getVM(ctx, ref)
		if err != nil {
			return nil, err
		}
		if vm.State == models.MachineStateError && len(vm.Diagnostics) > 0 {
			return vm, nil
		}
		if vm.State == models.MachineStateError {
			return vm, fmt.Errorf("VM entered error without diagnostics: %s", vm.Error)
		}
		select {
		case <-ctx.Done():
			return vm, fmt.Errorf("wait for VM diagnostics: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func withDaemonKernelPathOverride(t *testing.T, client *apiClient, kernelPath string) {
	t.Helper()
	_ = withDaemonEnvironmentOverride(t, client, map[string]string{
		"KILN_VM_KERNEL_PATH": kernelPath,
	})
}

func withDaemonEnvironmentOverride(t *testing.T, client *apiClient, values map[string]string) func() {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(daemonSystemtestDropin), 0755); err != nil {
		t.Fatalf("create kiln-vmd systemtest drop-in dir: %v", err)
	}
	content := "[Service]\n"
	for key, value := range values {
		content += fmt.Sprintf("Environment=%s=%s\n", key, value)
	}
	if err := os.WriteFile(daemonSystemtestDropin, []byte(content), 0644); err != nil {
		t.Fatalf("write kiln-vmd systemtest drop-in: %v", err)
	}
	var once sync.Once
	restore := func() {
		once.Do(func() {
			if err := os.Remove(daemonSystemtestDropin); err != nil && !os.IsNotExist(err) {
				t.Logf("remove kiln-vmd systemtest drop-in: %v", err)
			}
			reloadSystemd(t)
			restartDaemon(t, client)
		})
	}
	t.Cleanup(restore)
	reloadSystemd(t)
	restartDaemon(t, client)
	return restore
}

func reloadSystemd(t *testing.T) {
	t.Helper()
	cmd := exec.Command("systemctl", "daemon-reload", "--no-ask-password")
	out, err := cmd.CombinedOutput()
	if err != nil {
		if strings.Contains(string(out), "Interactive authentication required") ||
			strings.Contains(string(out), "Access denied") ||
			strings.Contains(string(out), "authentication") {
			t.Skipf("%s: %s", serviceRestartSudoMessage, out)
		}
		t.Fatalf("systemctl daemon-reload: %v\n%s", err, out)
	}
}

func restartDaemon(t *testing.T, client *apiClient) {
	t.Helper()
	requireExclusiveDaemonRestartHost(t, client)
	resetDaemonStartLimit(t)

	cmd := exec.Command("systemctl", "restart", "kiln-vmd", "--no-ask-password")
	out, err := cmd.CombinedOutput()
	if err != nil {
		if strings.Contains(string(out), "Interactive authentication required") ||
			strings.Contains(string(out), "Access denied") ||
			strings.Contains(string(out), "authentication") {
			t.Skipf("%s: %s", serviceRestartSudoMessage, out)
		}
		if strings.Contains(string(out), "start of the service was attempted too often") ||
			strings.Contains(string(out), "Start request repeated too quickly") ||
			strings.Contains(string(out), "start-limit-hit") {
			resetDaemonStartLimit(t)
			cmd = exec.Command("systemctl", "restart", "kiln-vmd", "--no-ask-password")
			if retryOut, retryErr := cmd.CombinedOutput(); retryErr == nil {
				waitForDaemonHealth(t, client)
				return
			} else {
				t.Fatalf("restart kiln-vmd after reset-failed (did not fall back to start): %v\n%s", retryErr, retryOut)
			}
		}
		t.Fatalf("restart kiln-vmd: %v\n%s", err, out)
	}

	waitForDaemonHealth(t, client)
}

func resetDaemonStartLimit(t *testing.T) {
	t.Helper()
	cmd := exec.Command("systemctl", "reset-failed", "kiln-vmd", "--no-ask-password")
	out, err := cmd.CombinedOutput()
	if err != nil {
		if strings.Contains(string(out), "Interactive authentication required") ||
			strings.Contains(string(out), "Access denied") ||
			strings.Contains(string(out), "authentication") {
			t.Skipf("%s: %s", serviceRestartSudoMessage, out)
		}
		t.Fatalf("reset kiln-vmd start limit: %v\n%s", err, out)
	}
}

func waitForDaemonHealth(t *testing.T, client *apiClient) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		if err := client.healthErr(); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("kiln-vmd did not become healthy after restart")
		}
		time.Sleep(500 * time.Millisecond)
	}
}

const serviceRestartSudoMessage = "sudo required - run task test:service to run these tests"
