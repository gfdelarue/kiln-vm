//go:build system && service

package systemtest

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gfdelarue/kiln-vm/internal/models"
)

func TestNetworkServiceGatewayDNSProxyRecoversAfterDaemonRestart(t *testing.T) {
	client := requireClient(t)
	cleanupResourcesByPrefix(t, client)
	requireDaemonRestartSupport(t, client)

	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	bringdownBaseline := captureBringdownBaseline(t, ctx, client)

	name := uniqueName("gateway-dns-proxy-restart")
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
		t.Fatalf("create restart gateway DNS VM: %v", err)
	}
	deleted := false
	t.Cleanup(func() {
		if deleted {
			return
		}
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cleanupCancel()
		if err := client.deleteVM(cleanupCtx, name); err != nil {
			t.Logf("cleanup restart gateway DNS VM %s: %v", name, err)
		}
	})

	vm, err = client.waitForVMState(ctx, name, models.MachineStateRunning)
	if err != nil {
		t.Fatalf("wait for restart gateway DNS VM: %v", err)
	}
	for _, transport := range []string{"udp", "tcp"} {
		result, probeErr := probeGuestDNS(ctx, client, name, vm.GatewayIP, transport)
		requireGuestDNSProbe(t, "pre-restart gateway "+transport, result, probeErr, transport)
	}
	beforeMetrics, err := client.vmMachineMetrics(ctx, vm.ID)
	if err != nil {
		t.Fatalf("read pre-restart gateway DNS metrics: %v", err)
	}
	requireReadyGatewayDNSMetrics(t, *beforeMetrics)
	beforeListeners, err := gatewayDNSListeners(vm.GatewayIP)
	if err != nil {
		t.Fatalf("inspect pre-restart gateway DNS listeners: %v", err)
	}
	if !beforeListeners.UDP || !beforeListeners.TCP {
		t.Fatalf("pre-restart gateway DNS listeners = %s, want UDP and TCP", beforeListeners)
	}

	restartDaemon(t, client)

	warm, err := client.waitForVMState(ctx, name, models.MachineStateRunning)
	if err != nil {
		t.Fatalf("wait for power-always VM after kiln-vmd restart: %v", err)
	}
	if warm.GatewayIP != vm.GatewayIP || warm.IP != vm.IP || warm.TAPDevice != vm.TAPDevice {
		t.Fatalf("post-restart identity = %s/%s/%s, want %s/%s/%s",
			warm.GatewayIP, warm.IP, warm.TAPDevice,
			vm.GatewayIP, vm.IP, vm.TAPDevice,
		)
	}
	for _, transport := range []string{"udp", "tcp"} {
		result, probeErr := probeGuestDNS(ctx, client, name, warm.GatewayIP, transport)
		requireGuestDNSProbe(t, "post-restart gateway "+transport, result, probeErr, transport)
	}
	afterMetrics, err := client.vmMachineMetrics(ctx, warm.ID)
	if err != nil {
		t.Fatalf("read post-restart gateway DNS metrics: %v", err)
	}
	requireReadyGatewayDNSMetrics(t, *afterMetrics)
	afterListeners, err := gatewayDNSListeners(warm.GatewayIP)
	if err != nil {
		t.Fatalf("inspect post-restart gateway DNS listeners: %v", err)
	}
	if !afterListeners.UDP || !afterListeners.TCP {
		t.Fatalf("post-restart gateway DNS listeners = %s, want recreated UDP and TCP proxy", afterListeners)
	}

	if err := client.deleteVM(ctx, name); err != nil {
		t.Fatalf("delete restart gateway DNS VM: %v", err)
	}
	deleted = true
	if err := waitForNoTap(ctx, vm.TAPDevice); err != nil {
		t.Fatal(err)
	}
	deletedListeners, err := gatewayDNSListeners(vm.GatewayIP)
	if err != nil {
		t.Fatalf("inspect post-delete gateway DNS listeners: %v", err)
	}
	if deletedListeners.UDP || deletedListeners.TCP {
		t.Fatalf("gateway DNS listeners survived restart-test bringdown: %s", deletedListeners)
	}
	requireVMBringdown(t, client, bringdownBaseline, vm)
}

func TestNetworkServiceGatewayDNSDistinguishesExternalResolutionFailure(t *testing.T) {
	client := requireClient(t)
	cleanupResourcesByPrefix(t, client)
	requireDaemonRestartSupport(t, client)

	const unavailableProbeName = "kiln-vm-gateway-dns-acceptance.invalid."
	_ = withDaemonEnvironmentOverride(t, client, map[string]string{
		"KILN_VM_GUEST_DNS_PROBE_NAME": unavailableProbeName,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	bringdownBaseline := captureBringdownBaseline(t, ctx, client)
	name := uniqueName("gateway-dns-external-failure")
	vm, err := client.createVM(ctx, models.CreateVMRequest{
		Name:       name,
		Profile:    "alpine",
		DiskSizeMB: 1024,
		CPU:        &models.CPUConfig{VCPUs: 1},
		MemoryMB:   512,
	})
	if err != nil {
		t.Fatalf("create external-resolution-failure VM: %v", err)
	}
	deleted := false
	t.Cleanup(func() {
		if deleted {
			return
		}
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cleanupCancel()
		if err := client.deleteVM(cleanupCtx, name); err != nil {
			t.Logf("cleanup external-resolution-failure VM %s: %v", name, err)
		}
	})

	vm, err = client.waitForVMState(ctx, name, models.MachineStateRunning)
	if err != nil {
		t.Fatalf("wait for external-resolution-failure VM: %v", err)
	}
	_, err = client.execVM(ctx, name, "true")
	failure := requireGatewayDNSOperationFailure(t, err, name, "resolution", "gateway_dns_resolution_failed")
	for _, forbidden := range []string{unavailableProbeName, strings.TrimSuffix(unavailableProbeName, "."), vm.GatewayIP, vm.TAPDevice} {
		if forbidden != "" && strings.Contains(failure.raw, forbidden) {
			t.Fatalf("external-resolution diagnostic leaked private probe detail %q: %s", forbidden, failure.raw)
		}
	}

	metrics, err := client.vmMachineMetrics(ctx, vm.ID)
	if err != nil {
		t.Fatalf("read external-resolution-failure metrics: %v", err)
	}
	status := metrics.GatewayDNS
	if status.Status != "degraded" || status.Reason != "external_resolution_failed" || !status.GatewayDNSPath || status.ExternalDNS {
		t.Fatalf("external-resolution-failure metrics = %+v, want healthy gateway path with failed external resolution", status)
	}
	if status.DesiredRules != 2 || status.InstalledRules != 2 || status.LastCheckedAt == nil || status.LastSuccessAt != nil {
		t.Fatalf("external-resolution-failure observation = %+v, want admitted checked path without prior success", status)
	}
	if directErr := probeHostDNS("udp4", vm.GatewayIP); directErr != nil {
		t.Logf("default-name physical-host proxy control unavailable (diagnostic only): %v", directErr)
	} else {
		t.Log("default-name physical-host proxy control answered while the configured .invalid probe remained a resolution failure")
	}

	if err := client.deleteVM(ctx, name); err != nil {
		t.Fatalf("delete external-resolution-failure VM: %v", err)
	}
	deleted = true
	if err := waitForNoTap(ctx, vm.TAPDevice); err != nil {
		t.Fatal(err)
	}
	listeners, err := gatewayDNSListeners(vm.GatewayIP)
	if err != nil {
		t.Fatalf("inspect external-resolution bringdown listeners: %v", err)
	}
	if listeners.UDP || listeners.TCP {
		t.Fatalf("gateway DNS listeners survived external-resolution bringdown: %s", listeners)
	}
	requireVMBringdown(t, client, bringdownBaseline, vm)
}
