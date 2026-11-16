package machine

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gfdelarue/kiln-vm/internal/models"
)

const cgroupCPUPeriodMicros = 100000

func configureMachineCPU(pid int, machine *models.Machine, poolCPUs int) (string, error) {
	if pid <= 0 || machine == nil {
		return "", nil
	}
	if machine.CPU.Kind == "" {
		machine.CPU.Kind = "shared"
	}
	if machine.CPU.Millis <= 0 {
		machine.CPU.Millis = machine.VCPUs * 1000
	}
	if machine.CPU.Kind == "dedicated" {
		return "", fmt.Errorf("dedicated CPU pinning is not implemented")
	}
	if machine.CPU.Kind != "shared" && machine.CPU.Kind != "pooled" {
		return "", fmt.Errorf("unsupported CPU kind %q", machine.CPU.Kind)
	}

	root, err := cgroupV2KilnRoot()
	if err != nil {
		return "", err
	}
	if machine.CPU.Kind == "pooled" {
		if poolCPUs <= 0 {
			return "", fmt.Errorf("pooled CPU is not configured")
		}
		root = filepath.Join(root, "pool")
		if err := os.MkdirAll(root, 0755); err != nil {
			return "", fmt.Errorf("create CPU pool: %w", err)
		}
		if err := os.WriteFile(filepath.Join(root, "cpu.max"), []byte(fmt.Sprintf("%d %d\n", poolCPUs*cgroupCPUPeriodMicros, cgroupCPUPeriodMicros)), 0644); err != nil {
			return "", fmt.Errorf("write CPU pool limit: %w", err)
		}
		if err := enableCgroupController(root, "cpu"); err != nil {
			return "", err
		}
	}

	dir := filepath.Join(root, sanitizeCgroupName(machine.ID))
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", fmt.Errorf("create cgroup %s: %w", dir, err)
	}
	quota := machine.CPU.Millis * cgroupCPUPeriodMicros / 1000
	if quota < 1 {
		quota = 1
	}
	limit := fmt.Sprintf("%d %d\n", quota, cgroupCPUPeriodMicros)
	if machine.CPU.Kind == "pooled" {
		limit = "max 100000\n"
	}
	if err := os.WriteFile(filepath.Join(dir, "cpu.max"), []byte(limit), 0644); err != nil {
		return "", fmt.Errorf("write cpu.max: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cpu.weight"), []byte("100\n"), 0644); err != nil {
		return "", fmt.Errorf("write CPU scheduling weight: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cgroup.procs"), []byte(fmt.Sprintf("%d\n", pid)), 0644); err != nil {
		return "", fmt.Errorf("assign firecracker process to cgroup: %w", err)
	}
	return dir, nil
}

func removeMachineCgroup(dir string) {
	if dir == "" {
		return
	}
	_ = os.Remove(dir)
}

func MachineCgroupPath(machineID string) string {
	return filepath.Join("/sys/fs/cgroup", "kiln-vm", sanitizeCgroupName(machineID))
}

func cgroupV2KilnRoot() (string, error) {
	const base = "/sys/fs/cgroup"
	if err := requireCgroupController(base, "cpu"); err != nil {
		return "", err
	}
	root := filepath.Join(base, "kiln-vm")
	if err := os.MkdirAll(root, 0755); err != nil {
		return "", fmt.Errorf("create kiln-vm cgroup root: %w", err)
	}
	if err := enableCgroupController(root, "cpu"); err != nil {
		return "", err
	}
	return root, nil
}

func requireCgroupController(dir, controller string) error {
	controllers, err := os.ReadFile(filepath.Join(dir, "cgroup.controllers"))
	if err != nil {
		return fmt.Errorf("cgroup v2 is required for kiln-vm CPU scheduling: %w", err)
	}
	if !strings.Contains(string(controllers), controller) {
		return fmt.Errorf("cgroup v2 %s controller is not available in %s", controller, dir)
	}
	return nil
}

func enableCgroupController(dir, controller string) error {
	if err := requireCgroupController(dir, controller); err != nil {
		return err
	}
	subtreeControlPath := filepath.Join(dir, "cgroup.subtree_control")
	subtreeControl, err := os.ReadFile(subtreeControlPath)
	if err != nil {
		return fmt.Errorf("read cgroup subtree control: %w", err)
	}
	if strings.Contains(string(subtreeControl), controller) {
		return nil
	}
	if err := os.WriteFile(subtreeControlPath, []byte("+"+controller+"\n"), 0644); err != nil {
		return fmt.Errorf("enable cgroup v2 %s controller in %s: %w", controller, dir, err)
	}
	return nil
}

func sanitizeCgroupName(value string) string {
	value = strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.' {
			return r
		}
		return '-'
	}, value)
	if value == "" {
		return "machine"
	}
	return value
}
