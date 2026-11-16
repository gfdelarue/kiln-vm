package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

type outputOwner struct {
	uid int
	gid int
}

func handoffOutputOwnership(paths ...string) error {
	owner, ok, err := sudoOutputOwner(os.Geteuid(), os.Getenv)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	for _, path := range paths {
		if path == "" {
			continue
		}
		if err := os.Chown(path, owner.uid, owner.gid); err != nil {
			return fmt.Errorf("handoff profile artifact output %s to sudo caller %d:%d: %w", path, owner.uid, owner.gid, err)
		}
	}
	return nil
}

func sudoOutputOwner(euid int, getenv func(string) string) (outputOwner, bool, error) {
	if euid != 0 {
		return outputOwner{}, false, nil
	}
	uidRaw := strings.TrimSpace(getenv("SUDO_UID"))
	gidRaw := strings.TrimSpace(getenv("SUDO_GID"))
	if uidRaw == "" && gidRaw == "" {
		return outputOwner{}, false, nil
	}
	if uidRaw == "" || gidRaw == "" {
		return outputOwner{}, false, fmt.Errorf("SUDO_UID and SUDO_GID must both be set when handing off profile artifact outputs")
	}
	uid, err := parseNonNegativeID(uidRaw, "SUDO_UID")
	if err != nil {
		return outputOwner{}, false, err
	}
	gid, err := parseNonNegativeID(gidRaw, "SUDO_GID")
	if err != nil {
		return outputOwner{}, false, err
	}
	return outputOwner{uid: uid, gid: gid}, true, nil
}

func parseNonNegativeID(raw, label string) (int, error) {
	value, err := strconv.Atoi(raw)
	if err != nil || value < 0 {
		return 0, fmt.Errorf("%s must be a non-negative integer, got %q", label, raw)
	}
	return value, nil
}
