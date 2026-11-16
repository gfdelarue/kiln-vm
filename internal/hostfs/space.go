package hostfs

import (
	"fmt"
	"math/bits"
	"path/filepath"

	"golang.org/x/sys/unix"
)

type FilesystemSnapshot struct {
	Path           string
	AvailableBytes uint64
	TotalBytes     uint64
}

func SnapshotFilesystem(path string) (FilesystemSnapshot, error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return FilesystemSnapshot{}, fmt.Errorf("stat filesystem %s: %w", path, err)
	}
	return FilesystemSnapshot{
		Path:           path,
		AvailableBytes: stat.Bavail * uint64(stat.Bsize),
		TotalBytes:     stat.Blocks * uint64(stat.Bsize),
	}, nil
}

func UsageAtLeastPercent(snapshot FilesystemSnapshot, percentage uint64) bool {
	if percentage == 0 || snapshot.TotalBytes == 0 || snapshot.AvailableBytes >= snapshot.TotalBytes {
		return false
	}
	used := snapshot.TotalBytes - snapshot.AvailableBytes
	leftHi, leftLo := bits.Mul64(used, 100)
	rightHi, rightLo := bits.Mul64(snapshot.TotalBytes, percentage)
	return leftHi > rightHi || (leftHi == rightHi && leftLo >= rightLo)
}

func IsMountPoint(path string) (bool, error) {
	clean := filepath.Clean(path)
	parent := filepath.Dir(clean)
	if parent == clean {
		return true, nil
	}
	var stat unix.Stat_t
	if err := unix.Stat(clean, &stat); err != nil {
		return false, fmt.Errorf("stat mountpoint %s: %w", clean, err)
	}
	var parentStat unix.Stat_t
	if err := unix.Stat(parent, &parentStat); err != nil {
		return false, fmt.Errorf("stat mountpoint parent %s: %w", parent, err)
	}
	return stat.Dev != parentStat.Dev || stat.Ino == parentStat.Ino, nil
}
