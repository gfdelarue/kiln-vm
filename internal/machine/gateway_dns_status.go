package machine

import (
	"strings"
	"time"

	"github.com/gfdelarue/kiln-vm/internal/models"
)

type gatewayDNSNetworkStatusProvider interface {
	GatewayDNSStatus(*Allocation) GatewayDNSNetworkStatus
}

// GatewayDNSProviderStatus projects provider-owned network components and the
// most recent guest probe into the authenticated public metrics schema. It is
// read-only and emits only bounded classifications; raw host command errors,
// resolver addresses, query names, and guest output never cross this boundary.
func (m *Manager) GatewayDNSProviderStatus(machine *models.Machine) models.GatewayDNSStatus {
	if m == nil || machine == nil || m.cfg == nil || machine.Backend != firecrackerBackend {
		return models.GatewayDNSStatus{Status: "unknown", Reason: "not_applicable"}
	}
	if machineHasPendingProviderNetworkCleanup(machine) {
		return models.GatewayDNSStatus{
			Status:       "degraded",
			Reason:       "cleanup_pending",
			DesiredRules: 2,
			LastError:    "gateway DNS provider cleanup is incomplete",
		}
	}
	if !m.cfg.GuestDNSProxyEnabled {
		return models.GatewayDNSStatus{Status: "unknown", Reason: "not_applicable"}
	}
	if machine.State != models.MachineStateRunning && machine.State != models.MachineStateCreating {
		return models.GatewayDNSStatus{Status: "unknown", Reason: "not_applicable"}
	}

	if strings.TrimSpace(machine.TAPDevice) == "" || strings.TrimSpace(machine.IP) == "" || strings.TrimSpace(machine.GatewayIP) == "" {
		return projectGatewayDNSProviderStatus(
			GatewayDNSNetworkStatus{},
			GatewayDNSReadinessStatus{},
			false,
			"network_identity_invalid",
		)
	}
	alloc, identityErr := publicAllocationForCleanup(machine)
	if identityErr != nil || alloc == nil {
		return projectGatewayDNSProviderStatus(
			GatewayDNSNetworkStatus{},
			GatewayDNSReadinessStatus{},
			false,
			"network_identity_invalid",
		)
	}
	networkProvider, ok := m.network.(gatewayDNSNetworkStatusProvider)
	if !ok {
		return projectGatewayDNSProviderStatus(
			GatewayDNSNetworkStatus{},
			GatewayDNSReadinessStatus{},
			false,
			"provider_status_unavailable",
		)
	}
	networkStatus := networkProvider.GatewayDNSStatus(alloc)
	readiness, readinessObserved := m.GatewayDNSReadinessStatus(machine.ID)
	return projectGatewayDNSProviderStatus(networkStatus, readiness, readinessObserved, "")
}

func machineHasPendingProviderNetworkCleanup(machine *models.Machine) bool {
	if machine == nil {
		return false
	}
	for _, diagnostic := range machine.Diagnostics {
		if diagnostic.Category == "provider_network" && diagnostic.Phase == "cleanup" && diagnostic.Retryable {
			return true
		}
	}
	return false
}

func projectGatewayDNSProviderStatus(
	networkStatus GatewayDNSNetworkStatus,
	readiness GatewayDNSReadinessStatus,
	readinessObserved bool,
	forcedReason string,
) models.GatewayDNSStatus {
	result := models.GatewayDNSStatus{
		Status:       "unknown",
		DesiredRules: 2,
		InFlight:     networkStatus.Proxy.InFlight,
	}
	if networkStatus.Firewall.UDPInstalled {
		result.InstalledRules++
	}
	if networkStatus.Firewall.TCPInstalled {
		result.InstalledRules++
	}
	result.LastCheckedAt = latestGatewayDNSTime(networkStatus.Firewall.CheckedAt, readiness.CheckedAt)
	result.LastSuccessAt = gatewayDNSTimePointer(readiness.LastSuccessAt)

	setDegraded := func(reason, detail string) models.GatewayDNSStatus {
		result.Status = "degraded"
		result.Reason = reason
		result.LastError = detail
		return result
	}
	if forcedReason != "" {
		detail := "gateway DNS provider status is unavailable"
		if forcedReason == "network_identity_invalid" {
			detail = "gateway DNS public network identity is incomplete or invalid"
		}
		return setDegraded(forcedReason, detail)
	}
	if networkStatus.AllocationState == string(networkAllocationCleanupPending) || networkStatus.AllocationState == string(networkAllocationReleasing) {
		return setDegraded("cleanup_pending", "gateway DNS provider cleanup is incomplete")
	}
	if !networkStatus.Desired {
		return setDegraded("network_not_desired", "gateway DNS network intent is not active")
	}
	if !networkStatus.HostObserved {
		return setDegraded("host_status_unavailable", "physical-host network status has not been observed")
	}
	if networkStatus.Host.TAPOwnershipCollision {
		return setDegraded("tap_ownership_collision", "physical-host TAP identity collides with an unowned link; inspect the conflicting link")
	}
	if !networkStatus.Host.Ready {
		return setDegraded("host_network_unready", "physical-host TAP, forwarding, or NAT state is incomplete")
	}
	if !networkStatus.ProxyObserved || !networkStatus.Proxy.Running {
		return setDegraded("listener_missing", "gateway DNS listener is not running")
	}
	if networkStatus.Firewall.OwnershipCollision {
		return setDegraded("firewall_ownership_collision", "gateway DNS firewall ownership namespace collides with unowned state; inspect the reserved chain")
	}
	if !networkStatus.Firewall.Ready {
		return setDegraded("admission_missing", "gateway DNS admission rules are incomplete")
	}
	if networkStatus.LastError != "" {
		return setDegraded("network_reconciliation_failed", "gateway DNS network reconciliation is incomplete")
	}
	if !readinessObserved || !readiness.Applicable || readiness.CheckedAt.IsZero() {
		result.Reason = "not_checked"
		return result
	}

	result.GatewayDNSPath = readiness.PathOK
	result.ExternalDNS = readiness.ResolutionOK
	if !readiness.Ready {
		switch readiness.Detail {
		case gatewayDNSDetailPathFailed:
			return setDegraded("path_failed", "guest could not reach gateway DNS over UDP and TCP")
		case gatewayDNSDetailResolutionFailed:
			return setDegraded("external_resolution_failed", "gateway DNS could not complete external recursive resolution")
		case gatewayDNSDetailProbeInvalid:
			return setDegraded("probe_output_invalid", "gateway DNS probe returned invalid structured output")
		default:
			return setDegraded("probe_unavailable", "gateway DNS readiness probe is unavailable")
		}
	}

	result.Status = "ready"
	result.Reason = ""
	result.LastError = ""
	return result
}

func latestGatewayDNSTime(values ...time.Time) *time.Time {
	var latest time.Time
	for _, value := range values {
		if value.After(latest) {
			latest = value
		}
	}
	return gatewayDNSTimePointer(latest)
}

func gatewayDNSTimePointer(value time.Time) *time.Time {
	if value.IsZero() {
		return nil
	}
	copy := value.UTC()
	return &copy
}
