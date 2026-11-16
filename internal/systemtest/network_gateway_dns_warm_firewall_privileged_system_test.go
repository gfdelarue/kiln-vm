//go:build system && privileged

package systemtest

import (
	"context"
	"fmt"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gfdelarue/kiln-vm/internal/models"
)

func TestNetworkPrivilegedGatewayDNSWarmStartPreservesIdentityAndFirewall(t *testing.T) {
	client := requireClient(t)

	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	bringdownBaseline := captureBringdownBaseline(t, ctx, client)
	firewallIdentity, err := readGatewayDNSFirewallIdentity(bringdownBaseline.dataDir)
	if err != nil {
		t.Fatalf("read durable gateway DNS firewall identity: %v", err)
	}
	baselineFirewall, err := warmFirewallSnapshot(firewallIdentity)
	if err != nil {
		t.Fatalf("capture pre-VM owner-derived firewall baseline: %v", err)
	}

	name := uniqueName("gateway-dns-warm-firewall")
	vm, err := client.createVM(ctx, models.CreateVMRequest{
		Name:       name,
		Profile:    "alpine",
		DiskSizeMB: 1024,
		CPU:        &models.CPUConfig{VCPUs: 1},
		MemoryMB:   512,
		Power: &models.VMPowerPolicy{
			Mode: models.VMPowerModeManual,
		},
	})
	if err != nil {
		t.Fatalf("create manual Alpine gateway DNS VM: %v", err)
	}
	deleted := false
	t.Cleanup(func() {
		if deleted {
			return
		}
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cleanupCancel()
		if err := client.deleteVM(cleanupCtx, name); err != nil {
			t.Logf("cleanup manual Alpine gateway DNS VM %s: %v", name, err)
		}
	})

	vm, err = client.waitForVMState(ctx, name, models.MachineStateRunning)
	if err != nil {
		t.Fatalf("wait for manual Alpine gateway DNS VM running: %v", err)
	}
	if _, err := waitForExecResult(ctx, client, name, "echo gateway-dns-warm-firewall-ready", func(result *execResult) bool {
		return result.ExitCode == 0 && strings.Contains(result.Stdout, "gateway-dns-warm-firewall-ready")
	}); err != nil {
		t.Fatalf("wait for manual Alpine gateway DNS VM exec readiness: %v", err)
	}
	coldIdentity := requireWarmVMIdentity(t, vm)
	requireWarmActiveFirewall(t, ctx, firewallIdentity, vm)

	stopped := stopVMAndWait(t, ctx, client, name)
	if stopped.State != models.MachineStateStopped {
		t.Fatalf("stopped VM state = %q, want %q", stopped.State, models.MachineStateStopped)
	}
	if err := waitForNoTap(ctx, coldIdentity.TAPDevice); err != nil {
		t.Fatal(err)
	}
	requireWarmNoListeners(t, ctx, coldIdentity.GatewayIP, "stopped VM")
	requireWarmFirewallBaseline(t, ctx, firewallIdentity, baselineFirewall, "stopped VM")

	warm := startVMAndWait(t, ctx, client, name)
	if _, err := waitForExecResult(ctx, client, name, "echo gateway-dns-warm-firewall-restarted", func(result *execResult) bool {
		return result.ExitCode == 0 && strings.Contains(result.Stdout, "gateway-dns-warm-firewall-restarted")
	}); err != nil {
		t.Fatalf("wait for warm manual Alpine gateway DNS VM exec readiness: %v", err)
	}
	warmIdentity := requireWarmVMIdentity(t, warm)
	if warmIdentity != coldIdentity {
		t.Fatalf("warm VM identity = %+v, want exact cold identity %+v", warmIdentity, coldIdentity)
	}
	requireWarmActiveFirewall(t, ctx, firewallIdentity, warm)

	warmListeners, err := gatewayDNSListeners(warm.GatewayIP)
	if err != nil {
		t.Fatalf("inspect warm gateway DNS listeners: %v", err)
	}
	if !warmListeners.UDP || !warmListeners.TCP {
		t.Fatalf("warm gateway DNS listeners = %s, want UDP and TCP", warmListeners)
	}
	for _, transport := range []string{"udp", "tcp"} {
		result, probeErr := probeGuestDNS(ctx, client, name, warm.GatewayIP, transport)
		requireGuestDNSProbe(t, "warm gateway "+transport, result, probeErr, transport)
	}
	warmMetrics, err := client.vmMachineMetrics(ctx, warm.ID)
	if err != nil {
		t.Fatalf("read warm gateway DNS metrics: %v", err)
	}
	requireReadyGatewayDNSMetrics(t, *warmMetrics)

	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cleanupCancel()
	if err := client.deleteVM(cleanupCtx, name); err != nil {
		t.Fatalf("delete warm manual Alpine gateway DNS VM: %v", err)
	}
	deleted = true
	if err := waitForNoTap(cleanupCtx, coldIdentity.TAPDevice); err != nil {
		t.Fatal(err)
	}
	requireWarmNoListeners(t, cleanupCtx, coldIdentity.GatewayIP, "deleted VM")
	requireVMBringdown(t, client, bringdownBaseline, vm)
	requireWarmFirewallBaseline(t, cleanupCtx, firewallIdentity, baselineFirewall, "deleted VM")
}

func TestNetworkPrivilegedGatewayDNSDeleteOneAllocationKeepsSiblingRulesAndResolution(t *testing.T) {
	client := requireClient(t)

	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	bringdownBaseline := captureBringdownBaseline(t, ctx, client)
	firewallIdentity, err := readGatewayDNSFirewallIdentity(bringdownBaseline.dataDir)
	if err != nil {
		t.Fatalf("read durable gateway DNS firewall identity: %v", err)
	}
	baselineFirewall, err := warmFirewallSnapshot(firewallIdentity)
	if err != nil {
		t.Fatalf("capture pre-VM owner-derived firewall baseline: %v", err)
	}

	names := []string{
		uniqueName("gateway-dns-delete-first"),
		uniqueName("gateway-dns-survivor"),
	}
	vms := make([]*models.VM, len(names))
	deleted := make([]bool, len(names))
	t.Cleanup(func() {
		for index, name := range names {
			if deleted[index] {
				continue
			}
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 2*time.Minute)
			if err := client.deleteVM(cleanupCtx, name); err != nil {
				t.Logf("cleanup sibling gateway DNS VM %s: %v", name, err)
			}
			cleanupCancel()
		}
	})

	for index, name := range names {
		vm, createErr := client.createVM(ctx, models.CreateVMRequest{
			Name:       name,
			Profile:    "alpine",
			DiskSizeMB: 1024,
			CPU:        &models.CPUConfig{VCPUs: 1},
			MemoryMB:   512,
			Power: &models.VMPowerPolicy{
				Mode: models.VMPowerModeAlways,
			},
		})
		if createErr != nil {
			t.Fatalf("create sibling gateway DNS VM %d: %v", index+1, createErr)
		}
		vm, err = client.waitForVMState(ctx, name, models.MachineStateRunning)
		if err != nil {
			t.Fatalf("wait for sibling gateway DNS VM %d running: %v", index+1, err)
		}
		if _, err := waitForExecResult(ctx, client, name, "echo gateway-dns-sibling-ready", func(result *execResult) bool {
			return result.ExitCode == 0 && strings.Contains(result.Stdout, "gateway-dns-sibling-ready")
		}); err != nil {
			t.Fatalf("wait for sibling gateway DNS VM %d exec readiness: %v", index+1, err)
		}
		vms[index] = vm
	}

	identities := []warmVMIdentity{
		requireWarmVMIdentity(t, vms[0]),
		requireWarmVMIdentity(t, vms[1]),
	}
	if identities[0].VMID == identities[1].VMID || identities[0].MachineID == identities[1].MachineID ||
		identities[0].IP == identities[1].IP || identities[0].GatewayIP == identities[1].GatewayIP ||
		identities[0].TAPDevice == identities[1].TAPDevice {
		t.Fatalf("sibling gateway DNS VM identities are not distinct: first=%+v second=%+v", identities[0], identities[1])
	}
	for _, identity := range identities {
		for _, rule := range baselineFirewall.ChainRules {
			if rule.TAPDevice == identity.TAPDevice {
				t.Fatalf("new VM TAP %s was already present in the owner-derived firewall baseline: %+v", identity.TAPDevice, rule)
			}
		}
	}
	for _, vm := range vms {
		requireWarmActiveFirewall(t, ctx, firewallIdentity, vm)
	}

	if err := client.deleteVM(ctx, names[0]); err != nil {
		t.Fatalf("delete first sibling gateway DNS VM: %v", err)
	}
	deleted[0] = true
	if err := waitForNoTap(ctx, identities[0].TAPDevice); err != nil {
		t.Fatal(err)
	}
	requireWarmNoListeners(t, ctx, identities[0].GatewayIP, "deleted first sibling VM")
	requireVMBringdown(t, client, bringdownBaseline, vms[0])
	requireWarmPartialCleanupFirewall(t, ctx, firewallIdentity, identities[0].TAPDevice, vms[1])

	for _, transport := range []string{"udp", "tcp"} {
		result, probeErr := probeGuestDNS(ctx, client, names[1], identities[1].GatewayIP, transport)
		requireGuestDNSProbe(t, "surviving sibling gateway "+transport, result, probeErr, transport)
	}
	survivorMetrics, err := client.vmMachineMetrics(ctx, identities[1].VMID)
	if err != nil {
		t.Fatalf("read surviving sibling gateway DNS metrics: %v", err)
	}
	requireReadyGatewayDNSMetrics(t, *survivorMetrics)

	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cleanupCancel()
	if err := client.deleteVM(cleanupCtx, names[1]); err != nil {
		t.Fatalf("delete surviving sibling gateway DNS VM: %v", err)
	}
	deleted[1] = true
	if err := waitForNoTap(cleanupCtx, identities[1].TAPDevice); err != nil {
		t.Fatal(err)
	}
	requireWarmNoListeners(t, cleanupCtx, identities[1].GatewayIP, "deleted surviving sibling VM")
	requireVMBringdown(t, client, bringdownBaseline, vms[1])
	requireWarmFirewallBaseline(t, cleanupCtx, firewallIdentity, baselineFirewall, "deleted sibling VMs")
}

type warmVMIdentity struct {
	VMID      string
	MachineID string
	IP        string
	GatewayIP string
	TAPDevice string
}

func requireWarmVMIdentity(t *testing.T, vm *models.VM) warmVMIdentity {
	t.Helper()
	if vm == nil {
		t.Fatal("gateway DNS VM is nil")
	}
	identity := warmVMIdentity{
		VMID:      vm.ID,
		MachineID: vm.MachineID,
		IP:        vm.IP,
		GatewayIP: vm.GatewayIP,
		TAPDevice: vm.TAPDevice,
	}
	if identity.VMID == "" || identity.MachineID == "" || identity.IP == "" || identity.GatewayIP == "" || identity.TAPDevice == "" {
		t.Fatalf("gateway DNS VM identity is incomplete: %+v", identity)
	}
	if !canonicalTAP(identity.TAPDevice) {
		t.Fatalf("gateway DNS VM TAP identity %q is not canonical", identity.TAPDevice)
	}
	return identity
}

func warmFirewallSnapshot(identity gatewayDNSFirewallIdentity) (gatewayDNSFirewallState, error) {
	out, err := exec.Command("iptables", "-w", "5", "-S").CombinedOutput()
	if err != nil {
		return gatewayDNSFirewallState{}, commandError("snapshot gateway DNS filter table", out, err)
	}
	state, err := parseGatewayDNSFirewallState(strings.TrimSpace(string(out))+"\n", identity)
	if err != nil {
		return gatewayDNSFirewallState{}, fmt.Errorf("parse gateway DNS firewall snapshot: %w", err)
	}
	return state, nil
}

func requireWarmActiveFirewall(t *testing.T, ctx context.Context, identity gatewayDNSFirewallIdentity, vm *models.VM) {
	t.Helper()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	last := "gateway DNS firewall has not been observed"
	for {
		state, err := warmFirewallSnapshot(identity)
		if err == nil {
			if stateErr := warmActiveFirewallError(state, vm); stateErr == nil {
				return
			} else {
				last = stateErr.Error()
			}
		} else {
			last = err.Error()
		}

		select {
		case <-ctx.Done():
			t.Fatalf("gateway DNS firewall did not converge to one hook and four nonduplicate allocation rules: %s: %v", last, ctx.Err())
		case <-ticker.C:
		}
	}
}

func requireWarmPartialCleanupFirewall(
	t *testing.T,
	ctx context.Context,
	identity gatewayDNSFirewallIdentity,
	removedTAP string,
	survivor *models.VM,
) {
	t.Helper()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	last := "gateway DNS firewall has not been observed"
	for {
		state, err := warmFirewallSnapshot(identity)
		if err == nil {
			removedRules := 0
			for _, rule := range state.ChainRules {
				if rule.TAPDevice == removedTAP {
					removedRules++
				}
			}
			survivorErr := warmActiveFirewallError(state, survivor)
			if removedRules == 0 && survivorErr == nil {
				return
			}
			last = fmt.Sprintf("removed TAP %s rules=%d survivor_error=%v owner_state=%+v", removedTAP, removedRules, survivorErr, state)
		} else {
			last = err.Error()
		}

		select {
		case <-ctx.Done():
			t.Fatalf("gateway DNS firewall did not remove only %s while preserving the sibling allocation: %s: %v", removedTAP, last, ctx.Err())
		case <-ticker.C:
		}
	}
}

func warmActiveFirewallError(state gatewayDNSFirewallState, vm *models.VM) error {
	if state.InputPolicy != "ACCEPT" || !state.ChainExists || len(state.HookPositions) != 1 || state.HookPositions[0] != 1 {
		return fmt.Errorf("owner state = %+v, want INPUT ACCEPT, one owner-derived chain, and one first-position hook", state)
	}
	allocationRuleCount := 0
	for _, rule := range state.ChainRules {
		if rule.TAPDevice != vm.TAPDevice {
			continue
		}
		allocationRuleCount++
		switch rule.Verdict {
		case gatewayDNSFirewallAdmit:
			if rule.Source != vm.IP+"/32" || rule.Destination != vm.GatewayIP+"/32" {
				return fmt.Errorf("allocation admission = %+v, want source %s/32 and gateway %s/32", rule, vm.IP, vm.GatewayIP)
			}
		case gatewayDNSFirewallGuard:
			if rule.Source != "" || rule.Destination != "" {
				return fmt.Errorf("allocation guard = %+v, want no source or destination selector", rule)
			}
		}
	}
	if allocationRuleCount != 4 {
		return fmt.Errorf("allocation %s has %d owned rules, want exactly four: %+v", vm.TAPDevice, allocationRuleCount, state.ChainRules)
	}
	for _, transport := range []string{"udp", "tcp"} {
		if state.ruleCount(vm.TAPDevice, transport, gatewayDNSFirewallAdmit) != 1 ||
			state.ruleCount(vm.TAPDevice, transport, gatewayDNSFirewallGuard) != 1 ||
			state.packetVerdict(vm.TAPDevice, vm.IP, vm.GatewayIP, transport) != "ACCEPT" {
			return fmt.Errorf("allocation %s %s rules are not one admit before one guard: %+v", vm.TAPDevice, transport, state.ChainRules)
		}
	}
	return nil
}

func requireWarmNoListeners(t *testing.T, ctx context.Context, gatewayIP, phase string) {
	t.Helper()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	last := "gateway DNS listeners have not been inspected"
	for {
		listeners, err := gatewayDNSListeners(gatewayIP)
		if err == nil && !listeners.UDP && !listeners.TCP {
			return
		}
		last = fmt.Sprintf("listeners=%s err=%v", listeners, err)
		select {
		case <-ctx.Done():
			t.Fatalf("%s retained gateway DNS listeners at %s: %s: %v", phase, gatewayIP, last, ctx.Err())
		case <-ticker.C:
		}
	}
}

func requireWarmFirewallBaseline(t *testing.T, ctx context.Context, identity gatewayDNSFirewallIdentity, want gatewayDNSFirewallState, phase string) {
	t.Helper()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	last := "gateway DNS firewall has not been inspected"
	for {
		got, err := warmFirewallSnapshot(identity)
		if err == nil && reflect.DeepEqual(got, want) {
			return
		}
		if err != nil {
			last = err.Error()
		} else {
			last = fmt.Sprintf("got owner-derived state %+v, want %+v", got, want)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("%s did not restore the exact pre-VM owner-derived firewall baseline: %s: %v", phase, last, ctx.Err())
		case <-ticker.C:
		}
	}
}
