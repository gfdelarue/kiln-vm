//go:build system && privileged && service

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

func TestNetworkServiceGatewayDNSFirewallDriftAndControllerRestartRecover(t *testing.T) {
	client := requireClient(t)
	cleanupResourcesByPrefix(t, client)
	requireDaemonRestartSupport(t, client)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	bringdownBaseline := captureBringdownBaseline(t, ctx, client)
	identity, err := readGatewayDNSFirewallIdentity(bringdownBaseline.dataDir)
	if err != nil {
		t.Fatalf("read durable gateway DNS firewall identity: %v", err)
	}
	baselineFirewall, err := gatewayDNSFirewallSnapshot(identity)
	if err != nil {
		t.Fatalf("snapshot gateway DNS firewall before test: %v", err)
	}
	if baselineFirewall.InputPolicy != "ACCEPT" {
		t.Fatalf("INPUT policy = %q, want ACCEPT for fail-closed guard proof", baselineFirewall.InputPolicy)
	}
	if baselineFirewall.ChainExists || len(baselineFirewall.HookPositions) != 0 || len(baselineFirewall.ChainRules) != 0 {
		t.Fatalf("gateway DNS firewall baseline is not empty: %+v", baselineFirewall)
	}
	name := uniqueName("gateway-dns-firewall-drift")
	sentinelComment := "kiln-vm-systemtest-gateway-dns-sentinel:" + name
	if err := addGatewayDNSFirewallSentinel(sentinelComment); err != nil {
		t.Fatalf("install unrelated INPUT sentinel: %v", err)
	}
	sentinelPresent := true
	t.Cleanup(func() {
		if sentinelPresent {
			if err := deleteGatewayDNSFirewallSentinel(sentinelComment); err != nil {
				t.Logf("cleanup unrelated INPUT sentinel: %v", err)
			}
		}
	})
	vm, err := client.createVM(ctx, models.CreateVMRequest{
		Name:       name,
		Profile:    "alpine",
		DiskSizeMB: 1024,
		CPU:        &models.CPUConfig{VCPUs: 1},
		MemoryMB:   512,
		Power: &models.VMPowerPolicy{
			Mode: models.VMPowerModeAlways,
		},
	})
	if err != nil {
		t.Fatalf("create firewall-drift gateway DNS VM: %v", err)
	}
	deleted := false
	t.Cleanup(func() {
		if deleted {
			return
		}
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cleanupCancel()
		if err := client.deleteVM(cleanupCtx, name); err != nil {
			t.Logf("cleanup firewall-drift gateway DNS VM %s: %v", name, err)
		}
	})

	vm, err = client.waitForVMState(ctx, name, models.MachineStateRunning)
	if err != nil {
		t.Fatalf("wait for firewall-drift gateway DNS VM: %v", err)
	}
	if vm.ID == "" || vm.MachineID == "" {
		t.Fatalf("firewall-drift gateway DNS VM identity is incomplete: vm_id=%q machine_id=%q", vm.ID, vm.MachineID)
	}
	vmID := vm.ID
	machineID := vm.MachineID
	guestBootID := requireGuestBootID(t, ctx, client, name)
	for _, transport := range []string{"udp", "tcp"} {
		result, probeErr := probeGuestDNS(ctx, client, name, vm.GatewayIP, transport)
		requireGuestDNSProbe(t, "pre-drift gateway "+transport, result, probeErr, transport)
		assertGatewayDNSFirewallRule(t, ctx, identity, vm, transport, gatewayDNSFirewallAdmit, true)
		assertGatewayDNSFirewallRule(t, ctx, identity, vm, transport, gatewayDNSFirewallGuard, true)
		assertGatewayDNSFirewallRuleCount(t, identity, vm, transport, gatewayDNSFirewallAdmit, 1)
		assertGatewayDNSFirewallRuleCount(t, identity, vm, transport, gatewayDNSFirewallGuard, 1)
	}
	activeFirewall, err := gatewayDNSFirewallSnapshot(identity)
	if err != nil {
		t.Fatalf("snapshot active gateway DNS firewall: %v", err)
	}
	requireActiveGatewayDNSFirewallState(t, activeFirewall, vm)
	assertGatewayDNSFirewallSentinel(t, sentinelComment, true)

	// Force the listener into the exact guard-only state used before admission
	// and during teardown. The reconciler may race to restore admission, so each
	// real guest proof accepts only an attempt whose before and after snapshots
	// both show the exact DROP guard and no ACCEPT rule.
	for _, transport := range []string{"udp", "tcp"} {
		if err := deleteGatewayDNSFirewallRule(identity, vm, transport, gatewayDNSFirewallAdmit); err != nil {
			t.Fatalf("delete exact %s gateway DNS admission: %v", transport, err)
		}
		assertGatewayDNSFirewallRule(t, ctx, identity, vm, transport, gatewayDNSFirewallGuard, true)
	}
	for _, transport := range []string{"udp", "tcp"} {
		proveGuestDNSDeniedByGuard(t, ctx, client, name, identity, vm, transport)
	}

	reconcileCtx, reconcileCancel := context.WithTimeout(ctx, 45*time.Second)
	defer reconcileCancel()
	for _, transport := range []string{"udp", "tcp"} {
		assertGatewayDNSFirewallRule(t, reconcileCtx, identity, vm, transport, gatewayDNSFirewallAdmit, true)
		assertGatewayDNSFirewallRule(t, reconcileCtx, identity, vm, transport, gatewayDNSFirewallGuard, true)
		assertGatewayDNSFirewallRuleCount(t, identity, vm, transport, gatewayDNSFirewallAdmit, 1)
		assertGatewayDNSFirewallRuleCount(t, identity, vm, transport, gatewayDNSFirewallGuard, 1)
		result, probeErr := probeGuestDNS(ctx, client, name, vm.GatewayIP, transport)
		requireGuestDNSProbe(t, "post-drift gateway "+transport, result, probeErr, transport)
	}
	for _, transport := range []string{"udp", "tcp"} {
		if err := deleteGatewayDNSFirewallRule(identity, vm, transport, gatewayDNSFirewallGuard); err != nil {
			t.Fatalf("delete exact %s gateway DNS guard: %v", transport, err)
		}
		assertGatewayDNSFirewallRule(t, reconcileCtx, identity, vm, transport, gatewayDNSFirewallGuard, true)
		assertGatewayDNSFirewallRuleCount(t, identity, vm, transport, gatewayDNSFirewallGuard, 1)
	}
	reconciledMetrics, err := client.vmMachineMetrics(ctx, vm.ID)
	if err != nil {
		t.Fatalf("read post-drift gateway DNS metrics: %v", err)
	}
	requireReadyGatewayDNSMetrics(t, *reconciledMetrics)
	if preRestartBootID := requireGuestBootID(t, ctx, client, name); preRestartBootID != guestBootID {
		t.Fatalf("rule-drift guest boot ID = %q, want unchanged %q", preRestartBootID, guestBootID)
	}

	identityBeforeRestart := identity
	restartDaemon(t, client)
	identity, err = readGatewayDNSFirewallIdentity(bringdownBaseline.dataDir)
	if err != nil {
		t.Fatalf("read gateway DNS firewall identity after restart: %v", err)
	}
	if identity != identityBeforeRestart {
		t.Fatalf("gateway DNS firewall identity after restart = %+v, want %+v", identity, identityBeforeRestart)
	}

	warm, err := client.waitForVMState(ctx, name, models.MachineStateRunning)
	if err != nil {
		t.Fatalf("wait for firewall-drift VM after controller restart: %v", err)
	}
	if warm.GatewayIP != vm.GatewayIP || warm.IP != vm.IP || warm.TAPDevice != vm.TAPDevice {
		t.Fatalf("post-reload identity = %s/%s/%s, want %s/%s/%s",
			warm.GatewayIP, warm.IP, warm.TAPDevice,
			vm.GatewayIP, vm.IP, vm.TAPDevice,
		)
	}
	if warm.ID != vmID || warm.MachineID != machineID {
		t.Fatalf("post-reload provider identity = vm:%q machine:%q, want vm:%q machine:%q", warm.ID, warm.MachineID, vmID, machineID)
	}
	restartBootID := requireGuestBootID(t, ctx, client, name)
	for _, transport := range []string{"udp", "tcp"} {
		assertGatewayDNSFirewallRule(t, ctx, identity, warm, transport, gatewayDNSFirewallAdmit, true)
		assertGatewayDNSFirewallRule(t, ctx, identity, warm, transport, gatewayDNSFirewallGuard, true)
		assertGatewayDNSFirewallRuleCount(t, identity, warm, transport, gatewayDNSFirewallAdmit, 1)
		assertGatewayDNSFirewallRuleCount(t, identity, warm, transport, gatewayDNSFirewallGuard, 1)
		result, probeErr := probeGuestDNS(ctx, client, name, warm.GatewayIP, transport)
		requireGuestDNSProbe(t, "post-reload gateway "+transport, result, probeErr, transport)
	}
	restartedFirewall, err := gatewayDNSFirewallSnapshot(identity)
	if err != nil {
		t.Fatalf("snapshot gateway DNS firewall after restart: %v", err)
	}
	requireActiveGatewayDNSFirewallState(t, restartedFirewall, warm)
	assertGatewayDNSFirewallSentinel(t, sentinelComment, true)

	// Remove the complete owner-derived firewall subsystem while the kiln-vm VM and
	// both gateway DNS listeners remain running. The INPUT policy is ACCEPT, so
	// privileged removal of both the hook and chain is an explicitly observable
	// repair window, not the guard-only fail-closed case proved above.
	preDriftFirewall, err := gatewayDNSFirewallSnapshot(identity)
	if err != nil {
		t.Fatalf("revalidate gateway DNS firewall before drift: %v", err)
	}
	if !reflect.DeepEqual(preDriftFirewall, restartedFirewall) {
		t.Fatalf("gateway DNS firewall changed before targeted drift mutation: got %+v, want exact target-only state %+v", preDriftFirewall, restartedFirewall)
	}
	requireActiveGatewayDNSFirewallState(t, preDriftFirewall, warm)
	if err := deleteGatewayDNSFirewallOwnedState(identity); err != nil {
		t.Fatalf("delete complete owned gateway DNS firewall state: %v", err)
	}
	absentFirewall, err := gatewayDNSFirewallSnapshot(identity)
	if err != nil {
		t.Fatalf("snapshot absent gateway DNS firewall state: %v", err)
	}
	if absentFirewall.InputPolicy != "ACCEPT" || absentFirewall.ChainExists || len(absentFirewall.HookPositions) != 0 || len(absentFirewall.ChainRules) != 0 {
		t.Fatalf("whole-state firewall drift = %+v, want INPUT ACCEPT with no owned hook, chain, or rules", absentFirewall)
	}
	absentListeners, err := gatewayDNSListeners(warm.GatewayIP)
	if err != nil {
		t.Fatalf("inspect gateway DNS listeners during whole-state firewall drift: %v", err)
	}
	if !absentListeners.UDP || !absentListeners.TCP {
		t.Fatalf("gateway DNS listeners during whole-state firewall drift = %s, want UDP and TCP still bound", absentListeners)
	}
	assertGatewayDNSFirewallSentinel(t, sentinelComment, true)
	if absentBootID := requireGuestBootID(t, ctx, client, name); absentBootID != restartBootID {
		t.Fatalf("whole-state firewall drift guest boot ID = %q, want unchanged %q", absentBootID, restartBootID)
	}

	wholeStateRecoveryCtx, wholeStateRecoveryCancel := context.WithTimeout(ctx, 45*time.Second)
	defer wholeStateRecoveryCancel()
	assertGatewayDNSFirewallSnapshot(t, wholeStateRecoveryCtx, identity, restartedFirewall)
	recovered, err := client.waitForVMState(wholeStateRecoveryCtx, name, models.MachineStateRunning)
	if err != nil {
		t.Fatalf("wait for firewall-drift VM after whole-state reconciliation: %v", err)
	}
	if recovered.ID != vmID || recovered.MachineID != machineID {
		t.Fatalf("post-reconciliation provider identity = vm:%q machine:%q, want vm:%q machine:%q", recovered.ID, recovered.MachineID, vmID, machineID)
	}
	if recovered.GatewayIP != vm.GatewayIP || recovered.IP != vm.IP || recovered.TAPDevice != vm.TAPDevice {
		t.Fatalf("post-reconciliation network identity = %s/%s/%s, want %s/%s/%s",
			recovered.GatewayIP, recovered.IP, recovered.TAPDevice,
			vm.GatewayIP, vm.IP, vm.TAPDevice,
		)
	}
	if recoveredBootID := requireGuestBootID(t, wholeStateRecoveryCtx, client, name); recoveredBootID != restartBootID {
		t.Fatalf("post-reconciliation guest boot ID = %q, want unchanged %q", recoveredBootID, restartBootID)
	}
	recoveredFirewall, err := gatewayDNSFirewallSnapshot(identity)
	if err != nil {
		t.Fatalf("snapshot gateway DNS firewall after whole-state reconciliation: %v", err)
	}
	requireActiveGatewayDNSFirewallState(t, recoveredFirewall, recovered)
	for _, transport := range []string{"udp", "tcp"} {
		assertGatewayDNSFirewallRuleCount(t, identity, recovered, transport, gatewayDNSFirewallAdmit, 1)
		assertGatewayDNSFirewallRuleCount(t, identity, recovered, transport, gatewayDNSFirewallGuard, 1)
		result, probeErr := probeGuestDNS(wholeStateRecoveryCtx, client, name, recovered.GatewayIP, transport)
		requireGuestDNSProbe(t, "post-whole-firewall-recovery gateway "+transport, result, probeErr, transport)
	}
	assertGatewayDNSFirewallSentinel(t, sentinelComment, true)
	warm = recovered

	// Begin teardown from a proven guard-only state with both listeners still
	// bound. Deactivate is therefore idempotently closed before Stop and Release.
	proveGuestDNSDeniedByGuard(t, ctx, client, name, identity, warm, "udp")
	for _, transport := range []string{"udp", "tcp"} {
		if err := deleteGatewayDNSFirewallRule(identity, warm, transport, gatewayDNSFirewallAdmit); err != nil {
			t.Fatalf("prepare guard-only teardown for %s: %v", transport, err)
		}
	}
	guardOnlyFirewall := requireGatewayDNSFirewallGuardOnlySnapshot(t, ctx, identity, warm)
	for _, transport := range []string{"udp", "tcp"} {
		if guardOnlyFirewall.ruleCount(warm.TAPDevice, transport, gatewayDNSFirewallAdmit) != 0 ||
			guardOnlyFirewall.ruleCount(warm.TAPDevice, transport, gatewayDNSFirewallGuard) != 1 ||
			guardOnlyFirewall.packetVerdict(warm.TAPDevice, warm.IP, warm.GatewayIP, transport) != "DROP" {
			t.Fatalf("%s teardown firewall is not guard-only: %+v", transport, guardOnlyFirewall)
		}
	}

	if err := client.deleteVM(ctx, name); err != nil {
		t.Fatalf("delete firewall-drift gateway DNS VM: %v", err)
	}
	deleted = true
	if err := waitForNoTap(ctx, vm.TAPDevice); err != nil {
		t.Fatal(err)
	}
	for _, transport := range []string{"udp", "tcp"} {
		assertGatewayDNSFirewallRule(t, ctx, identity, vm, transport, gatewayDNSFirewallAdmit, false)
		assertGatewayDNSFirewallRule(t, ctx, identity, vm, transport, gatewayDNSFirewallGuard, false)
		assertGatewayDNSFirewallRuleCount(t, identity, vm, transport, gatewayDNSFirewallAdmit, 0)
		assertGatewayDNSFirewallRuleCount(t, identity, vm, transport, gatewayDNSFirewallGuard, 0)
	}
	assertGatewayDNSFirewallSnapshot(t, ctx, identity, baselineFirewall)
	assertGatewayDNSFirewallSentinel(t, sentinelComment, true)
	listeners, err := gatewayDNSListeners(vm.GatewayIP)
	if err != nil {
		t.Fatalf("inspect firewall-drift bringdown listeners: %v", err)
	}
	if listeners.UDP || listeners.TCP {
		t.Fatalf("gateway DNS listeners survived firewall-drift bringdown: %s", listeners)
	}
	requireVMBringdown(t, client, bringdownBaseline, vm)
	identityAfterCleanup, err := readGatewayDNSFirewallIdentity(bringdownBaseline.dataDir)
	if err != nil || identityAfterCleanup != identity {
		t.Fatalf("durable firewall identity after cleanup = (%+v, %v), want %+v", identityAfterCleanup, err, identity)
	}
	if err := deleteGatewayDNSFirewallSentinel(sentinelComment); err != nil {
		t.Fatalf("remove unrelated INPUT sentinel: %v", err)
	}
	sentinelPresent = false
	assertGatewayDNSFirewallSentinel(t, sentinelComment, false)
}

func gatewayDNSFirewallSnapshot(identity gatewayDNSFirewallIdentity) (gatewayDNSFirewallState, error) {
	out, err := exec.Command("iptables", "-w", "5", "-S").CombinedOutput()
	if err != nil {
		return gatewayDNSFirewallState{}, commandError("snapshot gateway DNS firewall", out, err)
	}
	return parseGatewayDNSFirewallState(string(out), identity)
}

func requireGuestBootID(t *testing.T, ctx context.Context, client *apiClient, vmName string) string {
	t.Helper()
	result, err := client.execVM(ctx, vmName, "cat /proc/sys/kernel/random/boot_id")
	if err != nil {
		t.Fatalf("read guest boot ID from VM %s: %v", vmName, err)
	}
	if result == nil || result.ExitCode != 0 {
		t.Fatalf("read guest boot ID from VM %s returned %+v", vmName, result)
	}
	bootID := strings.TrimSpace(result.Stdout)
	if len(bootID) != 36 || strings.Count(bootID, "-") != 4 {
		t.Fatalf("guest boot ID from VM %s = %q, want canonical UUID text", vmName, bootID)
	}
	return bootID
}

func deleteGatewayDNSFirewallOwnedState(identity gatewayDNSFirewallIdentity) error {
	hookArgs := []string{
		"-w", "5", "-D", "INPUT",
		"-m", "comment",
		"--comment", identity.commentBase() + ":input",
		"-j", identity.Chain,
	}
	out, err := exec.Command("iptables", hookArgs...).CombinedOutput()
	if err != nil {
		return commandError("delete owned gateway DNS INPUT hook", out, err)
	}
	out, err = exec.Command("iptables", "-w", "5", "-F", identity.Chain).CombinedOutput()
	if err != nil {
		return commandError("flush owned gateway DNS chain", out, err)
	}
	out, err = exec.Command("iptables", "-w", "5", "-X", identity.Chain).CombinedOutput()
	if err != nil {
		return commandError("delete owned gateway DNS chain", out, err)
	}
	return nil
}

func assertGatewayDNSFirewallSnapshot(t *testing.T, ctx context.Context, identity gatewayDNSFirewallIdentity, want gatewayDNSFirewallState) {
	t.Helper()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		got, err := gatewayDNSFirewallSnapshot(identity)
		if err != nil {
			t.Fatalf("inspect gateway DNS firewall convergence: %v", err)
		}
		if reflect.DeepEqual(got, want) {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("gateway DNS firewall state = %+v, want exact state %+v: %v", got, want, ctx.Err())
		case <-ticker.C:
		}
	}
}

func requireActiveGatewayDNSFirewallState(t *testing.T, state gatewayDNSFirewallState, vm *models.VM) {
	t.Helper()
	if state.InputPolicy != "ACCEPT" || !state.ChainExists || len(state.HookPositions) != 1 || state.HookPositions[0] != 1 {
		t.Fatalf("active gateway DNS firewall = %+v, want INPUT ACCEPT, owner-derived chain, and one first-position exact hook", state)
	}
	if len(state.ChainRules) != 4 {
		t.Fatalf("active owner-derived chain has %d rules, want exactly two admissions and two guards: %+v", len(state.ChainRules), state.ChainRules)
	}
	seenGuard := false
	for _, rule := range state.ChainRules {
		if rule.Verdict == gatewayDNSFirewallGuard {
			seenGuard = true
		} else if seenGuard {
			t.Fatalf("gateway DNS admission appears after a DROP guard: %+v", state.ChainRules)
		}
	}
	for _, transport := range []string{"udp", "tcp"} {
		if state.ruleCount(vm.TAPDevice, transport, gatewayDNSFirewallAdmit) != 1 ||
			state.ruleCount(vm.TAPDevice, transport, gatewayDNSFirewallGuard) != 1 ||
			state.packetVerdict(vm.TAPDevice, vm.IP, vm.GatewayIP, transport) != "ACCEPT" {
			t.Fatalf("active %s gateway DNS firewall is not exactly admitted-before-guarded: %+v", transport, state)
		}
	}
	for _, rule := range state.ChainRules {
		switch rule.Verdict {
		case gatewayDNSFirewallAdmit:
			if rule.Source != vm.IP+"/32" || rule.Destination != vm.GatewayIP+"/32" {
				t.Fatalf("gateway DNS admission = %+v, want exact guest/own-gateway /32 selectors", rule)
			}
		case gatewayDNSFirewallGuard:
			if rule.Source != "" || rule.Destination != "" {
				t.Fatalf("gateway DNS guard = %+v, want no source or destination selector", rule)
			}
		}
	}
}

func gatewayDNSFirewallRuleArgs(identity gatewayDNSFirewallIdentity, vm *models.VM, transport, verdict string) []string {
	target := "ACCEPT"
	args := []string{"-i", vm.TAPDevice}
	if verdict == gatewayDNSFirewallGuard {
		target = "DROP"
	} else {
		args = append(args,
			"-s", vm.IP+"/32",
			"-d", vm.GatewayIP+"/32",
		)
	}
	return append(args,
		"-p", transport,
		"--dport", "53",
		"-m", "comment",
		"--comment", fmt.Sprintf("%s:alloc:%s:%s:%s", identity.commentBase(), vm.TAPDevice, transport, verdict),
		"-j", target,
	)
}

func gatewayDNSFirewallRuleExists(identity gatewayDNSFirewallIdentity, vm *models.VM, transport, verdict string) (bool, error) {
	args := []string{"-w", "5", "-C", identity.Chain}
	args = append(args, gatewayDNSFirewallRuleArgs(identity, vm, transport, verdict)...)
	out, err := exec.Command("iptables", args...).CombinedOutput()
	if err == nil {
		return true, nil
	}
	if iptablesRuleMissing(out, err) {
		return false, nil
	}
	return false, commandError(fmt.Sprintf("check %s gateway DNS %s", transport, verdict), out, err)
}

func deleteGatewayDNSFirewallRule(identity gatewayDNSFirewallIdentity, vm *models.VM, transport, verdict string) error {
	args := []string{"-w", "5", "-D", identity.Chain}
	args = append(args, gatewayDNSFirewallRuleArgs(identity, vm, transport, verdict)...)
	out, err := exec.Command("iptables", args...).CombinedOutput()
	if err == nil || iptablesRuleMissing(out, err) {
		return nil
	}
	return commandError(fmt.Sprintf("delete %s gateway DNS %s", transport, verdict), out, err)
}

func iptablesRuleMissing(out []byte, err error) bool {
	if !commandExitedWithCodeForTest(err, 1) {
		return false
	}
	message := strings.ToLower(strings.TrimSpace(string(out)))
	return strings.Contains(message, "does a matching rule exist in that chain") || strings.Contains(message, "rule does not exist")
}

func assertGatewayDNSFirewallRule(t *testing.T, ctx context.Context, identity gatewayDNSFirewallIdentity, vm *models.VM, transport, verdict string, want bool) {
	t.Helper()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		exists, err := gatewayDNSFirewallRuleExists(identity, vm, transport, verdict)
		if err != nil {
			t.Fatalf("inspect %s gateway DNS %s: %v", transport, verdict, err)
		}
		if exists == want {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("%s gateway DNS %s exists=%t, want %t: %v", transport, verdict, exists, want, ctx.Err())
		case <-ticker.C:
		}
	}
}

func assertGatewayDNSFirewallRuleCount(t *testing.T, identity gatewayDNSFirewallIdentity, vm *models.VM, transport, verdict string, want int) {
	t.Helper()
	state, err := gatewayDNSFirewallSnapshot(identity)
	if err != nil {
		t.Fatalf("list gateway DNS %s rules: %v", verdict, err)
	}
	count := state.ruleCount(vm.TAPDevice, transport, verdict)
	if count != want {
		t.Fatalf("%s gateway DNS %s count = %d, want %d: %+v", transport, verdict, count, want, state)
	}
}

func proveGuestDNSDeniedByGuard(t *testing.T, ctx context.Context, client *apiClient, vmName string, identity gatewayDNSFirewallIdentity, vm *models.VM, transport string) {
	t.Helper()
	proofCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	last := "guard-only attempt has not completed"
	for {
		for _, candidate := range []string{"udp", "tcp"} {
			if err := deleteGatewayDNSFirewallRule(identity, vm, candidate, gatewayDNSFirewallAdmit); err != nil {
				t.Fatalf("prepare guard-only %s proof: %v", candidate, err)
			}
		}
		before, err := gatewayDNSFirewallSnapshot(identity)
		if err != nil {
			t.Fatalf("snapshot before guard-only %s probe: %v", transport, err)
		}
		listeners, listenerErr := gatewayDNSListeners(vm.GatewayIP)
		listenerReady := listenerErr == nil && ((transport == "udp" && listeners.UDP) || (transport == "tcp" && listeners.TCP))
		if before.InputPolicy == "ACCEPT" && before.ruleCount(vm.TAPDevice, transport, gatewayDNSFirewallAdmit) == 0 &&
			before.ruleCount(vm.TAPDevice, transport, gatewayDNSFirewallGuard) == 1 &&
			before.packetVerdict(vm.TAPDevice, vm.IP, vm.GatewayIP, transport) == "DROP" && listenerReady {
			result, probeErr := probeGuestDNS(ctx, client, vmName, vm.GatewayIP, transport)
			after, snapshotErr := gatewayDNSFirewallSnapshot(identity)
			listenersAfter, listenerAfterErr := gatewayDNSListeners(vm.GatewayIP)
			listenerStillReady := listenerAfterErr == nil && ((transport == "udp" && listenersAfter.UDP) || (transport == "tcp" && listenersAfter.TCP))
			stableGuard := snapshotErr == nil && after.InputPolicy == "ACCEPT" && after.ruleCount(vm.TAPDevice, transport, gatewayDNSFirewallAdmit) == 0 &&
				after.ruleCount(vm.TAPDevice, transport, gatewayDNSFirewallGuard) == 1 &&
				after.packetVerdict(vm.TAPDevice, vm.IP, vm.GatewayIP, transport) == "DROP"
			if stableGuard && listenerStillReady {
				switch {
				case probeErr != nil:
					last = fmt.Sprintf("guest execution failed rather than DNS being denied: %v", probeErr)
				case result == nil:
					last = "guest execution returned no result"
				case result.ExitCode != 0:
					last = fmt.Sprintf("guest execution exited %d: %s", result.ExitCode, result.Stderr)
				case strings.TrimSpace(result.Stdout) != "":
					t.Fatalf("guard-only %s probe received bytes while the exact DROP remained first: stdout=%q stderr=%q", transport, result.Stdout, result.Stderr)
				default:
					if guestDNSProbeError(result, nil, transport) == nil {
						t.Fatalf("guard-only %s probe unexpectedly produced a valid DNS response", transport)
					}
					return
				}
			} else {
				last = fmt.Sprintf("reconciler changed guard-only state during probe: state=%+v snapshot_err=%v listeners=%s listener_err=%v", after, snapshotErr, listenersAfter, listenerAfterErr)
			}
		} else {
			last = fmt.Sprintf("state was not guard-only with a live listener: state=%+v listeners=%s listener_err=%v", before, listeners, listenerErr)
		}

		select {
		case <-proofCtx.Done():
			t.Fatalf("could not prove guest %s DNS denial under an exact guard and INPUT ACCEPT: %s: %v", transport, last, proofCtx.Err())
		case <-ticker.C:
		}
	}
}

func requireGatewayDNSFirewallGuardOnlySnapshot(t *testing.T, ctx context.Context, identity gatewayDNSFirewallIdentity, vm *models.VM) gatewayDNSFirewallState {
	t.Helper()
	proofCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	last := "guard-only state has not been observed"
	for {
		for _, transport := range []string{"udp", "tcp"} {
			if err := deleteGatewayDNSFirewallRule(identity, vm, transport, gatewayDNSFirewallAdmit); err != nil {
				t.Fatalf("prepare exact guard-only snapshot for %s: %v", transport, err)
			}
		}
		state, err := gatewayDNSFirewallSnapshot(identity)
		listeners, listenerErr := gatewayDNSListeners(vm.GatewayIP)
		if err == nil && state.InputPolicy == "ACCEPT" && listenerErr == nil && listeners.UDP && listeners.TCP {
			guardOnly := true
			for _, transport := range []string{"udp", "tcp"} {
				guardOnly = guardOnly &&
					state.ruleCount(vm.TAPDevice, transport, gatewayDNSFirewallAdmit) == 0 &&
					state.ruleCount(vm.TAPDevice, transport, gatewayDNSFirewallGuard) == 1 &&
					state.packetVerdict(vm.TAPDevice, vm.IP, vm.GatewayIP, transport) == "DROP"
			}
			if guardOnly {
				return state
			}
		}
		last = fmt.Sprintf("state=%+v state_err=%v listeners=%s listener_err=%v", state, err, listeners, listenerErr)
		select {
		case <-proofCtx.Done():
			t.Fatalf("gateway DNS firewall did not become guard-only with live UDP/TCP listeners: %s: %v", last, proofCtx.Err())
		case <-ticker.C:
		}
	}
}

func gatewayDNSFirewallSentinelArgs(comment string) []string {
	return []string{"-s", "192.0.2.123/32", "-m", "comment", "--comment", comment, "-j", "DROP"}
}

func addGatewayDNSFirewallSentinel(comment string) error {
	args := []string{"-w", "5", "-A", "INPUT"}
	args = append(args, gatewayDNSFirewallSentinelArgs(comment)...)
	out, err := exec.Command("iptables", args...).CombinedOutput()
	if err != nil {
		return commandError("add unrelated gateway DNS INPUT sentinel", out, err)
	}
	return nil
}

func deleteGatewayDNSFirewallSentinel(comment string) error {
	args := []string{"-w", "5", "-D", "INPUT"}
	args = append(args, gatewayDNSFirewallSentinelArgs(comment)...)
	out, err := exec.Command("iptables", args...).CombinedOutput()
	if err == nil || iptablesRuleMissing(out, err) {
		return nil
	}
	return commandError("delete unrelated gateway DNS INPUT sentinel", out, err)
}

func gatewayDNSFirewallSentinelExists(comment string) (bool, error) {
	args := []string{"-w", "5", "-C", "INPUT"}
	args = append(args, gatewayDNSFirewallSentinelArgs(comment)...)
	out, err := exec.Command("iptables", args...).CombinedOutput()
	if err == nil {
		return true, nil
	}
	if iptablesRuleMissing(out, err) {
		return false, nil
	}
	return false, commandError("inspect unrelated gateway DNS INPUT sentinel", out, err)
}

func assertGatewayDNSFirewallSentinel(t *testing.T, comment string, want bool) {
	t.Helper()
	exists, err := gatewayDNSFirewallSentinelExists(comment)
	if err != nil || exists != want {
		t.Fatalf("unrelated gateway DNS INPUT sentinel = (%t, %v), want (%t, nil)", exists, err, want)
	}
}
