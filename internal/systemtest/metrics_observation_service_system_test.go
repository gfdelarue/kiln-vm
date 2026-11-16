//go:build system && service

package systemtest

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/gfdelarue/kiln-vm/internal/models"
)

func TestServiceMetricsReportExplicitMemoryObservationForRunningVM(t *testing.T) {
	client := requireClient(t)
	cleanupResourcesByPrefix(t, client)

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	withSystemProfileImageCleanup(t, client, ctx)

	name := uniqueName("service-metrics-memory")
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

	if _, err := client.waitForVMState(ctx, vm.ID, models.MachineStateRunning); err != nil {
		t.Fatalf("wait for running: %v", err)
	}
	metrics, err := client.vmMachineMetrics(ctx, vm.ID)
	if err != nil {
		t.Fatalf("VM metrics: %v", err)
	}
	if !metrics.RuntimeObserved || metrics.RuntimePID == 0 {
		t.Fatalf("machine metrics did not observe runtime: %+v", metrics)
	}
	assertExplicitMachineMemoryObservation(t, *metrics)
	if metrics.MemoryLimitBytes != 512*1024*1024 {
		t.Fatalf("machine memory limit = %d, want 512MiB", metrics.MemoryLimitBytes)
	}
}
