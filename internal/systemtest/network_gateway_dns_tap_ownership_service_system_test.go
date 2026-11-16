//go:build system && privileged && service

package systemtest

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gfdelarue/kiln-vm/internal/models"
)

func TestNetworkServiceGatewayDNSRefusesForeignSameTypeTAPCollision(t *testing.T) {
	client := requireClient(t)
	cleanupResourcesByPrefix(t, client)

	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	baseline := captureBringdownBaseline(t, ctx, client)
	firewallIdentity, err := readGatewayDNSFirewallIdentity(baseline.dataDir)
	if err != nil {
		t.Fatalf("read durable gateway DNS firewall identity: %v", err)
	}
	name := uniqueName("gateway-dns-tap-owner")
	vm, err := client.createVM(ctx, models.CreateVMRequest{
		Name:       name,
		Profile:    "alpine",
		DiskSizeMB: 1024,
		CPU:        &models.CPUConfig{VCPUs: 1},
		MemoryMB:   512,
	})
	if err != nil {
		t.Fatalf("create TAP-ownership VM: %v", err)
	}
	deleted := false
	foreignAlias := "foreign-systemtest-owner"
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cleanupCancel()
		if alias, aliasErr := hostTAPAlias(vm.TAPDevice); aliasErr == nil && alias == foreignAlias {
			_ = deleteHostTAP(vm.TAPDevice)
		}
		if !deleted {
			if err := client.deleteVM(cleanupCtx, name); err != nil {
				t.Logf("cleanup TAP-ownership VM %s: %v", name, err)
			}
		}
	})

	vm, err = client.waitForVMState(ctx, name, models.MachineStateRunning)
	if err != nil {
		t.Fatalf("wait for TAP-ownership VM: %v", err)
	}
	requireHostTAPAlias(t, ctx, vm.TAPDevice, expectedTAPAlias(t, vm))
	for _, transport := range []string{"udp", "tcp"} {
		result, probeErr := probeGuestDNS(ctx, client, name, vm.GatewayIP, transport)
		requireGuestDNSProbe(t, "pre-collision gateway "+transport, result, probeErr, transport)
	}

	if err := deleteHostTAP(vm.TAPDevice); err != nil {
		t.Fatalf("delete owned TAP before collision: %v", err)
	}
	if err := createForeignTAP(vm.TAPDevice, foreignAlias); err != nil {
		t.Fatalf("create foreign same-type TAP collision: %v", err)
	}

	waitForTAPCollisionDegraded(t, ctx, client, vm.ID)
	requireHostTAPAlias(t, ctx, vm.TAPDevice, foreignAlias)
	addresses, err := hostTAPAddresses(vm.TAPDevice)
	if err != nil {
		t.Fatalf("inspect foreign TAP addresses: %v", err)
	}
	if strings.Join(addresses, ",") != "192.0.2.1/32" {
		t.Fatalf("foreign TAP addresses = %v, want untouched sentinel", addresses)
	}
	listeners, err := gatewayDNSListeners(vm.GatewayIP)
	if err != nil {
		t.Fatalf("inspect listeners after TAP collision: %v", err)
	}
	if listeners.UDP || listeners.TCP {
		t.Fatalf("gateway DNS listeners remained admitted after TAP collision: %s", listeners)
	}
	for _, transport := range []string{"udp", "tcp"} {
		exists, ruleErr := gatewayDNSFirewallRuleExists(firewallIdentity, vm, transport, gatewayDNSFirewallAdmit)
		if ruleErr != nil {
			t.Fatalf("inspect %s admission after TAP collision: %v", transport, ruleErr)
		}
		if exists {
			t.Fatalf("%s gateway DNS admission survived foreign TAP collision", transport)
		}
	}

	if err := deleteHostTAP(vm.TAPDevice); err != nil {
		t.Fatalf("remove foreign TAP collision: %v", err)
	}
	requireHostTAPAlias(t, ctx, vm.TAPDevice, expectedTAPAlias(t, vm))

	stopped := stopVMAndWait(t, ctx, client, name)
	if stopped.State != models.MachineStateStopped {
		t.Fatalf("state after collision recovery stop = %q, want stopped", stopped.State)
	}
	warm := startVMAndWait(t, ctx, client, name)
	for _, transport := range []string{"udp", "tcp"} {
		result, probeErr := probeGuestDNS(ctx, client, name, warm.GatewayIP, transport)
		requireGuestDNSProbe(t, "post-collision warm gateway "+transport, result, probeErr, transport)
	}

	if err := client.deleteVM(ctx, name); err != nil {
		t.Fatalf("delete TAP-ownership VM: %v", err)
	}
	deleted = true
	requireVMBringdown(t, client, baseline, warm)
}

func expectedTAPAlias(t *testing.T, vm *models.VM) string {
	t.Helper()
	index, err := strconv.ParseUint(strings.TrimPrefix(vm.TAPDevice, "kv"), 10, 32)
	if err != nil || index == 0 || vm.TAPDevice != fmt.Sprintf("kv%d", index) {
		t.Fatalf("VM TAP identity %q is not canonical", vm.TAPDevice)
	}
	mac := fmt.Sprintf("02:fc:00:00:%02x:%02x", byte(index>>8), byte(index))
	return fmt.Sprintf("kiln-vm-public-network:v1:%s:%s:%s:%s", vm.TAPDevice, vm.GatewayIP, vm.IP, mac)
}

func createForeignTAP(name, alias string) error {
	commands := [][]string{
		{"tuntap", "add", "dev", name, "mode", "tap"},
		{"link", "set", "dev", name, "alias", alias},
		{"addr", "add", "192.0.2.1/32", "dev", name},
		{"link", "set", name, "up"},
	}
	for _, args := range commands {
		out, err := exec.Command("ip", args...).CombinedOutput()
		if err != nil {
			return commandError(fmt.Sprintf("ip %v", args), out, err)
		}
	}
	return nil
}

func deleteHostTAP(name string) error {
	out, err := exec.Command("ip", "link", "del", name).CombinedOutput()
	if err == nil || linkMissingOutputForTest(out) {
		return nil
	}
	return commandError("delete host TAP "+name, out, err)
}

func hostTAPAlias(name string) (string, error) {
	out, err := exec.Command("ip", "-j", "-d", "link", "show", "dev", name).CombinedOutput()
	if err != nil {
		return "", commandError("inspect host TAP alias "+name, out, err)
	}
	var records []struct {
		IfName  string `json:"ifname"`
		IfAlias string `json:"ifalias"`
	}
	if err := json.Unmarshal(out, &records); err != nil {
		return "", fmt.Errorf("decode host TAP alias %s: %w", name, err)
	}
	if len(records) != 1 || records[0].IfName != name {
		return "", fmt.Errorf("decode host TAP alias %s: unexpected inventory", name)
	}
	return records[0].IfAlias, nil
}

func hostTAPAddresses(name string) ([]string, error) {
	out, err := exec.Command("ip", "-j", "-4", "addr", "show", "dev", name).CombinedOutput()
	if err != nil {
		return nil, commandError("inspect host TAP addresses "+name, out, err)
	}
	var records []struct {
		IfName   string `json:"ifname"`
		AddrInfo []struct {
			Family    string `json:"family"`
			Local     string `json:"local"`
			PrefixLen int    `json:"prefixlen"`
		} `json:"addr_info"`
	}
	if err := json.Unmarshal(out, &records); err != nil {
		return nil, fmt.Errorf("decode host TAP addresses %s: %w", name, err)
	}
	if len(records) != 1 || records[0].IfName != name {
		return nil, fmt.Errorf("decode host TAP addresses %s: unexpected inventory", name)
	}
	addresses := make([]string, 0, len(records[0].AddrInfo))
	for _, address := range records[0].AddrInfo {
		if address.Family == "inet" {
			addresses = append(addresses, fmt.Sprintf("%s/%d", address.Local, address.PrefixLen))
		}
	}
	return addresses, nil
}

func requireHostTAPAlias(t *testing.T, ctx context.Context, name, want string) {
	t.Helper()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		alias, err := hostTAPAlias(name)
		if err == nil && alias == want {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("host TAP %s alias did not become %q: alias=%q err=%v: %v", name, want, alias, err, ctx.Err())
		case <-ticker.C:
		}
	}
}

func waitForTAPCollisionDegraded(t *testing.T, ctx context.Context, client *apiClient, vmRef string) {
	t.Helper()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		metrics, err := client.vmMachineMetrics(ctx, vmRef)
		if err == nil && metrics.GatewayDNS.Status == "degraded" && metrics.GatewayDNS.Reason == "host_network_unready" {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("gateway DNS did not report host TAP ownership collision: metrics=%+v err=%v: %v", metrics, err, ctx.Err())
		case <-ticker.C:
		}
	}
}
