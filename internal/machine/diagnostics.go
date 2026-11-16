package machine

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gfdelarue/kiln-vm/internal/models"
)

const (
	firecrackerBackend             = "firecracker"
	machineDiagnosticTailBytes     = 4096
	defaultBootFailureRemediate    = "check the VM kernel path, rootfs, Firecracker binary, and host virtualization support; run kiln-vm get for details"
	defaultNetworkCleanupRemediate = "retry the lifecycle operation; kiln-vm will reconcile its owned DNS admission, proxy, NAT, and TAP artifacts"
)

type firecrackerStartPaths struct {
	SocketPath string
	VSockPath  string
	ConfigPath string
	KernelPath string
	RootfsPath string
	LogPath    string
}

type firecrackerStartError struct {
	Phase string
	Err   error
}

func (e *firecrackerStartError) Error() string {
	if e == nil || e.Err == nil {
		return "firecracker start failed"
	}
	return e.Err.Error()
}

func (e *firecrackerStartError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func startError(phase string, err error) error {
	if err == nil {
		return nil
	}
	return &firecrackerStartError{Phase: phase, Err: err}
}

func firecrackerErrorPhase(err error, fallback string) string {
	var startErr *firecrackerStartError
	if errors.As(err, &startErr) && startErr.Phase != "" {
		return startErr.Phase
	}
	return fallback
}

func buildFirecrackerFailureDiagnostic(machine *models.Machine, phase string, paths firecrackerStartPaths, alloc *Allocation, err error, cleanup string) models.Diagnostic {
	diagnostic := models.Diagnostic{
		Category:    "machine_boot",
		Phase:       phase,
		Action:      "start",
		Retryable:   false,
		Backend:     firecrackerBackend,
		KernelPath:  paths.KernelPath,
		KernelType:  kernelArtifactType(paths.KernelPath),
		RootfsPath:  paths.RootfsPath,
		ConfigPath:  paths.ConfigPath,
		SocketPath:  paths.SocketPath,
		VSockPath:   paths.VSockPath,
		LogPath:     paths.LogPath,
		Cleanup:     cleanup,
		Remediation: defaultBootFailureRemediate,
		Error:       errorString(err),
		StderrTail:  tailFile(paths.LogPath, machineDiagnosticTailBytes),
	}
	if machine != nil {
		diagnostic.Resource = machine.Name
		diagnostic.MachineID = machine.ID
		diagnostic.TAPDevice = machine.TAPDevice
	}
	if alloc != nil {
		diagnostic.TAPDevice = alloc.TAPDevice
	}
	if phase == "process_exit" {
		diagnostic.Remediation = "inspect the Firecracker log and kernel/rootfs compatibility; run kiln-vm get for config and log paths"
	}
	if strings.TrimSpace(diagnostic.StderrTail) == "" {
		diagnostic.StderrTail = ""
	}
	return diagnostic
}

func buildProviderNetworkCleanupDiagnosticForAllocation(machine *models.Machine, alloc *Allocation, action string, err error) models.Diagnostic {
	diagnostic := models.Diagnostic{
		Category:    "provider_network",
		Phase:       "cleanup",
		Action:      action,
		Retryable:   true,
		Backend:     firecrackerBackend,
		Cleanup:     "provider network cleanup incomplete",
		Remediation: defaultNetworkCleanupRemediate,
		Error:       errorString(err),
	}
	if machine != nil {
		diagnostic.Resource = machine.Name
		diagnostic.MachineID = machine.ID
		diagnostic.TAPDevice = machine.TAPDevice
	}
	if alloc != nil {
		diagnostic.TAPDevice = alloc.TAPDevice
		diagnostic.NetworkCIDR = alloc.BaseCIDR
		diagnostic.NetworkIndex = alloc.NetworkIndex
		diagnostic.GuestIP = alloc.GuestIP
		diagnostic.GatewayIP = alloc.GatewayIP
		diagnostic.GuestMAC = alloc.GuestMAC
		diagnostic.Netmask = alloc.Mask
		diagnostic.TAPAlias = publicNetworkTAPAlias(*alloc)
	}
	return diagnostic
}

func conciseDiagnosticError(diagnostic models.Diagnostic) string {
	base := strings.TrimSpace(diagnostic.Error)
	if base == "" {
		base = "machine boot failed"
	}
	if diagnostic.Phase != "" {
		base = fmt.Sprintf("%s: %s", diagnostic.Phase, base)
	}
	if diagnostic.KernelPath != "" {
		base += fmt.Sprintf(" (kernel=%s type=%s)", diagnostic.KernelPath, withDiagnosticDefault(diagnostic.KernelType, "unknown"))
	}
	if diagnostic.LogPath != "" {
		base += fmt.Sprintf("; log=%s", diagnostic.LogPath)
	}
	return base
}

func kernelArtifactType(path string) string {
	if strings.TrimSpace(path) == "" {
		return "missing"
	}
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "missing"
		}
		return "unreadable"
	}
	defer file.Close()

	header := make([]byte, 128)
	n, err := file.Read(header)
	if err != nil && n == 0 {
		return "unreadable"
	}
	header = header[:n]
	if len(header) >= 4 && string(header[:4]) == "\x7fELF" {
		return "elf"
	}
	if len(header) >= 2 && header[0] == 0x1f && header[1] == 0x8b {
		return "gzip"
	}
	if len(header) >= 0x3c && string(header[0x38:0x3c]) == "ARMd" {
		return "arm64-image"
	}
	if len(header) >= 2 && string(header[:2]) == "MZ" {
		return "pe-or-bzimage"
	}
	return "raw"
}

func tailFile(path string, limit int) string {
	if strings.TrimSpace(path) == "" || limit <= 0 {
		return ""
	}
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return ""
	}
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer file.Close()
	size := info.Size()
	offset := int64(0)
	if size > int64(limit) {
		offset = size - int64(limit)
	}
	buf := make([]byte, size-offset)
	if len(buf) == 0 {
		return ""
	}
	if _, err := file.ReadAt(buf, offset); err != nil {
		return ""
	}
	return strings.TrimRight(string(buf), "\x00")
}

func machineLogPath(dataDir, machineID string) string {
	if strings.TrimSpace(dataDir) == "" || strings.TrimSpace(machineID) == "" {
		return ""
	}
	return filepath.Join(dataDir, "machines", machineID, "console.log")
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func withDiagnosticDefault(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}
