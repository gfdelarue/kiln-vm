package machine

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type firecrackerProcess struct {
	PID        int
	MachineID  string
	ConfigPath string
}

func terminateOrphanFirecrackerProcesses(dataDir string, timeout time.Duration) ([]firecrackerProcess, error) {
	processes, err := findOwnedFirecrackerProcesses(dataDir)
	if err != nil {
		return nil, err
	}
	for _, process := range processes {
		proc, err := os.FindProcess(process.PID)
		if err != nil {
			continue
		}
		if err := proc.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
			slog.Warn("failed to signal orphaned Firecracker process", "pid", process.PID, "machine", process.MachineID, "error", err)
		}
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if allProcessesExited(processes) {
			return processes, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	for _, process := range processes {
		if !processExists(process.PID) {
			continue
		}
		proc, err := os.FindProcess(process.PID)
		if err != nil {
			continue
		}
		if err := proc.Signal(syscall.SIGKILL); err != nil && !errors.Is(err, os.ErrProcessDone) {
			slog.Warn("failed to kill orphaned Firecracker process", "pid", process.PID, "machine", process.MachineID, "error", err)
		}
	}
	return processes, nil
}

func findOwnedFirecrackerProcesses(dataDir string) ([]firecrackerProcess, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, fmt.Errorf("read /proc: %w", err)
	}
	var processes []firecrackerProcess
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		cmdline, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "cmdline"))
		if err != nil || len(cmdline) == 0 {
			continue
		}
		args := splitProcCmdline(cmdline)
		machineID, configPath := machineIDFromFirecrackerArgs(args, dataDir)
		if machineID == "" {
			continue
		}
		processes = append(processes, firecrackerProcess{
			PID:        pid,
			MachineID:  machineID,
			ConfigPath: configPath,
		})
	}
	return processes, nil
}

func machineIDFromFirecrackerArgs(args []string, dataDir string) (string, string) {
	configPath := firecrackerConfigPathFromArgs(args)
	if configPath == "" {
		return "", ""
	}
	machinesRoot := filepath.Clean(filepath.Join(dataDir, "machines"))
	cleanConfigPath := filepath.Clean(configPath)
	relative, err := filepath.Rel(machinesRoot, cleanConfigPath)
	if err != nil || relative == "." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || relative == ".." {
		return "", ""
	}
	parts := strings.Split(relative, string(filepath.Separator))
	if len(parts) < 2 || parts[0] == "" {
		return "", ""
	}
	if filepath.Base(cleanConfigPath) != "firecracker.sock.json" {
		return "", ""
	}
	return parts[0], cleanConfigPath
}

func firecrackerConfigPathFromArgs(args []string) string {
	for index, arg := range args {
		if arg == "--config-file" && index+1 < len(args) {
			return args[index+1]
		}
		if strings.HasPrefix(arg, "--config-file=") {
			return strings.TrimPrefix(arg, "--config-file=")
		}
	}
	return ""
}

func splitProcCmdline(cmdline []byte) []string {
	parts := strings.Split(strings.TrimRight(string(cmdline), "\x00"), "\x00")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		if part != "" {
			result = append(result, part)
		}
	}
	return result
}

func allProcessesExited(processes []firecrackerProcess) bool {
	for _, process := range processes {
		if processExists(process.PID) {
			return false
		}
	}
	return true
}

func processExists(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
