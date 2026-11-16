//go:build system

package systemtest

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gfdelarue/kiln-vm/internal/models"
)

func TestNetworkVMGatewayDNSConcurrentReadinessUsesIndependentAllocations(t *testing.T) {
	client := requireClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	bringdownBaseline := captureBringdownBaseline(t, ctx, client)

	vms := make([]*models.VM, 0, 2)
	deleted := make([]bool, 2)
	for index := 0; index < 2; index++ {
		name := uniqueName(fmt.Sprintf("gateway-dns-concurrent-%d", index+1))
		vm, err := client.createVM(ctx, models.CreateVMRequest{
			Name:       name,
			Profile:    "alpine",
			DiskSizeMB: 1024,
			CPU:        &models.CPUConfig{VCPUs: 1},
			MemoryMB:   512,
		})
		if err != nil {
			t.Fatalf("create concurrent gateway DNS VM %d: %v", index+1, err)
		}
		vms = append(vms, vm)
		cleanupIndex, cleanupName := index, name
		t.Cleanup(func() {
			if deleted[cleanupIndex] {
				return
			}
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cleanupCancel()
			if err := client.deleteVM(cleanupCtx, cleanupName); err != nil {
				t.Logf("cleanup concurrent gateway DNS VM %s: %v", cleanupName, err)
			}
		})
	}

	for index, vm := range vms {
		running, err := client.waitForVMState(ctx, vm.Name, models.MachineStateRunning)
		if err != nil {
			t.Fatalf("wait for concurrent gateway DNS VM %d: %v", index+1, err)
		}
		vms[index] = running
	}
	if vms[0].GatewayIP == vms[1].GatewayIP || vms[0].IP == vms[1].IP || vms[0].TAPDevice == vms[1].TAPDevice {
		t.Fatalf("concurrent VMs share public network identity: first=%s/%s/%s second=%s/%s/%s",
			vms[0].GatewayIP, vms[0].IP, vms[0].TAPDevice,
			vms[1].GatewayIP, vms[1].IP, vms[1].TAPDevice,
		)
	}

	type probeOutcome struct {
		index     int
		transport string
		result    *execResult
		err       error
	}
	start := make(chan struct{})
	outcomes := make(chan probeOutcome, len(vms)*2)
	var wg sync.WaitGroup
	for index, vm := range vms {
		for _, transport := range []string{"udp", "tcp"} {
			index, vm, transport := index, vm, transport
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				result, err := probeGuestDNS(ctx, client, vm.Name, vm.GatewayIP, transport)
				outcomes <- probeOutcome{index: index, transport: transport, result: result, err: err}
			}()
		}
	}
	close(start)
	wg.Wait()
	close(outcomes)
	for outcome := range outcomes {
		if err := guestDNSProbeError(outcome.result, outcome.err, outcome.transport); err != nil {
			t.Errorf("VM %d concurrent gateway %s probe: %v", outcome.index+1, outcome.transport, err)
		}
	}
	if t.Failed() {
		t.FailNow()
	}

	for index, vm := range vms {
		metrics, err := client.vmMachineMetrics(ctx, vm.ID)
		if err != nil {
			t.Fatalf("read concurrent VM %d gateway DNS metrics: %v", index+1, err)
		}
		requireReadyGatewayDNSMetrics(t, *metrics)
	}

	for sourceIndex, sourceVM := range vms {
		targetIndex := 1 - sourceIndex
		for _, transport := range []string{"udp", "tcp"} {
			requireGuestDNSProbeRejectedWithinBound(
				t,
				ctx,
				client,
				sourceVM.Name,
				vms[targetIndex].GatewayIP,
				transport,
				fmt.Sprintf("VM %d cross-allocation gateway %d %s", sourceIndex+1, targetIndex+1, transport),
			)
		}
	}

	first := vms[0]
	if err := client.deleteVM(ctx, first.Name); err != nil {
		t.Fatalf("delete first concurrent gateway DNS VM: %v", err)
	}
	deleted[0] = true
	if err := waitForNoTap(ctx, first.TAPDevice); err != nil {
		t.Fatal(err)
	}
	firstListeners, err := gatewayDNSListeners(first.GatewayIP)
	if err != nil {
		t.Fatalf("inspect deleted first concurrent VM listeners: %v", err)
	}
	if firstListeners.UDP || firstListeners.TCP {
		t.Fatalf("first concurrent VM gateway DNS listeners survived deletion: %s", firstListeners)
	}
	requireVMBringdown(t, client, bringdownBaseline, first)

	survivor := vms[1]
	for _, transport := range []string{"udp", "tcp"} {
		result, probeErr := probeGuestDNS(ctx, client, survivor.Name, survivor.GatewayIP, transport)
		requireGuestDNSProbe(t, "surviving second VM gateway "+transport, result, probeErr, transport)
	}
	survivorMetrics, err := client.vmMachineMetrics(ctx, survivor.ID)
	if err != nil {
		t.Fatalf("read surviving second VM gateway DNS metrics: %v", err)
	}
	requireReadyGatewayDNSMetrics(t, *survivorMetrics)

	if err := client.deleteVM(ctx, survivor.Name); err != nil {
		t.Fatalf("delete surviving second concurrent gateway DNS VM: %v", err)
	}
	deleted[1] = true
	if err := waitForNoTap(ctx, survivor.TAPDevice); err != nil {
		t.Fatal(err)
	}
	survivorListeners, err := gatewayDNSListeners(survivor.GatewayIP)
	if err != nil {
		t.Fatalf("inspect deleted surviving second VM listeners: %v", err)
	}
	if survivorListeners.UDP || survivorListeners.TCP {
		t.Fatalf("surviving second VM gateway DNS listeners survived deletion: %s", survivorListeners)
	}
	requireVMBringdown(t, client, bringdownBaseline, survivor)
}

func requireGuestDNSProbeRejectedWithinBound(
	t *testing.T,
	ctx context.Context,
	client *apiClient,
	vmName string,
	foreignGateway string,
	transport string,
	label string,
) {
	t.Helper()
	probeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	// A reachable kiln-vm proxy answers this malformed query locally with FORMERR,
	// without waiting on an external resolver. Zero response bytes therefore
	// prove cross-allocation isolation instead of conflating it with upstream
	// DNS latency.
	result, err := probeGuestMalformedDNS(probeCtx, client, vmName, foreignGateway, transport)
	if err != nil {
		t.Fatalf("%s did not complete as a bounded guest network probe: %v", label, err)
	}
	if result == nil {
		t.Fatalf("%s returned no guest execution result", label)
	}
	if result.ExitCode != 0 {
		t.Fatalf("%s failed to execute inside the guest: exit=%d stdout=%q stderr=%q", label, result.ExitCode, result.Stdout, result.Stderr)
	}
	if strings.TrimSpace(result.Stdout) != "" {
		t.Fatalf("%s received bytes from another allocation: stdout=%q stderr=%q", label, result.Stdout, result.Stderr)
	}
	if probeErr := guestDNSProbeError(result, nil, transport); probeErr == nil {
		t.Fatalf("%s unexpectedly produced a valid DNS response", label)
	}
}

func probeGuestMalformedDNS(ctx context.Context, client *apiClient, vmName, server, transport string) (*execResult, error) {
	// This is the normal fixture query with QR set, which is not a valid client
	// query. The proxy preserves its ID and returns FORMERR immediately.
	query := `\113\111\201\000\000\001\000\000\000\000\000\000\007example\003com\000\000\001\000\001`
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

func TestNetworkVMGatewayDNSReportsGuestPathFailureAndRecoversOnWarmStart(t *testing.T) {
	client := requireClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	bringdownBaseline := captureBringdownBaseline(t, ctx, client)

	name := uniqueName("gateway-dns-path-failure")
	vm, err := client.createVM(ctx, models.CreateVMRequest{
		Name:       name,
		Profile:    "alpine",
		DiskSizeMB: 1024,
		CPU:        &models.CPUConfig{VCPUs: 1},
		MemoryMB:   512,
	})
	if err != nil {
		t.Fatalf("create gateway DNS path-failure VM: %v", err)
	}
	deleted := false
	t.Cleanup(func() {
		if deleted {
			return
		}
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cleanupCancel()
		if err := client.deleteVM(cleanupCtx, name); err != nil {
			t.Logf("cleanup gateway DNS path-failure VM %s: %v", name, err)
		}
	})

	vm, err = client.waitForVMState(ctx, name, models.MachineStateRunning)
	if err != nil {
		t.Fatalf("wait for gateway DNS path-failure VM: %v", err)
	}
	for _, transport := range []string{"udp", "tcp"} {
		result, probeErr := probeGuestDNS(ctx, client, name, vm.GatewayIP, transport)
		requireGuestDNSProbe(t, "initial gateway "+transport, result, probeErr, transport)
	}
	readyMetrics, err := client.vmMachineMetrics(ctx, vm.ID)
	if err != nil {
		t.Fatalf("read ready gateway DNS metrics: %v", err)
	}
	requireReadyGatewayDNSMetrics(t, *readyMetrics)

	blockCommand := fmt.Sprintf("ip route replace blackhole %s/32", shellQuote(vm.GatewayIP))
	blocked, err := client.execVM(ctx, name, blockCommand)
	if err != nil {
		t.Fatalf("install guest-only gateway route fault: %v", err)
	}
	if blocked.ExitCode != 0 {
		t.Fatalf("install guest-only gateway route fault: exit=%d stdout=%q stderr=%q", blocked.ExitCode, blocked.Stdout, blocked.Stderr)
	}
	preFailureMetrics, err := client.vmMachineMetrics(ctx, vm.ID)
	if err != nil {
		t.Fatalf("read pre-failure gateway DNS metrics: %v", err)
	}
	requireReadyGatewayDNSMetrics(t, *preFailureMetrics)
	readySuccessAt := *preFailureMetrics.GatewayDNS.LastSuccessAt

	_, err = client.execVM(ctx, name, "true")
	failure := requireGatewayDNSOperationFailure(t, err, name, "path", "gateway_dns_path_failed")
	if strings.Contains(failure.raw, vm.GatewayIP) || strings.Contains(failure.raw, vm.TAPDevice) {
		t.Fatalf("gateway DNS path diagnostic leaked private allocation identity: %s", failure.raw)
	}

	degradedMetrics, err := client.vmMachineMetrics(ctx, vm.ID)
	if err != nil {
		t.Fatalf("read degraded gateway DNS metrics: %v", err)
	}
	status := degradedMetrics.GatewayDNS
	if status.Status != "degraded" || status.Reason != "path_failed" || status.GatewayDNSPath || status.ExternalDNS {
		t.Fatalf("gateway DNS path-failure metrics = %+v, want degraded path failure", status)
	}
	if status.LastSuccessAt == nil || !status.LastSuccessAt.Equal(readySuccessAt) {
		t.Fatalf("gateway DNS last success = %v, want preserved %s", status.LastSuccessAt, readySuccessAt)
	}

	stopped := stopVMAndWait(t, ctx, client, name)
	if stopped.State != models.MachineStateStopped {
		t.Fatalf("path-failure VM state after stop = %q, want stopped", stopped.State)
	}
	warm := startVMAndWait(t, ctx, client, name)
	if warm.GatewayIP != vm.GatewayIP || warm.IP != vm.IP || warm.TAPDevice != vm.TAPDevice {
		t.Fatalf("warm identity after path failure = %s/%s/%s, want %s/%s/%s",
			warm.GatewayIP, warm.IP, warm.TAPDevice,
			vm.GatewayIP, vm.IP, vm.TAPDevice,
		)
	}
	for _, transport := range []string{"udp", "tcp"} {
		result, probeErr := probeGuestDNS(ctx, client, name, warm.GatewayIP, transport)
		requireGuestDNSProbe(t, "recovered gateway "+transport, result, probeErr, transport)
	}
	recoveredMetrics, err := client.vmMachineMetrics(ctx, warm.ID)
	if err != nil {
		t.Fatalf("read recovered gateway DNS metrics: %v", err)
	}
	requireReadyGatewayDNSMetrics(t, *recoveredMetrics)
	if !recoveredMetrics.GatewayDNS.LastSuccessAt.After(readySuccessAt) {
		t.Fatalf("recovered gateway DNS last success = %s, want after %s", recoveredMetrics.GatewayDNS.LastSuccessAt, readySuccessAt)
	}

	if err := client.deleteVM(ctx, name); err != nil {
		t.Fatalf("delete recovered gateway DNS VM: %v", err)
	}
	deleted = true
	if err := waitForNoTap(ctx, vm.TAPDevice); err != nil {
		t.Fatal(err)
	}
	listeners, err := gatewayDNSListeners(vm.GatewayIP)
	if err != nil {
		t.Fatalf("inspect recovered VM bringdown listeners: %v", err)
	}
	if listeners.UDP || listeners.TCP {
		t.Fatalf("gateway DNS listeners survived recovered VM bringdown: %s", listeners)
	}
	requireVMBringdown(t, client, bringdownBaseline, vm)
}

type gatewayDNSOperationFailure struct {
	raw string
}

type gatewayDNSOperationDiagnostic struct {
	Category  string `json:"category"`
	Phase     string `json:"phase"`
	Step      string `json:"step"`
	MachineID string `json:"machine_id"`
	State     string `json:"state"`
	Mode      string `json:"mode"`
	Retryable bool   `json:"retryable"`
	Error     string `json:"error"`
}

func requireGatewayDNSOperationFailure(t *testing.T, err error, vmName, failureKind, detail string) gatewayDNSOperationFailure {
	t.Helper()
	statusErr, ok := err.(*apiStatusError)
	if !ok {
		t.Fatalf("gateway DNS %s failure = %T %v, want structured API error", failureKind, err, err)
	}
	if statusErr.StatusCode != http.StatusConflict {
		t.Fatalf("gateway DNS %s status = %d, want 409; body: %s", failureKind, statusErr.StatusCode, statusErr.Body)
	}
	var body struct {
		Code        string                          `json:"code"`
		Phase       string                          `json:"phase"`
		Resource    string                          `json:"resource"`
		Action      string                          `json:"action"`
		Retryable   bool                            `json:"retryable"`
		Diagnostics []gatewayDNSOperationDiagnostic `json:"diagnostics"`
	}
	if decodeErr := json.Unmarshal([]byte(statusErr.Body), &body); decodeErr != nil {
		t.Fatalf("decode gateway DNS %s failure: %v; body: %s", failureKind, decodeErr, statusErr.Body)
	}
	if body.Code != "gateway_dns_not_ready" || body.Phase != "gateway_dns_readiness" || body.Resource != vmName || body.Action != "verify" || !body.Retryable {
		t.Fatalf("gateway DNS %s operation identity = code:%q phase:%q resource:%q action:%q retryable:%t; body: %s",
			failureKind, body.Code, body.Phase, body.Resource, body.Action, body.Retryable, statusErr.Body)
	}
	if len(body.Diagnostics) != 1 {
		t.Fatalf("gateway DNS %s diagnostics = %+v, want one; body: %s", failureKind, body.Diagnostics, statusErr.Body)
	}
	diagnostic := body.Diagnostics[0]
	if diagnostic.Category != "provider_network" || diagnostic.Phase != "gateway_dns_readiness" || diagnostic.State != failureKind || diagnostic.Error != detail || !diagnostic.Retryable {
		t.Fatalf("gateway DNS %s diagnostic = %+v; body: %s", failureKind, diagnostic, statusErr.Body)
	}
	if diagnostic.MachineID == "" || (diagnostic.Mode != "udp" && diagnostic.Mode != "tcp") || diagnostic.Step == "" {
		t.Fatalf("gateway DNS %s diagnostic lacks bounded failure location: %+v", failureKind, diagnostic)
	}
	return gatewayDNSOperationFailure{raw: statusErr.Body}
}
