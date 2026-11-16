package machine

import (
	"context"
	"crypto/sha256"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultBaseCIDR              = "10.231.0.0/16"
	networkHostCommandTimeout    = 10 * time.Second
	networkIptablesWaitSeconds   = "5"
	networkRuleRemovalLimit      = 1024
	networkIptablesCommentBase   = "kiln-vm-public-network:v1"
	networkTAPAliasBase          = "kiln-vm-public-network:v1"
	defaultNetworkReconcileEvery = 15 * time.Second
)

type networkAllocationState string

const (
	networkAllocationActivating     networkAllocationState = "activating"
	networkAllocationActive         networkAllocationState = "active"
	networkAllocationReleasing      networkAllocationState = "releasing"
	networkAllocationCleanupPending networkAllocationState = "cleanup_pending"
)

type networkAllocationRecord struct {
	Allocation   Allocation
	State        networkAllocationState
	Host         PublicNetworkHostStatus
	HostObserved bool
	LastError    string
}

// PublicNetworkHostStatus is the most recently observed physical-host state
// required by one public allocation. It is cached on the allocation record so
// metrics and readiness paths never execute privileged commands.
type PublicNetworkHostStatus struct {
	TAPPresent            bool
	TAPIsTAP              bool
	TAPOwned              bool
	TAPOwnershipCollision bool
	TAPAddressInstalled   bool
	TAPLinkUp             bool
	IPv4Forwarding        bool
	MasqueradeRules       int
	ForwardIngressRules   int
	ForwardEgressRules    int
	Ready                 bool
	LastError             string
	CheckedAt             time.Time
}

// GatewayDNSNetworkStatus is a cached, command-free snapshot of the
// physical-host components serving one public allocation. It is intentionally
// internal; Manager projects only sanitized fields into the public API.
type GatewayDNSNetworkStatus struct {
	Desired         bool
	Ready           bool
	AllocationState string
	HostObserved    bool
	Host            PublicNetworkHostStatus
	ProxyObserved   bool
	Proxy           DNSProxyStatus
	Firewall        GatewayDNSFirewallStatus
	LastError       string
}

type publicNetworkHostOperations interface {
	// CreateTAP returns owned=true only when the failed call may have left a
	// kiln-vm-created TAP behind. Callers must never run generic cleanup after an
	// unowned collision or another pre-ownership failure.
	CreateTAP(Allocation) (owned bool, err error)
	SetupNAT(tapName, guestIP string) error
	TeardownNAT(tapName, guestIP string) error
	DeleteTAP(Allocation) error
	ReconcileActive(Allocation) (PublicNetworkHostStatus, error)
}

type dnsProxyController interface {
	Start(gatewayIP string) error
	Stop(gatewayIP string) error
	Status(gatewayIP string) (DNSProxyStatus, bool)
}

type gatewayDNSFirewallController interface {
	Guard(Allocation) error
	Activate(Allocation) error
	Deactivate(Allocation) error
	Release(Allocation) error
	ReconcileState(guarded, admitted []Allocation) error
	Status(Allocation) GatewayDNSFirewallStatus
}

type networkHostCommandRunner interface {
	Run(context.Context, string, ...string) ([]byte, error)
}

type execNetworkHostCommandRunner struct{}

func (execNetworkHostCommandRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	output, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return output, fmt.Errorf("run network host command %s: %w", name, err)
	}
	return output, nil
}

type execPublicNetworkHostOperations struct {
	runner         networkHostCommandRunner
	commandTimeout time.Duration
	forwardingPath string
	readFile       func(string) ([]byte, error)
	writeFile      func(string, []byte, os.FileMode) error
	now            func() time.Time
}

func newExecPublicNetworkHostOperations() *execPublicNetworkHostOperations {
	return &execPublicNetworkHostOperations{
		runner:         execNetworkHostCommandRunner{},
		commandTimeout: networkHostCommandTimeout,
		forwardingPath: "/proc/sys/net/ipv4/ip_forward",
		readFile:       os.ReadFile,
		writeFile:      os.WriteFile,
		now:            func() time.Time { return time.Now().UTC() },
	}
}

// NetworkManager handles TAP device and IP allocation for VMs.
type NetworkManager struct {
	mu              sync.Mutex
	lifecycleMu     sync.Mutex
	baseCIDR        string
	nextIdx         uint32 // index into /30 subnets
	dnsProxy        dnsProxyController
	dnsProxyEnabled bool
	dnsFirewall     gatewayDNSFirewallController
	host            publicNetworkHostOperations
	allocations     map[string]networkAllocationRecord
}

func NewNetworkManagerWithDNSDataDir(baseCIDR string, startIdx uint32, dnsProxy *DNSProxyManager, dataDir string) *NetworkManager {
	ownershipPath := ""
	if strings.TrimSpace(dataDir) != "" {
		ownershipPath = filepath.Join(dataDir, "network", gatewayDNSFirewallOwnerFile)
	}
	return newNetworkManagerWithDNSOwnershipPath(baseCIDR, startIdx, dnsProxy, ownershipPath)
}

func newNetworkManagerWithDNSOwnershipPath(baseCIDR string, startIdx uint32, dnsProxy *DNSProxyManager, ownershipPath string) *NetworkManager {
	var dnsFirewall *GatewayDNSFirewall
	if dnsProxy != nil && dnsProxy.enabled {
		dnsFirewall = NewGatewayDNSFirewallWithOwnershipPath(baseCIDR, ownershipPath)
	}
	return newNetworkManager(baseCIDR, startIdx, dnsProxy, dnsProxy != nil && dnsProxy.enabled, dnsFirewall, newExecPublicNetworkHostOperations())
}

func newNetworkManager(baseCIDR string, startIdx uint32, dnsProxy dnsProxyController, dnsProxyEnabled bool, dnsFirewall gatewayDNSFirewallController, host publicNetworkHostOperations) *NetworkManager {
	if startIdx < 1 {
		startIdx = 1
	}
	if host == nil {
		host = newExecPublicNetworkHostOperations()
	}
	return &NetworkManager{
		baseCIDR:        baseCIDR,
		nextIdx:         startIdx,
		dnsProxy:        dnsProxy,
		dnsProxyEnabled: dnsProxyEnabled,
		dnsFirewall:     dnsFirewall,
		host:            host,
		allocations:     make(map[string]networkAllocationRecord),
	}
}

// Allocation holds the network config for a single VM.
type Allocation struct {
	TAPDevice    string
	GuestIP      string // VM IP (second usable in /30)
	GatewayIP    string // host-side IP (first usable in /30)
	GuestMAC     string
	Mask         string
	BaseCIDR     string // immutable source CIDR used to derive this allocation
	NetworkIndex uint32 // immutable provider allocation generation/index
	OwnerID      string // immutable owning machine ID; journaled by the machine row
}

// Plan returns the next immutable /30 identity without touching the physical
// host. The machine manager persists this tuple before Reserve starts the
// crash-recoverable host transaction.
func (nm *NetworkManager) Plan(vmID string) (*Allocation, error) {
	ownerID := strings.TrimSpace(vmID)
	if ownerID == "" {
		return nil, fmt.Errorf("plan public network: machine owner ID is required")
	}
	nm.mu.Lock()
	idx := nm.nextIdx
	nm.nextIdx++
	nm.mu.Unlock()
	alloc, err := nm.allocationForIndex(idx)
	if err != nil {
		return nil, err
	}
	alloc.OwnerID = ownerID
	return alloc, nil
}

// Allocate is retained for direct NetworkManager consumers and focused host
// tests. Durable machine lifecycle code uses Plan, persists the tuple, then
// calls Reserve. Keeping the combined helper here does not make it suitable
// for a durable controller transaction.
func (nm *NetworkManager) Allocate(vmID string) (*Allocation, error) {
	for attempt := 0; attempt < 1024; attempt++ {
		alloc, err := nm.Plan(vmID)
		if err != nil {
			return nil, err
		}

		reserved, err := nm.Reserve(alloc)
		if err != nil {
			if tapAlreadyExistsError(err) && reserved == nil {
				continue
			}
			return reserved, err
		}
		return reserved, nil
	}
	return nil, fmt.Errorf("allocate TAP: no free kiln-vm TAP device found")
}

// Reserve recreates a previously assigned public VM network allocation. It is
// used when a stopped machine starts again so its public IP remains stable.
func (nm *NetworkManager) Reserve(alloc *Allocation) (*Allocation, error) {
	nm.lifecycleMu.Lock()
	defer nm.lifecycleMu.Unlock()

	if alloc == nil {
		return nil, fmt.Errorf("reserve TAP: allocation is incomplete")
	}
	reserved, idx, err := nm.validateCanonicalAllocation(*alloc)
	if err != nil {
		return nil, fmt.Errorf("reserve public network: %w", err)
	}
	nm.setAllocationState(reserved, networkAllocationActivating, nil)
	cleanupPending, err := nm.activateAllocation(&reserved)
	if err != nil {
		err = fmt.Errorf("reserve public network %s: %w", reserved.TAPDevice, err)
		if cleanupPending {
			return &reserved, err
		}
		return nil, err
	}
	nm.mu.Lock()
	if idx >= nm.nextIdx {
		nm.nextIdx = idx + 1
	}
	nm.mu.Unlock()
	nm.setAllocationState(reserved, networkAllocationActive, nil)
	return &reserved, nil
}

func (nm *NetworkManager) validateCanonicalAllocation(alloc Allocation) (Allocation, uint32, error) {
	ownerID := strings.TrimSpace(alloc.OwnerID)
	if ownerID == "" {
		return Allocation{}, 0, fmt.Errorf("allocation owner ID is required")
	}
	idx, ok := networkIndexFromTAP(alloc.TAPDevice)
	if !ok || alloc.TAPDevice != fmt.Sprintf("kv%d", idx) {
		return Allocation{}, 0, fmt.Errorf("invalid kiln-vm TAP identity %q", alloc.TAPDevice)
	}
	expected, err := nm.allocationForIndex(idx)
	if err != nil {
		return Allocation{}, 0, fmt.Errorf("derive canonical allocation for %s: %w", alloc.TAPDevice, err)
	}
	expected.OwnerID = ownerID
	alloc.OwnerID = ownerID
	if alloc != *expected {
		return Allocation{}, 0, fmt.Errorf(
			"allocation for %s is not the exact canonical tuple: got guest=%q gateway=%q mac=%q mask=%q; want guest=%q gateway=%q mac=%q mask=%q",
			alloc.TAPDevice,
			alloc.GuestIP,
			alloc.GatewayIP,
			alloc.GuestMAC,
			alloc.Mask,
			expected.GuestIP,
			expected.GatewayIP,
			expected.GuestMAC,
			expected.Mask,
		)
	}
	return *expected, idx, nil
}

type networkActivationStages struct {
	tap   bool
	nat   bool
	guard bool
	proxy bool
}

// activateAllocation returns cleanupPending only when a failed activation may
// have left kiln-vm-owned host stages behind. A caller may return alloc+err only
// in that case; pre-ownership failures and fully rolled-back failures return
// cleanupPending=false.
func (nm *NetworkManager) activateAllocation(alloc *Allocation) (cleanupPending bool, err error) {
	if alloc == nil {
		return false, fmt.Errorf("activate public network: allocation is nil")
	}
	stages := networkActivationStages{}
	tapOwned, err := nm.host.CreateTAP(*alloc)
	if err != nil {
		primary := fmt.Errorf("create TAP %s: %w", alloc.TAPDevice, err)
		if tapOwned {
			nm.setAllocationState(*alloc, networkAllocationCleanupPending, primary)
			return true, primary
		}
		nm.clearAllocationState(alloc.TAPDevice)
		return false, primary
	}
	if !tapOwned {
		primary := fmt.Errorf("create TAP %s: host reported success without ownership", alloc.TAPDevice)
		nm.clearAllocationState(alloc.TAPDevice)
		return false, primary
	}
	stages.tap = true
	if err := nm.host.SetupNAT(alloc.TAPDevice, alloc.GuestIP); err != nil {
		// SetupNAT performs its own rollback, but a rollback command can fail.
		// Retain full NAT cleanup intent so the generic pass retries any exact
		// rule that may have been installed before the primary failure.
		stages.nat = true
		return nm.rollbackActivation(alloc, stages, fmt.Errorf("setup NAT for %s: %w", alloc.TAPDevice, err))
	}
	stages.nat = true
	hostStatus, err := nm.host.ReconcileActive(*alloc)
	nm.setAllocationHostStatus(*alloc, hostStatus)
	if err != nil {
		return nm.rollbackActivation(alloc, stages, fmt.Errorf("reconcile physical-host network for %s: %w", alloc.TAPDevice, err))
	}
	if nm.dnsFirewall != nil {
		// Guard is desired before any listener can bind. Mark cleanup intent before
		// the call because a failed reconciliation may still have installed one of
		// the per-TAP host-local DNS DROP rules or the owned INPUT hook.
		stages.guard = true
		if err := nm.dnsFirewall.Guard(*alloc); err != nil {
			return nm.rollbackActivation(alloc, stages, fmt.Errorf("guard guest DNS for %s: %w", alloc.TAPDevice, err))
		}
	}
	if nm.dnsProxy != nil && nm.dnsProxyEnabled {
		// A listener controller may have bound one transport before reporting a
		// failure. Include proxy cleanup intent before Start so partial listeners
		// cannot escape the activation transaction.
		stages.proxy = true
	}
	if err := nm.startDNSProxy(alloc); err != nil {
		return nm.rollbackActivation(alloc, stages, err)
	}
	if nm.dnsFirewall != nil {
		// Start returns only after both UDP and TCP listeners are bound and cached
		// status confirms them running. Admission is therefore the final activation
		// edge and always retains the already-installed DROP guard behind it.
		if err := nm.dnsFirewall.Activate(*alloc); err != nil {
			return nm.rollbackActivation(alloc, stages, fmt.Errorf("setup guest DNS admission for %s: %w", alloc.TAPDevice, err))
		}
	}
	return false, nil
}

func (nm *NetworkManager) rollbackActivation(alloc *Allocation, stages networkActivationStages, primary error) (bool, error) {
	cleanupErr := nm.cleanupAllocationStages(alloc, stages)
	result := errors.Join(primary, cleanupErr)
	if cleanupErr != nil {
		nm.setAllocationState(*alloc, networkAllocationCleanupPending, result)
		return true, result
	} else {
		nm.clearAllocationState(alloc.TAPDevice)
	}
	return false, result
}

func (nm *NetworkManager) allocationForIndex(idx uint32) (*Allocation, error) {
	return allocationForIndex(nm.baseCIDR, idx)
}

func allocationForIndex(baseCIDR string, idx uint32) (*Allocation, error) {
	if idx == 0 {
		return nil, fmt.Errorf("network index must be greater than zero")
	}
	// Each /30 block: 4 IPs. Network, gateway, guest, broadcast
	// Subnet N: base + (N*4).
	baseIP, ipNet, err := parseBaseCIDR(baseCIDR)
	if err != nil {
		return nil, err
	}
	ones, bits := ipNet.Mask.Size()
	if bits != 32 || ones > 30 {
		return nil, fmt.Errorf("base CIDR %s is too small for /30 VM allocations", ipNet.String())
	}

	offset := uint64(idx) * 4
	capacity := uint64(1) << uint(32-ones)
	if offset+3 >= capacity {
		return nil, fmt.Errorf("base CIDR %s exhausted for network index %d", ipNet.String(), idx)
	}

	network := ipv4ToUint32(baseIP) + uint32(offset)
	gateway := uint32ToIPv4(network + 1)
	guest := uint32ToIPv4(network + 2)

	return &Allocation{
		TAPDevice:    fmt.Sprintf("kv%d", idx),
		GuestIP:      guest.String(),
		GatewayIP:    gateway.String(),
		GuestMAC:     guestMACForNetworkIndex(idx),
		Mask:         "255.255.255.252",
		BaseCIDR:     ipNet.String(),
		NetworkIndex: idx,
	}, nil
}

func parseBaseCIDR(baseCIDR string) (net.IP, *net.IPNet, error) {
	if strings.TrimSpace(baseCIDR) == "" {
		baseCIDR = defaultBaseCIDR
	}
	ip, ipNet, err := net.ParseCIDR(strings.TrimSpace(baseCIDR))
	if err != nil {
		return nil, nil, fmt.Errorf("parse base CIDR %q: %w", baseCIDR, err)
	}
	baseIP := ip.To4()
	if baseIP == nil {
		return nil, nil, fmt.Errorf("base CIDR %s is not IPv4", baseCIDR)
	}
	ipNet.IP = baseIP
	return baseIP, ipNet, nil
}

func ipv4ToUint32(ip net.IP) uint32 {
	v4 := ip.To4()
	return uint32(v4[0])<<24 | uint32(v4[1])<<16 | uint32(v4[2])<<8 | uint32(v4[3])
}

func uint32ToIPv4(v uint32) net.IP {
	return net.IPv4(byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
}

func guestMACForNetworkIndex(idx uint32) string {
	return fmt.Sprintf("02:FC:00:00:%02X:%02X", byte(idx>>8), byte(idx&0xff))
}

func publicNetworkTAPAlias(alloc Allocation) string {
	return fmt.Sprintf(
		"%s:%s:%s:%s:%s",
		networkTAPAliasBase,
		alloc.TAPDevice,
		alloc.GatewayIP,
		alloc.GuestIP,
		strings.ToLower(alloc.GuestMAC),
	)
}

// publicNetworkTAPStagingName is an allocation-specific creation identity.
// Linux cannot attach an ifalias in the same operation that creates a
// persistent TAP, so creating directly at the final kv<N> name leaves a
// crash window in which an alias-less final link is indistinguishable from a
// foreign collision. The machine UUID is durable before host mutation; a
// truncated SHA-256 digest gives us a <=15-byte staging name that can safely be
// recognized after a crash, before the exact alias is installed and the link
// is renamed to its final name.
func publicNetworkTAPStagingName(alloc Allocation) (string, error) {
	ownerID := strings.TrimSpace(alloc.OwnerID)
	if ownerID == "" {
		return "", fmt.Errorf("public network allocation owner ID is required")
	}
	digest := sha256.Sum256([]byte(strings.Join([]string{
		"kiln-vm-public-network-staging:v1",
		ownerID,
		alloc.TAPDevice,
		alloc.GatewayIP,
		alloc.GuestIP,
		strings.ToLower(alloc.GuestMAC),
	}, "\x00")))
	// Linux IFNAMSIZ permits 15 visible bytes. "kt" + 13 unpadded base32
	// digits carries 64 digest bits while remaining visibly kiln-vm-owned.
	token := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(digest[:8])
	return "kt" + strings.ToLower(token), nil
}

func networkIndexFromTAP(name string) (uint32, bool) {
	if !strings.HasPrefix(name, "kv") {
		return 0, false
	}
	value, err := strconv.ParseUint(strings.TrimPrefix(name, "kv"), 10, 32)
	if err != nil || value == 0 {
		return 0, false
	}
	return uint32(value), true
}

// Release tears down the TAP device and NAT rules for a VM.
func (nm *NetworkManager) Release(alloc *Allocation) error {
	nm.lifecycleMu.Lock()
	defer nm.lifecycleMu.Unlock()

	if alloc == nil {
		return nil
	}
	canonical, _, err := validateCleanupAllocation(*alloc)
	if err != nil {
		return fmt.Errorf("release public network: %w", err)
	}
	if strings.TrimSpace(canonical.OwnerID) == "" {
		return fmt.Errorf("release public network: allocation owner ID is required")
	}
	alloc = &canonical
	nm.setAllocationState(canonical, networkAllocationReleasing, nil)
	err = nm.cleanupAllocationStages(alloc, networkActivationStages{
		tap:   true,
		nat:   true,
		guard: nm.dnsFirewall != nil,
		proxy: nm.dnsProxy != nil && nm.dnsProxyEnabled,
	})
	if err != nil {
		nm.setAllocationState(*alloc, networkAllocationCleanupPending, err)
		return err
	}
	nm.clearAllocationState(alloc.TAPDevice)
	return nil
}

// validateCleanupAllocation proves a complete historical allocation without
// depending on the controller's current BridgeCIDR. That matters after a
// configuration change: cleanup must target the exact old tuple, while the
// kernel ownership alias still prevents mutation of a foreign same-name TAP.
func validateCleanupAllocation(alloc Allocation) (Allocation, uint32, error) {
	idx, ok := networkIndexFromTAP(alloc.TAPDevice)
	if !ok || alloc.TAPDevice != fmt.Sprintf("kv%d", idx) {
		return Allocation{}, 0, fmt.Errorf("invalid kiln-vm TAP identity %q", alloc.TAPDevice)
	}
	if alloc.NetworkIndex != 0 && alloc.NetworkIndex != idx {
		return Allocation{}, 0, fmt.Errorf("allocation index %d contradicts TAP %s", alloc.NetworkIndex, alloc.TAPDevice)
	}
	gateway := net.ParseIP(strings.TrimSpace(alloc.GatewayIP)).To4()
	guest := net.ParseIP(strings.TrimSpace(alloc.GuestIP)).To4()
	if gateway == nil || guest == nil {
		return Allocation{}, 0, fmt.Errorf("allocation %s requires exact IPv4 guest and gateway addresses", alloc.TAPDevice)
	}
	gatewayValue := ipv4ToUint32(gateway)
	guestValue := ipv4ToUint32(guest)
	if gatewayValue%4 != 1 || guestValue != gatewayValue+1 {
		return Allocation{}, 0, fmt.Errorf("allocation %s addresses %s/%s are not canonical /30 gateway/guest members", alloc.TAPDevice, alloc.GatewayIP, alloc.GuestIP)
	}
	if alloc.Mask != "255.255.255.252" {
		return Allocation{}, 0, fmt.Errorf("allocation %s has non-canonical mask %q", alloc.TAPDevice, alloc.Mask)
	}
	expectedMAC := guestMACForNetworkIndex(idx)
	if !strings.EqualFold(alloc.GuestMAC, expectedMAC) {
		return Allocation{}, 0, fmt.Errorf("allocation %s has non-canonical guest MAC %q", alloc.TAPDevice, alloc.GuestMAC)
	}
	canonical := alloc
	canonical.GatewayIP = gateway.String()
	canonical.GuestIP = guest.String()
	canonical.GuestMAC = expectedMAC
	canonical.NetworkIndex = idx
	if strings.TrimSpace(alloc.BaseCIDR) != "" {
		expected, err := allocationForIndex(alloc.BaseCIDR, idx)
		if err != nil {
			return Allocation{}, 0, fmt.Errorf("derive historical allocation for %s: %w", alloc.TAPDevice, err)
		}
		if canonical.TAPDevice != expected.TAPDevice || canonical.GuestIP != expected.GuestIP || canonical.GatewayIP != expected.GatewayIP || canonical.GuestMAC != expected.GuestMAC || canonical.Mask != expected.Mask {
			return Allocation{}, 0, fmt.Errorf("allocation %s contradicts historical base CIDR %s", alloc.TAPDevice, alloc.BaseCIDR)
		}
		canonical.BaseCIDR = expected.BaseCIDR
	}
	return canonical, idx, nil
}

// cleanupAllocationStages closes admission before touching the listener, and
// retains the guard until every other allocation-owned host artifact has been
// removed. A failed Deactivate must leave the listener running: otherwise an
// ACCEPT rule could be inherited by an unrelated future listener. A failed or
// unconfirmed Stop likewise retains the guard and all outer cleanup intent.
func (nm *NetworkManager) cleanupAllocationStages(alloc *Allocation, stages networkActivationStages) error {
	if alloc == nil {
		return nil
	}
	if stages.guard && nm.dnsFirewall != nil {
		if err := nm.dnsFirewall.Deactivate(*alloc); err != nil {
			return fmt.Errorf("deactivate guest DNS admission for %s: %w", alloc.TAPDevice, err)
		}
	}
	if stages.proxy && nm.dnsProxy != nil && nm.dnsProxyEnabled {
		if err := nm.dnsProxy.Stop(alloc.GatewayIP); err != nil {
			return fmt.Errorf("stop guest DNS proxy for %s: %w", alloc.TAPDevice, err)
		}
		if err := nm.confirmDNSProxyStopped(alloc); err != nil {
			return err
		}
	}

	var cleanupErrs []error
	if stages.nat {
		if err := nm.host.TeardownNAT(alloc.TAPDevice, alloc.GuestIP); err != nil {
			cleanupErrs = append(cleanupErrs, fmt.Errorf("teardown NAT for %s: %w", alloc.TAPDevice, err))
		}
	}
	if stages.tap {
		if err := nm.host.DeleteTAP(*alloc); err != nil {
			cleanupErrs = append(cleanupErrs, fmt.Errorf("delete TAP %s: %w", alloc.TAPDevice, err))
		}
	}
	if cleanupErr := errors.Join(cleanupErrs...); cleanupErr != nil {
		return cleanupErr
	}
	if stages.guard && nm.dnsFirewall != nil {
		if err := nm.dnsFirewall.Release(*alloc); err != nil {
			return fmt.Errorf("release guest DNS guard for %s: %w", alloc.TAPDevice, err)
		}
	}
	return nil
}

func (nm *NetworkManager) setAllocationState(alloc Allocation, state networkAllocationState, err error) {
	nm.mu.Lock()
	defer nm.mu.Unlock()
	if nm.allocations == nil {
		nm.allocations = make(map[string]networkAllocationRecord)
	}
	record := networkAllocationRecord{Allocation: alloc, State: state}
	if current, ok := nm.allocations[alloc.TAPDevice]; ok && current.Allocation == alloc {
		record.Host = current.Host
		record.HostObserved = current.HostObserved
	}
	if err != nil {
		record.LastError = err.Error()
	}
	nm.allocations[alloc.TAPDevice] = record
}

func (nm *NetworkManager) setAllocationHostStatus(alloc Allocation, status PublicNetworkHostStatus) {
	nm.mu.Lock()
	defer nm.mu.Unlock()
	if nm.allocations == nil {
		nm.allocations = make(map[string]networkAllocationRecord)
	}
	record, ok := nm.allocations[alloc.TAPDevice]
	if !ok || record.Allocation != alloc {
		record = networkAllocationRecord{Allocation: alloc, State: networkAllocationActivating}
	}
	record.Host = status
	record.HostObserved = true
	nm.allocations[alloc.TAPDevice] = record
}

func (nm *NetworkManager) clearAllocationState(tap string) {
	nm.mu.Lock()
	defer nm.mu.Unlock()
	delete(nm.allocations, tap)
}

func (nm *NetworkManager) allocationSnapshot() []networkAllocationRecord {
	nm.mu.Lock()
	defer nm.mu.Unlock()
	result := make([]networkAllocationRecord, 0, len(nm.allocations))
	for _, record := range nm.allocations {
		result = append(result, record)
	}
	return result
}

// GatewayDNSStatus returns the last reconciled component state without
// invoking iptables or touching a listener. Metrics and readiness paths must
// remain observational and cannot acquire privileged state as a side effect.
func (nm *NetworkManager) GatewayDNSStatus(alloc *Allocation) GatewayDNSNetworkStatus {
	if nm == nil || alloc == nil {
		return GatewayDNSNetworkStatus{}
	}

	nm.mu.Lock()
	record, recorded := nm.allocations[alloc.TAPDevice]
	nm.mu.Unlock()
	exact := recorded && record.Allocation == *alloc
	status := GatewayDNSNetworkStatus{
		AllocationState: string(record.State),
		HostObserved:    exact && record.HostObserved,
		Host:            record.Host,
		LastError:       record.LastError,
	}
	status.Desired = exact && (record.State == networkAllocationActive || record.State == networkAllocationActivating)
	if nm.dnsProxy != nil && nm.dnsProxyEnabled {
		status.Proxy, status.ProxyObserved = nm.dnsProxy.Status(alloc.GatewayIP)
	}
	if nm.dnsFirewall != nil {
		status.Firewall = nm.dnsFirewall.Status(*alloc)
	}
	status.Ready = status.Desired && status.LastError == "" && status.HostObserved && status.Host.Ready &&
		status.ProxyObserved && status.Proxy.Running && status.Firewall.Ready
	return status
}

// Reconcile repairs host, guard, listener, and admission drift and retries
// cleanup left by a failed lifecycle operation. It never invents desired
// allocations from TAPs or firewall state; the process-local registry remains
// authoritative and supplies complete guarded/admitted snapshots.
func (nm *NetworkManager) Reconcile() error {
	nm.lifecycleMu.Lock()
	defer nm.lifecycleMu.Unlock()

	records := nm.allocationSnapshot()
	admittedByTAP := make(map[string]Allocation, len(records))
	var reconcileErrs []error
	for _, record := range records {
		switch record.State {
		case networkAllocationActive, networkAllocationActivating:
			hostStatus, hostErr := nm.host.ReconcileActive(record.Allocation)
			nm.setAllocationHostStatus(record.Allocation, hostStatus)
			var allocationErrs []error
			if hostErr != nil {
				allocationErrs = append(allocationErrs, fmt.Errorf("reconcile physical-host network for %s: %w", record.Allocation.TAPDevice, hostErr))
			} else if nm.dnsFirewall != nil {
				if guardErr := nm.dnsFirewall.Guard(record.Allocation); guardErr != nil {
					allocationErrs = append(allocationErrs, fmt.Errorf("reconcile guest DNS guard for %s: %w", record.Allocation.TAPDevice, guardErr))
				}
			}
			if len(allocationErrs) == 0 {
				if proxyErr := nm.startDNSProxy(&record.Allocation); proxyErr != nil {
					allocationErrs = append(allocationErrs, proxyErr)
				}
			}
			if len(allocationErrs) == 0 && nm.dnsFirewall != nil {
				if admissionErr := nm.dnsFirewall.Activate(record.Allocation); admissionErr != nil {
					allocationErrs = append(allocationErrs, fmt.Errorf("reconcile guest DNS admission for %s: %w", record.Allocation.TAPDevice, admissionErr))
				}
			}
			if len(allocationErrs) == 0 {
				admittedByTAP[record.Allocation.TAPDevice] = record.Allocation
			} else if closeErr := nm.deactivateAndStopDNSProxy(&record.Allocation); closeErr != nil {
				allocationErrs = append(allocationErrs, closeErr)
			}
			allocationErr := errors.Join(allocationErrs...)
			if allocationErr != nil {
				reconcileErrs = append(reconcileErrs, allocationErr)
			}
			// A successful host+proxy pass deliberately clears any cached
			// LastError from a prior transient reconciliation failure.
			nm.setAllocationState(record.Allocation, networkAllocationActive, allocationErr)
		case networkAllocationReleasing, networkAllocationCleanupPending:
			err := nm.cleanupAllocationStages(&record.Allocation, networkActivationStages{
				tap:   true,
				nat:   true,
				guard: nm.dnsFirewall != nil,
				proxy: nm.dnsProxy != nil && nm.dnsProxyEnabled,
			})
			if err != nil {
				reconcileErrs = append(reconcileErrs, err)
				nm.setAllocationState(record.Allocation, networkAllocationCleanupPending, err)
			} else {
				nm.clearAllocationState(record.Allocation.TAPDevice)
			}
		}
	}
	if nm.dnsFirewall != nil {
		postRecords := nm.allocationSnapshot()
		guarded := make([]Allocation, 0, len(postRecords))
		admitted := make([]Allocation, 0, len(admittedByTAP))
		for _, record := range postRecords {
			switch record.State {
			case networkAllocationActive, networkAllocationActivating, networkAllocationReleasing, networkAllocationCleanupPending:
				guarded = append(guarded, record.Allocation)
			}
			if candidate, ok := admittedByTAP[record.Allocation.TAPDevice]; ok && candidate == record.Allocation &&
				(record.State == networkAllocationActive || record.State == networkAllocationActivating) {
				admitted = append(admitted, record.Allocation)
			}
		}
		if err := nm.dnsFirewall.ReconcileState(guarded, admitted); err != nil {
			reconcileErrs = append(reconcileErrs, fmt.Errorf("reconcile guest DNS guard/admission state: %w", err))
		}
	}
	return errors.Join(reconcileErrs...)
}

// deactivateAndStopDNSProxy closes admission before the listener. If closing
// admission fails, Stop is deliberately not attempted; if Stop fails or still
// reports running, the guard remains desired for the next reconciliation pass.
func (nm *NetworkManager) deactivateAndStopDNSProxy(alloc *Allocation) error {
	if alloc == nil {
		return nil
	}
	if nm.dnsFirewall != nil {
		if err := nm.dnsFirewall.Deactivate(*alloc); err != nil {
			return fmt.Errorf("deactivate guest DNS admission for %s: %w", alloc.TAPDevice, err)
		}
	}
	if nm.dnsProxy == nil || !nm.dnsProxyEnabled {
		return nil
	}
	if err := nm.dnsProxy.Stop(alloc.GatewayIP); err != nil {
		return fmt.Errorf("stop guest DNS proxy for %s: %w", alloc.TAPDevice, err)
	}
	return nm.confirmDNSProxyStopped(alloc)
}

func (nm *NetworkManager) StartReconciler(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = defaultNetworkReconcileEvery
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := nm.Reconcile(); err != nil {
					// Callers expose the cached component state through authenticated
					// diagnostics; avoid logging query or consumer payloads here.
					slog.Error("failed to reconcile gateway DNS network", "error", err)
				}
			}
		}
	}()
}

func (nm *NetworkManager) startDNSProxy(alloc *Allocation) error {
	if nm.dnsProxy == nil || !nm.dnsProxyEnabled {
		return nil
	}
	if err := nm.dnsProxy.Start(alloc.GatewayIP); err != nil {
		return fmt.Errorf("setup guest DNS proxy for %s: %w", alloc.TAPDevice, err)
	}
	status, observed := nm.dnsProxy.Status(alloc.GatewayIP)
	if !observed || !status.Running {
		return fmt.Errorf("setup guest DNS proxy for %s: listener did not report running after start", alloc.TAPDevice)
	}
	return nil
}

func (nm *NetworkManager) confirmDNSProxyStopped(alloc *Allocation) error {
	if nm.dnsProxy == nil || !nm.dnsProxyEnabled {
		return nil
	}
	status, observed := nm.dnsProxy.Status(alloc.GatewayIP)
	if observed && status.Running {
		return fmt.Errorf("stop guest DNS proxy for %s: listener still reports running", alloc.TAPDevice)
	}
	return nil
}

func (h *execPublicNetworkHostOperations) run(name string, args ...string) ([]byte, error) {
	runner := h.runner
	if runner == nil {
		runner = execNetworkHostCommandRunner{}
	}
	timeout := h.commandTimeout
	if timeout <= 0 {
		timeout = networkHostCommandTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	output, err := runner.Run(ctx, name, args...)
	if err != nil {
		return output, fmt.Errorf("run network host command %s: %w", name, err)
	}
	return output, nil
}

func (h *execPublicNetworkHostOperations) nowUTC() time.Time {
	if h.now != nil {
		return h.now().UTC()
	}
	return time.Now().UTC()
}

func (h *execPublicNetworkHostOperations) CreateTAP(alloc Allocation) (bool, error) {
	canonical, _, err := validateCleanupAllocation(alloc)
	if err != nil {
		return false, fmt.Errorf("create TAP: %w", err)
	}
	alloc = canonical
	name := alloc.TAPDevice
	gatewayIP := alloc.GatewayIP
	stagingName, err := publicNetworkTAPStagingName(alloc)
	if err != nil {
		return false, fmt.Errorf("create TAP: %w", err)
	}
	alias := publicNetworkTAPAlias(alloc)
	commands := [][]string{
		{"tuntap", "add", "dev", stagingName, "mode", "tap"},
		{"link", "set", "dev", stagingName, "alias", alias},
		{"link", "set", "dev", stagingName, "name", name},
		{"addr", "add", gatewayIP + "/30", "dev", name},
		{"link", "set", name, "up"},
	}
	owned := false
	for index, args := range commands {
		out, err := h.run("ip", args...)
		if err == nil {
			if index == 0 {
				owned = true
			}
			continue
		}
		primary := fmt.Errorf("ip %v: %s: %w", args, strings.TrimSpace(string(out)), err)
		if !owned {
			return false, primary
		}
		cleanupErr := h.deleteTAP(alloc)
		if cleanupErr != nil {
			return true, errors.Join(primary, cleanupErr)
		}
		return false, primary
	}
	return true, nil
}

func (h *execPublicNetworkHostOperations) DeleteTAP(alloc Allocation) error {
	canonical, _, err := validateCleanupAllocation(alloc)
	if err != nil {
		return fmt.Errorf("delete TAP: %w", err)
	}
	if strings.TrimSpace(canonical.OwnerID) == "" {
		return fmt.Errorf("delete TAP: allocation owner ID is required")
	}
	return h.deleteTAP(canonical)
}

func (h *execPublicNetworkHostOperations) deleteTAP(alloc Allocation) error {
	stagingName, err := publicNetworkTAPStagingName(alloc)
	if err != nil {
		return err
	}
	type cleanupTarget struct {
		name string
	}
	var targets []cleanupTarget
	var cleanupErrs []error

	finalLink, finalErr := h.inspectTAPLink(alloc.TAPDevice)
	if finalErr != nil {
		cleanupErrs = append(cleanupErrs, finalErr)
	} else if finalLink.present {
		switch {
		case !finalLink.isTAP:
			cleanupErrs = append(cleanupErrs, fmt.Errorf("refuse to delete unowned link collision %s (kind=%q type=%q)", alloc.TAPDevice, finalLink.kind, finalLink.tunType))
		case finalLink.alias != publicNetworkTAPAlias(alloc):
			cleanupErrs = append(cleanupErrs, fmt.Errorf("refuse to delete unowned TAP collision %s (alias=%q, want %q)", alloc.TAPDevice, finalLink.alias, publicNetworkTAPAlias(alloc)))
		default:
			targets = append(targets, cleanupTarget{name: alloc.TAPDevice})
		}
	}

	stagingLink, stagingErr := h.inspectTAPLink(stagingName)
	if stagingErr != nil {
		cleanupErrs = append(cleanupErrs, stagingErr)
	} else if stagingLink.present {
		stagingAddresses, addressErr := h.inspectTAPAddresses(stagingName)
		switch {
		case addressErr != nil:
			cleanupErrs = append(cleanupErrs, addressErr)
		case !stagingLink.isTAP:
			cleanupErrs = append(cleanupErrs, fmt.Errorf("refuse to delete divergent staging link %s (kind=%q type=%q)", stagingName, stagingLink.kind, stagingLink.tunType))
		case stagingLink.alias != "" && stagingLink.alias != publicNetworkTAPAlias(alloc):
			cleanupErrs = append(cleanupErrs, fmt.Errorf("refuse to delete divergent staging TAP %s (alias=%q)", stagingName, stagingLink.alias))
		case stagingLink.up:
			cleanupErrs = append(cleanupErrs, fmt.Errorf("refuse to delete divergent staging TAP %s with link UP", stagingName))
		case len(stagingAddresses) != 0:
			cleanupErrs = append(cleanupErrs, fmt.Errorf("refuse to delete divergent staging TAP %s with addresses %v", stagingName, stagingAddresses))
		default:
			targets = append(targets, cleanupTarget{name: stagingName})
		}
	}

	// A foreign final-name collision must not prevent deletion of a separately
	// proven staging TAP (and vice versa). Each target was fully inspected
	// before any mutation, and divergent links are left untouched.
	for _, target := range targets {
		out, deleteErr := h.run("ip", "link", "del", target.name)
		if deleteErr != nil && !missingLinkOutput(string(out)) {
			cleanupErrs = append(cleanupErrs, fmt.Errorf("delete TAP %s: %s: %w", target.name, strings.TrimSpace(string(out)), deleteErr))
		}
	}
	return errors.Join(cleanupErrs...)
}

func missingLinkOutput(out string) bool {
	text := strings.ToLower(out)
	return strings.Contains(text, "cannot find device") || strings.Contains(text, "does not exist") || strings.Contains(text, "no such device")
}

func missingAddressOutput(out string) bool {
	text := strings.ToLower(out)
	return strings.Contains(text, "cannot assign requested address") || strings.Contains(text, "address not found")
}

func tapAlreadyExistsError(err error) bool {
	text := err.Error()
	return strings.Contains(text, "File exists") || strings.Contains(text, "Device or resource busy")
}

type tapLinkInspection struct {
	present bool
	isTAP   bool
	up      bool
	kind    string
	tunType string
	alias   string
}

type ipLinkJSONRecord struct {
	IfName   string   `json:"ifname"`
	IfAlias  string   `json:"ifalias"`
	Flags    []string `json:"flags"`
	LinkInfo struct {
		InfoKind string `json:"info_kind"`
		InfoData struct {
			Type string `json:"type"`
		} `json:"info_data"`
	} `json:"linkinfo"`
}

func (h *execPublicNetworkHostOperations) inspectTAPLink(name string) (tapLinkInspection, error) {
	out, err := h.run("ip", "-j", "-d", "link", "show", "dev", name)
	if err != nil {
		if missingLinkOutput(string(out)) {
			return tapLinkInspection{}, nil
		}
		return tapLinkInspection{}, fmt.Errorf("inspect link %s: %s: %w", name, strings.TrimSpace(string(out)), err)
	}
	var records []ipLinkJSONRecord
	if err := json.Unmarshal(out, &records); err != nil {
		return tapLinkInspection{}, fmt.Errorf("decode link inventory for %s: %w", name, err)
	}
	if len(records) != 1 || records[0].IfName != name {
		return tapLinkInspection{}, fmt.Errorf("unexpected link inventory for %s", name)
	}
	record := records[0]
	result := tapLinkInspection{
		present: true,
		kind:    record.LinkInfo.InfoKind,
		tunType: record.LinkInfo.InfoData.Type,
		alias:   record.IfAlias,
	}
	result.isTAP = result.kind == "tun" && result.tunType == "tap"
	for _, flag := range record.Flags {
		if flag == "UP" {
			result.up = true
			break
		}
	}
	return result, nil
}

type ipAddressJSONRecord struct {
	IfName   string `json:"ifname"`
	AddrInfo []struct {
		Family    string `json:"family"`
		Local     string `json:"local"`
		PrefixLen int    `json:"prefixlen"`
	} `json:"addr_info"`
}

func (h *execPublicNetworkHostOperations) inspectTAPAddresses(name string) ([]string, error) {
	out, err := h.run("ip", "-j", "-4", "addr", "show", "dev", name)
	if err != nil {
		return nil, fmt.Errorf("inspect IPv4 addresses for %s: %s: %w", name, strings.TrimSpace(string(out)), err)
	}
	var records []ipAddressJSONRecord
	if err := json.Unmarshal(out, &records); err != nil {
		return nil, fmt.Errorf("decode IPv4 address inventory for %s: %w", name, err)
	}
	if len(records) != 1 || records[0].IfName != name {
		return nil, fmt.Errorf("unexpected IPv4 address inventory for %s", name)
	}
	addresses := make([]string, 0, len(records[0].AddrInfo))
	for _, address := range records[0].AddrInfo {
		if address.Family != "inet" || net.ParseIP(address.Local).To4() == nil || address.PrefixLen < 0 || address.PrefixLen > 32 {
			continue
		}
		addresses = append(addresses, fmt.Sprintf("%s/%d", address.Local, address.PrefixLen))
	}
	return addresses, nil
}

func (h *execPublicNetworkHostOperations) reconcileTAP(alloc Allocation) (PublicNetworkHostStatus, error) {
	status := PublicNetworkHostStatus{CheckedAt: h.nowUTC()}
	link, err := h.inspectTAPLink(alloc.TAPDevice)
	if err != nil {
		return status, err
	}
	if link.present && !link.isTAP {
		status.TAPPresent = true
		status.TAPOwnershipCollision = true
		return status, fmt.Errorf("link collision for %s: kind=%q type=%q, want tun/tap", alloc.TAPDevice, link.kind, link.tunType)
	}
	if link.present && link.isTAP && link.alias != publicNetworkTAPAlias(alloc) {
		status.TAPPresent = true
		status.TAPIsTAP = true
		status.TAPOwnershipCollision = true
		return status, fmt.Errorf("unowned TAP collision for %s: alias=%q, want %q", alloc.TAPDevice, link.alias, publicNetworkTAPAlias(alloc))
	}
	if !link.present {
		owned, createErr := h.CreateTAP(alloc)
		if createErr != nil {
			return status, fmt.Errorf("recreate missing TAP %s (owned=%t): %w", alloc.TAPDevice, owned, createErr)
		}
		if !owned {
			return status, fmt.Errorf("recreate missing TAP %s: host reported no ownership", alloc.TAPDevice)
		}
	} else {
		desiredAddress := alloc.GatewayIP + "/30"
		addresses, addressErr := h.inspectTAPAddresses(alloc.TAPDevice)
		if addressErr != nil {
			return status, addressErr
		}
		desiredPresent := false
		for _, address := range addresses {
			if address == desiredAddress {
				desiredPresent = true
				continue
			}
			out, deleteErr := h.run("ip", "addr", "del", address, "dev", alloc.TAPDevice)
			if deleteErr != nil && !missingAddressOutput(string(out)) {
				return status, fmt.Errorf("remove stale address %s from %s: %s: %w", address, alloc.TAPDevice, strings.TrimSpace(string(out)), deleteErr)
			}
		}
		if !desiredPresent {
			out, addErr := h.run("ip", "addr", "add", desiredAddress, "dev", alloc.TAPDevice)
			if addErr != nil && !strings.Contains(string(out), "File exists") {
				return status, fmt.Errorf("restore address %s on %s: %s: %w", desiredAddress, alloc.TAPDevice, strings.TrimSpace(string(out)), addErr)
			}
		}
		if !link.up {
			out, linkErr := h.run("ip", "link", "set", alloc.TAPDevice, "up")
			if linkErr != nil {
				return status, fmt.Errorf("bring TAP %s up: %s: %w", alloc.TAPDevice, strings.TrimSpace(string(out)), linkErr)
			}
		}
	}

	link, err = h.inspectTAPLink(alloc.TAPDevice)
	if err != nil {
		return status, err
	}
	addresses, err := h.inspectTAPAddresses(alloc.TAPDevice)
	if err != nil {
		return status, err
	}
	status.TAPPresent = link.present
	status.TAPIsTAP = link.isTAP
	status.TAPOwned = link.alias == publicNetworkTAPAlias(alloc)
	status.TAPLinkUp = link.up
	status.TAPAddressInstalled = len(addresses) == 1 && addresses[0] == alloc.GatewayIP+"/30"
	if !status.TAPPresent || !status.TAPIsTAP || !status.TAPOwned || !status.TAPLinkUp || !status.TAPAddressInstalled {
		return status, fmt.Errorf("TAP %s did not converge to exact type/address/link state", alloc.TAPDevice)
	}
	return status, nil
}

func (h *execPublicNetworkHostOperations) forwardingState() (bool, error) {
	readFile := h.readFile
	if readFile == nil {
		readFile = os.ReadFile
	}
	path := h.forwardingPath
	if path == "" {
		path = "/proc/sys/net/ipv4/ip_forward"
	}
	data, err := readFile(path)
	if err != nil {
		return false, err
	}
	return ipv4ForwardingEnabled(string(data)), nil
}

func (h *execPublicNetworkHostOperations) ensureIPv4Forwarding() (bool, error) {
	enabled, err := h.forwardingState()
	if err == nil && enabled {
		return true, nil
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("read IPv4 forwarding state: %w", err)
	}
	writeFile := h.writeFile
	if writeFile == nil {
		writeFile = os.WriteFile
	}
	path := h.forwardingPath
	if path == "" {
		path = "/proc/sys/net/ipv4/ip_forward"
	}
	if err := writeFile(path, []byte("1\n"), 0644); err != nil {
		return false, fmt.Errorf("enable IPv4 forwarding for guest NAT: %w", err)
	}
	enabled, err = h.forwardingState()
	if err != nil {
		return false, fmt.Errorf("verify IPv4 forwarding state: %w", err)
	}
	if !enabled {
		return false, fmt.Errorf("IPv4 forwarding remained disabled after repair")
	}
	return true, nil
}

func ipv4ForwardingEnabled(value string) bool {
	return strings.TrimSpace(value) == "1"
}

type publicNetworkIptablesRule struct {
	name    string
	table   string
	chain   string
	comment string
	spec    []string
}

func publicNetworkRules(tapName, guestIP string) []publicNetworkIptablesRule {
	masqueradeComment := publicNetworkRuleComment(tapName, "masquerade")
	ingressComment := publicNetworkRuleComment(tapName, "forward-ingress")
	egressComment := publicNetworkRuleComment(tapName, "forward-egress")
	return []publicNetworkIptablesRule{
		{
			name:    "masquerade",
			table:   "nat",
			chain:   "POSTROUTING",
			comment: masqueradeComment,
			spec:    []string{"-s", guestIP + "/32", "-m", "comment", "--comment", masqueradeComment, "-j", "MASQUERADE"},
		},
		{
			name:    "forward_ingress",
			chain:   "FORWARD",
			comment: ingressComment,
			spec:    []string{"-i", tapName, "-m", "comment", "--comment", ingressComment, "-j", "ACCEPT"},
		},
		{
			name:    "forward_egress",
			chain:   "FORWARD",
			comment: egressComment,
			spec:    []string{"-o", tapName, "-m", "comment", "--comment", egressComment, "-j", "ACCEPT"},
		},
	}
}

func publicNetworkRuleComment(tapName, component string) string {
	return fmt.Sprintf("%s:%s:%s", networkIptablesCommentBase, tapName, component)
}

func iptablesRuleArgs(rule publicNetworkIptablesRule, operation string) []string {
	return iptablesRuleArgsForSpec(rule, operation, rule.spec)
}

func iptablesRuleArgsForSpec(rule publicNetworkIptablesRule, operation string, spec []string) []string {
	args := []string{"-w", networkIptablesWaitSeconds}
	if rule.table != "" {
		args = append(args, "-t", rule.table)
	}
	args = append(args, operation, rule.chain)
	return append(args, spec...)
}

func (h *execPublicNetworkHostOperations) iptablesRuleInventory(rule publicNetworkIptablesRule) ([][]string, error) {
	args := []string{"-w", networkIptablesWaitSeconds}
	if rule.table != "" {
		args = append(args, "-t", rule.table)
	}
	args = append(args, "-S", rule.chain)
	out, err := h.run("iptables", args...)
	if err != nil {
		return nil, fmt.Errorf("list %s rule: %s: %w", rule.name, strings.TrimSpace(string(out)), err)
	}
	var inventory [][]string
	for _, line := range strings.Split(string(out), "\n") {
		tokens := canonicalNetworkCommandTokens(strings.Fields(strings.TrimSpace(line)))
		if len(tokens) < 2 || tokens[0] != "-A" || tokens[1] != rule.chain {
			continue
		}
		inventory = append(inventory, tokens)
	}
	return inventory, nil
}

func exactAndStaleOwnedIptablesRules(rule publicNetworkIptablesRule, inventory [][]string) (int, [][]string) {
	want := append([]string{"-A", rule.chain}, rule.spec...)
	count := 0
	var stale [][]string
	for _, tokens := range inventory {
		if equalNetworkCommandTokens(tokens, want) {
			count++
			continue
		}
		if networkCommandComment(tokens) == rule.comment {
			stale = append(stale, tokens)
		}
	}
	return count, stale
}

func networkCommandComment(tokens []string) string {
	for index, token := range tokens {
		if token == "--comment" && index+1 < len(tokens) {
			return tokens[index+1]
		}
	}
	return ""
}

func canonicalNetworkCommandTokens(tokens []string) []string {
	result := make([]string, len(tokens))
	for index, token := range tokens {
		result[index] = strings.Trim(token, `"'`)
	}
	return result
}

func equalNetworkCommandTokens(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func (h *execPublicNetworkHostOperations) reconcileExactIptablesRule(rule publicNetworkIptablesRule) (int, error) {
	inventory, err := h.iptablesRuleInventory(rule)
	if err != nil {
		return 0, err
	}
	count, stale := exactAndStaleOwnedIptablesRules(rule, inventory)
	removals := len(stale) + max(0, count-1)
	if removals > networkRuleRemovalLimit {
		return count, fmt.Errorf("normalize %s rule: duplicate-removal cap exhausted (%d owned removals)", rule.name, removals)
	}
	for _, tokens := range stale {
		args := iptablesRuleArgsForSpec(rule, "-D", tokens[2:])
		out, deleteErr := h.run("iptables", args...)
		if deleteErr != nil {
			if iptablesRuleMissingFromResult(out, deleteErr) {
				continue
			}
			return count, fmt.Errorf("delete stale owned %s rule: %s: %w", rule.name, strings.TrimSpace(string(out)), deleteErr)
		}
	}
	if count == 0 {
		args := iptablesRuleArgs(rule, "-A")
		out, addErr := h.run("iptables", args...)
		if addErr != nil {
			return 0, fmt.Errorf("add %s rule: %s: %w", rule.name, strings.TrimSpace(string(out)), addErr)
		}
		count = 1
	}
	duplicates := count - 1
	for attempt := 0; attempt < duplicates; attempt++ {
		args := iptablesRuleArgs(rule, "-D")
		out, deleteErr := h.run("iptables", args...)
		if deleteErr != nil {
			if iptablesRuleMissingFromResult(out, deleteErr) {
				break
			}
			return count, fmt.Errorf("delete duplicate %s rule: %s: %w", rule.name, strings.TrimSpace(string(out)), deleteErr)
		}
	}
	inventory, err = h.iptablesRuleInventory(rule)
	if err != nil {
		return 0, err
	}
	count, stale = exactAndStaleOwnedIptablesRules(rule, inventory)
	if count != 1 || len(stale) != 0 {
		return count, fmt.Errorf("normalize %s rule: observed %d exact and %d stale owned rules after repair, want 1/0", rule.name, count, len(stale))
	}
	return count, nil
}

func (h *execPublicNetworkHostOperations) removeAllOwnedIptablesRules(rule publicNetworkIptablesRule) error {
	inventory, err := h.iptablesRuleInventory(rule)
	if err != nil {
		return err
	}
	var owned [][]string
	for _, tokens := range inventory {
		if networkCommandComment(tokens) == rule.comment {
			owned = append(owned, tokens)
		}
	}
	if len(owned) > networkRuleRemovalLimit {
		return fmt.Errorf("remove %s rule: duplicate-removal cap exhausted (%d owned rules)", rule.name, len(owned))
	}
	for _, tokens := range owned {
		args := iptablesRuleArgsForSpec(rule, "-D", tokens[2:])
		out, deleteErr := h.run("iptables", args...)
		if deleteErr == nil || iptablesRuleMissingFromResult(out, deleteErr) {
			continue
		}
		return fmt.Errorf("delete owned %s rule: %s: %w", rule.name, strings.TrimSpace(string(out)), deleteErr)
	}
	remainingInventory, err := h.iptablesRuleInventory(rule)
	if err != nil {
		return err
	}
	_, stale := exactAndStaleOwnedIptablesRules(rule, remainingInventory)
	exact, _ := exactAndStaleOwnedIptablesRules(rule, remainingInventory)
	if exact != 0 || len(stale) != 0 {
		return fmt.Errorf("remove %s rule: %d owned rules remain", rule.name, exact+len(stale))
	}
	return nil
}

func (h *execPublicNetworkHostOperations) SetupNAT(tapName, guestIP string) error {
	if _, err := h.ensureIPv4Forwarding(); err != nil {
		return err
	}
	for _, rule := range publicNetworkRules(tapName, guestIP) {
		if _, err := h.reconcileExactIptablesRule(rule); err != nil {
			return errors.Join(err, h.TeardownNAT(tapName, guestIP))
		}
	}
	return nil
}

func (h *execPublicNetworkHostOperations) TeardownNAT(tapName, guestIP string) error {
	var cleanupErrs []error
	for _, rule := range publicNetworkRules(tapName, guestIP) {
		if err := h.removeAllOwnedIptablesRules(rule); err != nil {
			cleanupErrs = append(cleanupErrs, err)
		}
	}
	return errors.Join(cleanupErrs...)
}

func (h *execPublicNetworkHostOperations) ReconcileActive(alloc Allocation) (PublicNetworkHostStatus, error) {
	status, tapErr := h.reconcileTAP(alloc)
	if tapErr != nil {
		status.LastError = tapErr.Error()
		return status, tapErr
	}
	forwarding, forwardingErr := h.ensureIPv4Forwarding()
	status.IPv4Forwarding = forwarding
	if forwardingErr != nil {
		status.LastError = forwardingErr.Error()
		return status, forwardingErr
	}
	rules := publicNetworkRules(alloc.TAPDevice, alloc.GuestIP)
	counts := make([]int, len(rules))
	for index, rule := range rules {
		count, ruleErr := h.reconcileExactIptablesRule(rule)
		counts[index] = count
		if ruleErr != nil {
			status.MasqueradeRules = counts[0]
			if len(counts) > 1 {
				status.ForwardIngressRules = counts[1]
			}
			if len(counts) > 2 {
				status.ForwardEgressRules = counts[2]
			}
			status.LastError = ruleErr.Error()
			return status, ruleErr
		}
	}
	status.MasqueradeRules = counts[0]
	status.ForwardIngressRules = counts[1]
	status.ForwardEgressRules = counts[2]
	status.Ready = status.TAPPresent && status.TAPIsTAP && status.TAPOwned && status.TAPAddressInstalled && status.TAPLinkUp &&
		status.IPv4Forwarding && status.MasqueradeRules == 1 && status.ForwardIngressRules == 1 && status.ForwardEgressRules == 1
	if !status.Ready {
		err := fmt.Errorf("physical-host network for %s did not converge", alloc.TAPDevice)
		status.LastError = err.Error()
		return status, err
	}
	return status, nil
}

func iptablesRuleMissingFromResult(out []byte, err error) bool {
	if commandExitCode(err) != 1 {
		return false
	}
	text := strings.ToLower(strings.TrimSpace(string(out)))
	return strings.Contains(text, "does a matching rule exist in that chain") || strings.Contains(text, "rule does not exist")
}

type commandExitCoder interface {
	ExitCode() int
}

func commandExitCode(err error) int {
	var exitErr commandExitCoder
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}
