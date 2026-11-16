package image

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/gfdelarue/kiln-vm/internal/apierr"
	"github.com/gfdelarue/kiln-vm/internal/hostfs"
)

const (
	defaultMinFreeMB     int64 = 2048
	sparseProbeSizeBytes int64 = 64 * 1024 * 1024
)

type resourceSnapshot struct {
	Path              string
	AvailableBytes    uint64
	TotalBytes        uint64
	MemAvailableBytes uint64
	SwapFreeBytes     uint64
}

type MaterializationError struct {
	Stage         string
	Message       string
	Path          string
	RequestedMB   int64
	RequiredBytes uint64
	Snapshot      resourceSnapshot
	Cause         error
	CommandOutput string
	ResourceLimit bool
	ProcessKilled bool
}

func (e *MaterializationError) Error() string {
	var b strings.Builder
	if e.Message != "" {
		b.WriteString(e.Message)
	} else if e.Stage != "" {
		b.WriteString("image materialization failed during " + e.Stage)
	} else {
		b.WriteString("image materialization failed")
	}
	if e.Path != "" {
		b.WriteString(" at ")
		b.WriteString(e.Path)
	}
	if e.RequestedMB > 0 {
		b.WriteString(fmt.Sprintf(" for %dMiB rootfs", e.RequestedMB))
	}
	if e.RequiredBytes > 0 || e.Snapshot.AvailableBytes > 0 {
		b.WriteString(fmt.Sprintf(" (available %s", formatBytes(e.Snapshot.AvailableBytes)))
		if e.RequiredBytes > 0 {
			b.WriteString(fmt.Sprintf(", required %s", formatBytes(e.RequiredBytes)))
		}
		b.WriteString(")")
	}
	if e.ProcessKilled {
		b.WriteString(fmt.Sprintf("; process was killed, host MemAvailable=%s SwapFree=%s", formatBytes(e.Snapshot.MemAvailableBytes), formatBytes(e.Snapshot.SwapFreeBytes)))
	}
	if e.CommandOutput != "" {
		b.WriteString(": ")
		b.WriteString(e.CommandOutput)
	}
	if e.Cause != nil {
		b.WriteString(": ")
		b.WriteString(e.Cause.Error())
	}
	return b.String()
}

func (e *MaterializationError) Unwrap() error {
	return e.Cause
}

func (e *MaterializationError) Is(target error) bool {
	return e.ResourceLimit && target == apierr.ErrResourceExhausted
}

func newResourceError(stage, msg, path string, requestedMB int64, requiredBytes uint64, cause error) *MaterializationError {
	return &MaterializationError{
		Stage:         stage,
		Message:       msg,
		Path:          path,
		RequestedMB:   requestedMB,
		RequiredBytes: requiredBytes,
		Snapshot:      snapshotResources(path),
		Cause:         cause,
		ResourceLimit: true,
	}
}

func newCommandError(stage string, args []string, path string, requestedMB int64, output []byte, err error) error {
	trimmed := strings.TrimSpace(string(output))
	if len(trimmed) > 4096 {
		trimmed = trimmed[len(trimmed)-4096:]
	}
	cmdName := strings.Join(args, " ")
	processKilled := wasKilled(err)
	msg := fmt.Sprintf("%s failed", cmdName)
	return &MaterializationError{
		Stage:         stage,
		Message:       msg,
		Path:          path,
		RequestedMB:   requestedMB,
		Snapshot:      snapshotResources(path),
		Cause:         err,
		CommandOutput: trimmed,
		ResourceLimit: errors.Is(err, syscall.ENOSPC) || processKilled,
		ProcessKilled: processKilled,
	}
}

func runMaterializerCommand(ctx context.Context, stage, path string, requestedMB int64, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return newCommandError(stage, append([]string{name}, args...), path, requestedMB, out, err)
	}
	return nil
}

func createSparseFile(path string, sizeMB int64) error {
	if sizeMB <= 0 {
		sizeMB = DefaultRootfsSizeMB
	}
	if err := hostfs.CreateSparseFile(path, sizeMB, 0640); err != nil {
		return fmt.Errorf("create sparse rootfs: %w", err)
	}
	return nil
}

func checkMaterializationPreflight(dir string, rootfsSizeMB, minFreeMB int64) error {
	if err := os.MkdirAll(dir, 0750); err != nil {
		return fmt.Errorf("create build dir: %w", err)
	}
	for _, tool := range []string{"mkfs.ext4", "mount", "umount", "tar"} {
		if _, err := exec.LookPath(tool); err != nil {
			return fmt.Errorf("required image materialization tool %q is unavailable: %w", tool, err)
		}
	}
	if minFreeMB > 0 {
		required := uint64(minFreeMB) * 1024 * 1024
		snap := snapshotResources(dir)
		if snap.AvailableBytes < required {
			return &MaterializationError{
				Stage:         "preflight",
				Message:       "not enough host disk space to materialize VM image",
				Path:          dir,
				RequestedMB:   rootfsSizeMB,
				RequiredBytes: required,
				Snapshot:      snap,
				ResourceLimit: true,
			}
		}
	}
	if err := checkSparseSupport(dir); err != nil {
		return newResourceError("preflight", "filesystem does not support efficient sparse VM image files", dir, rootfsSizeMB, 0, err)
	}
	return nil
}

func checkSparseSupport(dir string) error {
	path := filepath.Join(dir, ".kiln-vm-sparse-probe")
	_ = os.Remove(path)
	defer os.Remove(path)
	if err := createSparseFile(path, sparseProbeSizeBytes/(1024*1024)); err != nil {
		return err
	}
	allocated, err := allocatedBytes(path)
	if err != nil {
		return err
	}
	if allocated > uint64(sparseProbeSizeBytes/4) {
		return fmt.Errorf("sparse probe allocated %s for %s apparent size", formatBytes(allocated), formatBytes(uint64(sparseProbeSizeBytes)))
	}
	return nil
}

func allocatedBytes(path string) (uint64, error) {
	allocated, err := hostfs.AllocatedBytes(path)
	if err != nil {
		return 0, fmt.Errorf("measure allocated file bytes: %w", err)
	}
	return allocated, nil
}

func snapshotResources(path string) resourceSnapshot {
	snap := resourceSnapshot{Path: path}
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err == nil {
		snap.AvailableBytes = stat.Bavail * uint64(stat.Bsize)
		snap.TotalBytes = stat.Blocks * uint64(stat.Bsize)
	}
	memAvailable, swapFree := readMemInfo()
	snap.MemAvailableBytes = memAvailable
	snap.SwapFreeBytes = swapFree
	return snap
}

func readMemInfo() (uint64, uint64) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0
	}
	defer f.Close()

	var memAvailable uint64
	var swapFree uint64
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}
		value, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			continue
		}
		switch strings.TrimSuffix(fields[0], ":") {
		case "MemAvailable":
			memAvailable = value * 1024
		case "SwapFree":
			swapFree = value * 1024
		}
	}
	return memAvailable, swapFree
}

func wasKilled(err error) bool {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return false
	}
	status, ok := exitErr.Sys().(syscall.WaitStatus)
	return ok && status.Signaled() && status.Signal() == syscall.SIGKILL
}

func formatBytes(bytes uint64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%dB", bytes)
	}
	div, exp := uint64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%ciB", float64(bytes)/float64(div), "KMGTPE"[exp])
}

func streamContainerToMountedRootfs(ctx context.Context, containerID, mountDir, path string, requestedMB int64) error {
	exportCmd := exec.CommandContext(ctx, "docker", "export", containerID)
	tarCmd := exec.CommandContext(ctx, "tar", "xf", "-", "-C", mountDir)

	reader, writer := io.Pipe()
	exportCmd.Stdout = writer
	tarCmd.Stdin = reader

	var exportErrBuf strings.Builder
	var tarErrBuf strings.Builder
	exportCmd.Stderr = &exportErrBuf
	tarCmd.Stderr = &tarErrBuf

	if err := tarCmd.Start(); err != nil {
		reader.Close()
		writer.Close()
		return fmt.Errorf("start tar extract: %w", err)
	}
	if err := exportCmd.Start(); err != nil {
		writer.CloseWithError(err)
		_ = tarCmd.Wait()
		reader.Close()
		return fmt.Errorf("start docker export: %w", err)
	}

	exportErr := exportCmd.Wait()
	if exportErr != nil {
		writer.CloseWithError(exportErr)
	} else {
		writer.Close()
	}
	tarErr := tarCmd.Wait()
	reader.Close()

	if exportErr != nil {
		return newCommandError("docker export", []string{"docker", "export", containerID}, path, requestedMB, []byte(exportErrBuf.String()), exportErr)
	}
	if tarErr != nil {
		return newCommandError("tar extract", []string{"tar", "xf", "-", "-C", mountDir}, path, requestedMB, []byte(tarErrBuf.String()), tarErr)
	}
	return nil
}

func promoteStagingDir(stagingDir, finalDir string) error {
	if err := os.RemoveAll(finalDir); err != nil {
		return fmt.Errorf("remove old image dir: %w", err)
	}
	if err := os.Rename(stagingDir, finalDir); err == nil {
		return nil
	} else if !errors.Is(err, syscall.EXDEV) {
		return fmt.Errorf("promote image staging dir: %w", err)
	}

	if err := os.MkdirAll(finalDir, 0750); err != nil {
		return fmt.Errorf("create image dir: %w", err)
	}
	if err := hostfs.CopyTreePreserveSparse(stagingDir, finalDir); err != nil {
		_ = os.RemoveAll(finalDir)
		return fmt.Errorf("copy image staging dir across filesystems: %w", err)
	}
	if err := os.RemoveAll(stagingDir); err != nil {
		return fmt.Errorf("remove promoted staging dir: %w", err)
	}
	return nil
}
