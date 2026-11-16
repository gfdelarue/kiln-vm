package libvirtaccess

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
)

type runtimeIdentity struct {
	uid int
	gid int
}

// PrepareReadOnlyFile makes a host file readable by libvirt's QEMU process.
func PrepareReadOnlyFile(path string) error {
	return prepareFile(path, false)
}

// PrepareWritableFile makes a host file writable by libvirt's QEMU process.
func PrepareWritableFile(path string) error {
	return prepareFile(path, true)
}

func prepareFile(path string, writable bool) error {
	if err := ensureAncestorExecute(path); err != nil {
		return err
	}
	ownedByRuntime := false
	if identity, ok := lookupRuntimeIdentity(); ok && os.Geteuid() == 0 {
		if err := os.Chown(path, identity.uid, identity.gid); err != nil {
			return fmt.Errorf("chown %s for libvirt qemu runtime: %w", path, err)
		}
		ownedByRuntime = true
	}
	mode := os.FileMode(0644)
	if writable {
		mode = 0666
		if ownedByRuntime {
			mode = 0600
		}
	}
	if err := os.Chmod(path, mode); err != nil {
		return fmt.Errorf("chmod %s for libvirt qemu runtime: %w", path, err)
	}
	return nil
}

func ensureAncestorExecute(path string) error {
	dir := filepath.Dir(path)
	for {
		info, err := os.Stat(dir)
		if err != nil {
			return fmt.Errorf("stat libvirt storage ancestor %s: %w", dir, err)
		}
		if info.IsDir() {
			mode := info.Mode().Perm()
			next := mode | 0111
			if next != mode {
				if err := os.Chmod(dir, next); err != nil {
					return fmt.Errorf("chmod libvirt storage ancestor %s: %w", dir, err)
				}
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return nil
		}
		dir = parent
	}
}

func lookupRuntimeIdentity() (runtimeIdentity, bool) {
	for _, name := range []string{"libvirt-qemu", "qemu"} {
		u, err := user.Lookup(name)
		if err != nil {
			continue
		}
		uid, uidErr := strconv.Atoi(u.Uid)
		gid, gidErr := strconv.Atoi(u.Gid)
		if uidErr == nil && gidErr == nil {
			return runtimeIdentity{uid: uid, gid: gid}, true
		}
	}
	return runtimeIdentity{}, false
}
