package machine

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/gfdelarue/kiln-vm/internal/models"

	"github.com/google/uuid"
)

// Guards are installed before the guest process starts. They are outside the
// guest and match its TAP, so guest root and Docker cannot remove or evade them.
// Host administrators remain trusted; a firewall flush is an administrative
// change to this boundary, not something a guest is allowed to perform.
func applyMachineIsolation(m *models.Machine) error {
	return changeMachineIsolation(m, false)
}

func releaseMachineIsolation(m *models.Machine) error {
	return changeMachineIsolation(m, true)
}

func changeMachineIsolation(m *models.Machine, remove bool) error {
	if m.NetworkPolicy == "" {
		return nil
	}
	if err := models.ValidateNetworkPolicy(m.NetworkPolicy, m.Backend); err != nil {
		return fmt.Errorf("check network policy: %w", err)
	}
	if _, err := uuid.Parse(m.ID); err != nil {
		return fmt.Errorf("invalid isolation owner: %w", err)
	}
	alloc, present, err := durablePublicAllocationFromMachine(m)
	if err != nil || !present {
		return fmt.Errorf("isolation requires a complete durable network identity: %v", err)
	}
	// Block IPv6 first. A partial application leaves the VM unstarted and keeps
	// the saved machine record available for explicit cleanup.
	for _, family := range []string{"ip6tables", "iptables"} {
		for _, table := range []string{"raw", "mangle", "filter"} {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			inventory, err := exec.CommandContext(ctx, family+"-save", "-t", table).CombinedOutput()
			if err != nil {
				cancel()
				return fmt.Errorf("inspect isolation firewall: %s: %w", inventory, err)
			}
			script, err := isolationTableTransaction(m.ID, *alloc, family == "ip6tables", string(inventory), remove, table)
			if err != nil {
				cancel()
				return err
			}
			if script != "" {
				cmd := exec.CommandContext(ctx, family+"-restore", "--wait", "5", "--noflush")
				cmd.Stdin = strings.NewReader(script)
				out, applyErr := cmd.CombinedOutput()
				if applyErr != nil {
					cancel()
					return fmt.Errorf("commit isolation firewall: %s: %w", out, applyErr)
				}
			}
			cancel()
		}
	}
	return nil
}

// The raw ingress guard runs before all filter-table chains, including Tailscale
// and Docker. Their ACCEPT rules cannot bypass its destination/source drops.
func isolationTableTransaction(owner string, alloc Allocation, ipv6 bool, inventory string, remove bool, table string) (string, error) {
	digest := sha256.Sum256([]byte(owner))
	chain := fmt.Sprintf("KVISO-%x", digest[:10])
	tag := "kiln-vm-isolation-v1:" + owner
	tap := alloc.TAPDevice
	hooks := []string{
		"INPUT -i " + tap,
		"FORWARD -i " + tap,
		"FORWARD -o " + tap,
	}
	if table == "raw" {
		hooks = []string{"PREROUTING -i " + tap}
	}
	wantedHooks := make(map[string]bool)
	for _, hook := range hooks {
		wantedHooks["-A "+hook+" -m comment --comment "+tag+" -j "+chain] = true
	}
	var deletions []string
	exists, ownedRules := false, 0
	for _, raw := range strings.Split(inventory, "\n") {
		line := strings.ReplaceAll(strings.TrimSpace(raw), `"`, "")
		if strings.HasPrefix(line, ":"+chain+" ") {
			exists = true
		}
		if strings.HasPrefix(line, "-A "+chain+" ") {
			if !strings.Contains(line, "--comment "+tag+" ") {
				return "", fmt.Errorf("isolation chain %s contains a foreign rule", chain)
			}
			ownedRules++
		} else if strings.HasSuffix(line, "-j "+chain) || strings.HasSuffix(line, "-g "+chain) {
			if !wantedHooks[line] {
				return "", fmt.Errorf("isolation chain %s has a foreign reference", chain)
			}
			deletions = append(deletions, strings.Replace(line, "-A ", "-D ", 1))
		}
	}
	if exists && ownedRules == 0 {
		return "", fmt.Errorf("refusing to adopt empty isolation chain %s", chain)
	}
	if len(deletions) > 64 {
		return "", fmt.Errorf("too many isolation hooks for %s", chain)
	}
	if remove && !exists {
		return "", nil
	}
	var out strings.Builder
	fmt.Fprintf(&out, "*%s\n", table)
	if !remove {
		fmt.Fprintf(&out, ":%s - [0:0]\n", chain)
	}
	for _, deletion := range deletions {
		fmt.Fprintln(&out, deletion)
	}
	if remove {
		fmt.Fprintf(&out, "-F %s\n-X %s\nCOMMIT\n", chain, chain)
		return out.String(), nil
	}
	rule := func(match, target string) {
		fmt.Fprintf(&out, "-A %s %s -m comment --comment %s -j %s\n", chain, match, tag, target)
	}
	if !ipv6 {
		in := "-i " + tap
		rule(in+" ! -s "+alloc.GuestIP+"/32", "DROP")
		for _, protocol := range []string{"udp", "tcp"} {
			rule(in+" -d "+alloc.GatewayIP+"/32 -p "+protocol+" --dport 53", "RETURN")
		}
		rule(in+" -m addrtype --dst-type LOCAL", "DROP")
		// Includes RFC1918, loopback, link-local/metadata, carrier NAT/tailnets,
		// benchmarking, multicast, and reserved destinations.
		for _, cidr := range []string{"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24", "192.168.0.0/16", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4"} {
			rule(in+" -d "+cidr, "DROP")
		}
		if table == "raw" {
			rule(in+" -p tcp -m multiport --dports 80,443", "RETURN")
		} else {
			rule(in+" -p tcp -m multiport --dports 80,443 -m conntrack --ctstate NEW,ESTABLISHED", "RETURN")
		}
		rule(in, "DROP")
		if table != "raw" {
			rule("-o "+tap+" -m conntrack --ctstate ESTABLISHED,RELATED", "RETURN")
		}
	}
	rule("", "DROP")
	for _, hook := range hooks {
		parts := strings.SplitN(hook, " ", 2)
		fmt.Fprintf(&out, "-I %s 1 %s -m comment --comment %s -j %s\n", parts[0], parts[1], tag, chain)
	}
	out.WriteString("COMMIT\n")
	return out.String(), nil
}
