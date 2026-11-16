package machine

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gfdelarue/kiln-vm/internal/models"
)

// startFirecrackerVM starts a Firecracker process and monitors it.
// In a full implementation this would use firecracker-go-sdk, but for Phase 1
// we shell out to the firecracker binary directly for simplicity and to avoid
// the SDK's CGo dependency chain.
func startFirecrackerVM(
	firecrackerBin, socketPath, vsockPath, kernelPath, rootfsPath string,
	machine *models.Machine,
	alloc *Allocation,
	logWriter io.WriteCloser,
	processes *sync.Map,
	poolCPUs int,
	onExit func(error),
) error {
	ctx, cancel := context.WithCancel(context.Background())

	// Build kernel boot args with IP configuration
	bootArgs := buildBootArgs(alloc, guestHostname(machine.Name))

	// Create Firecracker config file
	configPath := socketPath + ".json"
	for _, path := range []string{socketPath, vsockPath, configPath} {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			cancel()
			return startError("prepare_runtime_paths", fmt.Errorf("remove stale firecracker path %s: %w", path, err))
		}
	}
	vsockConfig := ""
	if machine.VSockCID != 0 {
		vsockConfig = fmt.Sprintf(`,
  "vsock": {
    "guest_cid": %d,
    "uds_path": %q
  }`, machine.VSockCID, vsockPath)
	}

	networkInterfaces := fmt.Sprintf(`"network-interfaces": [{
    "iface_id": "eth0",
    "guest_mac": %q,
    "host_dev_name": %q
  }]`, alloc.GuestMAC, alloc.TAPDevice)

	configContent := buildFirecrackerConfigContent(firecrackerConfigInput{
		kernelPath:        kernelPath,
		bootArgs:          bootArgs,
		rootfsPath:        rootfsPath,
		networkInterfaces: networkInterfaces,
		vcpuCount:         machine.VCPUs,
		memoryMB:          machine.MemoryMB,
		vsockConfig:       vsockConfig,
	})

	if err := os.WriteFile(configPath, []byte(configContent), 0640); err != nil {
		cancel()
		return startError("config_write", fmt.Errorf("write firecracker config: %w", err))
	}

	cmd := exec.CommandContext(ctx, firecrackerBin,
		"--api-sock", socketPath,
		"--config-file", configPath,
	)
	lifecycleWriter := newLifecycleLogWriter(logWriter)
	cmd.Stdout = lifecycleWriter
	cmd.Stderr = lifecycleWriter

	if err := cmd.Start(); err != nil {
		cancel()
		_ = lifecycleWriter.Close()
		return startError("process_start", fmt.Errorf("start firecracker: %w", err))
	}
	cgroupPath, err := configureMachineCPU(cmd.Process.Pid, machine, poolCPUs)
	if err != nil {
		cancel()
		_ = cmd.Wait()
		_ = lifecycleWriter.Close()
		cleanupFirecrackerRuntimePaths(socketPath, vsockPath)
		return startError("cgroup_assign", err)
	}

	lifecycleWriter.setShutdownHook(func() {
		go func() {
			time.Sleep(250 * time.Millisecond)
			cancel()
		}()
	})

	exitCh := make(chan struct{})
	proc := &VMProcess{
		Machine:    machine,
		CgroupPath: cgroupPath,
		Cmd:        cmd,
		Cancel:     cancel,
		ExitCh:     exitCh,
	}
	processes.Store(machine.ID, proc)

	// Monitor the process in background
	go func() {
		err := cmd.Wait()
		err = normalizeVMExitError(err, lifecycleWriter.gracefulGuestShutdown(), proc.ExpectedStop.Load())
		removeMachineCgroup(cgroupPath)
		lifecycleWriter.Close()
		cleanupFirecrackerRuntimePaths(socketPath, vsockPath)
		close(exitCh)
		// A prior generation can finish after a replacement boot has stored a
		// new process under the same machine ID. Delete only the process instance
		// owned by this monitor.
		processes.CompareAndDelete(machine.ID, proc)
		if onExit != nil {
			onExit(err)
		}
		if err != nil {
			slog.Error("VM exited with error", "machine", machine.ID, "error", err)
		} else {
			slog.Info("VM exited cleanly", "machine", machine.ID)
		}
	}()

	return nil
}

func cleanupFirecrackerRuntimePaths(paths ...string) {
	for _, path := range paths {
		if path == "" {
			continue
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			slog.Warn("failed to remove Firecracker runtime path", "path", path, "error", err)
		}
	}
}

type firecrackerConfigInput struct {
	kernelPath        string
	bootArgs          string
	rootfsPath        string
	networkInterfaces string
	vcpuCount         int
	memoryMB          int
	vsockConfig       string
}

func buildFirecrackerConfigContent(input firecrackerConfigInput) string {
	return fmt.Sprintf(`{
  "boot-source": {
    "kernel_image_path": %q,
    "boot_args": %q
  },
  "drives": [{
    "drive_id": "rootfs",
    "path_on_host": %q,
    "is_root_device": true,
    "is_read_only": false
  }],
  %s,
  "machine-config": {
    "vcpu_count": %d,
    "mem_size_mib": %d
  },
  "entropy": {}
%s}`, input.kernelPath, input.bootArgs, input.rootfsPath, input.networkInterfaces,
		input.vcpuCount, input.memoryMB, input.vsockConfig)
}

func buildBootArgs(alloc *Allocation, hostname string) string {
	return fmt.Sprintf(
		"console=ttyS0 reboot=k panic=1 pci=off random.trust_cpu=on init=/sbin/kiln-vm-init ip=%s::%s:%s:%s:eth0:off",
		alloc.GuestIP, alloc.GatewayIP, alloc.Mask, hostname,
	)
}

func normalizeVMExitError(err error, gracefulGuestShutdown, expectedStop bool) error {
	if gracefulGuestShutdown || expectedStop {
		return nil
	}
	return err
}

type lifecycleLogWriter struct {
	inner         io.WriteCloser
	hook          atomic.Value
	gracefulExit  atomic.Bool
	shutdownFired atomic.Bool
	mu            sync.Mutex
	tail          []byte
}

func newLifecycleLogWriter(inner io.WriteCloser) *lifecycleLogWriter {
	return &lifecycleLogWriter{inner: inner}
}

func (lw *lifecycleLogWriter) setShutdownHook(fn func()) {
	lw.hook.Store(fn)
}

func (lw *lifecycleLogWriter) Write(p []byte) (int, error) {
	if !lw.shutdownFired.Load() && lw.observe(p) {
		lw.gracefulExit.Store(true)
		lw.shutdownFired.Store(true)
		if hook, ok := lw.hook.Load().(func()); ok && hook != nil {
			hook()
		}
	}
	n, err := lw.inner.Write(p)
	if err != nil {
		return n, fmt.Errorf("write lifecycle log: %w", err)
	}
	return n, nil
}

func (lw *lifecycleLogWriter) Close() error {
	if err := lw.inner.Close(); err != nil {
		return fmt.Errorf("close lifecycle log: %w", err)
	}
	return nil
}

func (lw *lifecycleLogWriter) gracefulGuestShutdown() bool {
	return lw.gracefulExit.Load()
}

func guestShutdownMarker(p []byte) bool {
	return bytes.Contains(p, []byte("reboot: System halted"))
}

func (lw *lifecycleLogWriter) observe(p []byte) bool {
	lw.mu.Lock()
	defer lw.mu.Unlock()

	lw.tail = append(lw.tail, p...)
	if len(lw.tail) > 8192 {
		lw.tail = lw.tail[len(lw.tail)-8192:]
	}
	return guestShutdownMarker(lw.tail)
}
