package machine

import (
	"fmt"

	"github.com/gfdelarue/kiln-vm/internal/apierr"
	"github.com/gfdelarue/kiln-vm/internal/models"
)

type ResourceLimits struct {
	MaxRunningVMs    int
	MaxTotalVCPUs    int
	PooledCPUs       int
	MaxTotalMemoryMB int
}

type resourceUsage struct {
	RunningVMs     int
	TotalVCPUs     int
	TotalCPUMillis int
	TotalMemoryMB  int
}

func activeResourceUsage(machines []models.Machine, excludeID string) resourceUsage {
	var usage resourceUsage
	for _, machine := range machines {
		if machine.ID == excludeID || !machineConsumesResources(machine.State) {
			continue
		}
		usage.RunningVMs++
		usage.TotalVCPUs += machine.VCPUs
		usage.TotalCPUMillis += machineCPUMillis(machine)
		usage.TotalMemoryMB += machine.MemoryMB
	}
	return usage
}

func machineConsumesResources(state models.MachineState) bool {
	return state == models.MachineStateCreating ||
		state == models.MachineStateRunning ||
		state == models.MachineStateStopping
}

func checkResourceAdmission(existing []models.Machine, excludeID string, req *models.CreateMachineRequest, limits ResourceLimits) error {
	if req.CPU != nil && req.CPU.Kind == "pooled" {
		if limits.PooledCPUs <= 0 {
			return &apierr.ValidationError{Field: "cpu.kind", Message: "pooled CPU is not configured on this host"}
		}
		if req.VCPUs > limits.PooledCPUs {
			return &apierr.ValidationError{Field: "cpu.vcpus", Message: "guest vCPUs exceed the configured pool"}
		}
	}
	usage := activeResourceUsage(existing, excludeID)
	nextRunning := usage.RunningVMs + 1
	nextCPUMillis := usage.TotalCPUMillis + requestCPUMillis(req) + limits.PooledCPUs*1000
	nextMemoryMB := usage.TotalMemoryMB + req.MemoryMB

	if limits.MaxRunningVMs > 0 && nextRunning > limits.MaxRunningVMs {
		return fmt.Errorf("running VM limit exceeded (%d > %d): %w", nextRunning, limits.MaxRunningVMs, apierr.ErrConflict)
	}
	if limits.MaxTotalVCPUs > 0 && nextCPUMillis > limits.MaxTotalVCPUs*1000 {
		return fmt.Errorf("total vCPU limit exceeded (%s > %d): %w", formatCPUMillis(nextCPUMillis), limits.MaxTotalVCPUs, apierr.ErrConflict)
	}
	if limits.MaxTotalMemoryMB > 0 && nextMemoryMB > limits.MaxTotalMemoryMB {
		return fmt.Errorf("total memory limit exceeded (%dMB > %dMB): %w", nextMemoryMB, limits.MaxTotalMemoryMB, apierr.ErrConflict)
	}
	return nil
}

func machineCPUMillis(machine models.Machine) int {
	if machine.CPU.Kind == "pooled" {
		return 0
	}
	if machine.CPU.Millis > 0 {
		return machine.CPU.Millis
	}
	return machine.VCPUs * 1000
}

func requestCPUMillis(req *models.CreateMachineRequest) int {
	if req.CPU != nil && req.CPU.Kind == "pooled" {
		return 0
	}
	if req.CPU != nil && req.CPU.Millis > 0 {
		return req.CPU.Millis
	}
	return req.VCPUs * 1000
}

func formatCPUMillis(millis int) string {
	if millis%1000 == 0 {
		return fmt.Sprintf("%d", millis/1000)
	}
	return fmt.Sprintf("%.3g", float64(millis)/1000)
}
