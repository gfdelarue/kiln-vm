package machine

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gfdelarue/kiln-vm/internal/config"
	"github.com/gfdelarue/kiln-vm/internal/models"
)

func TestProjectGatewayDNSProviderStatusReportsReadyWithoutPrivateProbeIdentity(t *testing.T) {
	checkedAt := time.Date(2026, 7, 21, 18, 0, 0, 0, time.UTC)
	lastSuccess := checkedAt.Add(-time.Minute)
	status := projectGatewayDNSProviderStatus(
		GatewayDNSNetworkStatus{
			Desired:       true,
			Ready:         true,
			HostObserved:  true,
			Host:          PublicNetworkHostStatus{Ready: true},
			ProxyObserved: true,
			Proxy:         DNSProxyStatus{Running: true, InFlight: 3},
			Firewall: GatewayDNSFirewallStatus{
				Desired:       true,
				HookInstalled: true,
				UDPInstalled:  true,
				TCPInstalled:  true,
				Ready:         true,
				CheckedAt:     checkedAt,
			},
		},
		GatewayDNSReadinessStatus{
			Applicable:    true,
			Ready:         true,
			PathOK:        true,
			ResolutionOK:  true,
			Detail:        gatewayDNSDetailReady,
			CheckedAt:     checkedAt,
			LastSuccessAt: lastSuccess,
		},
		true,
		"",
	)
	if status.Status != "ready" || status.Reason != "" || !status.GatewayDNSPath || !status.ExternalDNS {
		t.Fatalf("projected status = %+v, want ready", status)
	}
	if status.DesiredRules != 2 || status.InstalledRules != 2 || status.InFlight != 3 {
		t.Fatalf("projected component counts = %+v", status)
	}
	if status.LastCheckedAt == nil || !status.LastCheckedAt.Equal(checkedAt) || status.LastSuccessAt == nil || !status.LastSuccessAt.Equal(lastSuccess) {
		t.Fatalf("projected timestamps = %+v", status)
	}
	if status.LastError != "" {
		t.Fatalf("ready status exposed an error: %q", status.LastError)
	}
}

func TestProjectGatewayDNSProviderStatusClassifiesComponentsBeforeGuestProbe(t *testing.T) {
	tests := []struct {
		name       string
		network    GatewayDNSNetworkStatus
		wantReason string
	}{
		{name: "intent", network: GatewayDNSNetworkStatus{}, wantReason: "network_not_desired"},
		{name: "host status", network: GatewayDNSNetworkStatus{Desired: true}, wantReason: "host_status_unavailable"},
		{name: "host network", network: GatewayDNSNetworkStatus{Desired: true, HostObserved: true}, wantReason: "host_network_unready"},
		{name: "listener", network: GatewayDNSNetworkStatus{Desired: true, HostObserved: true, Host: PublicNetworkHostStatus{Ready: true}}, wantReason: "listener_missing"},
		{
			name: "admission",
			network: GatewayDNSNetworkStatus{
				Desired: true, HostObserved: true, Host: PublicNetworkHostStatus{Ready: true}, ProxyObserved: true, Proxy: DNSProxyStatus{Running: true},
			},
			wantReason: "admission_missing",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			status := projectGatewayDNSProviderStatus(test.network, GatewayDNSReadinessStatus{}, false, "")
			if status.Status != "degraded" || status.Reason != test.wantReason || status.LastError == "" {
				t.Fatalf("projected status = %+v, want degraded %s", status, test.wantReason)
			}
		})
	}
}

func TestProjectGatewayDNSProviderStatusClassifiesProcessLocalCleanupPending(t *testing.T) {
	for _, allocationState := range []networkAllocationState{networkAllocationCleanupPending, networkAllocationReleasing} {
		t.Run(string(allocationState), func(t *testing.T) {
			status := projectGatewayDNSProviderStatus(
				GatewayDNSNetworkStatus{
					Desired:         true,
					AllocationState: string(allocationState),
					Firewall: GatewayDNSFirewallStatus{
						UDPInstalled: true,
						TCPInstalled: true,
					},
					LastError: "private host cleanup output 172.16.0.1",
				},
				GatewayDNSReadinessStatus{},
				false,
				"",
			)

			if status.Status != "degraded" || status.Reason != "cleanup_pending" {
				t.Fatalf("projected status = %+v, want degraded cleanup_pending", status)
			}
			if status.InstalledRules != 2 {
				t.Fatalf("projected installed rules = %d, want retained cached count 2", status.InstalledRules)
			}
			if status.LastError != "gateway DNS provider cleanup is incomplete" {
				t.Fatalf("projected cleanup error = %q, want bounded operator message", status.LastError)
			}
			if strings.Contains(status.LastError, "172.16.0.1") || strings.Contains(status.LastError, "private host") {
				t.Fatalf("cleanup status leaked raw provider detail: %+v", status)
			}
		})
	}
}

func TestProjectGatewayDNSProviderStatusClassifiesOwnershipCollisions(t *testing.T) {
	tests := []struct {
		name        string
		network     GatewayDNSNetworkStatus
		wantReason  string
		wantMessage string
	}{
		{
			name: "TAP ownership",
			network: GatewayDNSNetworkStatus{
				Desired:      true,
				HostObserved: true,
				Host: PublicNetworkHostStatus{
					TAPPresent:            true,
					TAPOwnershipCollision: true,
				},
				LastError: "raw ip link output with foreign alias secret",
			},
			wantReason:  "tap_ownership_collision",
			wantMessage: "physical-host TAP identity collides with an unowned link; inspect the conflicting link",
		},
		{
			name: "firewall ownership",
			network: GatewayDNSNetworkStatus{
				Desired:       true,
				HostObserved:  true,
				Host:          PublicNetworkHostStatus{Ready: true},
				ProxyObserved: true,
				Proxy:         DNSProxyStatus{Running: true},
				Firewall: GatewayDNSFirewallStatus{
					OwnershipCollision: true,
				},
				LastError: "raw iptables output with foreign chain secret",
			},
			wantReason:  "firewall_ownership_collision",
			wantMessage: "gateway DNS firewall ownership namespace collides with unowned state; inspect the reserved chain",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			status := projectGatewayDNSProviderStatus(test.network, GatewayDNSReadinessStatus{}, false, "")
			if status.Status != "degraded" || status.Reason != test.wantReason || status.LastError != test.wantMessage {
				t.Fatalf("projected status = %+v, want degraded %s", status, test.wantReason)
			}
			if strings.Contains(status.LastError, "secret") || strings.Contains(status.LastError, "raw") {
				t.Fatalf("collision status leaked raw provider detail: %+v", status)
			}
		})
	}
}

func TestProjectGatewayDNSProviderStatusNeverLeaksRawNetworkOrProbeDetails(t *testing.T) {
	privateValues := []string{"example.com", "172.16.0.5", "/run/kiln-vm/provider-runtime", "secret stderr"}
	status := projectGatewayDNSProviderStatus(
		GatewayDNSNetworkStatus{
			Desired:       true,
			HostObserved:  true,
			Host:          PublicNetworkHostStatus{Ready: true},
			ProxyObserved: true,
			Proxy:         DNSProxyStatus{Running: true},
			Firewall: GatewayDNSFirewallStatus{
				Desired: true, UDPInstalled: true, TCPInstalled: true, Ready: true,
			},
			LastError: strings.Join(privateValues, " "),
		},
		GatewayDNSReadinessStatus{
			Applicable: true, FailureKind: "path", Detail: gatewayDNSDetailPathFailed,
		},
		true,
		"",
	)
	serialized := status.Status + " " + status.Reason + " " + status.LastError
	for _, private := range privateValues {
		if strings.Contains(serialized, private) {
			t.Fatalf("public gateway DNS status leaked %q: %+v", private, status)
		}
	}
}

func TestProjectGatewayDNSProviderStatusClassifiesInvalidPublicIdentity(t *testing.T) {
	status := projectGatewayDNSProviderStatus(
		GatewayDNSNetworkStatus{},
		GatewayDNSReadinessStatus{},
		false,
		"network_identity_invalid",
	)
	if status.Status != "degraded" || status.Reason != "network_identity_invalid" || status.DesiredRules != 2 {
		t.Fatalf("projected status = %+v, want degraded invalid identity", status)
	}
}

func TestGatewayDNSProviderStatusKeepsRetryableCleanupVisible(t *testing.T) {
	for _, proxyEnabled := range []bool{true, false} {
		manager := &Manager{cfg: &config.Config{GuestDNSProxyEnabled: proxyEnabled}}
		for _, state := range []models.MachineState{models.MachineStateStopping, models.MachineStateError} {
			t.Run(fmt.Sprintf("enabled=%t/%s", proxyEnabled, state), func(t *testing.T) {
				machine := &models.Machine{
					Backend: firecrackerBackend,
					State:   state,
					Diagnostics: []models.Diagnostic{{
						Category:  "provider_network",
						Phase:     "cleanup",
						Retryable: true,
						Error:     "private raw host failure",
					}},
				}
				status := manager.GatewayDNSProviderStatus(machine)
				if status.Status != "degraded" || status.Reason != "cleanup_pending" || status.DesiredRules != 2 || status.LastError == "" {
					t.Fatalf("GatewayDNSProviderStatus = %+v, want bounded cleanup_pending degradation", status)
				}
				if strings.Contains(status.LastError, "private raw host failure") {
					t.Fatalf("cleanup status leaked raw host error: %+v", status)
				}
			})
		}
	}
}
