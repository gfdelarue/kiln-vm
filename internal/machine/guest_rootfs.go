package machine

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

const maxGuestFileBytes = 1 << 20

var errGuestFileTooLarge = fmt.Errorf("larger than %d bytes", maxGuestFileBytes)

func readGuestFile(root *os.Root, name string) (string, error) {
	info, err := root.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("inspect: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", nil
	}
	if info.Size() > maxGuestFileBytes {
		return "", errGuestFileTooLarge
	}
	data, err := root.ReadFile(name)
	if err != nil {
		return "", fmt.Errorf("read: %w", err)
	}
	return string(data), nil
}

// writeGuestFile replaces anything at name that is not a regular file. A
// mounted guest rootfs is guest-controlled, and kiln-vmd writes to it as root.
func writeGuestFile(root *os.Root, name string, data []byte, perm os.FileMode) error {
	info, err := root.Lstat(name)
	if err == nil && !info.Mode().IsRegular() {
		if err := root.Remove(name); err != nil {
			return fmt.Errorf("replace %s: %w", info.Mode().Type(), err)
		}
	} else if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("inspect: %w", err)
	}
	if err := root.WriteFile(name, data, perm); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	return nil
}

func writeGuestNameConfig(root *os.Root, hostname string, servers []string) error {
	if err := root.MkdirAll("etc", 0755); err != nil {
		return fmt.Errorf("create etc dir: %w", err)
	}
	if len(servers) > 0 {
		if err := writeGuestFile(root, "etc/resolv.conf", []byte(renderResolvConf(servers)), 0644); err != nil {
			return fmt.Errorf("write resolv.conf: %w", err)
		}
	}
	if hostname != "" {
		if err := writeGuestFile(root, "etc/hostname", []byte(hostname+"\n"), 0644); err != nil {
			return fmt.Errorf("write hostname: %w", err)
		}
	}

	hosts, err := readGuestFile(root, "etc/hosts")
	if errors.Is(err, errGuestFileTooLarge) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read hosts: %w", err)
	}
	if updated := withHostsEntries(hosts, hostname); updated != hosts {
		if err := writeGuestFile(root, "etc/hosts", []byte(updated), 0644); err != nil {
			return fmt.Errorf("write hosts: %w", err)
		}
	}
	return nil
}

func guestHostname(name string) string {
	hostname := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			return r
		case r >= 'A' && r <= 'Z':
			return r + ('a' - 'A')
		case r == '_', r == ' ':
			return '-'
		}
		return -1
	}, name)
	if len(hostname) > 63 {
		hostname = hostname[:63]
	}
	return strings.Trim(hostname, "-")
}

func withHostsEntries(hosts, hostname string) string {
	names := map[string]bool{}
	for _, line := range strings.Split(hosts, "\n") {
		line, _, _ = strings.Cut(line, "#")
		fields := strings.Fields(line)
		for _, name := range fields[min(1, len(fields)):] {
			names[strings.ToLower(name)] = true
		}
	}

	var missing []string
	if !names["localhost"] {
		missing = append(missing, "127.0.0.1 localhost", "::1 localhost ip6-localhost ip6-loopback")
	}
	if hostname != "" && !names[hostname] {
		missing = append(missing, "127.0.1.1 "+hostname)
	}
	if len(missing) == 0 {
		return hosts
	}
	if hosts != "" && !strings.HasSuffix(hosts, "\n") {
		hosts += "\n"
	}
	return hosts + strings.Join(missing, "\n") + "\n"
}
