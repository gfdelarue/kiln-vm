//go:build system

package systemtest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNetworkGatewayDNSFirewallHelperParsesDynamicV2OwnershipAndOrdering(t *testing.T) {
	identity := mustGatewayDNSFirewallIdentity(t, "00112233445566778899aabbccddeeff")
	output := strings.Join([]string{
		"-P INPUT ACCEPT",
		"-P FORWARD DROP",
		"-N " + identity.Chain,
		`-A INPUT -m comment --comment "` + identity.commentBase() + `:input" -j ` + identity.Chain,
		`-A INPUT -s 192.0.2.123/32 -m comment --comment unrelated-sentinel -j DROP`,
		gatewayDNSFirewallFixtureRule(identity, "kv7", "172.16.0.26", "172.16.0.25", "udp", gatewayDNSFirewallAdmit),
		gatewayDNSFirewallFixtureRule(identity, "kv7", "172.16.0.26", "172.16.0.25", "tcp", gatewayDNSFirewallAdmit),
		gatewayDNSFirewallFixtureRule(identity, "kv7", "172.16.0.26", "172.16.0.25", "udp", gatewayDNSFirewallGuard),
		gatewayDNSFirewallFixtureRule(identity, "kv7", "172.16.0.26", "172.16.0.25", "tcp", gatewayDNSFirewallGuard),
	}, "\n") + "\n"

	state, err := parseGatewayDNSFirewallState(output, identity)
	if err != nil {
		t.Fatalf("parse dynamic v2 state: %v", err)
	}
	if state.InputPolicy != "ACCEPT" || !state.ChainExists || len(state.HookPositions) != 1 || state.HookPositions[0] != 1 {
		t.Fatalf("state = %+v, want INPUT ACCEPT and one first-position dynamic hook", state)
	}
	for _, transport := range []string{"udp", "tcp"} {
		if got := state.ruleCount("kv7", transport, gatewayDNSFirewallAdmit); got != 1 {
			t.Fatalf("%s admission count = %d, want 1", transport, got)
		}
		if got := state.ruleCount("kv7", transport, gatewayDNSFirewallGuard); got != 1 {
			t.Fatalf("%s guard count = %d, want 1", transport, got)
		}
		if got := state.packetVerdict("kv7", "172.16.0.26", "172.16.0.25", transport); got != "ACCEPT" {
			t.Fatalf("admitted %s verdict = %q, want ACCEPT", transport, got)
		}
		if got := state.packetVerdict("kv7", "172.16.0.26", "172.16.0.29", transport); got != "DROP" {
			t.Fatalf("cross-allocation %s verdict = %q, want DROP", transport, got)
		}
		if got := state.packetVerdict("kv7", "198.51.100.77", "172.16.0.25", transport); got != "DROP" {
			t.Fatalf("spoofed-source %s verdict = %q, want DROP", transport, got)
		}
	}

	guardOnly := strings.ReplaceAll(output, gatewayDNSFirewallFixtureRule(identity, "kv7", "172.16.0.26", "172.16.0.25", "udp", gatewayDNSFirewallAdmit)+"\n", "")
	guardOnly = strings.ReplaceAll(guardOnly, gatewayDNSFirewallFixtureRule(identity, "kv7", "172.16.0.26", "172.16.0.25", "tcp", gatewayDNSFirewallAdmit)+"\n", "")
	state, err = parseGatewayDNSFirewallState(guardOnly, identity)
	if err != nil {
		t.Fatalf("parse guard-only state: %v", err)
	}
	for _, transport := range []string{"udp", "tcp"} {
		if got := state.packetVerdict("kv7", "172.16.0.26", "172.16.0.25", transport); got != "DROP" {
			t.Fatalf("guard-only %s verdict = %q, want DROP under INPUT ACCEPT", transport, got)
		}
	}
}

func TestNetworkGatewayDNSFirewallHelperParsesCustomCIDRAdmissionAndBroadGuard(t *testing.T) {
	identity := mustGatewayDNSFirewallIdentity(t, "00112233445566778899aabbccddeeff")
	output := strings.Join([]string{
		"-P INPUT ACCEPT",
		"-N " + identity.Chain,
		`-A INPUT -m comment --comment "` + identity.commentBase() + `:input" -j ` + identity.Chain,
		gatewayDNSFirewallFixtureRule(identity, "kv1", "10.77.0.6", "10.77.0.5", "udp", gatewayDNSFirewallAdmit),
		gatewayDNSFirewallFixtureRule(identity, "kv1", "10.77.0.6", "10.77.0.5", "udp", gatewayDNSFirewallGuard),
	}, "\n") + "\n"

	state, err := parseGatewayDNSFirewallState(output, identity)
	if err != nil {
		t.Fatalf("parse custom-CIDR state: %v", err)
	}
	if got := state.packetVerdict("kv1", "10.77.0.6", "10.77.0.5", "udp"); got != "ACCEPT" {
		t.Fatalf("custom-CIDR own gateway verdict = %q, want ACCEPT", got)
	}
	if got := state.packetVerdict("kv1", "10.77.0.6", "172.16.0.165", "udp"); got != "DROP" {
		t.Fatalf("custom-to-historical gateway verdict = %q, want DROP", got)
	}
	guard := state.ChainRules[1]
	if guard.Verdict != gatewayDNSFirewallGuard || guard.Source != "" || guard.Destination != "" {
		t.Fatalf("custom-CIDR guard = %+v, want source/destination-independent DROP", guard)
	}
}

func TestNetworkGatewayDNSFirewallHelperRejectsOwnerAndChainCollisions(t *testing.T) {
	identity := mustGatewayDNSFirewallIdentity(t, "00112233445566778899aabbccddeeff")
	otherOwner := "00112233445566778899aabbffeeddcc" // Same 96-bit chain prefix, different full owner.
	otherIdentity := mustGatewayDNSFirewallIdentity(t, otherOwner)
	if otherIdentity.Chain != identity.Chain {
		t.Fatalf("collision fixture chains differ: %s != %s", otherIdentity.Chain, identity.Chain)
	}

	tests := []struct {
		name string
		line string
		want string
	}{
		{
			name: "foreign rule in owner-derived chain",
			line: `-A ` + identity.Chain + ` -s 192.0.2.1/32 -j DROP`,
			want: "not exact",
		},
		{
			name: "foreign reference from FORWARD",
			line: `-A FORWARD -j ` + identity.Chain,
			want: "foreign reference",
		},
		{
			name: "same chain prefix with another full owner",
			line: gatewayDNSFirewallFixtureRule(otherIdentity, "kv7", "172.16.0.26", "172.16.0.25", "udp", gatewayDNSFirewallGuard),
			want: "not exact",
		},
		{
			name: "narrow guard with source and destination",
			line: `-A ` + identity.Chain + ` -i kv7 -s 172.16.0.26/32 -d 172.16.0.25/32 -p udp --dport 53 -m comment --comment "` +
				identity.commentBase() + `:alloc:kv7:udp:guard" -j DROP`,
			want: "not exact",
		},
		{
			name: "owner comment outside owner chain",
			line: `-A INPUT -m comment --comment "` + identity.commentBase() + `:alloc:kv7:udp:guard" -j DROP`,
			want: "outside",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			output := "-P INPUT ACCEPT\n-N " + identity.Chain + "\n" + test.line + "\n"
			if _, err := parseGatewayDNSFirewallState(output, identity); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("parse collision error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestNetworkGatewayDNSFirewallHelperValidatesDurableOwnerFile(t *testing.T) {
	ownerID := "00112233445566778899aabbccddeeff"
	identity := mustGatewayDNSFirewallIdentity(t, ownerID)
	if len(identity.Chain) != 28 || identity.Chain != gatewayDNSFirewallChainPrefix+ownerID[:gatewayDNSFirewallChainIDChars] {
		t.Fatalf("derived identity = %+v", identity)
	}

	for _, contents := range []string{
		"foreign\n",
		gatewayDNSFirewallIdentityVersion + "\nowner-id=" + strings.ToUpper(ownerID) + "\n",
		gatewayDNSFirewallIdentityVersion + "\nowner-id=0011\n",
		gatewayDNSFirewallIdentityVersion + "\nowner-id=" + ownerID,
		gatewayDNSFirewallIdentityVersion + "\nowner-id=" + ownerID + "\nextra\n",
	} {
		if _, err := parseGatewayDNSFirewallIdentity([]byte(contents)); err == nil {
			t.Fatalf("invalid owner contents parsed: %q", contents)
		}
	}

	dataDir := t.TempDir()
	path := gatewayDNSFirewallIdentityPath(dataDir)
	if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil {
		t.Fatalf("create identity directory: %v", err)
	}
	contents := gatewayDNSFirewallIdentityVersion + "\nowner-id=" + ownerID + "\n"
	if err := os.WriteFile(path, []byte(contents), 0640); err != nil {
		t.Fatalf("write identity: %v", err)
	}
	got, err := readGatewayDNSFirewallIdentity(dataDir)
	if err != nil || got != identity {
		t.Fatalf("read identity = (%+v, %v), want %+v", got, err, identity)
	}
	if err := os.Chmod(path, 0660); err != nil {
		t.Fatalf("make identity writable: %v", err)
	}
	if _, err := readGatewayDNSFirewallIdentity(dataDir); err == nil || !strings.Contains(err.Error(), "protected regular file") {
		t.Fatalf("writable owner identity error = %v", err)
	}
}

func mustGatewayDNSFirewallIdentity(t *testing.T, ownerID string) gatewayDNSFirewallIdentity {
	t.Helper()
	identity, err := parseGatewayDNSFirewallIdentity([]byte(gatewayDNSFirewallIdentityVersion + "\nowner-id=" + ownerID + "\n"))
	if err != nil {
		t.Fatalf("parse owner identity: %v", err)
	}
	return identity
}

func gatewayDNSFirewallFixtureRule(identity gatewayDNSFirewallIdentity, tap, source, destination, transport, verdict string) string {
	target := "ACCEPT"
	selectors := ""
	if verdict == gatewayDNSFirewallGuard {
		target = "DROP"
	} else {
		selectors = ` -s ` + source + `/32 -d ` + destination + `/32`
	}
	return `-A ` + identity.Chain +
		` -i ` + tap + selectors + ` -p ` + transport + ` -m ` + transport + ` --dport 53 -m comment --comment "` +
		identity.commentBase() + `:alloc:` + tap + `:` + transport + `:` + verdict + `" -j ` + target
}
