package metrics

import (
	"testing"

	"github.com/gfdelarue/kiln-vm/internal/config"
	"github.com/gfdelarue/kiln-vm/internal/models"
)

func TestCapacityReservesStoppingMachinesUntilRelease(t *testing.T) {
	s := Service{cfg: &config.Config{MaxRunningVMs: 20, MaxTotalVCPUs: 18, MaxTotalMemoryMB: 40960}}
	machines := []models.Machine{
		{State: models.MachineStateCreating, VCPUs: 4, MemoryMB: 8192},
		{State: models.MachineStateRunning, VCPUs: 4, MemoryMB: 8192},
		{State: models.MachineStateStopping, VCPUs: 4, MemoryMB: 8192},
	}
	c := s.capacity(machines)
	if c.AvailableRunningVMs != 17 || c.AvailableCPUMillis != 6000 || c.AvailableMemoryBytes != 16384*1024*1024 {
		t.Fatalf("stopping VM allocation was advertised as available: %+v", c)
	}
	if c.RunningOrStartingMachines != 2 {
		t.Fatalf("stopping is allocated but not running or starting: %+v", c)
	}
	machines[2].State = models.MachineStateStopped
	c = s.capacity(machines)
	if c.AvailableRunningVMs != 18 || c.AvailableCPUMillis != 10000 || c.AvailableMemoryBytes != 24576*1024*1024 {
		t.Fatalf("stopped VM allocation was not released: %+v", c)
	}
}
