//go:build system

package systemtest

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gfdelarue/kiln-vm/internal/models"
)

func TestAPISmokeVMExecStopCleanup(t *testing.T) {
	client := requireClient(t)
	cleanupResourcesByPrefix(t, client)

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	withSystemProfileImageCleanup(t, client, ctx)

	name := uniqueName("api-smoke")
	vm, err := client.createVM(ctx, models.CreateVMRequest{
		Name:     name,
		Profile:  "alpine",
		CPU:      &models.CPUConfig{VCPUs: 1},
		MemoryMB: 512,
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
		if err := client.deleteVM(cleanupCtx, vm.ID); err != nil && !isAPIStatus(err, http.StatusNotFound) {
			t.Logf("cleanup VM %s: %v", vm.ID, err)
		}
	})

	vm, err = client.waitForVMState(ctx, vm.ID, models.MachineStateRunning)
	if err != nil {
		t.Fatalf("wait for running: %v", err)
	}
	if vm.IP == "" {
		t.Fatal("expected running VM to have guest IP")
	}
	if vm.GatewayIP == "" {
		t.Fatal("expected running VM to have gateway IP")
	}
	if vm.TAPDevice == "" {
		t.Fatal("expected running VM to have TAP device")
	}
	if vm.VSockCID == 0 {
		t.Fatal("expected running VM to have vsock CID")
	}
	if err := client.waitForExecReady(ctx, vm.ID); err != nil {
		t.Fatalf("wait for exec readiness: %v", err)
	}

	result, err := client.execVM(ctx, vm.ID, "echo kiln-vm-systemtest-ok && uname -m")
	if err != nil {
		t.Fatalf("exec VM: %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("exec exit code = %d, stderr=%q stdout=%q", result.ExitCode, result.Stderr, result.Stdout)
	}
	if !strings.Contains(result.Stdout, "kiln-vm-systemtest-ok") {
		t.Fatalf("exec stdout missing marker: %q", result.Stdout)
	}

	machineMetrics, err := client.vmMachineMetrics(ctx, vm.ID)
	if err != nil {
		t.Fatalf("VM metrics: %v", err)
	}
	if machineMetrics.MachineID != vm.MachineID {
		t.Fatalf("machine metrics ID = %q, want %q", machineMetrics.MachineID, vm.MachineID)
	}
	if !machineMetrics.RuntimeObserved || machineMetrics.RuntimePID == 0 {
		t.Fatalf("machine metrics did not observe running runtime: %+v", machineMetrics)
	}
	if machineMetrics.HealthStatus != "running" {
		t.Fatalf("machine health = %q, want running", machineMetrics.HealthStatus)
	}
	if machineMetrics.CPULimitMillis != 1000 {
		t.Fatalf("machine CPU limit = %dm, want 1000m", machineMetrics.CPULimitMillis)
	}
	if machineMetrics.MemoryLimitBytes != 512*1024*1024 {
		t.Fatalf("machine memory limit = %d, want 512MiB", machineMetrics.MemoryLimitBytes)
	}
	assertExplicitMachineMemoryObservation(t, *machineMetrics)
	summary, err := client.metricsSummary(ctx)
	if err != nil {
		t.Fatalf("metrics summary: %v", err)
	}
	if summary.Node.Status == "" || summary.Node.Capacity.MaxRunningVMs == 0 {
		t.Fatalf("summary node metrics incomplete: %+v", summary.Node)
	}
	if !summaryIncludesMachine(summary, vm.MachineID) {
		t.Fatalf("summary does not include machine %s: %+v", vm.MachineID, summary.Machines)
	}
	if !summaryReportsExplicitMachineMemoryObservation(t, summary, vm.MachineID) {
		t.Fatalf("summary does not report explicit memory observation for machine %s: %+v", vm.MachineID, summary.Machines)
	}

	stopStarted := time.Now().Add(-time.Second)
	if err := client.stopVM(ctx, vm.ID); err != nil {
		t.Fatalf("stop VM: %v", err)
	}
	if _, err := client.waitForVMState(ctx, vm.ID, models.MachineStateStopped); err != nil {
		t.Fatalf("wait for stopped: %v", err)
	}
	if err := waitForNoTap(ctx, vm.TAPDevice); err != nil {
		t.Fatal(err)
	}
	assertNoStopErrorLog(t, stopStarted, vm.MachineID)

	if err := client.deleteVM(ctx, vm.ID); err != nil {
		t.Fatalf("delete VM: %v", err)
	}
}

func summaryIncludesMachine(summary *models.MetricsSummary, machineID string) bool {
	for _, machine := range summary.Machines {
		if machine.MachineID == machineID {
			return true
		}
	}
	return false
}

func summaryReportsExplicitMachineMemoryObservation(t *testing.T, summary *models.MetricsSummary, machineID string) bool {
	t.Helper()
	for _, machine := range summary.Machines {
		if machine.MachineID == machineID {
			assertExplicitMachineMemoryObservation(t, machine)
			return true
		}
	}
	return false
}

func TestAPIFailureCleanup(t *testing.T) {
	client := requireClient(t)
	cleanupResourcesByPrefix(t, client)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	badVMName := uniqueName("bad-vm")
	_, err := client.createVM(ctx, models.CreateVMRequest{
		Name:     badVMName,
		Profile:  "profile-that-does-not-exist",
		MemoryMB: 512,
	})
	if err == nil {
		t.Fatal("expected VM create with unknown profile to fail")
	}
	expectAPIStatus(t, err, http.StatusBadRequest)

	vms, err := client.listVMs(ctx)
	if err != nil {
		t.Fatalf("list VMs: %v", err)
	}
	for _, vm := range vms {
		if vm.Name == badVMName {
			t.Fatalf("failed VM create left VM record: %+v", vm)
		}
	}
}
