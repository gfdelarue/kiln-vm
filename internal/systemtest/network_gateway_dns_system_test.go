//go:build system

package systemtest

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/gfdelarue/kiln-vm/internal/models"

	"github.com/miekg/dns"
)

const directDNSControl = "1.1.1.1"

var dnsQuery = []byte{
	0x4b, 0x49, // transaction ID
	0x01, 0x00, // recursion desired
	0x00, 0x01, // one question
	0x00, 0x00, // no answers
	0x00, 0x00, // no authority records
	0x00, 0x00, // no additional records
	0x07, 'e', 'x', 'a', 'm', 'p', 'l', 'e',
	0x03, 'c', 'o', 'm',
	0x00,
	0x00, 0x01, // A
	0x00, 0x01, // IN
}

func TestNetworkGatewayDNSResponseValidationRequiresNoErrorAddressAnswer(t *testing.T) {
	var query dns.Msg
	if err := query.Unpack(dnsQuery); err != nil {
		t.Fatalf("parse fixture query: %v", err)
	}
	response := new(dns.Msg)
	response.SetReply(&query)
	response.Answer = []dns.RR{&dns.A{
		Hdr: dns.RR_Header{Name: "example.com.", Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60},
		A:   net.ParseIP("192.0.2.10").To4(),
	}}
	validWire, err := response.Pack()
	if err != nil {
		t.Fatalf("pack valid response: %v", err)
	}
	if err := validateDNSResponse(validWire); err != nil {
		t.Fatalf("valid NOERROR A response rejected: %v", err)
	}
	cnameResponse := response.Copy()
	cnameResponse.Answer = []dns.RR{
		&dns.CNAME{
			Hdr:    dns.RR_Header{Name: "example.com.", Rrtype: dns.TypeCNAME, Class: dns.ClassINET, Ttl: 60},
			Target: "alias.example.",
		},
		&dns.A{
			Hdr: dns.RR_Header{Name: "alias.example.", Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60},
			A:   net.ParseIP("192.0.2.11").To4(),
		},
	}
	cnameWire, err := cnameResponse.Pack()
	if err != nil {
		t.Fatalf("pack valid CNAME response: %v", err)
	}
	if err := validateDNSResponse(cnameWire); err != nil {
		t.Fatalf("valid CNAME-chain A response rejected: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*dns.Msg)
	}{
		{name: "SERVFAIL", mutate: func(message *dns.Msg) { message.Rcode = dns.RcodeServerFailure }},
		{name: "NOERROR without address", mutate: func(message *dns.Msg) { message.Answer = nil }},
		{name: "query rather than response", mutate: func(message *dns.Msg) { message.Response = false }},
		{name: "wrong transaction", mutate: func(message *dns.Msg) { message.Id++ }},
		{name: "wrong question", mutate: func(message *dns.Msg) { message.Question[0].Name = "other.example." }},
		{name: "truncated without fallback", mutate: func(message *dns.Msg) { message.Truncated = true; message.Answer = nil }},
		{name: "unrelated A owner", mutate: func(message *dns.Msg) { message.Answer[0].Header().Name = "unrelated.example." }},
		{name: "non-IN A", mutate: func(message *dns.Msg) { message.Answer[0].Header().Class = dns.ClassCHAOS }},
		{name: "CNAME with unrelated A", mutate: func(message *dns.Msg) {
			message.Answer = []dns.RR{
				&dns.CNAME{Hdr: dns.RR_Header{Name: "example.com.", Rrtype: dns.TypeCNAME, Class: dns.ClassINET}, Target: "alias.example."},
				&dns.A{Hdr: dns.RR_Header{Name: "unrelated.example.", Rrtype: dns.TypeA, Class: dns.ClassINET}, A: net.ParseIP("192.0.2.12").To4()},
			}
		}},
		{name: "CNAME loop", mutate: func(message *dns.Msg) {
			message.Answer = []dns.RR{
				&dns.CNAME{Hdr: dns.RR_Header{Name: "example.com.", Rrtype: dns.TypeCNAME, Class: dns.ClassINET}, Target: "alias.example."},
				&dns.CNAME{Hdr: dns.RR_Header{Name: "alias.example.", Rrtype: dns.TypeCNAME, Class: dns.ClassINET}, Target: "example.com."},
			}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := response.Copy()
			test.mutate(candidate)
			wire, err := candidate.Pack()
			if err != nil {
				t.Fatalf("pack response: %v", err)
			}
			if err := validateDNSResponse(wire); err == nil {
				t.Fatal("invalid DNS response passed acceptance validation")
			}
		})
	}
}

func TestNetworkVMGatewayDNSResolvesThroughConfiguredGateway(t *testing.T) {
	client := requireClient(t)

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	bringdownBaseline := captureBringdownBaseline(t, ctx, client)

	name := uniqueName("gateway-dns")
	vm, err := client.createVM(ctx, models.CreateVMRequest{
		Name:       name,
		Profile:    "alpine",
		DiskSizeMB: 1024,
		CPU:        &models.CPUConfig{VCPUs: 1},
		MemoryMB:   512,
	})
	if err != nil {
		t.Fatalf("create gateway DNS VM: %v", err)
	}
	deleted := false
	t.Cleanup(func() {
		if deleted {
			return
		}
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cleanupCancel()
		if err := client.deleteVM(cleanupCtx, name); err != nil {
			t.Errorf("cleanup gateway DNS VM %s: %v", name, err)
		}
	})

	vm, err = client.waitForVMState(ctx, name, models.MachineStateRunning)
	if err != nil {
		t.Fatalf("wait for gateway DNS VM running: %v", err)
	}
	if vm.GatewayIP == "" || vm.IP == "" || vm.TAPDevice == "" {
		t.Fatalf("gateway DNS VM is missing public network identity: %+v", vm)
	}
	if _, err := waitForExecResult(ctx, client, name, "echo gateway-dns-vm-ready", func(result *execResult) bool {
		return result.ExitCode == 0 && strings.Contains(result.Stdout, "gateway-dns-vm-ready")
	}); err != nil {
		t.Fatalf("wait for gateway DNS VM exec readiness: %v", err)
	}

	resolverResult, err := client.execVM(ctx, name, "awk '$1 == \"nameserver\" {print $2}' /etc/resolv.conf")
	if err != nil {
		t.Fatalf("read guest resolver configuration: %v", err)
	}
	resolver := strings.TrimSpace(resolverResult.Stdout)

	runningListeners, err := gatewayDNSListeners(vm.GatewayIP)
	if err != nil {
		t.Fatalf("inspect running gateway DNS listeners: %v", err)
	}
	coldHostUDPErr := probeHostDNS("udp4", vm.GatewayIP)
	coldHostTCPErr := probeHostDNS("tcp4", vm.GatewayIP)
	coldDirectUDP, coldDirectUDPErr := probeGuestDNS(ctx, client, name, directDNSControl, "udp")
	coldDirectTCP, coldDirectTCPErr := probeGuestDNS(ctx, client, name, directDNSControl, "tcp")
	coldGatewayUDP, coldGatewayUDPErr := probeGuestDNS(ctx, client, name, vm.GatewayIP, "udp")
	coldGatewayTCP, coldGatewayTCPErr := probeGuestDNS(ctx, client, name, vm.GatewayIP, "tcp")
	coldMetrics, err := client.vmMachineMetrics(ctx, vm.ID)
	if err != nil {
		t.Fatalf("read cold gateway DNS metrics: %v", err)
	}

	stopped := stopVMAndWait(t, ctx, client, name)
	if err := waitForNoTap(ctx, vm.TAPDevice); err != nil {
		t.Fatal(err)
	}
	stoppedListeners, err := gatewayDNSListeners(vm.GatewayIP)
	if err != nil {
		t.Fatalf("inspect stopped gateway DNS listeners: %v", err)
	}

	warm := startVMAndWait(t, ctx, client, name)
	if _, err := waitForExecResult(ctx, client, name, "echo gateway-dns-vm-warm", func(result *execResult) bool {
		return result.ExitCode == 0 && strings.Contains(result.Stdout, "gateway-dns-vm-warm")
	}); err != nil {
		t.Fatalf("wait for warm gateway DNS VM exec readiness: %v", err)
	}
	warmListeners, err := gatewayDNSListeners(warm.GatewayIP)
	if err != nil {
		t.Fatalf("inspect warm gateway DNS listeners: %v", err)
	}
	warmHostUDPErr := probeHostDNS("udp4", warm.GatewayIP)
	warmHostTCPErr := probeHostDNS("tcp4", warm.GatewayIP)
	warmGatewayUDP, warmGatewayUDPErr := probeGuestDNS(ctx, client, name, warm.GatewayIP, "udp")
	warmGatewayTCP, warmGatewayTCPErr := probeGuestDNS(ctx, client, name, warm.GatewayIP, "tcp")
	warmMetrics, err := client.vmMachineMetrics(ctx, warm.ID)
	if err != nil {
		t.Fatalf("read warm gateway DNS metrics: %v", err)
	}

	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cleanupCancel()
	if err := client.deleteVM(cleanupCtx, name); err != nil {
		t.Fatalf("delete gateway DNS VM: %v", err)
	}
	deleted = true
	if err := waitForNoTap(cleanupCtx, vm.TAPDevice); err != nil {
		t.Fatal(err)
	}
	deletedListeners, err := gatewayDNSListeners(vm.GatewayIP)
	if err != nil {
		t.Fatalf("inspect deleted gateway DNS listeners: %v", err)
	}

	t.Run("guest resolver is the allocation gateway", func(t *testing.T) {
		if resolverResult.ExitCode != 0 || resolver != vm.GatewayIP {
			t.Fatalf("guest resolver = %q (exit=%d stderr=%q), want allocation gateway %q", resolver, resolverResult.ExitCode, resolverResult.Stderr, vm.GatewayIP)
		}
	})
	t.Run("cold physical-host proxy answers UDP and TCP", func(t *testing.T) {
		if !runningListeners.UDP || !runningListeners.TCP || coldHostUDPErr != nil || coldHostTCPErr != nil {
			t.Fatalf("running listeners=%s host UDP=%v TCP=%v", runningListeners, coldHostUDPErr, coldHostTCPErr)
		}
	})
	t.Run("cold guest direct DNS control is diagnostic only", func(t *testing.T) {
		for _, diagnostic := range []struct {
			transport string
			result    *execResult
			err       error
		}{
			{transport: "udp", result: coldDirectUDP, err: coldDirectUDPErr},
			{transport: "tcp", result: coldDirectTCP, err: coldDirectTCPErr},
		} {
			if probeErr := guestDNSProbeError(diagnostic.result, diagnostic.err, diagnostic.transport); probeErr != nil {
				t.Logf("direct public resolver %s control unavailable (not an acceptance requirement): %v", diagnostic.transport, probeErr)
			} else {
				t.Logf("direct public resolver %s control answered", diagnostic.transport)
			}
		}
	})
	t.Run("cold guest resolves through gateway over UDP", func(t *testing.T) {
		requireGuestDNSProbe(t, "gateway UDP", coldGatewayUDP, coldGatewayUDPErr, "udp")
	})
	t.Run("cold guest resolves through gateway over TCP", func(t *testing.T) {
		requireGuestDNSProbe(t, "gateway TCP", coldGatewayTCP, coldGatewayTCPErr, "tcp")
	})
	t.Run("cold provider metrics report the complete gateway DNS path", func(t *testing.T) {
		requireReadyGatewayDNSMetrics(t, *coldMetrics)
	})
	t.Run("stop closes both gateway DNS listeners", func(t *testing.T) {
		if stopped.State != models.MachineStateStopped {
			t.Fatalf("stopped VM state = %q, want stopped", stopped.State)
		}
		if stoppedListeners.UDP || stoppedListeners.TCP {
			t.Fatalf("gateway DNS listeners survived VM stop: %s", stoppedListeners)
		}
	})
	t.Run("warm start preserves identity and restores the proxy", func(t *testing.T) {
		if warm.GatewayIP != vm.GatewayIP || warm.IP != vm.IP || warm.TAPDevice != vm.TAPDevice {
			t.Fatalf("warm public network identity = %s/%s/%s, want %s/%s/%s", warm.GatewayIP, warm.IP, warm.TAPDevice, vm.GatewayIP, vm.IP, vm.TAPDevice)
		}
		if !warmListeners.UDP || !warmListeners.TCP || warmHostUDPErr != nil || warmHostTCPErr != nil {
			t.Fatalf("warm listeners=%s host UDP=%v TCP=%v", warmListeners, warmHostUDPErr, warmHostTCPErr)
		}
	})
	t.Run("warm guest resolves through gateway over UDP", func(t *testing.T) {
		requireGuestDNSProbe(t, "warm gateway UDP", warmGatewayUDP, warmGatewayUDPErr, "udp")
	})
	t.Run("warm guest resolves through gateway over TCP", func(t *testing.T) {
		requireGuestDNSProbe(t, "warm gateway TCP", warmGatewayTCP, warmGatewayTCPErr, "tcp")
	})
	t.Run("warm provider metrics report the complete gateway DNS path", func(t *testing.T) {
		requireReadyGatewayDNSMetrics(t, *warmMetrics)
	})
	t.Run("delete closes both gateway DNS listeners", func(t *testing.T) {
		if deletedListeners.UDP || deletedListeners.TCP {
			t.Fatalf("gateway DNS listeners survived VM deletion: %s", deletedListeners)
		}
		requireVMBringdown(t, client, bringdownBaseline, vm)
	})
}

type dnsListeners struct {
	UDP   bool
	TCP   bool
	Lines []string
}

func (l dnsListeners) String() string {
	if len(l.Lines) == 0 {
		return "none"
	}
	return strings.Join(l.Lines, " | ")
}

func gatewayDNSListeners(gatewayIP string) (dnsListeners, error) {
	out, err := exec.Command("ss", "-H", "-l", "-n", "-u", "-t").CombinedOutput()
	if err != nil {
		return dnsListeners{}, commandError("list gateway DNS listeners", out, err)
	}
	want := net.JoinHostPort(gatewayIP, "53")
	listeners := dnsListeners{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		hasExactAddress := false
		for _, field := range fields[1:] {
			if field == want {
				hasExactAddress = true
				break
			}
		}
		if !hasExactAddress {
			continue
		}
		listeners.Lines = append(listeners.Lines, strings.TrimSpace(line))
		switch fields[0] {
		case "udp":
			listeners.UDP = true
		case "tcp":
			listeners.TCP = true
		}
	}
	return listeners, nil
}

func requireGuestDNSProbe(t *testing.T, label string, result *execResult, err error, transport string) {
	t.Helper()
	if probeErr := guestDNSProbeError(result, err, transport); probeErr == nil {
		return
	} else if result == nil {
		t.Fatalf("%s failed: %v (no execution result)", label, probeErr)
	} else {
		t.Fatalf("%s failed: %v; exit=%d stdout=%q stderr=%q", label, probeErr, result.ExitCode, result.Stdout, result.Stderr)
	}
}

func guestDNSProbeError(result *execResult, execErr error, transport string) error {
	if execErr != nil {
		return fmt.Errorf("execute guest probe: %w", execErr)
	}
	if result == nil {
		return fmt.Errorf("guest probe returned no execution result")
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("guest probe exited %d", result.ExitCode)
	}
	wire, err := hex.DecodeString(strings.TrimSpace(result.Stdout))
	if err != nil {
		return fmt.Errorf("decode guest DNS response hex: %w", err)
	}
	if transport == "tcp" {
		if len(wire) < 2 {
			return fmt.Errorf("short TCP DNS frame: %d bytes", len(wire))
		}
		frameLength := int(binary.BigEndian.Uint16(wire[:2]))
		if frameLength != len(wire)-2 {
			return fmt.Errorf("TCP DNS frame length = %d, payload = %d", frameLength, len(wire)-2)
		}
		wire = wire[2:]
	} else if transport != "udp" {
		return fmt.Errorf("unsupported DNS transport %q", transport)
	}
	return validateDNSResponse(wire)
}

func probeHostDNS(network, server string) error {
	address := net.JoinHostPort(server, "53")
	conn, err := net.DialTimeout(network, address, 3*time.Second)
	if err != nil {
		return fmt.Errorf("dial %s %s: %w", network, address, err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		return fmt.Errorf("set DNS probe deadline: %w", err)
	}

	if network == "tcp4" {
		var header [2]byte
		binary.BigEndian.PutUint16(header[:], uint16(len(dnsQuery)))
		if _, err := conn.Write(append(header[:], dnsQuery...)); err != nil {
			return fmt.Errorf("write TCP DNS query: %w", err)
		}
		if _, err := io.ReadFull(conn, header[:]); err != nil {
			return fmt.Errorf("read TCP DNS response length: %w", err)
		}
		response := make([]byte, int(binary.BigEndian.Uint16(header[:])))
		if _, err := io.ReadFull(conn, response); err != nil {
			return fmt.Errorf("read TCP DNS response: %w", err)
		}
		return validateDNSResponse(response)
	}

	if _, err := conn.Write(dnsQuery); err != nil {
		return fmt.Errorf("write UDP DNS query: %w", err)
	}
	response := make([]byte, 4096)
	n, err := conn.Read(response)
	if err != nil {
		return fmt.Errorf("read UDP DNS response: %w", err)
	}
	return validateDNSResponse(response[:n])
}

func validateDNSResponse(response []byte) error {
	var message dns.Msg
	if err := message.Unpack(response); err != nil {
		return fmt.Errorf("parse DNS response: %w", err)
	}
	wantID := binary.BigEndian.Uint16(dnsQuery[:2])
	if message.Id != wantID {
		return fmt.Errorf("DNS transaction ID = %x, want %x", message.Id, wantID)
	}
	if !message.Response {
		return fmt.Errorf("DNS response bit is not set")
	}
	if message.Rcode != dns.RcodeSuccess {
		return fmt.Errorf("DNS response code = %s, want NOERROR", dns.RcodeToString[message.Rcode])
	}
	if message.Truncated {
		return fmt.Errorf("DNS response is truncated and was not retried over TCP")
	}
	if len(message.Question) != 1 || message.Question[0].Name != "example.com." || message.Question[0].Qtype != dns.TypeA || message.Question[0].Qclass != dns.ClassINET {
		return fmt.Errorf("DNS response question = %+v, want example.com. IN A", message.Question)
	}
	addressCount, err := relevantDNSAddressCount(message.Answer, message.Question[0].Name)
	if err != nil {
		return err
	}
	if addressCount == 0 {
		return fmt.Errorf("DNS NOERROR response contained no relevant IN A answer (answers=%d)", len(message.Answer))
	}
	return nil
}

func relevantDNSAddressCount(answers []dns.RR, queryName string) (int, error) {
	cnameTargets := make(map[string][]string)
	for _, answer := range answers {
		header := answer.Header()
		if header.Class != dns.ClassINET {
			continue
		}
		record, ok := answer.(*dns.CNAME)
		if !ok {
			continue
		}
		owner := dns.CanonicalName(header.Name)
		target := dns.CanonicalName(record.Target)
		cnameTargets[owner] = append(cnameTargets[owner], target)
	}

	terminal := dns.CanonicalName(queryName)
	seen := make(map[string]bool, len(cnameTargets)+1)
	for {
		if seen[terminal] {
			return 0, fmt.Errorf("DNS CNAME chain contains a loop")
		}
		seen[terminal] = true
		targets := cnameTargets[terminal]
		if len(targets) == 0 {
			break
		}
		for _, target := range targets[1:] {
			if target != targets[0] {
				return 0, fmt.Errorf("DNS CNAME owner %q has conflicting targets", terminal)
			}
		}
		terminal = targets[0]
	}

	addressCount := 0
	for _, answer := range answers {
		record, ok := answer.(*dns.A)
		if !ok || record.Hdr.Class != dns.ClassINET || dns.CanonicalName(record.Hdr.Name) != terminal || record.A.To4() == nil {
			continue
		}
		addressCount++
	}
	return addressCount, nil
}

func probeGuestDNS(ctx context.Context, client *apiClient, vmName, server, transport string) (*execResult, error) {
	query := `\113\111\001\000\000\001\000\000\000\000\000\000\007example\003com\000\000\001\000\001`
	ncArgs := "-u -w 3"
	payload := query
	if transport == "tcp" {
		ncArgs = "-w 3"
		payload = `\000\035` + query
	}
	command := fmt.Sprintf(
		`hex=$({ printf '%%b' '%s' | timeout 5 nc %s %s 53; } | od -An -tx1 | tr -d ' \n'); printf '%%s\n' "$hex"`,
		payload,
		ncArgs,
		shellQuote(server),
	)
	return client.execVM(ctx, vmName, command)
}

func requireReadyGatewayDNSMetrics(t *testing.T, metrics models.MachineMetrics) {
	t.Helper()
	status := metrics.GatewayDNS
	if status.Status != "ready" || status.Reason != "" || !status.GatewayDNSPath || !status.ExternalDNS {
		t.Fatalf("gateway DNS metrics = %+v, want ready path and external resolution", status)
	}
	if status.DesiredRules != 2 || status.InstalledRules != 2 {
		t.Fatalf("gateway DNS firewall metrics = desired:%d installed:%d, want 2/2", status.DesiredRules, status.InstalledRules)
	}
	if status.LastCheckedAt == nil || status.LastSuccessAt == nil || status.LastError != "" {
		t.Fatalf("gateway DNS observation metrics = %+v, want checked successful observation without error", status)
	}
}
