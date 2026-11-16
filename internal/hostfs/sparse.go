package hostfs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

const ioctlFICLONE = 0x40049409

func CreateSparseFile(path string, sizeMB int64, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return fmt.Errorf("create sparse file: %w", err)
	}
	if err := f.Truncate(sizeMB * 1024 * 1024); err != nil {
		f.Close()
		return fmt.Errorf("size sparse file: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close sparse file: %w", err)
	}
	return nil
}

func AllocatedBytes(path string) (uint64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, fmt.Errorf("stat allocated file: %w", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("stat data unavailable for %s", path)
	}
	return uint64(stat.Blocks) * 512, nil
}

func CopyTreePreserveSparse(srcDir, dstDir string) error {
	if err := filepath.WalkDir(srcDir, func(srcPath string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return fmt.Errorf("walk sparse copy source: %w", walkErr)
		}
		rel, err := filepath.Rel(srcDir, srcPath)
		if err != nil {
			return fmt.Errorf("resolve sparse copy path: %w", err)
		}
		if rel == "." {
			return nil
		}
		dstPath := filepath.Join(dstDir, rel)
		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("stat sparse copy entry: %w", err)
		}
		mode := info.Mode()
		switch {
		case mode.IsDir():
			return os.MkdirAll(dstPath, mode.Perm())
		case mode.Type()&os.ModeSymlink != 0:
			target, err := os.Readlink(srcPath)
			if err != nil {
				return fmt.Errorf("read sparse copy symlink: %w", err)
			}
			return os.Symlink(target, dstPath)
		case mode.IsRegular():
			if err := os.MkdirAll(filepath.Dir(dstPath), 0750); err != nil {
				return fmt.Errorf("create sparse copy parent: %w", err)
			}
			if err := CopyFilePreserveSparse(context.Background(), srcPath, dstPath, mode.Perm()); err != nil {
				return err
			}
			return os.Chtimes(dstPath, info.ModTime(), info.ModTime())
		default:
			return nil
		}
	}); err != nil {
		return fmt.Errorf("copy sparse tree: %w", err)
	}
	return nil
}

func CopyFilePreserveSparse(ctx context.Context, srcPath, dstPath string, mode os.FileMode) error {
	if err := cloneFile(srcPath, dstPath, mode); err == nil {
		return nil
	}
	if err := copySparseFile(ctx, srcPath, dstPath, mode); err != nil {
		_ = os.Remove(dstPath)
		return err
	}
	return nil
}

func cloneFile(srcPath, dstPath string, mode os.FileMode) error {
	src, err := os.Open(srcPath)
	if err != nil {
		return fmt.Errorf("open reflink source: %w", err)
	}
	defer src.Close()

	info, err := src.Stat()
	if err != nil {
		return fmt.Errorf("stat reflink source: %w", err)
	}
	dst, err := os.OpenFile(dstPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return fmt.Errorf("create reflink destination: %w", err)
	}
	defer dst.Close()
	if err := unix.IoctlSetInt(int(dst.Fd()), ioctlFICLONE, int(src.Fd())); err != nil {
		return fmt.Errorf("reflink sparse file: %w", err)
	}
	if err := dst.Chmod(mode); err != nil {
		return fmt.Errorf("chmod reflink destination: %w", err)
	}
	if err := dst.Truncate(info.Size()); err != nil {
		return fmt.Errorf("size reflink destination: %w", err)
	}
	return nil
}

func copySparseFile(ctx context.Context, srcPath, dstPath string, mode os.FileMode) error {
	src, err := os.Open(srcPath)
	if err != nil {
		return fmt.Errorf("open sparse source: %w", err)
	}
	defer src.Close()

	info, err := src.Stat()
	if err != nil {
		return fmt.Errorf("stat sparse source: %w", err)
	}

	dst, err := os.OpenFile(dstPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return fmt.Errorf("create sparse destination: %w", err)
	}
	defer dst.Close()
	if err := dst.Truncate(info.Size()); err != nil {
		return fmt.Errorf("size sparse destination: %w", err)
	}

	var offset int64
	buf := make([]byte, 1024*1024)
	for offset < info.Size() {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("copy sparse file cancelled: %w", err)
		}
		data, err := unix.Seek(int(src.Fd()), offset, unix.SEEK_DATA)
		if err != nil {
			if errors.Is(err, unix.ENXIO) {
				return nil
			}
			return fmt.Errorf("seek next sparse data: %w", err)
		}
		hole, err := unix.Seek(int(src.Fd()), data, unix.SEEK_HOLE)
		if err != nil {
			return fmt.Errorf("seek next sparse hole: %w", err)
		}
		if _, err := src.Seek(data, io.SeekStart); err != nil {
			return fmt.Errorf("seek sparse source: %w", err)
		}
		if _, err := dst.Seek(data, io.SeekStart); err != nil {
			return fmt.Errorf("seek sparse destination: %w", err)
		}
		if _, err := io.CopyBuffer(dst, io.LimitReader(src, hole-data), buf); err != nil {
			return fmt.Errorf("copy sparse extent: %w", err)
		}
		offset = hole
	}
	return nil
}
