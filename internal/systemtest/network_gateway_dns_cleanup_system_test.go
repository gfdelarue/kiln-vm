//go:build system

package systemtest

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gfdelarue/kiln-vm/internal/models"
)

type bringdownBaseline struct {
	dataDir string
}

type targetFirecrackerProcess struct {
	pid        int
	apiSocket  string
	configPath string
}

func captureBringdownBaseline(t *testing.T, ctx context.Context, client *apiClient) bringdownBaseline {
	t.Helper()
	report, err := client.diskUsage(ctx)
	if err != nil {
		t.Fatalf("capture gateway DNS bringdown disk baseline: %v", err)
	}
	if strings.TrimSpace(report.DataDir) == "" {
		t.Fatal("gateway DNS bringdown disk baseline has no provider data directory")
	}
	return bringdownBaseline{dataDir: filepath.Clean(report.DataDir)}
}

// requireVMBringdown verifies the consumer records, provider record,
// target Firecracker process, and provider runtime all disappear. The
// disk-usage API is used for the final check because it scans kiln-vm-owned paths
// with the controller's physical-host privileges and therefore exposes an
// orphaned machine rootfs even when this test process cannot traverse the data
// directory. The rootfs is also the only durable container that can retain the
// gateway DNS readiness tool staged at
// /run/kiln-vm/provider-runtime/dns-probe/kiln-vm-agent.
func requireVMBringdown(t *testing.T, client *apiClient, baseline bringdownBaseline, vm *models.VM) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := waitForVMBringdown(ctx, client, baseline, vm); err != nil {
		t.Fatal(err)
	}
}

func waitForVMBringdown(ctx context.Context, client *apiClient, baseline bringdownBaseline, vm *models.VM) error {
	if vm == nil || vm.ID == "" || vm.Name == "" || vm.MachineID == "" {
		return fmt.Errorf("gateway DNS bringdown target is incomplete: %+v", vm)
	}

	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	last := "provider cleanup has not been observed"
	for {
		if detail, complete := vmBringdownState(ctx, client, baseline, vm); complete {
			return nil
		} else {
			last = detail
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("gateway DNS VM %s (%s) and machine %s did not complete provider bringdown: %s: %w", vm.Name, vm.ID, vm.MachineID, last, ctx.Err())
		case <-ticker.C:
		}
	}
}

func vmBringdownState(ctx context.Context, client *apiClient, baseline bringdownBaseline, vm *models.VM) (string, bool) {
	if _, err := client.getVM(ctx, vm.ID); !isAPIStatus(err, http.StatusNotFound) {
		return fmt.Sprintf("VM lookup remains present or failed unexpectedly: %v", err), false
	}

	vms, err := client.listVMs(ctx)
	if err != nil {
		return fmt.Sprintf("list VMs: %v", err), false
	}
	for _, candidate := range vms {
		if candidate.ID == vm.ID || candidate.Name == vm.Name || candidate.MachineID == vm.MachineID {
			return fmt.Sprintf("VM inventory still contains target: %+v", candidate), false
		}
	}
	report, err := client.diskUsage(ctx)
	if err != nil {
		return fmt.Sprintf("read provider disk usage: %v", err), false
	}
	dataDir := filepath.Clean(report.DataDir)
	if dataDir != baseline.dataDir {
		return fmt.Sprintf("provider data directory changed from %q to %q", baseline.dataDir, dataDir), false
	}
	processes, err := targetFirecrackerProcesses(dataDir, vm.MachineID)
	if err != nil {
		return fmt.Sprintf("inspect target Firecracker process: %v", err), false
	}
	if len(processes) > 0 {
		return fmt.Sprintf("target Firecracker process remains: %+v", processes), false
	}

	runtimeDir := filepath.Join(dataDir, "machines", vm.MachineID)
	for _, entry := range report.Entries {
		entryPath := filepath.Clean(entry.Path)
		if diskEntryTargetsVM(entry, entryPath, runtimeDir, vm) {
			return fmt.Sprintf("provider disk inventory still contains target runtime or rootfs-backed staged provider tool: %+v", entry), false
		}
	}
	return "", true
}

// targetFirecrackerProcesses reads command lines only for processes
// whose kernel comm name is firecracker and keeps no details for unrelated
// processes. A process belongs to the target only when both Firecracker
// runtime arguments resolve to the exact machine-owned paths. This avoids the
// misleading weaker assertion that no Firecracker may run anywhere on a
// shared physical host.
func targetFirecrackerProcesses(dataDir, machineID string) ([]targetFirecrackerProcess, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, fmt.Errorf("read /proc: %w", err)
	}
	targetDir := filepath.Join(dataDir, "machines", machineID)
	targetSocket := filepath.Join(targetDir, "firecracker.sock")
	targetConfig := targetSocket + ".json"
	processes := []targetFirecrackerProcess{}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		comm, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "comm"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read /proc/%d/comm while checking target machine %s: %w", pid, machineID, err)
		}
		if strings.TrimSpace(string(comm)) != "firecracker" {
			continue
		}
		cmdline, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "cmdline"))
		if errors.Is(err, os.ErrNotExist) {
			// The process exited between ReadDir and ReadFile.
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read /proc/%d/cmdline while checking target machine %s: %w", pid, machineID, err)
		}
		apiSocket, configPath := firecrackerRuntimeArgs(cmdline)
		if filepath.Clean(apiSocket) != targetSocket || filepath.Clean(configPath) != targetConfig {
			continue
		}
		processes = append(processes, targetFirecrackerProcess{
			pid:        pid,
			apiSocket:  apiSocket,
			configPath: configPath,
		})
	}
	return processes, nil
}

func firecrackerRuntimeArgs(cmdline []byte) (apiSocket, configPath string) {
	args := strings.Split(strings.TrimRight(string(cmdline), "\x00"), "\x00")
	for index, arg := range args {
		switch {
		case arg == "--api-sock" && index+1 < len(args):
			apiSocket = args[index+1]
		case strings.HasPrefix(arg, "--api-sock="):
			apiSocket = strings.TrimPrefix(arg, "--api-sock=")
		case arg == "--config-file" && index+1 < len(args):
			configPath = args[index+1]
		case strings.HasPrefix(arg, "--config-file="):
			configPath = strings.TrimPrefix(arg, "--config-file=")
		}
	}
	return apiSocket, configPath
}

func diskEntryTargetsVM(entry models.DiskUsageEntry, entryPath, runtimeDir string, vm *models.VM) bool {
	if entry.ID == vm.MachineID || entry.Name == vm.Name || entryPath == runtimeDir || strings.HasPrefix(entryPath, runtimeDir+string(filepath.Separator)) {
		return true
	}
	// Catch target-tagged kiln-vm scratch/runtime entries outside machines/<id>
	// without comparing aggregate disk totals or making claims about unrelated
	// kiln-vm resources created concurrently.
	for _, component := range strings.Split(entryPath, string(filepath.Separator)) {
		if component == vm.MachineID {
			return true
		}
	}
	return false
}
