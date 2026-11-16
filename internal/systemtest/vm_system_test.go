//go:build system

package systemtest

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gfdelarue/kiln-vm/internal/models"
)

func TestVMPersistenceAcrossStopStart(t *testing.T) {
	client := requireClient(t)
	cleanupResourcesByPrefix(t, client)

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	profileImagesBefore := snapshotVMProfileImages(t, client, ctx)
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		cleanupNewVMProfileImages(t, client, cleanupCtx, profileImagesBefore)
	})

	name := uniqueName("vm-persist")
	vm, err := client.createVM(ctx, models.CreateVMRequest{
		Name:       name,
		Profile:    "alpine",
		DiskSizeMB: 128,
		CPU:        &models.CPUConfig{VCPUs: 1},
		MemoryMB:   512,
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
	if vm.MachineID == "" || vm.ImageID == "" {
		t.Fatalf("VM missing backing resources: %+v", vm)
	}

	result, err := client.execVM(ctx, name, "printf '%s\n' kiln-vm-persist-ok > /root/persist.txt && sync")
	if err != nil {
		t.Fatalf("write persistent marker: %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("write marker exit=%d stdout=%q stderr=%q", result.ExitCode, result.Stdout, result.Stderr)
	}

	tap := vm.TAPDevice
	if err := client.stopVM(ctx, name); err != nil {
		t.Fatalf("stop VM: %v", err)
	}
	if _, err := client.waitForVMState(ctx, name, models.MachineStateStopped); err != nil {
		t.Fatalf("wait for stopped: %v", err)
	}
	if err := waitForNoTap(ctx, tap); err != nil {
		t.Fatal(err)
	}

	if _, err := client.startVM(ctx, name); err != nil {
		t.Fatalf("start VM: %v", err)
	}
	if _, err := client.waitForVMState(ctx, name, models.MachineStateRunning); err != nil {
		t.Fatalf("wait for restarted VM running: %v", err)
	}
	result, err = client.execVM(ctx, name, "cat /root/persist.txt")
	if err != nil {
		t.Fatalf("read persistent marker: %v", err)
	}
	if result.ExitCode != 0 || !strings.Contains(result.Stdout, "kiln-vm-persist-ok") {
		t.Fatalf("persistent marker mismatch: exit=%d stdout=%q stderr=%q", result.ExitCode, result.Stdout, result.Stderr)
	}
}

func TestVMExecWakesStoppedOnDemandVM(t *testing.T) {
	client := requireClient(t)
	cleanupResourcesByPrefix(t, client)

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	profileImagesBefore := snapshotVMProfileImages(t, client, ctx)
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		cleanupNewVMProfileImages(t, client, cleanupCtx, profileImagesBefore)
	})

	name := uniqueName("vm-power-exec-wake")
	_, err := client.createVM(ctx, models.CreateVMRequest{
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
		t.Fatalf("create VM: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := client.deleteVM(cleanupCtx, name); err != nil {
			t.Logf("cleanup VM %s: %v", name, err)
		}
	})

	if _, err := client.waitForVMState(ctx, name, models.MachineStateRunning); err != nil {
		t.Fatalf("wait for VM running: %v", err)
	}
	if _, err := client.execVM(ctx, name, "printf wake-on-use-ok >/root/wake-on-use.txt && sync"); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	if err := client.stopVM(ctx, name); err != nil {
		t.Fatalf("stop VM: %v", err)
	}
	if _, err := client.waitForVMState(ctx, name, models.MachineStateStopped); err != nil {
		t.Fatalf("wait for VM stopped: %v", err)
	}

	result, err := client.execVM(ctx, name, "cat /root/wake-on-use.txt")
	if err != nil {
		t.Fatalf("exec should wake stopped on-demand VM: %v", err)
	}
	if result.ExitCode != 0 || !strings.Contains(result.Stdout, "wake-on-use-ok") {
		t.Fatalf("wake-on-use exec mismatch: exit=%d stdout=%q stderr=%q", result.ExitCode, result.Stdout, result.Stderr)
	}
	vm, err := client.getVM(ctx, name)
	if err != nil {
		t.Fatalf("get VM after exec: %v", err)
	}
	if vm.State != models.MachineStateRunning {
		t.Fatalf("VM state after wake-on-use exec = %s, want running", vm.State)
	}
}

func TestServiceVMStopRejectsActiveShellUseLease(t *testing.T) {
	client := requireClient(t)
	cleanupResourcesByPrefix(t, client)

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	profileImagesBefore := snapshotVMProfileImages(t, client, ctx)
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		cleanupNewVMProfileImages(t, client, cleanupCtx, profileImagesBefore)
	})

	name := uniqueName("vm-power-active-shell")
	_, err := client.createVM(ctx, models.CreateVMRequest{
		Name:       name,
		Profile:    "alpine",
		DiskSizeMB: 128,
		CPU:        &models.CPUConfig{VCPUs: 1},
		MemoryMB:   512,
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
	if _, err := client.waitForVMState(ctx, name, models.MachineStateRunning); err != nil {
		t.Fatalf("wait for VM running: %v", err)
	}

	shell, _, err := client.dialVMShell(ctx, name)
	if err != nil {
		t.Fatalf("dial VM shell: %v", err)
	}
	defer func() { _ = shell.Close() }()
	waitForVMActiveLeases(t, ctx, client, name, 1)

	err = client.stopVM(ctx, name)
	var statusErr *apiStatusError
	if !errors.As(err, &statusErr) || statusErr.StatusCode != http.StatusConflict || !strings.Contains(statusErr.Body, "active use leases") {
		t.Fatalf("stop VM with active shell error = %v, want active lease conflict", err)
	}
	vm, err := client.getVM(ctx, name)
	if err != nil {
		t.Fatalf("get VM after rejected stop: %v", err)
	}
	if vm.State != models.MachineStateRunning {
		t.Fatalf("VM state after rejected stop = %s, want running", vm.State)
	}

	if err := shell.Close(); err != nil {
		t.Fatalf("close VM shell: %v", err)
	}
	waitForVMActiveLeases(t, ctx, client, name, 0)
	if err := client.stopVM(ctx, name); err != nil {
		t.Fatalf("stop VM after shell release: %v", err)
	}
	if _, err := client.waitForVMState(ctx, name, models.MachineStateStopped); err != nil {
		t.Fatalf("wait for VM stopped after shell release: %v", err)
	}
}

func TestVMManualPowerDoesNotAutoStartStoppedVM(t *testing.T) {
	client := requireClient(t)
	cleanupResourcesByPrefix(t, client)

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	profileImagesBefore := snapshotVMProfileImages(t, client, ctx)
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		cleanupNewVMProfileImages(t, client, cleanupCtx, profileImagesBefore)
	})

	name := uniqueName("vm-power-manual")
	_, err := client.createVM(ctx, models.CreateVMRequest{
		Name:       name,
		Profile:    "alpine",
		DiskSizeMB: 128,
		CPU:        &models.CPUConfig{VCPUs: 1},
		MemoryMB:   512,
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

	if _, err := client.waitForVMState(ctx, name, models.MachineStateRunning); err != nil {
		t.Fatalf("wait for VM running: %v", err)
	}
	if _, err := client.execVM(ctx, name, "printf manual-power-ok >/root/manual-power.txt && sync"); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	if err := client.stopVM(ctx, name); err != nil {
		t.Fatalf("stop VM: %v", err)
	}
	if _, err := client.waitForVMState(ctx, name, models.MachineStateStopped); err != nil {
		t.Fatalf("wait for VM stopped: %v", err)
	}

	if _, err := client.execVM(ctx, name, "cat /root/manual-power.txt"); err == nil {
		t.Fatal("exec woke a stopped manual VM, want conflict")
	} else {
		expectAPIStatus(t, err, http.StatusConflict)
	}

	if _, err := client.startVM(ctx, name); err != nil {
		t.Fatalf("explicit start VM: %v", err)
	}
	if _, err := client.waitForVMState(ctx, name, models.MachineStateRunning); err != nil {
		t.Fatalf("wait for explicitly started VM: %v", err)
	}
	result, err := client.execVM(ctx, name, "cat /root/manual-power.txt")
	if err != nil {
		t.Fatalf("exec after explicit start: %v", err)
	}
	if result.ExitCode != 0 || !strings.Contains(result.Stdout, "manual-power-ok") {
		t.Fatalf("manual VM marker mismatch: exit=%d stdout=%q stderr=%q", result.ExitCode, result.Stdout, result.Stderr)
	}
}

func TestVMOnDemandIdleStopsAndWakesAgain(t *testing.T) {
	client := requireClient(t)
	cleanupResourcesByPrefix(t, client)

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	profileImagesBefore := snapshotVMProfileImages(t, client, ctx)
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		cleanupNewVMProfileImages(t, client, cleanupCtx, profileImagesBefore)
	})

	name := uniqueName("vm-power-idle")
	vm, err := client.createVM(ctx, models.CreateVMRequest{
		Name:       name,
		Profile:    "alpine",
		DiskSizeMB: 128,
		CPU:        &models.CPUConfig{VCPUs: 1},
		MemoryMB:   512,
		Power: &models.VMPowerPolicy{
			Mode:               models.VMPowerModeOnDemand,
			IdleTimeoutSeconds: 2,
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
	tap := vm.TAPDevice
	if _, err := client.execVM(ctx, name, "printf idle-power-ok >/root/idle-power.txt && sync"); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	vm = waitForVMIdleStopped(t, ctx, client, name)
	if vm.Power.StopReason != "idle_timeout" || vm.Power.LastStoppedAt == nil {
		t.Fatalf("idle stop power state = %+v, want idle_timeout and timestamp", vm.Power)
	}
	if err := waitForNoTap(ctx, tap); err != nil {
		t.Fatal(err)
	}

	result, err := client.execVM(ctx, name, "cat /root/idle-power.txt")
	if err != nil {
		t.Fatalf("exec should wake idle-stopped VM: %v", err)
	}
	if result.ExitCode != 0 || !strings.Contains(result.Stdout, "idle-power-ok") {
		t.Fatalf("idle wake marker mismatch: exit=%d stdout=%q stderr=%q", result.ExitCode, result.Stdout, result.Stderr)
	}
	vm, err = client.getVM(ctx, name)
	if err != nil {
		t.Fatalf("get VM after idle wake: %v", err)
	}
	if vm.State != models.MachineStateRunning {
		t.Fatalf("VM state after idle wake = %s, want running", vm.State)
	}
}

func waitForVMIdleStopped(t *testing.T, ctx context.Context, client *apiClient, ref string) *models.VM {
	t.Helper()
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		vm, err := client.getVM(ctx, ref)
		if err != nil {
			t.Fatalf("get VM %s while waiting idle stop: %v", ref, err)
		}
		if vm.State == models.MachineStateStopped && vm.Power.StopReason == "idle_timeout" && vm.Power.LastStoppedAt != nil {
			return vm
		}
		if vm.State == models.MachineStateError {
			t.Fatalf("VM %s entered error state while waiting idle stop: %s", ref, vm.Error)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("wait for VM %s idle stop metadata: %v; last VM=%+v", ref, ctx.Err(), vm)
		case <-ticker.C:
		}
	}
}

func waitForVMActiveLeases(t *testing.T, ctx context.Context, client *apiClient, ref string, want int) *models.VM {
	t.Helper()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		vm, err := client.getVM(ctx, ref)
		if err != nil {
			t.Fatalf("get VM %s while waiting for active leases: %v", ref, err)
		}
		if vm.Power.ActiveLeases == want {
			return vm
		}
		select {
		case <-ctx.Done():
			t.Fatalf("wait for VM %s active leases = %d: %v; last active_leases=%d", ref, want, ctx.Err(), vm.Power.ActiveLeases)
		case <-ticker.C:
		}
	}
}

func TestVMUniqueNameAndDeleteKeepsCachedBaseImage(t *testing.T) {
	client := requireClient(t)
	cleanupResourcesByPrefix(t, client)

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	profileImagesBefore := snapshotVMProfileImages(t, client, ctx)
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		cleanupNewVMProfileImages(t, client, cleanupCtx, profileImagesBefore)
	})

	name := uniqueName("vm-delete")
	vm, err := client.createVM(ctx, models.CreateVMRequest{
		Name:       name,
		Profile:    "alpine",
		DiskSizeMB: 128,
		CPU:        &models.CPUConfig{VCPUs: 1},
		MemoryMB:   512,
	})
	if err != nil {
		t.Fatalf("create VM: %v", err)
	}

	_, err = client.createVM(ctx, models.CreateVMRequest{
		Name:       name,
		Profile:    "alpine",
		DiskSizeMB: 128,
		CPU:        &models.CPUConfig{VCPUs: 1},
		MemoryMB:   512,
	})
	if err == nil {
		t.Fatal("expected duplicate VM name to fail")
	}
	expectAPIStatus(t, err, http.StatusConflict)

	if err := client.deleteVM(ctx, name); err != nil {
		t.Fatalf("delete VM: %v", err)
	}
	if _, err := client.getVM(ctx, name); err == nil {
		t.Fatal("expected VM lookup to fail after delete")
	} else {
		expectAPIStatus(t, err, http.StatusNotFound)
	}
	summary, err := client.metricsSummary(ctx)
	if err != nil {
		t.Fatalf("metrics summary after VM delete: %v", err)
	}
	for _, machine := range summary.Machines {
		if machine.MachineID == vm.MachineID {
			t.Fatalf("backing machine %s still present after VM delete: %+v", vm.MachineID, machine)
		}
	}
	profileImages, err := client.listVMProfileImages(ctx)
	if err != nil {
		t.Fatalf("list VM profile images: %v", err)
	}
	foundCachedBase := false
	for _, image := range profileImages {
		if image.ImageID == vm.ImageID {
			foundCachedBase = true
			break
		}
	}
	if !foundCachedBase {
		t.Fatalf("expected cached base image %s to remain after VM delete", vm.ImageID)
	}
}

func TestVMProfileImageCacheReusesBaseAndIsolatesDisks(t *testing.T) {
	client := requireClient(t)
	cleanupResourcesByPrefix(t, client)

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	profileImagesBefore := snapshotVMProfileImages(t, client, ctx)
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		cleanupNewVMProfileImages(t, client, cleanupCtx, profileImagesBefore)
	})

	firstName := uniqueName("vm-cache-a")
	secondName := uniqueName("vm-cache-b")
	for _, name := range []string{firstName, secondName} {
		name := name
		t.Cleanup(func() {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			if err := client.deleteVM(cleanupCtx, name); err != nil {
				t.Logf("cleanup VM %s: %v", name, err)
			}
		})
	}

	first, err := client.createVM(ctx, models.CreateVMRequest{
		Name:       firstName,
		Profile:    "alpine",
		DiskSizeMB: 128,
		CPU:        &models.CPUConfig{VCPUs: 1},
		MemoryMB:   512,
	})
	if err != nil {
		t.Fatalf("create first VM: %v", err)
	}
	if _, err := client.waitForVMState(ctx, firstName, models.MachineStateRunning); err != nil {
		t.Fatalf("wait for first VM running: %v", err)
	}
	if _, err := client.execVM(ctx, firstName, "printf first >/root/cache-marker.txt && sync"); err != nil {
		t.Fatalf("write first marker: %v", err)
	}
	if err := client.stopVM(ctx, firstName); err != nil {
		t.Fatalf("stop first VM before using second VM: %v", err)
	}
	if _, err := client.waitForVMState(ctx, firstName, models.MachineStateStopped); err != nil {
		t.Fatalf("wait for first stopped before using second VM: %v", err)
	}

	second, err := client.createVM(ctx, models.CreateVMRequest{
		Name:       secondName,
		Profile:    "alpine",
		DiskSizeMB: 128,
		CPU:        &models.CPUConfig{VCPUs: 1},
		MemoryMB:   512,
	})
	if err != nil {
		t.Fatalf("create second VM: %v", err)
	}
	if first.ImageID != second.ImageID {
		t.Fatalf("expected VMs to share cached base image, got %s and %s", first.ImageID, second.ImageID)
	}
	if first.MachineID == second.MachineID {
		t.Fatalf("expected separate backing machines, both used %s", first.MachineID)
	}
	if _, err := client.waitForVMState(ctx, secondName, models.MachineStateRunning); err != nil {
		t.Fatalf("wait for second VM running: %v", err)
	}
	if _, err := client.execVM(ctx, secondName, "printf second >/root/cache-marker.txt && sync"); err != nil {
		t.Fatalf("write second marker: %v", err)
	}
	if err := client.stopVM(ctx, secondName); err != nil {
		t.Fatalf("stop second VM: %v", err)
	}
	if _, err := client.waitForVMState(ctx, secondName, models.MachineStateStopped); err != nil {
		t.Fatalf("wait for second stopped: %v", err)
	}

	if _, err := client.startVM(ctx, firstName); err != nil {
		t.Fatalf("restart first VM before reading marker: %v", err)
	}
	if _, err := client.waitForVMState(ctx, firstName, models.MachineStateRunning); err != nil {
		t.Fatalf("wait for first running before reading marker: %v", err)
	}
	firstResult, err := client.execVM(ctx, firstName, "cat /root/cache-marker.txt")
	if err != nil {
		t.Fatalf("read first marker after restart: %v", err)
	}
	if err := client.stopVM(ctx, firstName); err != nil {
		t.Fatalf("stop first VM after reading marker: %v", err)
	}
	if _, err := client.waitForVMState(ctx, firstName, models.MachineStateStopped); err != nil {
		t.Fatalf("wait for first stopped after reading marker: %v", err)
	}

	if _, err := client.startVM(ctx, secondName); err != nil {
		t.Fatalf("restart second VM before reading marker: %v", err)
	}
	if _, err := client.waitForVMState(ctx, secondName, models.MachineStateRunning); err != nil {
		t.Fatalf("wait for second running before reading marker: %v", err)
	}
	secondResult, err := client.execVM(ctx, secondName, "cat /root/cache-marker.txt")
	if err != nil {
		t.Fatalf("read second marker after restart: %v", err)
	}
	if strings.TrimSpace(firstResult.Stdout) != "first" {
		t.Fatalf("first marker = %q, want first", firstResult.Stdout)
	}
	if strings.TrimSpace(secondResult.Stdout) != "second" {
		t.Fatalf("second marker = %q, want second", secondResult.Stdout)
	}
}
