package machine

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gfdelarue/kiln-vm/internal/models"
)

func TestKernelArtifactTypeClassifiesUsefulFailureHints(t *testing.T) {
	dir := t.TempDir()
	elf := filepath.Join(dir, "vmlinux")
	if err := os.WriteFile(elf, []byte("\x7fELFrest"), 0600); err != nil {
		t.Fatalf("write elf: %v", err)
	}
	gzip := filepath.Join(dir, "kernel.gz")
	if err := os.WriteFile(gzip, []byte{0x1f, 0x8b, 0x08}, 0600); err != nil {
		t.Fatalf("write gzip: %v", err)
	}
	arm64 := filepath.Join(dir, "Image")
	arm64Header := make([]byte, 0x40)
	copy(arm64Header[0x38:0x3c], []byte("ARMd"))
	if err := os.WriteFile(arm64, arm64Header, 0600); err != nil {
		t.Fatalf("write arm64 image: %v", err)
	}

	tests := map[string]string{
		filepath.Join(dir, "missing"): "missing",
		elf:                           "elf",
		gzip:                          "gzip",
		arm64:                         "arm64-image",
	}
	for path, want := range tests {
		if got := kernelArtifactType(path); got != want {
			t.Fatalf("kernelArtifactType(%s) = %q, want %q", path, got, want)
		}
	}
}

func TestFirecrackerFailureDiagnosticCapturesBackendFacts(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "console.log")
	if err := os.WriteFile(logPath, []byte("boot line\nkernel panic\n"), 0600); err != nil {
		t.Fatalf("write log: %v", err)
	}
	paths := firecrackerStartPaths{
		SocketPath: filepath.Join(dir, "firecracker.sock"),
		VSockPath:  filepath.Join(dir, "firecracker.vsock"),
		ConfigPath: filepath.Join(dir, "firecracker.sock.json"),
		KernelPath: filepath.Join(dir, "missing-kernel"),
		RootfsPath: filepath.Join(dir, "rootfs.ext4"),
		LogPath:    logPath,
	}
	machine := &models.Machine{
		ID:        "machine-1",
		Name:      "bad-boot",
		TAPDevice: "kv123",
	}
	diag := buildFirecrackerFailureDiagnostic(machine, "process_start", paths, nil, errors.New("start firecracker: exit status 1"), "released runtime network artifacts")

	if diag.Category != "machine_boot" || diag.Phase != "process_start" || diag.Backend != "firecracker" {
		t.Fatalf("diagnostic identity = %+v", diag)
	}
	if diag.MachineID != machine.ID || diag.Resource != machine.Name || diag.TAPDevice != "kv123" {
		t.Fatalf("diagnostic machine facts = %+v", diag)
	}
	if diag.KernelType != "missing" || diag.KernelPath != paths.KernelPath || diag.ConfigPath != paths.ConfigPath || diag.LogPath != logPath {
		t.Fatalf("diagnostic paths = %+v", diag)
	}
	if !strings.Contains(diag.StderrTail, "kernel panic") || diag.Cleanup == "" || diag.Remediation == "" {
		t.Fatalf("diagnostic detail = %+v", diag)
	}
	if got := conciseDiagnosticError(diag); !strings.Contains(got, "process_start") || !strings.Contains(got, "kernel=") || !strings.Contains(got, "log=") {
		t.Fatalf("conciseDiagnosticError = %q", got)
	}
}
