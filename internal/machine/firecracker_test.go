package machine

import (
	"bytes"
	"errors"
	"io"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/gfdelarue/kiln-vm/internal/models"
)

func TestLifecycleLogWriterDetectsSplitShutdownMarker(t *testing.T) {
	writer := newLifecycleLogWriter(nopWriteCloser{Writer: io.Discard})

	var hookCalls atomic.Int32
	writer.setShutdownHook(func() {
		hookCalls.Add(1)
	})

	chunks := [][]byte{
		[]byte("booting guest\nkiln-init: work"),
		[]byte("load exited with status 0\n"),
		[]byte("[    3.018207] reboot: System halted\n"),
	}

	for _, chunk := range chunks {
		if _, err := writer.Write(chunk); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	if !writer.gracefulGuestShutdown() {
		t.Fatal("expected graceful guest shutdown to be detected")
	}

	if hookCalls.Load() != 1 {
		t.Fatalf("expected shutdown hook to fire once, got %d", hookCalls.Load())
	}
}

func TestNormalizeVMExitErrorClearsGracefulGuestShutdown(t *testing.T) {
	err := errors.New("signal: terminated")

	if got := normalizeVMExitError(err, true, false); got != nil {
		t.Fatalf("expected graceful guest shutdown to clear error, got %v", got)
	}
}

func TestNormalizeVMExitErrorClearsExpectedStop(t *testing.T) {
	err := errors.New("signal: terminated")

	if got := normalizeVMExitError(err, false, true); got != nil {
		t.Fatalf("expected intentional stop to clear error, got %v", got)
	}
}

func TestNormalizeVMExitErrorPreservesUnexpectedExit(t *testing.T) {
	err := errors.New("signal: terminated")

	if got := normalizeVMExitError(err, false, false); !errors.Is(got, err) {
		t.Fatalf("expected unexpected exit error to be preserved, got %v", got)
	}
}

func TestFirecrackerBootArgsTrustCPUEntropy(t *testing.T) {
	args := buildBootArgs(&Allocation{
		GuestIP:   "172.16.0.2",
		GatewayIP: "172.16.0.1",
		Mask:      "255.255.255.252",
	}, "dev")

	if !bytes.Contains([]byte(args), []byte("random.trust_cpu=on")) {
		t.Fatalf("boot args missing random.trust_cpu=on: %s", args)
	}
}

func TestFirecrackerBootArgsSetGuestHostname(t *testing.T) {
	args := buildBootArgs(&Allocation{
		GuestIP:   "172.16.1.250",
		GatewayIP: "172.16.1.249",
		Mask:      "255.255.255.252",
	}, "devbox-ubuntu")

	want := "ip=172.16.1.250::172.16.1.249:255.255.255.252:devbox-ubuntu:eth0:off"
	if !bytes.Contains([]byte(args), []byte(want)) {
		t.Fatalf("boot args = %s, want %s", args, want)
	}
}

func TestFirecrackerConfigEnablesEntropyDevice(t *testing.T) {
	config := buildFirecrackerConfigContent(firecrackerConfigInput{
		kernelPath:        "/kernel",
		bootArgs:          "console=ttyS0",
		rootfsPath:        "/rootfs",
		networkInterfaces: `"network-interfaces": []`,
		vcpuCount:         1,
		memoryMB:          512,
	})

	if !bytes.Contains([]byte(config), []byte(`"entropy": {}`)) {
		t.Fatalf("config missing entropy device: %s", config)
	}
}

func TestStartFirecrackerVMClassifiesProcessStartFailure(t *testing.T) {
	dir := t.TempDir()
	err := startFirecrackerVM(
		filepath.Join(dir, "missing-firecracker"),
		filepath.Join(dir, "firecracker.sock"),
		filepath.Join(dir, "firecracker.vsock"),
		filepath.Join(dir, "vmlinux"),
		filepath.Join(dir, "rootfs.ext4"),
		&models.Machine{ID: "machine-1", Name: "machine-1", VCPUs: 1, MemoryMB: 128},
		&Allocation{GuestIP: "172.16.0.2", GatewayIP: "172.16.0.1", Mask: "255.255.255.252", TAPDevice: "kv0", GuestMAC: "02:fc:00:00:00:01"},
		nopWriteCloser{Writer: io.Discard},
		nil,
		0,
		nil,
	)
	if err == nil {
		t.Fatal("startFirecrackerVM succeeded, want process start failure")
	}
	var startErr *firecrackerStartError
	if !errors.As(err, &startErr) {
		t.Fatalf("error = %T %v, want firecrackerStartError", err, err)
	}
	if startErr.Phase != "process_start" {
		t.Fatalf("phase = %q, want process_start", startErr.Phase)
	}
}

type nopWriteCloser struct {
	io.Writer
}

func (nopWriteCloser) Close() error {
	return nil
}
