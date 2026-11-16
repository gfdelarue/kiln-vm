//go:build system

package systemtest

import (
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
)

const (
	gatewayDNSFirewallIdentityVersion = "kiln-vm-gateway-dns-firewall:v2"
	gatewayDNSFirewallCommentBase     = "kiln-vm-gateway-dns:v2"
	gatewayDNSFirewallChainPrefix     = "KVD_"
	gatewayDNSFirewallOwnerIDChars    = 32
	gatewayDNSFirewallChainIDChars    = 24
	gatewayDNSFirewallOwnerFile       = "gateway-dns-firewall.owner"

	gatewayDNSFirewallAdmit = "admit"
	gatewayDNSFirewallGuard = "guard"
)

type gatewayDNSFirewallIdentity struct {
	OwnerID string
	Chain   string
}

func gatewayDNSFirewallIdentityPath(dataDir string) string {
	return filepath.Join(filepath.Clean(dataDir), "network", gatewayDNSFirewallOwnerFile)
}

func readGatewayDNSFirewallIdentity(dataDir string) (gatewayDNSFirewallIdentity, error) {
	path := gatewayDNSFirewallIdentityPath(dataDir)
	info, err := os.Lstat(path)
	if err != nil {
		return gatewayDNSFirewallIdentity{}, fmt.Errorf("inspect gateway DNS firewall owner identity %s: %w", path, err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 {
		return gatewayDNSFirewallIdentity{}, fmt.Errorf("gateway DNS firewall owner identity %s is not a protected regular file", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return gatewayDNSFirewallIdentity{}, fmt.Errorf("read gateway DNS firewall owner identity %s: %w", path, err)
	}
	identity, err := parseGatewayDNSFirewallIdentity(data)
	if err != nil {
		return gatewayDNSFirewallIdentity{}, fmt.Errorf("parse gateway DNS firewall owner identity %s: %w", path, err)
	}
	return identity, nil
}

func parseGatewayDNSFirewallIdentity(data []byte) (gatewayDNSFirewallIdentity, error) {
	prefix := gatewayDNSFirewallIdentityVersion + "\nowner-id="
	text := string(data)
	if !strings.HasPrefix(text, prefix) || !strings.HasSuffix(text, "\n") || strings.Count(text, "\n") != 2 {
		return gatewayDNSFirewallIdentity{}, fmt.Errorf("unexpected v2 owner identity contents")
	}
	ownerID := strings.TrimSuffix(strings.TrimPrefix(text, prefix), "\n")
	if len(ownerID) != gatewayDNSFirewallOwnerIDChars || ownerID != strings.ToLower(ownerID) {
		return gatewayDNSFirewallIdentity{}, fmt.Errorf("owner identity must be %d lowercase hexadecimal characters", gatewayDNSFirewallOwnerIDChars)
	}
	for _, char := range ownerID {
		if !strings.ContainsRune("0123456789abcdef", char) {
			return gatewayDNSFirewallIdentity{}, fmt.Errorf("owner identity is not lowercase hexadecimal")
		}
	}
	return gatewayDNSFirewallIdentity{
		OwnerID: ownerID,
		Chain:   gatewayDNSFirewallChainPrefix + ownerID[:gatewayDNSFirewallChainIDChars],
	}, nil
}

func (i gatewayDNSFirewallIdentity) commentBase() string {
	return gatewayDNSFirewallCommentBase + ":" + i.OwnerID
}

type gatewayDNSFirewallRule struct {
	Position    int
	TAPDevice   string
	Source      string
	Destination string
	Transport   string
	Verdict     string
	Target      string
	Comment     string
}

type gatewayDNSFirewallState struct {
	Identity      gatewayDNSFirewallIdentity
	InputPolicy   string
	ChainExists   bool
	HookPositions []int
	ChainRules    []gatewayDNSFirewallRule
	RelevantLines []string
}

func (s gatewayDNSFirewallState) ruleCount(tap, transport, verdict string) int {
	count := 0
	for _, rule := range s.ChainRules {
		if rule.TAPDevice == tap && rule.Transport == transport && rule.Verdict == verdict {
			count++
		}
	}
	return count
}

// packetVerdict evaluates a host-local DNS packet after the kernel has routed
// it through INPUT. Forwarded external DNS never traverses this owned chain.
func (s gatewayDNSFirewallState) packetVerdict(tap, source, destination, transport string) string {
	if len(s.HookPositions) != 1 || s.HookPositions[0] != 1 {
		return s.InputPolicy
	}
	for _, rule := range s.ChainRules {
		if rule.TAPDevice != tap || rule.Transport != transport {
			continue
		}
		if rule.Source != "" && rule.Source != source+"/32" {
			continue
		}
		if rule.Destination != "" && rule.Destination != destination+"/32" {
			continue
		}
		return rule.Target
	}
	return s.InputPolicy
}

func parseGatewayDNSFirewallState(output string, identity gatewayDNSFirewallIdentity) (gatewayDNSFirewallState, error) {
	if parsed, err := parseGatewayDNSFirewallIdentity([]byte(gatewayDNSFirewallIdentityVersion + "\nowner-id=" + identity.OwnerID + "\n")); err != nil || parsed != identity {
		return gatewayDNSFirewallState{}, fmt.Errorf("invalid expected gateway DNS firewall identity %+v", identity)
	}

	state := gatewayDNSFirewallState{Identity: identity}
	positions := make(map[string]int)
	ownerNamespace := identity.commentBase() + ":"
	for _, rawLine := range strings.Split(output, "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}
		tokens, err := splitIptablesLine(line)
		if err != nil {
			return gatewayDNSFirewallState{}, fmt.Errorf("parse iptables line %q: %w", line, err)
		}
		if len(tokens) == 3 && tokens[0] == "-P" && tokens[1] == "INPUT" {
			state.InputPolicy = tokens[2]
			continue
		}
		if len(tokens) == 2 && tokens[0] == "-N" && tokens[1] == identity.Chain {
			if state.ChainExists {
				return gatewayDNSFirewallState{}, fmt.Errorf("duplicate owner-derived chain declaration %s", identity.Chain)
			}
			state.ChainExists = true
			state.RelevantLines = append(state.RelevantLines, line)
			continue
		}
		if len(tokens) < 2 || tokens[0] != "-A" {
			continue
		}

		chain := tokens[1]
		positions[chain]++
		position := positions[chain]
		target, hasTarget := gatewayDNSFirewallOption(tokens, "-j", "--jump", "-g", "--goto")
		comment, hasComment := gatewayDNSFirewallOption(tokens, "--comment")
		referencesChain := hasTarget && target == identity.Chain
		usesOwnerNamespace := hasComment && strings.HasPrefix(comment, ownerNamespace)
		insideOwnerChain := chain == identity.Chain
		if !referencesChain && !usesOwnerNamespace && !insideOwnerChain {
			continue
		}
		state.RelevantLines = append(state.RelevantLines, line)

		if referencesChain {
			if chain != "INPUT" || !exactGatewayDNSFirewallHook(tokens, identity) {
				return gatewayDNSFirewallState{}, fmt.Errorf("owner-derived chain %s has foreign reference from %s rule %d", identity.Chain, chain, position)
			}
			state.HookPositions = append(state.HookPositions, position)
			continue
		}
		if usesOwnerNamespace && chain != identity.Chain {
			return gatewayDNSFirewallState{}, fmt.Errorf("owner namespace appears outside the owner-derived chain at %s rule %d", chain, position)
		}
		if insideOwnerChain {
			rule, err := parseGatewayDNSFirewallAllocationRule(tokens, identity, position)
			if err != nil {
				return gatewayDNSFirewallState{}, fmt.Errorf("owner-derived chain rule %d is not exact: %w", position, err)
			}
			state.ChainRules = append(state.ChainRules, rule)
		}
	}
	if state.InputPolicy == "" {
		return gatewayDNSFirewallState{}, fmt.Errorf("iptables snapshot has no INPUT policy")
	}
	if (!state.ChainExists && (len(state.HookPositions) != 0 || len(state.ChainRules) != 0)) || (state.ChainExists && len(state.ChainRules) != positions[identity.Chain]) {
		return gatewayDNSFirewallState{}, fmt.Errorf("owner-derived chain declaration and rules disagree")
	}
	return state, nil
}

func exactGatewayDNSFirewallHook(tokens []string, identity gatewayDNSFirewallIdentity) bool {
	if len(tokens) != 8 || tokens[0] != "-A" || tokens[1] != "INPUT" {
		return false
	}
	module, moduleOK := gatewayDNSFirewallOption(tokens, "-m", "--match")
	comment, commentOK := gatewayDNSFirewallOption(tokens, "--comment")
	target, targetOK := gatewayDNSFirewallOption(tokens, "-j", "--jump")
	return moduleOK && module == "comment" && commentOK && comment == identity.commentBase()+":input" && targetOK && target == identity.Chain
}

func parseGatewayDNSFirewallAllocationRule(tokens []string, identity gatewayDNSFirewallIdentity, position int) (gatewayDNSFirewallRule, error) {
	rule := gatewayDNSFirewallRule{Position: position}
	if len(tokens) < 2 || tokens[0] != "-A" || tokens[1] != identity.Chain {
		return rule, fmt.Errorf("not an append rule for %s", identity.Chain)
	}
	seen := make(map[string]bool)
	modules := make(map[string]bool)
	set := func(field, value string, destination *string) error {
		if seen[field] {
			return fmt.Errorf("duplicate %s", field)
		}
		if value == "" {
			return fmt.Errorf("empty %s", field)
		}
		seen[field] = true
		*destination = value
		return nil
	}
	for index := 2; index < len(tokens); {
		option := tokens[index]
		if option == "!" {
			return rule, fmt.Errorf("negated selector")
		}
		if index+1 >= len(tokens) {
			return rule, fmt.Errorf("option %s has no value", option)
		}
		value := tokens[index+1]
		var err error
		switch option {
		case "-i", "--in-interface":
			err = set("input interface", value, &rule.TAPDevice)
		case "-s", "--source":
			err = set("source", value, &rule.Source)
		case "-d", "--destination":
			err = set("destination", value, &rule.Destination)
		case "-p", "--protocol":
			err = set("protocol", strings.ToLower(value), &rule.Transport)
		case "--dport", "--destination-port":
			var port string
			err = set("destination port", value, &port)
			if err == nil && port != "53" {
				return rule, fmt.Errorf("destination port is %q", port)
			}
		case "--comment":
			err = set("comment", value, &rule.Comment)
		case "-j", "--jump":
			err = set("target", value, &rule.Target)
		case "-m", "--match":
			module := strings.ToLower(value)
			if module != "comment" && module != "udp" && module != "tcp" {
				return rule, fmt.Errorf("unknown match module %q", value)
			}
			if modules[module] {
				return rule, fmt.Errorf("duplicate match module %q", value)
			}
			modules[module] = true
		default:
			return rule, fmt.Errorf("unknown option %q", option)
		}
		if err != nil {
			return rule, err
		}
		index += 2
	}
	for _, field := range []string{"input interface", "protocol", "destination port", "comment", "target"} {
		if !seen[field] {
			return rule, fmt.Errorf("missing %s", field)
		}
	}
	if !modules["comment"] {
		return rule, fmt.Errorf("missing comment match")
	}
	if rule.Transport != "udp" && rule.Transport != "tcp" {
		return rule, fmt.Errorf("transport is %q", rule.Transport)
	}
	if modules["udp"] && modules["tcp"] || (modules["udp"] || modules["tcp"]) && !modules[rule.Transport] {
		return rule, fmt.Errorf("protocol match module disagrees with %s", rule.Transport)
	}
	if !canonicalTAP(rule.TAPDevice) {
		return rule, fmt.Errorf("TAP identity %q is not canonical", rule.TAPDevice)
	}
	commentPrefix := identity.commentBase() + ":alloc:" + rule.TAPDevice + ":" + rule.Transport + ":"
	if !strings.HasPrefix(rule.Comment, commentPrefix) {
		return rule, fmt.Errorf("comment %q does not match %q", rule.Comment, commentPrefix)
	}
	rule.Verdict = strings.TrimPrefix(rule.Comment, commentPrefix)
	switch rule.Verdict {
	case gatewayDNSFirewallAdmit:
		if rule.Target != "ACCEPT" {
			return rule, fmt.Errorf("admission target is %q", rule.Target)
		}
		if !seen["source"] || !seen["destination"] {
			return rule, fmt.Errorf("admission requires exact source and destination")
		}
		var err error
		rule.Source, err = canonicalIPv4HostPrefix(rule.Source)
		if err != nil {
			return rule, fmt.Errorf("admission source: %w", err)
		}
		rule.Destination, err = canonicalIPv4HostPrefix(rule.Destination)
		if err != nil {
			return rule, fmt.Errorf("admission destination: %w", err)
		}
	case gatewayDNSFirewallGuard:
		if rule.Target != "DROP" {
			return rule, fmt.Errorf("guard target is %q", rule.Target)
		}
		if seen["source"] || seen["destination"] {
			return rule, fmt.Errorf("guard must not contain source or destination")
		}
	default:
		return rule, fmt.Errorf("verdict is %q", rule.Verdict)
	}
	return rule, nil
}

func gatewayDNSFirewallOption(tokens []string, names ...string) (string, bool) {
	for index, token := range tokens {
		for _, name := range names {
			if token == name && index+1 < len(tokens) {
				return tokens[index+1], true
			}
		}
	}
	return "", false
}

func canonicalTAP(value string) bool {
	if !strings.HasPrefix(value, "kv") {
		return false
	}
	index, err := strconv.ParseUint(strings.TrimPrefix(value, "kv"), 10, 32)
	return err == nil && index > 0 && value == fmt.Sprintf("kv%d", index)
}

func canonicalIPv4HostPrefix(value string) (string, error) {
	prefix, err := netip.ParsePrefix(value)
	if err != nil || !prefix.Addr().Is4() || prefix.Bits() != 32 || prefix.Masked().Addr() != prefix.Addr() {
		return "", fmt.Errorf("%q is not a canonical IPv4 /32", value)
	}
	return prefix.String(), nil
}

func splitIptablesLine(line string) ([]string, error) {
	var (
		tokens  []string
		current strings.Builder
		quote   rune
		escaped bool
		started bool
	)
	flush := func() {
		if !started {
			return
		}
		tokens = append(tokens, current.String())
		current.Reset()
		started = false
	}
	for _, char := range line {
		if escaped {
			current.WriteRune(char)
			escaped = false
			started = true
			continue
		}
		if char == '\\' && quote != '\'' {
			escaped = true
			started = true
			continue
		}
		if quote != 0 {
			if char == quote {
				quote = 0
				started = true
				continue
			}
			current.WriteRune(char)
			started = true
			continue
		}
		switch {
		case char == '\'' || char == '"':
			quote = char
			started = true
		case unicode.IsSpace(char):
			flush()
		default:
			current.WriteRune(char)
			started = true
		}
	}
	if escaped {
		return nil, fmt.Errorf("trailing escape")
	}
	if quote != 0 {
		return nil, fmt.Errorf("unterminated quote")
	}
	flush()
	return tokens, nil
}
