package image

import (
	"context"
	"fmt"
	"os"
	"os/exec"
)

func EnsureRootfsSize(ctx context.Context, rootfsPath string, sizeMB int64) error {
	if sizeMB <= 0 {
		return fmt.Errorf("rootfs size must be positive")
	}
	info, err := os.Stat(rootfsPath)
	if err != nil {
		return fmt.Errorf("stat rootfs: %w", err)
	}
	wantBytes := sizeMB * 1024 * 1024
	switch {
	case info.Size() == wantBytes:
		return nil
	case info.Size() > wantBytes:
		return fmt.Errorf("rootfs is %d bytes, larger than requested %d bytes", info.Size(), wantBytes)
	}
	if err := os.Truncate(rootfsPath, wantBytes); err != nil {
		return fmt.Errorf("expand rootfs sparse file: %w", err)
	}
	cmd := exec.CommandContext(ctx, "resize2fs", "-f", rootfsPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("resize rootfs to %dMiB: %s: %w", sizeMB, out, err)
	}
	return nil
}
