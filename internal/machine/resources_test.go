package machine

import (
	"context"
	"errors"
	"fmt"
	"github.com/gfdelarue/kiln-vm/internal/config"
	"strings"
	"testing"

	"github.com/gfdelarue/kiln-vm/internal/apierr"
	"github.com/gfdelarue/kiln-vm/internal/models"
)

func TestActiveResourceUsageCountsOnlyResourceConsumingStates(t *testing.T) {
	machines := []models.Machine{
		{ID: "creating", State: models.MachineStateCreating, VCPUs: 1, MemoryMB: 256},
		{ID: "running", State: models.MachineStateRunning, VCPUs: 2, MemoryMB: 512},
		{ID: "stopping", State: models.MachineStateStopping, VCPUs: 3, MemoryMB: 1024},
		{ID: "stopped", State: models.MachineStateStopped, VCPUs: 4, MemoryMB: 2048},
		{ID: "error", State: models.MachineStateError, VCPUs: 5, MemoryMB: 4096},
	}

	usage := activeResourceUsage(machines, "")

	if usage.RunningVMs != 3 {
		t.Fatalf("RunningVMs = %d, want 3", usage.RunningVMs)
	}
	if usage.TotalVCPUs != 6 {
		t.Fatalf("TotalVCPUs = %d, want 6", usage.TotalVCPUs)
	}
	if usage.TotalCPUMillis != 6000 {
		t.Fatalf("TotalCPUMillis = %d, want 6000", usage.TotalCPUMillis)
	}
	if usage.TotalMemoryMB != 1792 {
		t.Fatalf("TotalMemoryMB = %d, want 1792", usage.TotalMemoryMB)
	}
}

func TestActiveResourceUsageExcludesRestartedMachine(t *testing.T) {
	machines := []models.Machine{
		{ID: "same", State: models.MachineStateStopped, VCPUs: 4, MemoryMB: 4096},
		{ID: "other", State: models.MachineStateRunning, VCPUs: 2, MemoryMB: 2048},
	}

	usage := activeResourceUsage(machines, "same")

	if usage.RunningVMs != 1 || usage.TotalVCPUs != 2 || usage.TotalCPUMillis != 2000 || usage.TotalMemoryMB != 2048 {
		t.Fatalf("usage = %+v, want one active machine with 2 vCPU and 2048MB", usage)
	}
}

func TestCheckResourceAdmissionAccountsFractionalCPUMillis(t *testing.T) {
	existing := []models.Machine{
		{ID: "m-1", State: models.MachineStateRunning, VCPUs: 1, CPU: models.CPUConfig{Kind: "shared", VCPUs: 1, Millis: 750}, MemoryMB: 512},
	}
	req := &models.CreateMachineRequest{CPU: &models.CPUConfig{Kind: "shared", VCPUs: 1, Millis: 250}, VCPUs: 1, MemoryMB: 512}

	if err := checkResourceAdmission(existing, "", req, ResourceLimits{MaxTotalVCPUs: 1, MaxTotalMemoryMB: 2048}); err != nil {
		t.Fatalf("checkResourceAdmission: %v", err)
	}

	req.CPU.Millis = 251
	if err := checkResourceAdmission(existing, "", req, ResourceLimits{MaxTotalVCPUs: 1, MaxTotalMemoryMB: 2048}); !errors.Is(err, apierr.ErrConflict) {
		t.Fatalf("error = %v, want conflict", err)
	}
}

func TestCheckResourceAdmissionRejectsExceededLimits(t *testing.T) {
	existing := []models.Machine{
		{ID: "m-1", State: models.MachineStateRunning, VCPUs: 2, MemoryMB: 2048},
	}
	req := &models.CreateMachineRequest{VCPUs: 2, MemoryMB: 2048}

	tests := []struct {
		name   string
		limits ResourceLimits
	}{
		{name: "running VM count", limits: ResourceLimits{MaxRunningVMs: 1}},
		{name: "total vCPU", limits: ResourceLimits{MaxTotalVCPUs: 3}},
		{name: "total memory", limits: ResourceLimits{MaxTotalMemoryMB: 3072}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkResourceAdmission(existing, "", req, tt.limits)
			if !errors.Is(err, apierr.ErrConflict) {
				t.Fatalf("error = %v, want conflict", err)
			}
		})
	}
}

func TestCheckResourceAdmissionAllowsUnlimitedZeroLimits(t *testing.T) {
	existing := []models.Machine{
		{ID: "m-1", State: models.MachineStateRunning, VCPUs: 64, MemoryMB: 262144},
	}
	req := &models.CreateMachineRequest{VCPUs: 64, MemoryMB: 262144}

	if err := checkResourceAdmission(existing, "", req, ResourceLimits{}); err != nil {
		t.Fatalf("checkResourceAdmission: %v", err)
	}
}

func TestManagerCheckResourceAdmissionReconcilesExitedStoppingFirecrackerMachines(t *testing.T) {
	store := &resourceAdmissionStore{machines: []models.Machine{
		{ID: "builder", Name: "builder", State: models.MachineStateStopping, VCPUs: 2, MemoryMB: 4096},
		{ID: "db", Name: "db", State: models.MachineStateRunning, VCPUs: 1, MemoryMB: 1024},
		{ID: "api", Name: "api", State: models.MachineStateCreating, VCPUs: 1, MemoryMB: 1024},
		{ID: "worker", Name: "worker", State: models.MachineStateCreating, VCPUs: 1, MemoryMB: 768},
	}}
	manager := &Manager{
		db:     store,
		limits: ResourceLimits{MaxTotalMemoryMB: 8192},
	}
	req := &models.CreateMachineRequest{VCPUs: 1, MemoryMB: 2048}

	if err := manager.checkResourceAdmission("", req); err != nil {
		t.Fatalf("checkResourceAdmission: %v", err)
	}
	if got := store.state("builder"); got != models.MachineStateStopped {
		t.Fatalf("builder state = %s, want reconciled stopped", got)
	}
}

func TestManagerCheckResourceAdmissionKeepsStoppingFirecrackerWithProcessActive(t *testing.T) {
	store := &resourceAdmissionStore{machines: []models.Machine{
		{ID: "builder", Name: "builder", State: models.MachineStateStopping, VCPUs: 2, MemoryMB: 4096},
		{ID: "db", Name: "db", State: models.MachineStateRunning, VCPUs: 1, MemoryMB: 1024},
		{ID: "api", Name: "api", State: models.MachineStateCreating, VCPUs: 1, MemoryMB: 1024},
		{ID: "worker", Name: "worker", State: models.MachineStateCreating, VCPUs: 1, MemoryMB: 768},
	}}
	manager := &Manager{
		db:     store,
		limits: ResourceLimits{MaxTotalMemoryMB: 8192},
	}
	manager.processes.Store("builder", &VMProcess{})
	req := &models.CreateMachineRequest{VCPUs: 1, MemoryMB: 2048}

	if err := manager.checkResourceAdmission("", req); !errors.Is(err, apierr.ErrConflict) {
		t.Fatalf("checkResourceAdmission error = %v, want conflict while builder process is still active", err)
	}
	if got := store.state("builder"); got != models.MachineStateStopping {
		t.Fatalf("builder state = %s, want still stopping", got)
	}
}

type resourceAdmissionStore struct {
	libvirtDispatchStore
	machines []models.Machine
}

func (s *resourceAdmissionStore) ListMachines() ([]models.Machine, error) {
	return append([]models.Machine(nil), s.machines...), nil
}

func (s *resourceAdmissionStore) UpdateMachineState(id string, state models.MachineState, errMsg string) error {
	for i := range s.machines {
		if s.machines[i].ID == id {
			s.machines[i].State = state
			s.machines[i].Error = errMsg
		}
	}
	return nil
}

func (s *resourceAdmissionStore) state(id string) models.MachineState {
	for _, machine := range s.machines {
		if machine.ID == id {
			return machine.State
		}
	}
	return ""
}

func TestPooledCampaignsLeaveCapacityForCI(t *testing.T) {
	limits := ResourceLimits{MaxRunningVMs: 21, MaxTotalVCPUs: 24, PooledCPUs: 18, MaxTotalMemoryMB: 55804}
	var existing []models.Machine
	req := &models.CreateMachineRequest{VCPUs: 18, MemoryMB: 2380, CPU: &models.CPUConfig{Kind: "pooled", VCPUs: 18, Millis: 18000}}
	for i := 0; i < 20; i++ {
		if err := checkResourceAdmission(existing, "", req, limits); err != nil {
			t.Fatalf("campaign %d: %v", i, err)
		}
		existing = append(existing, models.Machine{State: models.MachineStateRunning, VCPUs: 18, MemoryMB: 2380, CPU: *req.CPU, ID: fmt.Sprint(i)})
	}
	ci := &models.CreateMachineRequest{VCPUs: 4, MemoryMB: 8192}
	if err := checkResourceAdmission(existing, "", ci, limits); err != nil {
		t.Fatalf("CI admission: %v", err)
	}
	existing = append(existing, models.Machine{ID: "ci", State: models.MachineStateRunning, VCPUs: 4, MemoryMB: 8192})
	if err := checkResourceAdmission(existing, "", req, limits); err == nil {
		t.Fatal("accepted over VM and RAM limits")
	}
	limits.MaxRunningVMs = 0
	limits.MaxTotalMemoryMB = 0
	ci.VCPUs = 3
	if err := checkResourceAdmission(existing, "", ci, limits); err == nil {
		t.Fatal("oversubscribed reserved CPU capacity")
	}
	limits.PooledCPUs = 0
	if err := checkResourceAdmission(nil, "", req, limits); err == nil {
		t.Fatal("accepted an unconfigured pool")
	}
}

func TestFirecrackerRestartPreservesCPUAllocation(t *testing.T) {
	for _, cpu := range []models.CPUConfig{{Kind: "pooled", VCPUs: 18, Millis: 18000}, {Kind: "shared", VCPUs: 4, Millis: 1200}} {
		t.Run(cpu.Kind, func(t *testing.T) {
			guest := &models.Machine{ID: "restart", Name: "restart", Backend: "firecracker", State: models.MachineStateStopped, VCPUs: cpu.VCPUs, CPU: cpu, MemoryMB: 512, VSockCID: 3}
			store := &resourceAdmissionStore{libvirtDispatchStore: libvirtDispatchStore{machine: guest}, machines: []models.Machine{{ID: "ci", State: models.MachineStateRunning, VCPUs: 4, CPU: models.CPUConfig{Kind: "shared", Millis: 4000, VCPUs: 4}, MemoryMB: 8192}}}
			manager := NewManager(&config.Config{DataDir: t.TempDir(), MaxTotalVCPUs: 24, PooledCPUs: 18}, store)
			_, err := manager.Start(context.Background(), guest.ID)
			// No root disk is created: reaching this error proves admission used the
			// stored CPU allocation instead of treating guest parallelism as a quota.
			if err == nil || !strings.Contains(err.Error(), "rootfs not found") {
				t.Fatalf("restart did not pass admission: %v", err)
			}
		})
	}
}
