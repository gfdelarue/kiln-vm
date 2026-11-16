package machine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func TestGatewayDNSFirewallActivateInstallsExactScopedRulesAndEarlyHook(t *testing.T) {
	runner := newFakeGatewayDNSFirewallRunner()
	runner.appendRule("INPUT", []string{"-p", "tcp", "--dport", "22", "-j", "ACCEPT"})
	firewall := newGatewayDNSFirewallWithRunner("172.16.0.0/16", runner)
	alloc := mustGatewayDNSFirewallAllocation(t, "172.16.0.0/16", 1)

	if err := firewall.Activate(alloc); err != nil {
		t.Fatalf("Activate: %v", err)
	}

	assertFakeGatewayDNSFirewallHook(t, firewall, runner, true)
	assertFakeGatewayDNSFirewallRules(t, firewall, runner, alloc)
	input := runner.rules("INPUT")
	if len(input) != 2 || !equalGatewayDNSFirewallTokens(input[1], []string{"-A", "INPUT", "-p", "tcp", "--dport", "22", "-j", "ACCEPT"}) {
		t.Fatalf("unrelated INPUT rule changed: %#v", input)
	}
	status := firewall.Status(alloc)
	if !status.Desired || !status.GuardDesired || !status.AdmissionDesired || !status.HookInstalled ||
		!status.UDPInstalled || !status.TCPInstalled || !status.GuardUDPInstalled || !status.GuardTCPInstalled ||
		!status.GuardReady || !status.AdmissionReady || !status.Ready || status.LastError != "" {
		t.Fatalf("Status = %+v, want desired and ready", status)
	}
	if status.CheckedAt.IsZero() {
		t.Fatal("Status.CheckedAt is zero")
	}

	for _, command := range runner.commandsSnapshot() {
		if command.name != "iptables" || len(command.args) < 3 || command.args[0] != "-w" || command.args[1] != "5" {
			t.Fatalf("command = %#v, want argv-only iptables -w 5", command)
		}
		if !command.hadDeadline {
			t.Fatalf("command = %#v, want bounded context deadline", command)
		}
	}
}

func TestGatewayDNSFirewallInputGuardIsTapScopedAndSourceDestinationIndependent(t *testing.T) {
	runner := newFakeGatewayDNSFirewallRunner()
	externalForward := []string{"-A", "FORWARD", "-d", "1.1.1.1/32", "-p", "udp", "--dport", "53", "-j", "ACCEPT"}
	runner.appendFullRule("FORWARD", externalForward)

	firewall := newGatewayDNSFirewallWithRunner("10.77.0.0/20", runner)
	first := mustGatewayDNSFirewallAllocation(t, "10.77.0.0/20", 1)
	second := mustGatewayDNSFirewallAllocation(t, "10.77.0.0/20", 2)
	historical := mustGatewayDNSFirewallAllocation(t, "172.16.0.0/16", 41)
	if err := firewall.ReconcileState([]Allocation{first, second, historical}, []Allocation{first, second}); err != nil {
		t.Fatalf("ReconcileState: %v", err)
	}

	for _, protocol := range []string{"udp", "tcp"} {
		if verdict := fakeGatewayDNSFirewallInputVerdict(firewall, runner, first.TAPDevice, first.GuestIP, first.GatewayIP, protocol); verdict != "ACCEPT" {
			t.Fatalf("first exact own-gateway %s verdict = %s, want ACCEPT", protocol, verdict)
		}
		if verdict := fakeGatewayDNSFirewallInputVerdict(firewall, runner, second.TAPDevice, second.GuestIP, second.GatewayIP, protocol); verdict != "ACCEPT" {
			t.Fatalf("second exact own-gateway %s verdict = %s, want ACCEPT", protocol, verdict)
		}
		if verdict := fakeGatewayDNSFirewallInputVerdict(firewall, runner, first.TAPDevice, first.GuestIP, second.GatewayIP, protocol); verdict != "DROP" {
			t.Fatalf("current cross-allocation %s verdict = %s, want DROP", protocol, verdict)
		}
		if verdict := fakeGatewayDNSFirewallInputVerdict(firewall, runner, first.TAPDevice, first.GuestIP, historical.GatewayIP, protocol); verdict != "DROP" {
			t.Fatalf("current-to-historical gateway %s verdict = %s, want DROP", protocol, verdict)
		}
		if verdict := fakeGatewayDNSFirewallInputVerdict(firewall, runner, historical.TAPDevice, historical.GuestIP, first.GatewayIP, protocol); verdict != "DROP" {
			t.Fatalf("historical-to-current gateway %s verdict = %s, want DROP", protocol, verdict)
		}
		if verdict := fakeGatewayDNSFirewallInputVerdict(firewall, runner, first.TAPDevice, "198.51.100.77", first.GatewayIP, protocol); verdict != "DROP" {
			t.Fatalf("spoofed-source own-gateway %s verdict = %s, want DROP", protocol, verdict)
		}
	}

	for _, protocol := range []string{"udp", "tcp"} {
		admit, err := firewall.parseAllocationRule(firewall.ruleTokens(first, protocol, gatewayDNSFirewallVerdictAdmit))
		if err != nil || admit.source != first.GuestIP+"/32" || admit.destination != first.GatewayIP+"/32" {
			t.Fatalf("custom-CIDR %s admission = (%+v, %v), want exact %s/32 -> %s/32", protocol, admit, err, first.GuestIP, first.GatewayIP)
		}
		guard, err := firewall.parseAllocationRule(firewall.ruleTokens(first, protocol, gatewayDNSFirewallVerdictGuard))
		if err != nil || guard.source != "" || guard.destination != "" || guard.inputInterface != first.TAPDevice || guard.target != "DROP" {
			t.Fatalf("custom-CIDR %s guard = (%+v, %v), want source/destination-independent per-TAP DROP", protocol, guard, err)
		}
	}

	// External guest DNS is routed through FORWARD, not INPUT. kiln-vm's owned
	// firewall hook must therefore leave that path entirely untouched.
	forwardRules := runner.rules("FORWARD")
	if len(forwardRules) != 1 || !firewall.equalTokens(forwardRules[0], externalForward) {
		t.Fatalf("external FORWARD DNS path changed: %#v", forwardRules)
	}
	for _, command := range runner.commandsSnapshot() {
		if len(command.args) > 3 && command.args[3] == "FORWARD" {
			t.Fatalf("gateway DNS firewall mutated FORWARD for external DNS: %#v", command)
		}
	}
}

func TestGatewayDNSFirewallGuardAdmissionLifecycleIsFailClosedWithInputAccept(t *testing.T) {
	runner := newFakeGatewayDNSFirewallRunner()
	if runner.inputPolicy != "ACCEPT" {
		t.Fatalf("fake INPUT policy = %s, want ACCEPT", runner.inputPolicy)
	}
	firewall := newGatewayDNSFirewallWithRunner("172.16.0.0/16", runner)
	alloc := mustGatewayDNSFirewallAllocation(t, "172.16.0.0/16", 1)

	if err := firewall.Guard(alloc); err != nil {
		t.Fatalf("Guard: %v", err)
	}
	if verdict := fakeGatewayDNSFirewallVerdict(firewall, runner, alloc, "udp"); verdict != "DROP" {
		t.Fatalf("guard-only UDP verdict = %s, want DROP", verdict)
	}
	if verdict := fakeGatewayDNSFirewallVerdict(firewall, runner, alloc, "tcp"); verdict != "DROP" {
		t.Fatalf("guard-only TCP verdict = %s, want DROP", verdict)
	}
	status := firewall.Status(alloc)
	if !status.GuardDesired || !status.GuardReady || status.AdmissionDesired || status.AdmissionReady || status.Desired || status.Ready ||
		status.UDPInstalled || status.TCPInstalled {
		t.Fatalf("guard-only status = %+v", status)
	}

	if err := firewall.Activate(alloc); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	assertFakeGatewayDNSFirewallRules(t, firewall, runner, alloc)
	for _, protocol := range []string{"udp", "tcp"} {
		if verdict := fakeGatewayDNSFirewallVerdict(firewall, runner, alloc, protocol); verdict != "ACCEPT" {
			t.Fatalf("admitted %s verdict = %s, want ACCEPT", protocol, verdict)
		}
	}

	if err := firewall.Deactivate(alloc); err != nil {
		t.Fatalf("Deactivate: %v", err)
	}
	for _, protocol := range []string{"udp", "tcp"} {
		if verdict := fakeGatewayDNSFirewallVerdict(firewall, runner, alloc, protocol); verdict != "DROP" {
			t.Fatalf("deactivated %s verdict = %s, want DROP", protocol, verdict)
		}
	}
	status = firewall.Status(alloc)
	if !status.GuardReady || status.AdmissionDesired || status.AdmissionReady || status.UDPInstalled || status.TCPInstalled {
		t.Fatalf("deactivated status = %+v", status)
	}

	if err := firewall.Release(alloc); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if verdict := fakeGatewayDNSFirewallVerdict(firewall, runner, alloc, "udp"); verdict != "ACCEPT" {
		t.Fatalf("released verdict = %s, want host INPUT policy ACCEPT", verdict)
	}
}

func TestGatewayDNSFirewallActivateCreatesDropsBeforeAcceptsAndHooksLast(t *testing.T) {
	runner := newFakeGatewayDNSFirewallRunner()
	firewall := newGatewayDNSFirewallWithRunner("172.16.0.0/16", runner)
	alloc := mustGatewayDNSFirewallAllocation(t, "172.16.0.0/16", 1)

	if err := firewall.Activate(alloc); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	firstGuard, firstAdmit, hook := -1, -1, -1
	for index, command := range runner.commandsSnapshot() {
		joined := strings.Join(command.args, " ")
		switch {
		case strings.Contains(joined, ":guard") && firstGuard < 0:
			firstGuard = index
		case strings.Contains(joined, ":admit") && firstAdmit < 0:
			firstAdmit = index
		case fakeGatewayDNSFirewallOperation(command.args) == "-I" && len(command.args) > 3 && command.args[3] == "INPUT":
			hook = index
		}
	}
	if firstGuard < 0 || firstAdmit < 0 || hook < 0 || firstGuard > firstAdmit || firstAdmit > hook {
		t.Fatalf("mutation order guard/admit/hook = %d/%d/%d, commands=%#v", firstGuard, firstAdmit, hook, runner.commandsSnapshot())
	}
}

func TestGatewayDNSFirewallDeactivateFailureLeavesClosingDropBeforeStaleAccept(t *testing.T) {
	runner := newFakeGatewayDNSFirewallRunner()
	firewall := newGatewayDNSFirewallWithRunner("172.16.0.0/16", runner)
	alloc := mustGatewayDNSFirewallAllocation(t, "172.16.0.0/16", 1)
	if err := firewall.Activate(alloc); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	runner.failOnce(func(args []string) bool {
		return fakeGatewayDNSFirewallOperation(args) == "-D" && len(args) > 3 && args[3] == firewall.identity.Chain
	}, errors.New("injected stale admission delete failure"))

	err := firewall.Deactivate(alloc)
	if err == nil || !strings.Contains(err.Error(), "injected stale admission delete failure") {
		t.Fatalf("Deactivate error = %v", err)
	}
	for _, protocol := range []string{"udp", "tcp"} {
		if verdict := fakeGatewayDNSFirewallVerdict(firewall, runner, alloc, protocol); verdict != "DROP" {
			t.Fatalf("failed deactivate %s verdict = %s, want closing DROP before stale ACCEPT", protocol, verdict)
		}
	}
	if status := firewall.Status(alloc); status.AdmissionDesired || !status.GuardDesired || status.LastError == "" {
		t.Fatalf("failed deactivate status = %+v", status)
	}
	if err := firewall.ReconcileState([]Allocation{alloc}, nil); err != nil {
		t.Fatalf("retry guard-only reconcile: %v", err)
	}
}

func TestGatewayDNSFirewallReconcileStateRejectsAdmissionOutsideGuardWithoutCommands(t *testing.T) {
	runner := newFakeGatewayDNSFirewallRunner()
	firewall := newGatewayDNSFirewallWithRunner("172.16.0.0/16", runner)
	alloc := mustGatewayDNSFirewallAllocation(t, "172.16.0.0/16", 1)
	if err := firewall.ReconcileState(nil, []Allocation{alloc}); err == nil || !strings.Contains(err.Error(), "not present in the guarded snapshot") {
		t.Fatalf("ReconcileState error = %v", err)
	}
	if commands := runner.commandsSnapshot(); len(commands) != 0 {
		t.Fatalf("invalid snapshot ran commands: %#v", commands)
	}
}

func TestGatewayDNSFirewallReconcileRepairsLateDuplicateHookAndOwnedRuleDrift(t *testing.T) {
	runner := newFakeGatewayDNSFirewallRunner()
	runner.appendRule("INPUT", []string{"-p", "tcp", "--dport", "22", "-j", "ACCEPT"})
	firewall := newGatewayDNSFirewallWithRunner("172.16.0.0/16", runner)
	first := mustGatewayDNSFirewallAllocation(t, "172.16.0.0/16", 1)
	stale := mustGatewayDNSFirewallAllocation(t, "172.16.0.0/16", 2)
	if err := firewall.Activate(first); err != nil {
		t.Fatalf("Activate: %v", err)
	}

	// Move the owned hook behind the unrelated rule, duplicate it, duplicate
	// one desired rule, and add a stale owned allocation rule.
	runner.setRules("INPUT", [][]string{
		{"-A", "INPUT", "-p", "tcp", "--dport", "22", "-j", "ACCEPT"},
		firewall.hookRuleTokens(),
		firewall.hookRuleTokens(),
	})
	runner.appendFullRule(firewall.identity.Chain, firewall.ruleTokens(first, "udp", gatewayDNSFirewallVerdictAdmit))
	runner.appendFullRule(firewall.identity.Chain, firewall.ruleTokens(stale, "udp", gatewayDNSFirewallVerdictAdmit))
	runner.appendFullRule(firewall.identity.Chain, firewall.ruleTokens(stale, "tcp", gatewayDNSFirewallVerdictAdmit))

	if err := firewall.Reconcile([]Allocation{first}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	assertFakeGatewayDNSFirewallHook(t, firewall, runner, true)
	assertFakeGatewayDNSFirewallRules(t, firewall, runner, first)

	input := runner.rules("INPUT")
	if len(input) != 2 || !equalGatewayDNSFirewallTokens(input[1], []string{"-A", "INPUT", "-p", "tcp", "--dport", "22", "-j", "ACCEPT"}) {
		t.Fatalf("INPUT rules after repair = %#v", input)
	}
	mutations := runner.mutationCount()
	if err := firewall.Reconcile([]Allocation{first}); err != nil {
		t.Fatalf("idempotent Reconcile: %v", err)
	}
	if got := runner.mutationCount(); got != mutations {
		t.Fatalf("idempotent Reconcile mutations = %d, want unchanged %d", got, mutations)
	}
}

func TestGatewayDNSFirewallReconcileTreatsRealIptablesRenderingAsIdentical(t *testing.T) {
	runner := newFakeGatewayDNSFirewallRunner()
	firewall := newGatewayDNSFirewallWithRunner("172.16.0.0/16", runner)
	alloc := mustGatewayDNSFirewallAllocation(t, "172.16.0.0/16", 1)
	if err := firewall.Activate(alloc); err != nil {
		t.Fatalf("Activate: %v", err)
	}

	// iptables -S reorders interface/source/destination selectors and renders
	// the implicit protocol match module. This fixture is copied from a real
	// iptables renderer rather than from the mutation argv.
	runner.setRules(firewall.identity.Chain, [][]string{
		realRenderedGatewayDNSFirewallRule(firewall, alloc, "udp", gatewayDNSFirewallVerdictAdmit),
		realRenderedGatewayDNSFirewallRule(firewall, alloc, "tcp", gatewayDNSFirewallVerdictAdmit),
		realRenderedGatewayDNSFirewallRule(firewall, alloc, "udp", gatewayDNSFirewallVerdictGuard),
		realRenderedGatewayDNSFirewallRule(firewall, alloc, "tcp", gatewayDNSFirewallVerdictGuard),
	})
	mutations := runner.mutationCount()
	if err := firewall.Reconcile([]Allocation{alloc}); err != nil {
		t.Fatalf("Reconcile rendered rules: %v", err)
	}
	if got := runner.mutationCount(); got != mutations {
		t.Fatalf("render-equivalent Reconcile mutations = %d, want unchanged %d", got, mutations)
	}
	if status := firewall.Status(alloc); !status.Ready {
		t.Fatalf("Status = %+v, want ready", status)
	}
}

func TestGatewayDNSFirewallReconcileAddsMissingBeforePruningStale(t *testing.T) {
	runner := newFakeGatewayDNSFirewallRunner()
	firewall := newGatewayDNSFirewallWithRunner("172.16.0.0/16", runner)
	first := mustGatewayDNSFirewallAllocation(t, "172.16.0.0/16", 1)
	second := mustGatewayDNSFirewallAllocation(t, "172.16.0.0/16", 2)
	if err := firewall.Activate(first); err != nil {
		t.Fatalf("Activate first: %v", err)
	}
	runner.clearCommands()

	if err := firewall.Reconcile([]Allocation{second}); err != nil {
		t.Fatalf("Reconcile second: %v", err)
	}
	assertFakeGatewayDNSFirewallRules(t, firewall, runner, second)

	commands := runner.commandsSnapshot()
	firstAdd := -1
	firstDelete := -1
	for index, command := range commands {
		operation := fakeGatewayDNSFirewallOperation(command.args)
		if operation == "-A" && firstAdd < 0 {
			firstAdd = index
		}
		if operation == "-D" && len(command.args) > 3 && command.args[3] == firewall.identity.Chain && firstDelete < 0 {
			firstDelete = index
		}
	}
	if firstAdd < 0 || firstDelete < 0 || firstAdd > firstDelete {
		t.Fatalf("commands did not add missing rules before stale deletion: %#v", commands)
	}
}

func TestGatewayDNSFirewallZeroDesiredRemovesOnlyOwnedState(t *testing.T) {
	runner := newFakeGatewayDNSFirewallRunner()
	runner.appendRule("INPUT", []string{"-p", "tcp", "--dport", "22", "-j", "ACCEPT"})
	firewall := newGatewayDNSFirewallWithRunner("172.16.0.0/16", runner)
	alloc := mustGatewayDNSFirewallAllocation(t, "172.16.0.0/16", 1)
	if err := firewall.Activate(alloc); err != nil {
		t.Fatalf("Activate: %v", err)
	}

	if err := firewall.Reconcile(nil); err != nil {
		t.Fatalf("Reconcile nil: %v", err)
	}
	if runner.hasChain(firewall.identity.Chain) {
		t.Fatalf("owned chain %s survived zero desired snapshot", firewall.identity.Chain)
	}
	input := runner.rules("INPUT")
	want := [][]string{{"-A", "INPUT", "-p", "tcp", "--dport", "22", "-j", "ACCEPT"}}
	if !equalFakeGatewayDNSFirewallRuleSets(input, want) {
		t.Fatalf("INPUT rules = %#v, want unrelated sentinel %#v", input, want)
	}
	status := firewall.Status(alloc)
	if status.Desired || status.GuardDesired || status.AdmissionDesired || status.HookInstalled || status.UDPInstalled || status.TCPInstalled ||
		status.GuardUDPInstalled || status.GuardTCPInstalled || status.GuardReady || status.AdmissionReady || status.Ready || status.LastError != "" {
		t.Fatalf("Status after cleanup = %+v", status)
	}
}

func TestGatewayDNSFirewallRejectsUnownedChainCollisionWithoutMutation(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*fakeGatewayDNSFirewallRunner)
		want  string
	}{
		{
			name: "unowned rule in reserved chain",
			setup: func(runner *fakeGatewayDNSFirewallRunner) {
				runner.ensureChain(gatewayDNSFirewallChain)
				runner.appendRule(gatewayDNSFirewallChain, []string{"-s", "192.0.2.1/32", "-j", "DROP"})
			},
			want: "contains unowned rule",
		},
		{
			name: "unowned INPUT reference",
			setup: func(runner *fakeGatewayDNSFirewallRunner) {
				runner.ensureChain(gatewayDNSFirewallChain)
				runner.appendRule("INPUT", []string{"-s", "192.0.2.1/32", "-j", gatewayDNSFirewallChain})
			},
			want: "unowned reference",
		},
		{
			name: "unowned reference from another chain",
			setup: func(runner *fakeGatewayDNSFirewallRunner) {
				runner.ensureChain(gatewayDNSFirewallChain)
				runner.ensureChain("FOREIGN")
				runner.appendRule("FOREIGN", []string{"-j", gatewayDNSFirewallChain})
			},
			want: "unowned reference",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			runner := newFakeGatewayDNSFirewallRunner()
			tc.setup(runner)
			before := runner.snapshot()
			firewall := newGatewayDNSFirewallWithRunner("172.16.0.0/16", runner)
			alloc := mustGatewayDNSFirewallAllocation(t, "172.16.0.0/16", 1)

			err := firewall.Reconcile([]Allocation{alloc})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Reconcile error = %v, want %q", err, tc.want)
			}
			if after := runner.snapshot(); after != before {
				t.Fatalf("firewall mutated on ownership collision:\nbefore=%s\nafter=%s", before, after)
			}
			status := firewall.Status(alloc)
			if status.Ready || !status.OwnershipCollision || status.LastError == "" {
				t.Fatalf("Status after collision = %+v, want failed", status)
			}
		})
	}
}

func TestGatewayDNSFirewallRejectsMalformedRuleInOwnershipNamespace(t *testing.T) {
	alloc := mustGatewayDNSFirewallAllocation(t, "172.16.0.0/16", 1)
	valid := gatewayDNSFirewallRuleTokens(alloc, "udp")
	tests := []struct {
		name   string
		mutate func([]string) []string
	}{
		{
			name: "wrong target",
			mutate: func(rule []string) []string {
				rule[len(rule)-1] = "DROP"
				return rule
			},
		},
		{
			name: "duplicate source",
			mutate: func(rule []string) []string {
				return append(rule, "-s", alloc.GuestIP+"/32")
			},
		},
		{
			name: "unknown selector",
			mutate: func(rule []string) []string {
				return append(rule, "--sport", "53")
			},
		},
		{
			name: "negated selector",
			mutate: func(rule []string) []string {
				return append(rule, "!", "-s", alloc.GuestIP+"/32")
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			runner := newFakeGatewayDNSFirewallRunner()
			runner.ensureChain(gatewayDNSFirewallChain)
			runner.appendFullRule(gatewayDNSFirewallChain, tc.mutate(append([]string(nil), valid...)))
			before := runner.snapshot()
			firewall := newGatewayDNSFirewallWithRunner("172.16.0.0/16", runner)
			err := firewall.Reconcile([]Allocation{alloc})
			if err == nil || !strings.Contains(err.Error(), "invalid owned rule") {
				t.Fatalf("Reconcile error = %v, want invalid owned rule", err)
			}
			if after := runner.snapshot(); after != before {
				t.Fatalf("firewall mutated malformed owned rule:\nbefore=%s\nafter=%s", before, after)
			}
		})
	}
}

func TestGatewayDNSFirewallRejectsNarrowOrMalformedGuardInOwnershipNamespace(t *testing.T) {
	alloc := mustGatewayDNSFirewallAllocation(t, "172.16.0.0/16", 1)
	valid := gatewayDNSFirewallGuardRuleTokens(alloc, "udp")
	tests := []struct {
		name   string
		mutate func([]string) []string
	}{
		{
			name: "source selector",
			mutate: func(rule []string) []string {
				return append(rule, "-s", alloc.GuestIP+"/32")
			},
		},
		{
			name: "destination selector",
			mutate: func(rule []string) []string {
				return append(rule, "-d", alloc.GatewayIP+"/32")
			},
		},
		{
			name: "accept target",
			mutate: func(rule []string) []string {
				rule[len(rule)-1] = "ACCEPT"
				return rule
			},
		},
		{
			name: "missing input interface",
			mutate: func(rule []string) []string {
				return append(rule[:2], rule[4:]...)
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			runner := newFakeGatewayDNSFirewallRunner()
			runner.ensureChain(gatewayDNSFirewallChain)
			runner.appendFullRule(gatewayDNSFirewallChain, tc.mutate(append([]string(nil), valid...)))
			before := runner.snapshot()
			firewall := newGatewayDNSFirewallWithRunner("172.16.0.0/16", runner)
			err := firewall.Reconcile([]Allocation{alloc})
			if err == nil || !strings.Contains(err.Error(), "invalid owned rule") {
				t.Fatalf("Reconcile error = %v, want invalid owned guard", err)
			}
			if after := runner.snapshot(); after != before {
				t.Fatalf("malformed owned guard was mutated:\nbefore=%s\nafter=%s", before, after)
			}
		})
	}
}

func TestGatewayDNSFirewallAdoptsOnlyEmptyChainItCreatedDuringPartialFailure(t *testing.T) {
	runner := newFakeGatewayDNSFirewallRunner()
	firewall := newGatewayDNSFirewallWithRunner("172.16.0.0/16", runner)
	alloc := mustGatewayDNSFirewallAllocation(t, "172.16.0.0/16", 1)
	if err := firewall.Activate(alloc); err != nil {
		t.Fatalf("Activate: %v", err)
	}

	// Model a same-process partial cleanup: all owned evidence disappeared but
	// deletion of the now-empty chain did not complete. The controller knows it
	// created this chain and can safely heal it.
	runner.setRules("INPUT", nil)
	runner.setRules(gatewayDNSFirewallChain, nil)
	if err := firewall.Reconcile([]Allocation{alloc}); err != nil {
		t.Fatalf("Reconcile known empty chain: %v", err)
	}
	assertFakeGatewayDNSFirewallHook(t, firewall, runner, true)
	assertFakeGatewayDNSFirewallRules(t, firewall, runner, alloc)

	// Unit-test constructors intentionally share an injected owner identity. A
	// production fresh controller instead reloads the same durable random owner.
	runner.setRules("INPUT", nil)
	runner.setRules(firewall.identity.Chain, nil)
	fresh := newGatewayDNSFirewall("172.16.0.0/16", runner, "", firewall.identity, nil)
	if err := fresh.Reconcile([]Allocation{alloc}); err != nil {
		t.Fatalf("fresh Reconcile known private empty chain: %v", err)
	}
	assertFakeGatewayDNSFirewallRules(t, fresh, runner, alloc)
}

func TestGatewayDNSFirewallDurableEvidenceRecoversCrashWindow(t *testing.T) {
	for _, crashPoint := range []string{"before chain creation", "after empty chain creation"} {
		t.Run(crashPoint, func(t *testing.T) {
			runner := newFakeGatewayDNSFirewallRunner()
			ownershipPath := filepath.Join(t.TempDir(), "network", gatewayDNSFirewallOwnerFile)
			first := newGatewayDNSFirewallWithRunnerAndOwnershipPath("172.16.0.0/16", runner, ownershipPath)
			first.mu.Lock()
			if err := first.ensureOwnershipEvidenceLocked(); err != nil {
				first.mu.Unlock()
				t.Fatalf("ensure ownership evidence: %v", err)
			}
			first.mu.Unlock()
			if len(first.identity.Chain) > 28 || !strings.HasPrefix(first.identity.Chain, gatewayDNSFirewallChainPrefix) {
				t.Fatalf("derived chain %q violates Linux/private-name contract", first.identity.Chain)
			}
			if crashPoint == "after empty chain creation" {
				runner.ensureChain(first.identity.Chain)
			}

			// Model a fresh controller after the first process disappeared between
			// the durable marker and the first owned rule.
			fresh := newGatewayDNSFirewallWithRunnerAndOwnershipPath("172.16.0.0/16", runner, ownershipPath)
			alloc := mustGatewayDNSFirewallAllocation(t, "172.16.0.0/16", 1)
			if err := fresh.Reconcile([]Allocation{alloc}); err != nil {
				t.Fatalf("fresh Reconcile: %v", err)
			}
			if fresh.identity != first.identity {
				t.Fatalf("fresh identity = %+v, want durable %+v", fresh.identity, first.identity)
			}
			assertFakeGatewayDNSFirewallRules(t, fresh, runner, alloc)
			assertFakeGatewayDNSFirewallHook(t, fresh, runner, true)

			if err := fresh.Reconcile(nil); err != nil {
				t.Fatalf("cleanup Reconcile: %v", err)
			}
			if _, err := os.Stat(ownershipPath); err != nil {
				t.Fatalf("durable ownership identity disappeared after chain cleanup: %v", err)
			}
		})
	}
}

func TestGatewayDNSFirewallFreshControllerPreservesGuardOnlyState(t *testing.T) {
	runner := newFakeGatewayDNSFirewallRunner()
	ownershipPath := filepath.Join(t.TempDir(), "network", gatewayDNSFirewallOwnerFile)
	first := newGatewayDNSFirewallWithRunnerAndOwnershipPath("172.16.0.0/16", runner, ownershipPath)
	alloc := mustGatewayDNSFirewallAllocation(t, "172.16.0.0/16", 1)
	if err := first.Guard(alloc); err != nil {
		t.Fatalf("Guard: %v", err)
	}
	if verdict := fakeGatewayDNSFirewallVerdict(first, runner, alloc, "udp"); verdict != "DROP" {
		t.Fatalf("first guard verdict = %s", verdict)
	}

	fresh := newGatewayDNSFirewallWithRunnerAndOwnershipPath("172.16.0.0/16", runner, ownershipPath)
	if fresh.identity != first.identity {
		t.Fatalf("fresh identity = %+v, want %+v", fresh.identity, first.identity)
	}
	if err := fresh.ReconcileState([]Allocation{alloc}, nil); err != nil {
		t.Fatalf("fresh guard-only ReconcileState: %v", err)
	}
	if verdict := fakeGatewayDNSFirewallVerdict(fresh, runner, alloc, "udp"); verdict != "DROP" {
		t.Fatalf("fresh guard verdict = %s, want DROP", verdict)
	}
	if status := fresh.Status(alloc); !status.GuardReady || status.AdmissionReady || status.UDPInstalled || status.TCPInstalled {
		t.Fatalf("fresh guard status = %+v", status)
	}
}

func TestGatewayDNSFirewallRejectsPrivateChainCollisionForDurableOwner(t *testing.T) {
	runner := newFakeGatewayDNSFirewallRunner()
	ownershipPath := filepath.Join(t.TempDir(), "network", gatewayDNSFirewallOwnerFile)
	first := newGatewayDNSFirewallWithRunnerAndOwnershipPath("172.16.0.0/16", runner, ownershipPath)
	runner.ensureChain(first.identity.Chain)
	runner.appendRule(first.identity.Chain, []string{"-s", "192.0.2.1/32", "-j", "DROP"})
	before := runner.snapshot()

	fresh := newGatewayDNSFirewallWithRunnerAndOwnershipPath("172.16.0.0/16", runner, ownershipPath)
	alloc := mustGatewayDNSFirewallAllocation(t, "172.16.0.0/16", 1)
	err := fresh.Guard(alloc)
	if err == nil || !errors.Is(err, errGatewayDNSFirewallOwnershipCollision) {
		t.Fatalf("Guard collision error = %v", err)
	}
	if after := runner.snapshot(); after != before {
		t.Fatalf("private-chain collision was mutated:\nbefore=%s\nafter=%s", before, after)
	}
	if status := fresh.Status(alloc); !status.OwnershipCollision || status.LastError == "" {
		t.Fatalf("collision status = %+v", status)
	}
}

func TestGatewayDNSFirewallRejectsChangedDurableOwnerIdentityWithoutCommands(t *testing.T) {
	runner := newFakeGatewayDNSFirewallRunner()
	ownershipPath := filepath.Join(t.TempDir(), "network", gatewayDNSFirewallOwnerFile)
	firewall := newGatewayDNSFirewallWithRunnerAndOwnershipPath("172.16.0.0/16", runner, ownershipPath)
	replacement, err := newGatewayDNSFirewallIdentity("ffeeddccbbaa99887766554433221100")
	if err != nil {
		t.Fatalf("replacement identity: %v", err)
	}
	if err := os.WriteFile(ownershipPath, []byte(gatewayDNSFirewallIdentityContents(replacement)), 0640); err != nil {
		t.Fatalf("replace identity: %v", err)
	}
	alloc := mustGatewayDNSFirewallAllocation(t, "172.16.0.0/16", 1)
	err = firewall.Guard(alloc)
	if err == nil || !errors.Is(err, errGatewayDNSFirewallOwnershipCollision) || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("Guard changed identity error = %v", err)
	}
	if commands := runner.commandsSnapshot(); len(commands) != 0 {
		t.Fatalf("changed identity ran iptables commands: %#v", commands)
	}
}

func TestGatewayDNSFirewallRejectsInvalidDurableOwnershipEvidence(t *testing.T) {
	ownershipPath := filepath.Join(t.TempDir(), "network", gatewayDNSFirewallOwnerFile)
	if err := os.MkdirAll(filepath.Dir(ownershipPath), 0750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(ownershipPath, []byte("foreign\n"), 0640); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	runner := newFakeGatewayDNSFirewallRunner()
	before := runner.snapshot()
	firewall := newGatewayDNSFirewallWithRunnerAndOwnershipPath("172.16.0.0/16", runner, ownershipPath)
	alloc := mustGatewayDNSFirewallAllocation(t, "172.16.0.0/16", 1)
	err := firewall.Reconcile([]Allocation{alloc})
	if err == nil || !strings.Contains(err.Error(), "unexpected contents") || !errors.Is(err, errGatewayDNSFirewallOwnershipCollision) {
		t.Fatalf("Reconcile error = %v, want ownership evidence collision", err)
	}
	if after := runner.snapshot(); after != before {
		t.Fatalf("invalid ownership evidence allowed firewall mutation:\nbefore=%s\nafter=%s", before, after)
	}
}

func TestGatewayDNSFirewallRejectsInvalidAllocationWithoutCommands(t *testing.T) {
	valid := mustGatewayDNSFirewallAllocation(t, "172.16.0.0/16", 1)
	tests := []struct {
		name  string
		alloc Allocation
	}{
		{name: "non kiln-vm TAP", alloc: Allocation{TAPDevice: "eth0", GuestIP: valid.GuestIP, GatewayIP: valid.GatewayIP}},
		{name: "non canonical kiln-vm TAP", alloc: Allocation{TAPDevice: "kv01", GuestIP: valid.GuestIP, GatewayIP: valid.GatewayIP}},
		{name: "zero kiln-vm TAP", alloc: Allocation{TAPDevice: "kv0", GuestIP: valid.GuestIP, GatewayIP: valid.GatewayIP}},
		{name: "swapped members", alloc: Allocation{TAPDevice: valid.TAPDevice, GuestIP: valid.GatewayIP, GatewayIP: valid.GuestIP}},
		{name: "wrong guest", alloc: Allocation{TAPDevice: valid.TAPDevice, GuestIP: "172.16.0.10", GatewayIP: valid.GatewayIP}},
		{name: "CIDR suffix", alloc: Allocation{TAPDevice: valid.TAPDevice, GuestIP: valid.GuestIP + "/32", GatewayIP: valid.GatewayIP}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			runner := newFakeGatewayDNSFirewallRunner()
			firewall := newGatewayDNSFirewallWithRunner("172.16.0.0/16", runner)
			if err := firewall.Activate(tc.alloc); err == nil {
				t.Fatal("Activate succeeded for invalid allocation")
			}
			if commands := runner.commandsSnapshot(); len(commands) != 0 {
				t.Fatalf("invalid allocation ran commands: %#v", commands)
			}
		})
	}

	runner := newFakeGatewayDNSFirewallRunner()
	firewall := newGatewayDNSFirewallWithRunner("172.16.0.0/16", runner)
	if err := firewall.Reconcile([]Allocation{valid, valid}); err == nil || !strings.Contains(err.Error(), "duplicate guarded allocation") {
		t.Fatalf("duplicate snapshot error = %v", err)
	}
	if commands := runner.commandsSnapshot(); len(commands) != 0 {
		t.Fatalf("duplicate snapshot ran commands: %#v", commands)
	}
}

func TestGatewayDNSFirewallActivateRollsBackPartialHostState(t *testing.T) {
	runner := newFakeGatewayDNSFirewallRunner()
	runner.failOnce(func(args []string) bool {
		return fakeGatewayDNSFirewallOperation(args) == "-I"
	}, errors.New("injected hook failure"))
	firewall := newGatewayDNSFirewallWithRunner("172.16.0.0/16", runner)
	alloc := mustGatewayDNSFirewallAllocation(t, "172.16.0.0/16", 1)

	err := firewall.Activate(alloc)
	if err == nil || !strings.Contains(err.Error(), "injected hook failure") {
		t.Fatalf("Activate error = %v, want injected failure", err)
	}
	if !runner.hasChain(firewall.identity.Chain) {
		t.Fatalf("fail-closed guard chain disappeared after failed admission: %s", runner.snapshot())
	}
	assertFakeGatewayDNSFirewallHook(t, firewall, runner, true)
	status := firewall.Status(alloc)
	if status.Desired || status.AdmissionDesired || status.Ready || !status.GuardDesired || !status.GuardReady {
		t.Fatalf("Status after rolled-back activation = %+v", status)
	}
	if verdict := fakeGatewayDNSFirewallVerdict(firewall, runner, alloc, "udp"); verdict != "DROP" {
		t.Fatalf("verdict after failed admission = %s, want DROP", verdict)
	}
}

func TestGatewayDNSFirewallReleaseRetainsRemovalIntentForRetry(t *testing.T) {
	runner := newFakeGatewayDNSFirewallRunner()
	firewall := newGatewayDNSFirewallWithRunner("172.16.0.0/16", runner)
	alloc := mustGatewayDNSFirewallAllocation(t, "172.16.0.0/16", 1)
	if err := firewall.Activate(alloc); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	runner.failOnce(func(args []string) bool {
		return fakeGatewayDNSFirewallOperation(args) == "-D"
	}, errors.New("injected delete failure"))

	if err := firewall.Release(alloc); err == nil || !strings.Contains(err.Error(), "injected delete failure") {
		t.Fatalf("Release error = %v, want injected failure", err)
	}
	status := firewall.Status(alloc)
	if status.Desired || status.Ready || status.LastError == "" {
		t.Fatalf("Status after failed release = %+v", status)
	}
	if err := firewall.Reconcile(nil); err != nil {
		t.Fatalf("retry Reconcile: %v", err)
	}
	if runner.hasChain(firewall.identity.Chain) {
		t.Fatalf("owned chain survived retry: %s", runner.snapshot())
	}
}

func TestGatewayDNSFirewallReleaseAcceptsVerifiedHistoricalAllocation(t *testing.T) {
	runner := newFakeGatewayDNSFirewallRunner()
	firewall := newGatewayDNSFirewallWithRunner("10.77.0.0/16", runner)
	alloc := mustGatewayDNSFirewallAllocation(t, "172.16.0.0/16", 41)
	if err := firewall.Release(alloc); err != nil {
		t.Fatalf("Release historical allocation: %v", err)
	}
	if commands := runner.commandsSnapshot(); len(commands) != 2 || fakeGatewayDNSFirewallOperation(commands[0].args) != "-S" || fakeGatewayDNSFirewallOperation(commands[1].args) != "-S" {
		t.Fatalf("historical release commands = %#v, want observation without mutation", commands)
	}
}

func TestGatewayDNSFirewallDeactivateAndGuardSnapshotAcceptVerifiedHistoricalAllocation(t *testing.T) {
	runner := newFakeGatewayDNSFirewallRunner()
	firewall := newGatewayDNSFirewallWithRunner("172.16.0.0/16", runner)
	alloc := mustGatewayDNSFirewallAllocation(t, "172.16.0.0/16", 41)
	alloc.OwnerID = "historical-machine"

	if err := firewall.Activate(alloc); err != nil {
		t.Fatalf("Activate historical allocation before configuration change: %v", err)
	}
	firewall.baseCIDR = "10.77.0.0/16"
	if err := firewall.Deactivate(alloc); err != nil {
		t.Fatalf("Deactivate historical allocation after configuration change: %v", err)
	}
	for _, protocol := range []string{"udp", "tcp"} {
		if verdict := fakeGatewayDNSFirewallVerdict(firewall, runner, alloc, protocol); verdict != "DROP" {
			t.Fatalf("historical deactivated %s verdict = %s, want DROP", protocol, verdict)
		}
	}

	if err := firewall.ReconcileState([]Allocation{alloc}, nil); err != nil {
		t.Fatalf("ReconcileState historical guarded allocation: %v", err)
	}
	if err := firewall.ReconcileState([]Allocation{alloc}, []Allocation{alloc}); err == nil || !strings.Contains(err.Error(), "exact current kiln-vm tuple") {
		t.Fatalf("ReconcileState admitted historical allocation error = %v, want current-CIDR rejection", err)
	}
}

func TestGatewayDNSFirewallAcceptsDurableOwnerWithoutWeakeningNetworkTuple(t *testing.T) {
	runner := newFakeGatewayDNSFirewallRunner()
	firewall := newGatewayDNSFirewallWithRunner("172.16.0.0/16", runner)
	alloc := mustGatewayDNSFirewallAllocation(t, "172.16.0.0/16", 1)
	alloc.OwnerID = "machine-uuid"

	if err := firewall.Activate(alloc); err != nil {
		t.Fatalf("Activate allocation with durable owner: %v", err)
	}
	if status := firewall.Status(alloc); !status.Ready {
		t.Fatalf("Status(owner allocation) = %+v, want ready", status)
	}

	wrongOwner := alloc
	wrongOwner.OwnerID = "different-machine"
	if status := firewall.Status(wrongOwner); status.Desired || status.Ready {
		t.Fatalf("Status(substituted owner) = %+v, want identity mismatch", status)
	}
	wrongNetwork := alloc
	wrongNetwork.GuestIP = "172.16.0.10"
	if err := firewall.Guard(wrongNetwork); err == nil || !strings.Contains(err.Error(), "exact current kiln-vm tuple") {
		t.Fatalf("Guard(wrong network tuple) error = %v, want exact-tuple rejection", err)
	}
}

func TestGatewayDNSFirewallConcurrentSnapshotOperationsRemainConsistent(t *testing.T) {
	runner := newFakeGatewayDNSFirewallRunner()
	firewall := newGatewayDNSFirewallWithRunner("172.16.0.0/16", runner)
	first := mustGatewayDNSFirewallAllocation(t, "172.16.0.0/16", 1)
	second := mustGatewayDNSFirewallAllocation(t, "172.16.0.0/16", 2)

	var wg sync.WaitGroup
	for worker := 0; worker < 12; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for iteration := 0; iteration < 20; iteration++ {
				switch (worker + iteration) % 3 {
				case 0:
					_ = firewall.Activate(first)
				case 1:
					_ = firewall.Release(first)
				case 2:
					_ = firewall.Reconcile([]Allocation{second})
				}
			}
		}(worker)
	}
	wg.Wait()

	if err := firewall.Reconcile([]Allocation{first, second}); err != nil {
		t.Fatalf("final Reconcile: %v", err)
	}
	assertFakeGatewayDNSFirewallHook(t, firewall, runner, true)
	assertFakeGatewayDNSFirewallRules(t, firewall, runner, first, second)
	for _, alloc := range []Allocation{first, second} {
		if status := firewall.Status(alloc); !status.Ready {
			t.Fatalf("Status(%s) = %+v, want ready", alloc.TAPDevice, status)
		}
	}
}

func TestSplitGatewayDNSFirewallIptablesLine(t *testing.T) {
	line := `-A INPUT -m comment --comment "kiln-vm gateway dns" -j KILN_VM_GUEST_DNS`
	got, err := splitGatewayDNSFirewallIptablesLine(line)
	if err != nil {
		t.Fatalf("split: %v", err)
	}
	want := []string{"-A", "INPUT", "-m", "comment", "--comment", "kiln-vm gateway dns", "-j", "KILN_VM_GUEST_DNS"}
	if !equalGatewayDNSFirewallTokens(got, want) {
		t.Fatalf("split = %#v, want %#v", got, want)
	}
	for _, line := range []string{`-A INPUT "unterminated`, `-A INPUT trailing\`} {
		if _, err := splitGatewayDNSFirewallIptablesLine(line); err == nil {
			t.Fatalf("split %q succeeded", line)
		}
	}
}

func TestCanonicalGatewayDNSFirewallRuleIgnoresIptablesImplicitProtocolModule(t *testing.T) {
	alloc := mustGatewayDNSFirewallAllocation(t, "172.16.0.0/16", 1)
	approved := gatewayDNSFirewallRuleTokens(alloc, "udp")
	if strings.Contains(strings.Join(approved, " "), "-m udp") {
		t.Fatalf("approved mutation argv contains explicit protocol match: %#v", approved)
	}
	rendered := append([]string(nil), approved...)
	protocolIndex := -1
	for index, token := range rendered {
		if token == "-p" {
			protocolIndex = index
			break
		}
	}
	if protocolIndex < 0 {
		t.Fatalf("rule has no protocol: %#v", rendered)
	}
	rendered = append(rendered[:protocolIndex+2], append([]string{"-m", "udp"}, rendered[protocolIndex+2:]...)...)
	if canonicalGatewayDNSFirewallRule(approved) != canonicalGatewayDNSFirewallRule(rendered) {
		t.Fatalf("implicit module rendering changed canonical rule:\napproved=%#v\nrendered=%#v", approved, rendered)
	}
}

func mustGatewayDNSFirewallAllocation(t *testing.T, baseCIDR string, index uint32) Allocation {
	t.Helper()
	alloc, err := allocationForIndex(baseCIDR, index)
	if err != nil {
		t.Fatalf("allocationForIndex(%d): %v", index, err)
	}
	return *alloc
}

func realRenderedGatewayDNSFirewallRule(firewall *GatewayDNSFirewall, alloc Allocation, protocol, verdict string) []string {
	rendered := firewall.ruleTokens(alloc, protocol, verdict)
	for index := 0; index+1 < len(rendered); index++ {
		if rendered[index] != "-p" {
			continue
		}
		withModule := make([]string, 0, len(rendered)+2)
		withModule = append(withModule, rendered[:index+2]...)
		withModule = append(withModule, "-m", protocol)
		withModule = append(withModule, rendered[index+2:]...)
		return withModule
	}
	return rendered
}

func assertFakeGatewayDNSFirewallHook(t *testing.T, firewall *GatewayDNSFirewall, runner *fakeGatewayDNSFirewallRunner, want bool) {
	t.Helper()
	input := runner.rules("INPUT")
	count := 0
	position := 0
	for index, rule := range input {
		if firewall.equalTokens(rule, firewall.hookRuleTokens()) {
			count++
			position = index + 1
		}
	}
	if !want && count != 0 {
		t.Fatalf("owned INPUT hook count = %d, want zero: %#v", count, input)
	}
	if want && (count != 1 || position != 1) {
		t.Fatalf("owned INPUT hook count/position = %d/%d, want 1/1: %#v", count, position, input)
	}
}

func assertFakeGatewayDNSFirewallRules(t *testing.T, firewall *GatewayDNSFirewall, runner *fakeGatewayDNSFirewallRunner, allocations ...Allocation) {
	t.Helper()
	want := make([][]string, 0, len(allocations)*4)
	for _, alloc := range allocations {
		want = append(want,
			firewall.ruleTokens(alloc, "udp", gatewayDNSFirewallVerdictAdmit),
			firewall.ruleTokens(alloc, "tcp", gatewayDNSFirewallVerdictAdmit),
		)
	}
	for _, alloc := range allocations {
		want = append(want,
			firewall.ruleTokens(alloc, "udp", gatewayDNSFirewallVerdictGuard),
			firewall.ruleTokens(alloc, "tcp", gatewayDNSFirewallVerdictGuard),
		)
	}
	got := runner.rules(firewall.identity.Chain)
	if !equalFakeGatewayDNSFirewallRuleSetsFor(firewall, got, want) {
		t.Fatalf("owned chain rules:\ngot  %#v\nwant %#v", got, want)
	}
	for index, rule := range want {
		if !firewall.equalTokens(got[index], rule) {
			t.Fatalf("owned chain rule %d is out of fail-closed order:\ngot  %#v\nwant %#v", index+1, got, want)
		}
	}
}

func equalFakeGatewayDNSFirewallRuleSets(left, right [][]string) bool {
	return equalFakeGatewayDNSFirewallRuleSetsFor(gatewayDNSFirewallTestController(), left, right)
}

func equalFakeGatewayDNSFirewallRuleSetsFor(firewall *GatewayDNSFirewall, left, right [][]string) bool {
	if len(left) != len(right) {
		return false
	}
	canonical := func(rules [][]string) []string {
		result := make([]string, 0, len(rules))
		for _, rule := range rules {
			result = append(result, firewall.canonicalRule(rule))
		}
		sort.Strings(result)
		return result
	}
	leftCanonical := canonical(left)
	rightCanonical := canonical(right)
	for index := range leftCanonical {
		if leftCanonical[index] != rightCanonical[index] {
			return false
		}
	}
	return true
}

type fakeGatewayDNSFirewallCommand struct {
	name        string
	args        []string
	hadDeadline bool
}

type fakeGatewayDNSFirewallFailure struct {
	match func([]string) bool
	err   error
}

type fakeGatewayDNSFirewallRunner struct {
	mu          sync.Mutex
	chains      map[string][][]string
	inputPolicy string
	commands    []fakeGatewayDNSFirewallCommand
	mutations   int
	failures    []fakeGatewayDNSFirewallFailure
}

func newFakeGatewayDNSFirewallRunner() *fakeGatewayDNSFirewallRunner {
	return &fakeGatewayDNSFirewallRunner{
		inputPolicy: "ACCEPT",
		chains: map[string][][]string{
			"INPUT": {},
		},
	}
}

func (r *fakeGatewayDNSFirewallRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, hadDeadline := ctx.Deadline()
	command := fakeGatewayDNSFirewallCommand{name: name, args: append([]string(nil), args...), hadDeadline: hadDeadline}
	r.commands = append(r.commands, command)
	if name != "iptables" || len(args) < 3 || args[0] != "-w" || args[1] != "5" {
		return nil, fmt.Errorf("unexpected command %s %#v", name, args)
	}
	for index, failure := range r.failures {
		if failure.match(args) {
			r.failures = append(r.failures[:index], r.failures[index+1:]...)
			return []byte(failure.err.Error()), failure.err
		}
	}

	operation := args[2]
	switch operation {
	case "-S":
		return []byte(r.renderLocked()), nil
	case "-N":
		if len(args) != 4 {
			return nil, fmt.Errorf("invalid -N argv %#v", args)
		}
		if _, exists := r.chains[args[3]]; exists {
			return nil, fmt.Errorf("chain %s exists", args[3])
		}
		r.chains[args[3]] = nil
	case "-X":
		if len(args) != 4 {
			return nil, fmt.Errorf("invalid -X argv %#v", args)
		}
		chain := args[3]
		if len(r.chains[chain]) != 0 {
			return nil, fmt.Errorf("chain %s is not empty", chain)
		}
		for source, rules := range r.chains {
			for _, rule := range rules {
				if target, ok := gatewayDNSFirewallRuleTarget(rule); ok && target == chain {
					return nil, fmt.Errorf("chain %s is referenced by %s", chain, source)
				}
			}
		}
		delete(r.chains, chain)
	case "-A":
		if len(args) < 5 {
			return nil, fmt.Errorf("invalid -A argv %#v", args)
		}
		chain := args[3]
		if _, exists := r.chains[chain]; !exists {
			return nil, fmt.Errorf("chain %s missing", chain)
		}
		r.chains[chain] = append(r.chains[chain], append([]string{"-A", chain}, args[4:]...))
	case "-I":
		if len(args) < 6 {
			return nil, fmt.Errorf("invalid -I argv %#v", args)
		}
		chain := args[3]
		position, err := strconv.Atoi(args[4])
		if err != nil || position < 1 || position > len(r.chains[chain])+1 {
			return nil, fmt.Errorf("invalid insertion position %q", args[4])
		}
		rule := append([]string{"-A", chain}, args[5:]...)
		index := position - 1
		r.chains[chain] = append(r.chains[chain], nil)
		copy(r.chains[chain][index+1:], r.chains[chain][index:])
		r.chains[chain][index] = rule
	case "-D":
		if len(args) != 5 {
			return nil, fmt.Errorf("invalid -D argv %#v", args)
		}
		chain := args[3]
		position, err := strconv.Atoi(args[4])
		if err != nil || position < 1 || position > len(r.chains[chain]) {
			return nil, fmt.Errorf("invalid deletion position %q in %s", args[4], chain)
		}
		index := position - 1
		r.chains[chain] = append(r.chains[chain][:index], r.chains[chain][index+1:]...)
	default:
		return nil, fmt.Errorf("unsupported operation %q", operation)
	}
	r.mutations++
	return nil, nil
}

func (r *fakeGatewayDNSFirewallRunner) ensureChain(chain string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.chains[chain]; !exists {
		r.chains[chain] = nil
	}
}

func (r *fakeGatewayDNSFirewallRunner) appendRule(chain string, match []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.chains[chain]; !exists {
		r.chains[chain] = nil
	}
	r.chains[chain] = append(r.chains[chain], append([]string{"-A", chain}, match...))
}

func (r *fakeGatewayDNSFirewallRunner) appendFullRule(chain string, rule []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.chains[chain] = append(r.chains[chain], append([]string(nil), rule...))
}

func (r *fakeGatewayDNSFirewallRunner) setRules(chain string, rules [][]string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.chains[chain] = cloneFakeGatewayDNSFirewallRules(rules)
}

func (r *fakeGatewayDNSFirewallRunner) rules(chain string) [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return cloneFakeGatewayDNSFirewallRules(r.chains[chain])
}

func (r *fakeGatewayDNSFirewallRunner) hasChain(chain string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, exists := r.chains[chain]
	return exists
}

func (r *fakeGatewayDNSFirewallRunner) commandsSnapshot() []fakeGatewayDNSFirewallCommand {
	r.mu.Lock()
	defer r.mu.Unlock()
	commands := make([]fakeGatewayDNSFirewallCommand, len(r.commands))
	for index, command := range r.commands {
		commands[index] = command
		commands[index].args = append([]string(nil), command.args...)
	}
	return commands
}

func (r *fakeGatewayDNSFirewallRunner) clearCommands() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.commands = nil
}

func (r *fakeGatewayDNSFirewallRunner) mutationCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.mutations
}

func (r *fakeGatewayDNSFirewallRunner) failOnce(match func([]string) bool, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failures = append(r.failures, fakeGatewayDNSFirewallFailure{match: match, err: err})
}

func (r *fakeGatewayDNSFirewallRunner) snapshot() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.renderLocked()
}

func (r *fakeGatewayDNSFirewallRunner) renderLocked() string {
	var lines []string
	lines = append(lines, "-P INPUT "+r.inputPolicy)
	var custom []string
	for chain := range r.chains {
		if chain != "INPUT" {
			custom = append(custom, chain)
		}
	}
	sort.Strings(custom)
	for _, chain := range custom {
		lines = append(lines, "-N "+chain)
	}
	orderedChains := append([]string{"INPUT"}, custom...)
	for _, chain := range orderedChains {
		for _, rule := range r.chains[chain] {
			lines = append(lines, joinFakeGatewayDNSFirewallTokens(rule))
		}
	}
	return strings.Join(lines, "\n") + "\n"
}

// fakeGatewayDNSFirewallVerdict evaluates the exact guest->own-gateway DNS flow
// after the kernel has routed it through INPUT. It keeps the fail-closed tests
// honest on the common INPUT ACCEPT host configuration.
func fakeGatewayDNSFirewallVerdict(firewall *GatewayDNSFirewall, runner *fakeGatewayDNSFirewallRunner, alloc Allocation, protocol string) string {
	return fakeGatewayDNSFirewallInputVerdict(firewall, runner, alloc.TAPDevice, alloc.GuestIP, alloc.GatewayIP, protocol)
}

func fakeGatewayDNSFirewallInputVerdict(firewall *GatewayDNSFirewall, runner *fakeGatewayDNSFirewallRunner, tap, source, destination, protocol string) string {
	runner.mu.Lock()
	defer runner.mu.Unlock()
	hooked := false
	for _, rule := range runner.chains["INPUT"] {
		if firewall.equalTokens(rule, firewall.hookRuleTokens()) {
			hooked = true
			break
		}
	}
	if hooked {
		for _, rule := range runner.chains[firewall.identity.Chain] {
			key, err := firewall.parseAllocationRule(rule)
			if err != nil || key.inputInterface != tap || key.protocol != protocol {
				continue
			}
			if key.source != "" && key.source != source+"/32" {
				continue
			}
			if key.destination != "" && key.destination != destination+"/32" {
				continue
			}
			return key.target
		}
	}
	return runner.inputPolicy
}

func joinFakeGatewayDNSFirewallTokens(tokens []string) string {
	quoted := make([]string, len(tokens))
	for index, token := range tokens {
		if strings.ContainsAny(token, " \t") {
			quoted[index] = strconv.Quote(token)
		} else {
			quoted[index] = token
		}
	}
	return strings.Join(quoted, " ")
}

func cloneFakeGatewayDNSFirewallRules(rules [][]string) [][]string {
	clone := make([][]string, len(rules))
	for index, rule := range rules {
		clone[index] = append([]string(nil), rule...)
	}
	return clone
}

func fakeGatewayDNSFirewallOperation(args []string) string {
	if len(args) < 3 {
		return ""
	}
	return args[2]
}

const (
	gatewayDNSFirewallTestOwnerID = "00112233445566778899aabbccddeeff"
	gatewayDNSFirewallChain       = "KVD_00112233445566778899aabb"
)

func newGatewayDNSFirewallWithRunner(baseCIDR string, runner gatewayDNSFirewallCommandRunner) *GatewayDNSFirewall {
	identity, err := newGatewayDNSFirewallIdentity(gatewayDNSFirewallTestOwnerID)
	return newGatewayDNSFirewall(baseCIDR, runner, "", identity, err)
}

func gatewayDNSFirewallTestController() *GatewayDNSFirewall {
	identity, _ := newGatewayDNSFirewallIdentity(gatewayDNSFirewallTestOwnerID)
	return newGatewayDNSFirewall("", nil, "", identity, nil)
}

func gatewayDNSFirewallRuleTokens(alloc Allocation, protocol string) []string {
	return gatewayDNSFirewallTestController().ruleTokens(alloc, protocol, gatewayDNSFirewallVerdictAdmit)
}

func gatewayDNSFirewallGuardRuleTokens(alloc Allocation, protocol string) []string {
	return gatewayDNSFirewallTestController().ruleTokens(alloc, protocol, gatewayDNSFirewallVerdictGuard)
}

func canonicalGatewayDNSFirewallRule(tokens []string) string {
	return gatewayDNSFirewallTestController().canonicalRule(tokens)
}

func equalGatewayDNSFirewallTokens(left, right []string) bool {
	return gatewayDNSFirewallTestController().equalTokens(left, right)
}
