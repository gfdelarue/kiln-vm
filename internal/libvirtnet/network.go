package libvirtnet

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
)

const (
	ManagedName   = "kiln-vm"
	ManagedBridge = "kiln-vm-br0"
)

type Runner func(ctx context.Context, args ...string) (string, error)

type Network struct {
	ConnectURI string
	Name       string
	CIDR       string
	Run        Runner
}

var ensureMu sync.Mutex

func (n Network) Managed() bool {
	return n.Name == ManagedName
}

func (n Network) Ensure(ctx context.Context) error {
	ensureMu.Lock()
	defer ensureMu.Unlock()

	info, err := n.virsh(ctx, "net-info", n.Name)
	if err != nil {
		if !n.Managed() {
			return fmt.Errorf("libvirt network %q is not available; kiln-vmd creates only the %q network: %w", n.Name, ManagedName, err)
		}
		if err := n.define(ctx); err != nil {
			return err
		}
		if info, err = n.virsh(ctx, "net-info", n.Name); err != nil {
			return fmt.Errorf("inspect libvirt network %q after define: %w", n.Name, err)
		}
	}
	if !n.Managed() {
		if !strings.EqualFold(InfoField(info, "Active"), "yes") {
			return fmt.Errorf("libvirt network %q is not active; start it with `virsh -c %s net-start %s`", n.Name, n.ConnectURI, n.Name)
		}
		return nil
	}
	if !strings.EqualFold(InfoField(info, "Active"), "yes") {
		if _, err := n.virsh(ctx, "net-start", n.Name); err != nil {
			return fmt.Errorf("start libvirt network %q: %w", n.Name, err)
		}
	}
	if !strings.EqualFold(InfoField(info, "Autostart"), "yes") {
		if _, err := n.virsh(ctx, "net-autostart", n.Name); err != nil {
			return fmt.Errorf("enable autostart for libvirt network %q: %w", n.Name, err)
		}
	}
	return nil
}

func (n Network) define(ctx context.Context) error {
	xml, err := ManagedXML(n.CIDR)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp("", "kiln-vm-network-*.xml")
	if err != nil {
		return fmt.Errorf("create libvirt network definition: %w", err)
	}
	defer os.Remove(file.Name())
	if _, err := file.WriteString(xml); err != nil {
		_ = file.Close()
		return fmt.Errorf("write libvirt network definition: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close libvirt network definition: %w", err)
	}
	if _, err := n.virsh(ctx, "net-define", file.Name()); err != nil {
		return fmt.Errorf("define libvirt network %q: %w", n.Name, err)
	}
	return nil
}

func ManagedXML(cidr string) (string, error) {
	gateway, netmask, first, last, err := addressPlan(cidr)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf(`<network>
  <name>%s</name>
  <forward mode='nat'/>
  <bridge name='%s' stp='on' delay='0'/>
  <ip address='%s' netmask='%s'>
    <dhcp>
      <range start='%s' end='%s'/>
    </dhcp>
  </ip>
</network>
`, ManagedName, ManagedBridge, gateway, netmask, first, last), nil
}

func addressPlan(cidr string) (gateway, netmask, first, last string, err error) {
	ip, ipNet, err := net.ParseCIDR(strings.TrimSpace(cidr))
	if err != nil || ip.To4() == nil {
		return "", "", "", "", fmt.Errorf("libvirt network CIDR %q must be an IPv4 CIDR", cidr)
	}
	ones, bits := ipNet.Mask.Size()
	if bits != 32 || ones > 29 {
		return "", "", "", "", fmt.Errorf("libvirt network CIDR %q must be an IPv4 /29 or larger", cidr)
	}
	if !ip.To4().Equal(ipNet.IP.To4()) {
		return "", "", "", "", fmt.Errorf("libvirt network CIDR %q is not network-aligned; use %s", cidr, ipNet.String())
	}
	base := binary.BigEndian.Uint32(ipNet.IP.To4())
	broadcast := base | ^binary.BigEndian.Uint32(net.IP(ipNet.Mask).To4())
	return uint32IP(base + 1), net.IP(ipNet.Mask).String(), uint32IP(base + 2), uint32IP(broadcast - 1), nil
}

func uint32IP(value uint32) string {
	ip := make(net.IP, 4)
	binary.BigEndian.PutUint32(ip, value)
	return ip.String()
}

func InfoField(info, field string) string {
	for line := range strings.SplitSeq(info, "\n") {
		key, value, ok := strings.Cut(line, ":")
		if ok && strings.EqualFold(strings.TrimSpace(key), field) {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func (n Network) virsh(ctx context.Context, args ...string) (string, error) {
	connectURI := n.ConnectURI
	if connectURI == "" {
		connectURI = "qemu:///system"
	}
	full := append([]string{"-c", connectURI}, args...)
	if n.Run != nil {
		return n.Run(ctx, full...)
	}
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "virsh", full...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("virsh %s: %w: %s", strings.Join(full, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}
