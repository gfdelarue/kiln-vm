package machine

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

const (
	gatewayDNSFirewallCommentBase = "kiln-vm-gateway-dns:v2"
	gatewayDNSFirewallWaitSeconds = "5"
	gatewayDNSFirewallOwnerFile   = "gateway-dns-firewall.owner"

	defaultGatewayDNSFirewallCommandTimeout = 10 * time.Second
)

var errGatewayDNSFirewallOwnershipCollision = errors.New("gateway DNS firewall ownership collision")

// GatewayDNSFirewallStatus is the most recently observed firewall state for
// one public-network allocation. Status is intentionally cached: inspecting
// it never runs a privileged command.
type GatewayDNSFirewallStatus struct {
	// Desired, UDPInstalled, TCPInstalled, and Ready retain their public
	// admission meaning. Guard fields are additive provider detail; public
	// installed-rule counts continue to count ACCEPT rules only.
	Desired            bool      `json:"desired"`
	GuardDesired       bool      `json:"guard_desired"`
	AdmissionDesired   bool      `json:"admission_desired"`
	HookInstalled      bool      `json:"hook_installed"`
	UDPInstalled       bool      `json:"udp_installed"`
	TCPInstalled       bool      `json:"tcp_installed"`
	GuardUDPInstalled  bool      `json:"guard_udp_installed"`
	GuardTCPInstalled  bool      `json:"guard_tcp_installed"`
	GuardReady         bool      `json:"guard_ready"`
	AdmissionReady     bool      `json:"admission_ready"`
	OwnershipCollision bool      `json:"ownership_collision"`
	Ready              bool      `json:"ready"`
	LastError          string    `json:"last_error,omitempty"`
	CheckedAt          time.Time `json:"checked_at,omitempty"`
}

type gatewayDNSFirewallCommandRunner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

type gatewayDNSFirewallExecRunner struct{}

func (gatewayDNSFirewallExecRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	output, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return output, fmt.Errorf("run gateway DNS firewall command %s: %w", name, err)
	}
	return output, nil
}

// GatewayDNSFirewall owns the scoped physical-host INPUT admission needed by
// kiln-vm's per-allocation gateway DNS proxies. All mutations and desired-snapshot
// replacements are serialized so a reconciliation pass cannot prune an
// allocation that is being activated concurrently.
type GatewayDNSFirewall struct {
	mu                 sync.Mutex
	baseCIDR           string
	runner             gatewayDNSFirewallCommandRunner
	commandTimeout     time.Duration
	identity           gatewayDNSFirewallIdentity
	guarded            map[string]Allocation
	admitted           map[string]Allocation
	observed           map[string]gatewayDNSFirewallObserved
	hookInstalled      bool
	ownershipPath      string
	ownershipErr       error
	ownershipCollision bool
	lastErr            error
	checkedAt          time.Time
}

type gatewayDNSFirewallObserved struct {
	admitUDP bool
	admitTCP bool
	guardUDP bool
	guardTCP bool
}

func NewGatewayDNSFirewallWithOwnershipPath(baseCIDR, ownershipPath string) *GatewayDNSFirewall {
	return newGatewayDNSFirewallWithRunnerAndOwnershipPath(baseCIDR, gatewayDNSFirewallExecRunner{}, ownershipPath)
}

func newGatewayDNSFirewallWithRunnerAndOwnershipPath(baseCIDR string, runner gatewayDNSFirewallCommandRunner, ownershipPath string) *GatewayDNSFirewall {
	path := strings.TrimSpace(ownershipPath)
	var (
		identity gatewayDNSFirewallIdentity
		err      error
	)
	if path == "" {
		identity, err = randomGatewayDNSFirewallIdentity()
	} else {
		identity, err = loadOrCreateGatewayDNSFirewallIdentity(path)
	}
	return newGatewayDNSFirewall(baseCIDR, runner, path, identity, err)
}

func newGatewayDNSFirewall(baseCIDR string, runner gatewayDNSFirewallCommandRunner, ownershipPath string, identity gatewayDNSFirewallIdentity, ownershipErr error) *GatewayDNSFirewall {
	if runner == nil {
		runner = gatewayDNSFirewallExecRunner{}
	}
	return &GatewayDNSFirewall{
		baseCIDR:       baseCIDR,
		runner:         runner,
		commandTimeout: defaultGatewayDNSFirewallCommandTimeout,
		identity:       identity,
		guarded:        make(map[string]Allocation),
		admitted:       make(map[string]Allocation),
		observed:       make(map[string]gatewayDNSFirewallObserved),
		ownershipPath:  ownershipPath,
		ownershipErr:   ownershipErr,
	}
}

// Guard installs per-TAP host-local UDP/TCP port-53 DROP guards for an
// allocation. The guard deliberately has no source or destination selector:
// the exact guest-to-own-gateway admission is inserted ahead of it, while every
// other physical-host-local DNS destination reached through that TAP remains
// closed. Forwarded external DNS never traverses this INPUT-only chain. The
// guard remains desired after a failed pass so periodic reconciliation can
// finish fail-closed convergence before a listener is started.
func (f *GatewayDNSFirewall) Guard(alloc Allocation) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if err := f.validateAllocation(alloc); err != nil {
		return fmt.Errorf("guard gateway DNS firewall: %w", err)
	}
	if current, ok := f.guarded[alloc.TAPDevice]; ok && current != alloc {
		return fmt.Errorf("guard gateway DNS firewall: TAP %s already has a different guarded allocation", alloc.TAPDevice)
	}
	f.guarded[alloc.TAPDevice] = alloc
	return f.reconcileLocked()
}

// Activate admits an allocation while retaining its DROP guards behind the
// exact ACCEPT rules. It also establishes the guard when called directly so the
// temporary compatibility caller remains fail-closed while integration moves
// to Guard -> listener start -> Activate.
func (f *GatewayDNSFirewall) Activate(alloc Allocation) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if err := f.validateAllocation(alloc); err != nil {
		return fmt.Errorf("activate gateway DNS firewall: %w", err)
	}
	if current, ok := f.guarded[alloc.TAPDevice]; ok && current != alloc {
		return fmt.Errorf("activate gateway DNS firewall: TAP %s already has a different guarded allocation", alloc.TAPDevice)
	}
	if current, ok := f.admitted[alloc.TAPDevice]; ok && current != alloc {
		return fmt.Errorf("activate gateway DNS firewall: TAP %s already has a different admitted allocation", alloc.TAPDevice)
	}
	if current, ok := f.admitted[alloc.TAPDevice]; ok && current == alloc {
		return f.reconcileLocked()
	}

	f.guarded[alloc.TAPDevice] = alloc
	f.admitted[alloc.TAPDevice] = alloc
	if err := f.reconcileLocked(); err != nil {
		primary := err
		// Failed admission always falls back to guard-only desired state. The
		// positional reconciler inserts DROP guards ahead of stale ACCEPTs before
		// pruning, so any successful rollback mutation closes rather than opens.
		delete(f.admitted, alloc.TAPDevice)
		rollbackErr := f.reconcileLocked()
		if rollbackErr != nil {
			rollbackErr = fmt.Errorf("rollback gateway DNS firewall admission to guard: %w", rollbackErr)
		}
		return errors.Join(primary, rollbackErr)
	}
	return nil
}

// Deactivate removes admission but deliberately retains the allocation guard.
// Call this before stopping a listener; Release is the final post-listener/TAP
// cleanup operation.
func (f *GatewayDNSFirewall) Deactivate(alloc Allocation) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	canonical, _, err := validateCleanupAllocation(alloc)
	if err != nil {
		return fmt.Errorf("deactivate gateway DNS firewall: %w", err)
	}
	alloc = canonical
	if current, ok := f.guarded[alloc.TAPDevice]; ok && current != alloc {
		return fmt.Errorf("deactivate gateway DNS firewall: TAP %s has a different guarded allocation", alloc.TAPDevice)
	}
	if current, ok := f.admitted[alloc.TAPDevice]; ok && current != alloc {
		return fmt.Errorf("deactivate gateway DNS firewall: TAP %s has a different admitted allocation", alloc.TAPDevice)
	}
	f.guarded[alloc.TAPDevice] = alloc
	delete(f.admitted, alloc.TAPDevice)
	return f.reconcileLocked()
}

// Release removes an allocation from the desired registry and reconciles the
// remaining complete snapshot. The removal remains desired if a host command
// fails, allowing a later periodic Reconcile call to finish cleanup.
func (f *GatewayDNSFirewall) Release(alloc Allocation) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	canonical, _, err := validateCleanupAllocation(alloc)
	if err != nil {
		return fmt.Errorf("release gateway DNS firewall: %w", err)
	}
	alloc = canonical
	if current, ok := f.guarded[alloc.TAPDevice]; ok && current != alloc {
		return fmt.Errorf("release gateway DNS firewall: TAP %s has a different guarded allocation", alloc.TAPDevice)
	}
	if current, ok := f.admitted[alloc.TAPDevice]; ok && current != alloc {
		return fmt.Errorf("release gateway DNS firewall: TAP %s has a different admitted allocation", alloc.TAPDevice)
	}
	delete(f.admitted, alloc.TAPDevice)
	delete(f.guarded, alloc.TAPDevice)
	return f.reconcileLocked()
}

// ReconcileState atomically replaces the complete guarded/admitted snapshots.
// Every admitted allocation must also be guarded. A failed pass retains the
// supplied snapshots so a later pass can heal partial host state.
func (f *GatewayDNSFirewall) ReconcileState(guarded, admitted []Allocation) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	guardedSnapshot, err := f.validatedGuardSnapshot(guarded)
	if err != nil {
		return fmt.Errorf("reconcile gateway DNS firewall: %w", err)
	}
	admittedSnapshot, err := f.validatedSnapshot("admitted", admitted)
	if err != nil {
		return fmt.Errorf("reconcile gateway DNS firewall: %w", err)
	}
	for tap, alloc := range admittedSnapshot {
		guardedAlloc, ok := guardedSnapshot[tap]
		if !ok || guardedAlloc != alloc {
			return fmt.Errorf("reconcile gateway DNS firewall: admitted allocation %s is not present in the guarded snapshot", tap)
		}
	}
	f.guarded = guardedSnapshot
	f.admitted = admittedSnapshot
	return f.reconcileLocked()
}

// Reconcile is a temporary compatibility wrapper for the current network
// controller. It treats every active allocation as both guarded and admitted.
func (f *GatewayDNSFirewall) Reconcile(allocations []Allocation) error {
	return f.ReconcileState(allocations, allocations)
}

// Status returns cached state for alloc. It performs no host command and is
// safe to use from metrics and diagnostics paths.
func (f *GatewayDNSFirewall) Status(alloc Allocation) GatewayDNSFirewallStatus {
	f.mu.Lock()
	defer f.mu.Unlock()

	status := GatewayDNSFirewallStatus{CheckedAt: f.checkedAt}
	if err := f.validateAllocation(alloc); err != nil {
		status.LastError = err.Error()
		return status
	}
	guarded, guardDesired := f.guarded[alloc.TAPDevice]
	admitted, admissionDesired := f.admitted[alloc.TAPDevice]
	status.GuardDesired = guardDesired && guarded == alloc
	status.AdmissionDesired = admissionDesired && admitted == alloc
	status.Desired = status.AdmissionDesired
	status.HookInstalled = f.hookInstalled
	status.OwnershipCollision = f.ownershipCollision
	if observed, ok := f.observed[alloc.TAPDevice]; ok {
		status.UDPInstalled = observed.admitUDP
		status.TCPInstalled = observed.admitTCP
		status.GuardUDPInstalled = observed.guardUDP
		status.GuardTCPInstalled = observed.guardTCP
	}
	if f.lastErr != nil {
		status.LastError = f.lastErr.Error()
	}
	status.GuardReady = status.GuardDesired && status.HookInstalled && status.GuardUDPInstalled && status.GuardTCPInstalled && status.LastError == ""
	status.AdmissionReady = status.AdmissionDesired && status.GuardReady && status.UDPInstalled && status.TCPInstalled
	status.Ready = status.AdmissionReady
	return status
}

func (f *GatewayDNSFirewall) validatedSnapshot(kind string, allocations []Allocation) (map[string]Allocation, error) {
	desired := make(map[string]Allocation, len(allocations))
	for _, alloc := range allocations {
		if err := f.validateAllocation(alloc); err != nil {
			return nil, err
		}
		if existing, ok := desired[alloc.TAPDevice]; ok {
			if existing == alloc {
				return nil, fmt.Errorf("duplicate %s allocation for TAP %s", kind, alloc.TAPDevice)
			}
			return nil, fmt.Errorf("conflicting %s allocations for TAP %s", kind, alloc.TAPDevice)
		}
		desired[alloc.TAPDevice] = alloc
	}
	return desired, nil
}

// validatedGuardSnapshot accepts complete historical allocations as well as
// allocations from the current base CIDR. A guard is a per-TAP, host-local
// DNS DROP-only rule, and cleanup_pending records must remain guarded after a
// BridgeCIDR change. Admission remains subject to validatedSnapshot's exact
// current-CIDR check.
func (f *GatewayDNSFirewall) validatedGuardSnapshot(allocations []Allocation) (map[string]Allocation, error) {
	desired := make(map[string]Allocation, len(allocations))
	for _, alloc := range allocations {
		canonical, _, err := validateCleanupAllocation(alloc)
		if err != nil {
			return nil, err
		}
		if existing, ok := desired[canonical.TAPDevice]; ok {
			if existing == canonical {
				return nil, fmt.Errorf("duplicate guarded allocation for TAP %s", canonical.TAPDevice)
			}
			return nil, fmt.Errorf("conflicting guarded allocations for TAP %s", canonical.TAPDevice)
		}
		desired[canonical.TAPDevice] = canonical
	}
	return desired, nil
}

func (f *GatewayDNSFirewall) validateAllocation(alloc Allocation) error {
	idx, ok := networkIndexFromTAP(alloc.TAPDevice)
	if !ok || alloc.TAPDevice != fmt.Sprintf("kv%d", idx) {
		return fmt.Errorf("invalid kiln-vm TAP identity %q", alloc.TAPDevice)
	}
	expected, err := allocationForIndex(f.baseCIDR, idx)
	if err != nil {
		return fmt.Errorf("derive allocation for TAP %s: %w", alloc.TAPDevice, err)
	}
	// OwnerID is durable machine identity, not a network coordinate. Preserve it
	// in the desired registry so a same-TAP owner substitution is still a
	// collision, but compare the exact provider tuple independently because
	// allocationForIndex intentionally has no machine owner.
	gotNetworkTuple := alloc
	gotNetworkTuple.OwnerID = ""
	if gotNetworkTuple != *expected {
		return fmt.Errorf(
			"allocation %s is not the exact current kiln-vm tuple: got guest/gateway %s/%s mac=%s mask=%s base=%s index=%d; want guest/gateway %s/%s mac=%s mask=%s base=%s index=%d",
			alloc.TAPDevice,
			alloc.GuestIP,
			alloc.GatewayIP,
			alloc.GuestMAC,
			alloc.Mask,
			alloc.BaseCIDR,
			alloc.NetworkIndex,
			expected.GuestIP,
			expected.GatewayIP,
			expected.GuestMAC,
			expected.Mask,
			expected.BaseCIDR,
			expected.NetworkIndex,
		)
	}
	return nil
}

func (f *GatewayDNSFirewall) reconcileLocked() error {
	checkedAt := time.Now().UTC()
	if f.ownershipErr != nil {
		err := fmt.Errorf("load gateway DNS firewall ownership evidence: %w", f.ownershipErr)
		f.recordReconcileResultLocked(checkedAt, nil, err)
		return err
	}
	if err := f.ensureOwnershipEvidenceLocked(); err != nil {
		f.recordReconcileResultLocked(checkedAt, nil, err)
		return err
	}
	inventory, err := f.inventoryLocked()
	if err != nil {
		f.recordReconcileResultLocked(checkedAt, nil, err)
		return err
	}
	f.recordObservedInventoryLocked(inventory)
	if err := f.validateOwnership(inventory); err != nil {
		err = fmt.Errorf("%w: %v", errGatewayDNSFirewallOwnershipCollision, err)
		f.recordReconcileResultLocked(checkedAt, inventory, err)
		return err
	}

	if len(f.guarded) == 0 {
		err = f.removeAllLocked(inventory)
	} else {
		err = f.ensureDesiredLocked(inventory)
	}

	// Always refresh cached status after a mutation attempt. A failed command
	// may still have left a safe guard or duplicate behind; observational status
	// must describe that concrete state without guessing from desired intent.
	postInventory, postErr := f.inventoryLocked()
	if postErr == nil {
		if ownershipErr := f.validateOwnership(postInventory); ownershipErr != nil {
			postErr = fmt.Errorf("%w: %v", errGatewayDNSFirewallOwnershipCollision, ownershipErr)
		} else {
			f.recordObservedInventoryLocked(postInventory)
		}
	}
	result := errors.Join(err, postErr)
	if postInventory == nil {
		postInventory = inventory
	}
	f.recordReconcileResultLocked(checkedAt, postInventory, result)
	return result
}

func (f *GatewayDNSFirewall) recordReconcileResultLocked(checkedAt time.Time, inventory *gatewayDNSFirewallInventory, err error) {
	f.checkedAt = checkedAt
	f.lastErr = err
	f.ownershipCollision = errors.Is(err, errGatewayDNSFirewallOwnershipCollision)
	if inventory == nil {
		f.hookInstalled = false
		f.observed = make(map[string]gatewayDNSFirewallObserved)
	}
}

func (f *GatewayDNSFirewall) recordObservedInventoryLocked(inventory *gatewayDNSFirewallInventory) {
	f.hookInstalled = len(inventory.ownedHookPositions) == 1 && inventory.ownedHookPositions[0] == 1
	f.observed = make(map[string]gatewayDNSFirewallObserved)
	for _, rule := range inventory.chainRules {
		key, owned := f.ownedRuleKey(rule.tokens)
		if !owned {
			continue
		}
		observed := f.observed[key.tap]
		switch key.verdict + ":" + key.protocol {
		case "admit:udp":
			observed.admitUDP = true
		case "admit:tcp":
			observed.admitTCP = true
		case "guard:udp":
			observed.guardUDP = true
		case "guard:tcp":
			observed.guardTCP = true
		}
		f.observed[key.tap] = observed
	}
}

func (f *GatewayDNSFirewall) ensureDesiredLocked(inventory *gatewayDNSFirewallInventory) error {
	if !inventory.chainExists {
		if _, err := f.runIptablesLocked("-N", f.identity.Chain); err != nil {
			if collisionInventory, inspectErr := f.inventoryLocked(); inspectErr == nil && collisionInventory.chainExists {
				return fmt.Errorf("%w: private chain %s appeared during creation", errGatewayDNSFirewallOwnershipCollision, f.identity.Chain)
			}
			return fmt.Errorf("create private gateway DNS chain %s: %w", f.identity.Chain, err)
		}
		inventory = &gatewayDNSFirewallInventory{chainExists: true}
	}

	// Before adding, moving, or pruning any admission rule, ensure every guarded
	// allocation has both per-TAP host-local DNS DROP rules somewhere in the
	// live chain. This is the invariant that makes subsequent position-aware
	// normalization safe.
	guardRules := f.desiredGuardRulesLocked()
	existingCounts := make(map[string]int)
	for _, rule := range inventory.chainRules {
		existingCounts[f.canonicalRule(rule.tokens)]++
	}
	guardsAdded := false
	for _, rule := range guardRules {
		canonical := f.canonicalRule(rule)
		if existingCounts[canonical] > 0 {
			continue
		}
		args := append([]string{"-A", f.identity.Chain}, rule[2:]...)
		if _, err := f.runIptablesLocked(args...); err != nil {
			return fmt.Errorf("install gateway DNS DROP guard: %w", err)
		}
		existingCounts[canonical] = 1
		guardsAdded = true
	}
	if guardsAdded {
		var err error
		inventory, err = f.inventoryLocked()
		if err != nil {
			return fmt.Errorf("refresh gateway DNS rules after guard installation: %w", err)
		}
		if err := f.validateOwnership(inventory); err != nil {
			return fmt.Errorf("%w: %v", errGatewayDNSFirewallOwnershipCollision, err)
		}
	}

	desiredRules := f.desiredRulesLocked()
	current := append([]gatewayDNSFirewallListedRule(nil), inventory.chainRules...)
	for index, desired := range desiredRules {
		if index < len(current) && f.canonicalRule(current[index].tokens) == f.canonicalRule(desired) {
			continue
		}
		position := index + 1
		args := append([]string{"-I", f.identity.Chain, strconv.Itoa(position)}, desired[2:]...)
		if _, err := f.runIptablesLocked(args...); err != nil {
			return fmt.Errorf("insert gateway DNS rule at position %d: %w", position, err)
		}
		inserted := gatewayDNSFirewallListedRule{chain: f.identity.Chain, position: position, tokens: append([]string(nil), desired...)}
		current = append(current, gatewayDNSFirewallListedRule{})
		copy(current[index+1:], current[index:])
		current[index] = inserted
		for ruleIndex := index + 1; ruleIndex < len(current); ruleIndex++ {
			current[ruleIndex].position = ruleIndex + 1
		}
	}

	// The exact desired prefix now exists. Remove only the stale/duplicate suffix
	// in descending position order, which never creates a guard or admission gap.
	for position := len(current); position > len(desiredRules); position-- {
		if _, err := f.runIptablesLocked("-D", f.identity.Chain, strconv.Itoa(position)); err != nil {
			return fmt.Errorf("remove stale or duplicate gateway DNS rule %d: %w", position, err)
		}
	}

	latest, err := f.inventoryLocked()
	if err != nil {
		return fmt.Errorf("refresh gateway DNS rules before hook normalization: %w", err)
	}
	if err := f.validateOwnership(latest); err != nil {
		return fmt.Errorf("%w: %v", errGatewayDNSFirewallOwnershipCollision, err)
	}
	if err := f.normalizeHookLocked(latest); err != nil {
		return err
	}
	return nil
}

func (f *GatewayDNSFirewall) normalizeHookLocked(inventory *gatewayDNSFirewallInventory) error {
	positions := append([]int(nil), inventory.ownedHookPositions...)
	if len(positions) == 1 && positions[0] == 1 {
		return nil
	}

	inserted := false
	if len(positions) == 0 || positions[0] != 1 {
		args := append([]string{"-I", "INPUT", "1"}, f.hookMatchTokens()...)
		if _, err := f.runIptablesLocked(args...); err != nil {
			return fmt.Errorf("insert owned gateway DNS INPUT hook: %w", err)
		}
		inserted = true
	}

	// Inserting at position one shifts every pre-existing hook down by one.
	removePositions := make([]int, 0, len(positions))
	for index, position := range positions {
		if !inserted && index == 0 && position == 1 {
			continue
		}
		if inserted {
			position++
		}
		removePositions = append(removePositions, position)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(removePositions)))
	for _, position := range removePositions {
		if _, err := f.runIptablesLocked("-D", "INPUT", strconv.Itoa(position)); err != nil {
			return fmt.Errorf("remove duplicate or late gateway DNS INPUT hook %d: %w", position, err)
		}
	}
	return nil
}

func (f *GatewayDNSFirewall) removeAllLocked(inventory *gatewayDNSFirewallInventory) error {
	var errs []error

	hookPositions := append([]int(nil), inventory.ownedHookPositions...)
	sort.Sort(sort.Reverse(sort.IntSlice(hookPositions)))
	for _, position := range hookPositions {
		if _, err := f.runIptablesLocked("-D", "INPUT", strconv.Itoa(position)); err != nil {
			errs = append(errs, fmt.Errorf("remove owned gateway DNS INPUT hook %d: %w", position, err))
		}
	}

	chainPositions := make([]int, 0, len(inventory.chainRules))
	for _, rule := range inventory.chainRules {
		chainPositions = append(chainPositions, rule.position)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(chainPositions)))
	for _, position := range chainPositions {
		if _, err := f.runIptablesLocked("-D", f.identity.Chain, strconv.Itoa(position)); err != nil {
			errs = append(errs, fmt.Errorf("remove owned gateway DNS rule %d: %w", position, err))
		}
	}

	if inventory.chainExists {
		if _, err := f.runIptablesLocked("-X", f.identity.Chain); err != nil {
			errs = append(errs, fmt.Errorf("delete empty owned gateway DNS chain: %w", err))
		}
	}
	return errors.Join(errs...)
}

func (f *GatewayDNSFirewall) desiredRulesLocked() [][]string {
	admitted := sortedGatewayDNSFirewallAllocations(f.admitted)
	guarded := sortedGatewayDNSFirewallAllocations(f.guarded)
	rules := make([][]string, 0, (len(admitted)+len(guarded))*2)
	for _, alloc := range admitted {
		for _, protocol := range []string{"udp", "tcp"} {
			rules = append(rules, f.ruleTokens(alloc, protocol, gatewayDNSFirewallVerdictAdmit))
		}
	}
	for _, alloc := range guarded {
		for _, protocol := range []string{"udp", "tcp"} {
			rules = append(rules, f.ruleTokens(alloc, protocol, gatewayDNSFirewallVerdictGuard))
		}
	}
	return rules
}

func (f *GatewayDNSFirewall) desiredGuardRulesLocked() [][]string {
	guarded := sortedGatewayDNSFirewallAllocations(f.guarded)
	rules := make([][]string, 0, len(guarded)*2)
	for _, alloc := range guarded {
		for _, protocol := range []string{"udp", "tcp"} {
			rules = append(rules, f.ruleTokens(alloc, protocol, gatewayDNSFirewallVerdictGuard))
		}
	}
	return rules
}

func sortedGatewayDNSFirewallAllocations(source map[string]Allocation) []Allocation {
	allocations := make([]Allocation, 0, len(source))
	for _, alloc := range source {
		allocations = append(allocations, alloc)
	}
	sort.Slice(allocations, func(i, j int) bool {
		left, _ := networkIndexFromTAP(allocations[i].TAPDevice)
		right, _ := networkIndexFromTAP(allocations[j].TAPDevice)
		return left < right
	})
	return allocations
}

func (f *GatewayDNSFirewall) inventoryLocked() (*gatewayDNSFirewallInventory, error) {
	out, err := f.runIptablesLocked("-S")
	if err != nil {
		return nil, fmt.Errorf("list filter firewall state: %w", err)
	}
	return f.parseInventory(string(out))
}

func (f *GatewayDNSFirewall) runIptablesLocked(args ...string) ([]byte, error) {
	timeout := f.commandTimeout
	if timeout <= 0 {
		timeout = defaultGatewayDNSFirewallCommandTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	argv := append([]string{"-w", gatewayDNSFirewallWaitSeconds}, args...)
	out, err := f.runner.Run(ctx, "iptables", argv...)
	if err != nil {
		detail := strings.TrimSpace(string(out))
		if detail == "" {
			return out, fmt.Errorf("iptables %s: %w", strings.Join(argv, " "), err)
		}
		return out, fmt.Errorf("iptables %s: %s: %w", strings.Join(argv, " "), detail, err)
	}
	return out, nil
}

func (f *GatewayDNSFirewall) ensureOwnershipEvidenceLocked() error {
	if f.ownershipPath == "" {
		return nil
	}
	if f.ownershipErr != nil {
		return fmt.Errorf("gateway DNS firewall ownership evidence is unavailable: %w", f.ownershipErr)
	}
	identity, present, err := readGatewayDNSFirewallIdentity(f.ownershipPath)
	if err != nil {
		return err
	}
	if present {
		if identity != f.identity {
			return fmt.Errorf("%w: ownership identity %s changed", errGatewayDNSFirewallOwnershipCollision, f.ownershipPath)
		}
		return nil
	}
	if err := persistGatewayDNSFirewallIdentity(f.ownershipPath, f.identity); err != nil {
		return fmt.Errorf("persist gateway DNS firewall ownership identity: %w", err)
	}
	identity, present, err = readGatewayDNSFirewallIdentity(f.ownershipPath)
	if err != nil {
		return err
	}
	if !present || identity != f.identity {
		return fmt.Errorf("%w: ownership identity %s was replaced during creation", errGatewayDNSFirewallOwnershipCollision, f.ownershipPath)
	}
	return nil
}

type gatewayDNSFirewallInventory struct {
	chainExists        bool
	allRules           []gatewayDNSFirewallListedRule
	inputRules         []gatewayDNSFirewallListedRule
	chainRules         []gatewayDNSFirewallListedRule
	ownedHookPositions []int
}

type gatewayDNSFirewallListedRule struct {
	chain    string
	position int
	tokens   []string
}

func (f *GatewayDNSFirewall) parseInventory(output string) (*gatewayDNSFirewallInventory, error) {
	inventory := &gatewayDNSFirewallInventory{}
	positions := make(map[string]int)
	for _, rawLine := range strings.Split(output, "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}
		tokens, err := splitGatewayDNSFirewallIptablesLine(line)
		if err != nil {
			return nil, fmt.Errorf("parse iptables rule %q: %w", line, err)
		}
		if len(tokens) == 2 && tokens[0] == "-N" && tokens[1] == f.identity.Chain {
			inventory.chainExists = true
			continue
		}
		if len(tokens) < 2 || tokens[0] != "-A" {
			continue
		}
		chain := tokens[1]
		positions[chain]++
		rule := gatewayDNSFirewallListedRule{chain: chain, position: positions[chain], tokens: tokens}
		inventory.allRules = append(inventory.allRules, rule)
		switch chain {
		case "INPUT":
			inventory.inputRules = append(inventory.inputRules, rule)
			if f.equalTokens(tokens, f.hookRuleTokens()) {
				inventory.ownedHookPositions = append(inventory.ownedHookPositions, rule.position)
			}
		case f.identity.Chain:
			inventory.chainExists = true
			inventory.chainRules = append(inventory.chainRules, rule)
		}
	}
	return inventory, nil
}

func (f *GatewayDNSFirewall) validateOwnership(inventory *gatewayDNSFirewallInventory) error {
	ownerNamespace := f.commentBase() + ":"
	for _, rule := range inventory.allRules {
		target, referencesOwnedChain := gatewayDNSFirewallRuleTarget(rule.tokens)
		comment, hasComment := gatewayDNSFirewallRuleOption(rule.tokens, "--comment")
		if referencesOwnedChain && target == f.identity.Chain {
			if rule.chain != "INPUT" || !f.equalTokens(rule.tokens, f.hookRuleTokens()) {
				return fmt.Errorf("owned gateway DNS chain has unowned reference from %s rule %d", rule.chain, rule.position)
			}
		}
		if hasComment && strings.HasPrefix(comment, ownerNamespace) {
			switch {
			case rule.chain == "INPUT" && f.equalTokens(rule.tokens, f.hookRuleTokens()):
			case rule.chain == f.identity.Chain:
				if _, err := f.parseAllocationRule(rule.tokens); err != nil {
					return fmt.Errorf("owned gateway DNS chain contains invalid owned rule %d: %w", rule.position, err)
				}
			default:
				return fmt.Errorf("%s rule %d collides with gateway DNS owner identity %s", rule.chain, rule.position, f.identity.OwnerID)
			}
		}
	}
	for _, rule := range inventory.chainRules {
		comment, ok := gatewayDNSFirewallRuleOption(rule.tokens, "--comment")
		if !ok || !strings.HasPrefix(comment, ownerNamespace) {
			return fmt.Errorf("owned gateway DNS chain contains unowned rule %d", rule.position)
		}
		if _, err := f.parseAllocationRule(rule.tokens); err != nil {
			return fmt.Errorf("owned gateway DNS chain contains invalid owned rule %d: %w", rule.position, err)
		}
	}
	return nil
}

const (
	gatewayDNSFirewallVerdictAdmit = "admit"
	gatewayDNSFirewallVerdictGuard = "guard"
)

func (f *GatewayDNSFirewall) commentBase() string {
	return gatewayDNSFirewallCommentBase + ":" + f.identity.OwnerID
}

func (f *GatewayDNSFirewall) ruleTokens(alloc Allocation, protocol, verdict string) []string {
	rule := []string{
		"-A", f.identity.Chain,
		"-i", alloc.TAPDevice,
	}
	if verdict == gatewayDNSFirewallVerdictAdmit {
		rule = append(rule,
			"-s", alloc.GuestIP+"/32",
			"-d", alloc.GatewayIP+"/32",
		)
	}
	rule = append(rule,
		"-p", protocol,
		"--dport", "53",
		"-m", "comment",
		"--comment", f.allocationComment(alloc.TAPDevice, protocol, verdict),
	)
	if verdict == gatewayDNSFirewallVerdictGuard {
		return append(rule, "-j", "DROP")
	}
	return append(rule, "-j", "ACCEPT")
}

func (f *GatewayDNSFirewall) allocationComment(tap, protocol, verdict string) string {
	return f.commentBase() + ":alloc:" + tap + ":" + protocol + ":" + verdict
}

func (f *GatewayDNSFirewall) hookRuleTokens() []string {
	return append([]string{"-A", "INPUT"}, f.hookMatchTokens()...)
}

func (f *GatewayDNSFirewall) hookMatchTokens() []string {
	return []string{
		"-m", "comment",
		"--comment", f.commentBase() + ":input",
		"-j", f.identity.Chain,
	}
}

type gatewayDNSFirewallOwnedRule struct {
	tap      string
	protocol string
	verdict  string
}

func (f *GatewayDNSFirewall) ownedRuleKey(tokens []string) (gatewayDNSFirewallOwnedRule, bool) {
	key, err := f.parseAllocationRule(tokens)
	if err != nil {
		return gatewayDNSFirewallOwnedRule{}, false
	}
	return gatewayDNSFirewallOwnedRule{tap: key.inputInterface, protocol: key.protocol, verdict: key.verdict}, true
}

func gatewayDNSFirewallRuleTarget(tokens []string) (string, bool) {
	for index, token := range tokens {
		if token != "-j" && token != "--jump" && token != "-g" && token != "--goto" {
			continue
		}
		if index+1 >= len(tokens) {
			return "", false
		}
		return tokens[index+1], true
	}
	return "", false
}

func gatewayDNSFirewallRuleOption(tokens []string, option string) (string, bool) {
	for index, token := range tokens {
		if token == option && index+1 < len(tokens) {
			return tokens[index+1], true
		}
	}
	return "", false
}

func (f *GatewayDNSFirewall) canonicalRule(tokens []string) string {
	if key, err := f.parseAllocationRule(tokens); err == nil {
		return strings.Join([]string{
			"allocation",
			key.chain,
			key.inputInterface,
			key.source,
			key.destination,
			key.protocol,
			key.destinationPort,
			key.comment,
			key.target,
			key.verdict,
		}, "\x00")
	}

	// iptables -S may render the implicitly selected TCP/UDP match module as
	// "-m tcp" or "-m udp" even though the mutation argv only needs -p. Treat
	// that renderer detail as equivalent to the approved strict rule shapes.
	normalized := make([]string, 0, len(tokens))
	for index := 0; index < len(tokens); index++ {
		if index+1 < len(tokens) && tokens[index] == "-m" && (tokens[index+1] == "tcp" || tokens[index+1] == "udp") {
			index++
			continue
		}
		normalized = append(normalized, tokens[index])
	}
	return strings.Join(normalized, "\x00")
}

// gatewayDNSFirewallRuleKey models the only two semantic shapes admitted
// inside the kiln-vm-owned INPUT chain: an exact guest-to-own-gateway ACCEPT and a
// source/destination-independent per-TAP DNS DROP guard. iptables is free to
// reorder independent selectors and to render the implicit protocol matcher,
// so reconciliation compares this strict key rather than argv order. Unknown,
// duplicate, negated, forbidden, or missing selectors are rejected instead of
// being normalized into apparent ownership.
type gatewayDNSFirewallRuleKey struct {
	chain           string
	inputInterface  string
	source          string
	destination     string
	protocol        string
	destinationPort string
	comment         string
	target          string
	verdict         string
}

func (f *GatewayDNSFirewall) parseAllocationRule(tokens []string) (gatewayDNSFirewallRuleKey, error) {
	var key gatewayDNSFirewallRuleKey
	if len(tokens) < 2 || tokens[0] != "-A" || tokens[1] != f.identity.Chain {
		return key, fmt.Errorf("not an append rule for %s", f.identity.Chain)
	}
	key.chain = tokens[1]
	seen := make(map[string]bool)
	modules := make(map[string]bool)
	set := func(field, value string, destination *string) error {
		if seen[field] {
			return fmt.Errorf("duplicate %s selector", field)
		}
		if value == "" {
			return fmt.Errorf("empty %s selector", field)
		}
		seen[field] = true
		*destination = value
		return nil
	}
	for index := 2; index < len(tokens); {
		option := tokens[index]
		if option == "!" {
			return key, fmt.Errorf("negated selectors are not owned")
		}
		if index+1 >= len(tokens) {
			return key, fmt.Errorf("option %s has no value", option)
		}
		value := tokens[index+1]
		var err error
		switch option {
		case "-i", "--in-interface":
			err = set("input interface", value, &key.inputInterface)
		case "-s", "--source":
			err = set("source", value, &key.source)
		case "-d", "--destination":
			err = set("destination", value, &key.destination)
		case "-p", "--protocol":
			err = set("protocol", strings.ToLower(value), &key.protocol)
		case "--dport", "--destination-port":
			err = set("destination port", value, &key.destinationPort)
		case "--comment":
			err = set("comment", value, &key.comment)
		case "-j", "--jump":
			err = set("target", value, &key.target)
		case "-m", "--match":
			module := strings.ToLower(value)
			if module != "comment" && module != "udp" && module != "tcp" {
				return key, fmt.Errorf("unowned match module %q", value)
			}
			if modules[module] {
				return key, fmt.Errorf("duplicate match module %q", module)
			}
			modules[module] = true
		default:
			return key, fmt.Errorf("unowned option %q", option)
		}
		if err != nil {
			return key, err
		}
		index += 2
	}

	for field, present := range map[string]bool{
		"input interface":  seen["input interface"],
		"protocol":         seen["protocol"],
		"destination port": seen["destination port"],
		"comment":          seen["comment"],
		"target":           seen["target"],
	} {
		if !present {
			return key, fmt.Errorf("missing %s selector", field)
		}
	}
	if !modules["comment"] {
		return key, fmt.Errorf("missing comment match module")
	}
	if key.protocol != "udp" && key.protocol != "tcp" {
		return key, fmt.Errorf("unowned protocol %q", key.protocol)
	}
	if modules["udp"] && modules["tcp"] {
		return key, fmt.Errorf("multiple protocol match modules are not owned")
	}
	if (modules["udp"] || modules["tcp"]) && !modules[key.protocol] {
		return key, fmt.Errorf("protocol match module disagrees with %s", key.protocol)
	}
	if key.destinationPort != "53" {
		return key, fmt.Errorf("unowned destination port %q", key.destinationPort)
	}
	index, ok := networkIndexFromTAP(key.inputInterface)
	if !ok || key.inputInterface != fmt.Sprintf("kv%d", index) {
		return key, fmt.Errorf("invalid kiln-vm TAP identity %q", key.inputInterface)
	}
	commentPrefix := f.commentBase() + ":alloc:" + key.inputInterface + ":" + key.protocol + ":"
	if !strings.HasPrefix(key.comment, commentPrefix) {
		return key, fmt.Errorf("ownership comment %q does not match owner/allocation prefix %q", key.comment, commentPrefix)
	}
	key.verdict = strings.TrimPrefix(key.comment, commentPrefix)
	switch key.verdict {
	case gatewayDNSFirewallVerdictAdmit:
		if key.target != "ACCEPT" {
			return key, fmt.Errorf("admission rule has unowned target %q", key.target)
		}
		if !seen["source"] || !seen["destination"] {
			return key, fmt.Errorf("admission rule requires exact source and destination selectors")
		}
		canonicalSource, err := canonicalGatewayDNSFirewallHostPrefix(key.source)
		if err != nil {
			return key, fmt.Errorf("invalid admission source: %w", err)
		}
		key.source = canonicalSource
		canonicalDestination, err := canonicalGatewayDNSFirewallHostPrefix(key.destination)
		if err != nil {
			return key, fmt.Errorf("invalid admission destination: %w", err)
		}
		key.destination = canonicalDestination
	case gatewayDNSFirewallVerdictGuard:
		if key.target != "DROP" {
			return key, fmt.Errorf("guard rule has unowned target %q", key.target)
		}
		if seen["source"] || seen["destination"] {
			return key, fmt.Errorf("guard rule must not contain source or destination selectors")
		}
	default:
		return key, fmt.Errorf("ownership comment has unowned verdict %q", key.verdict)
	}
	return key, nil
}

func canonicalGatewayDNSFirewallHostPrefix(value string) (string, error) {
	prefix, err := netip.ParsePrefix(value)
	if err != nil || !prefix.Addr().Is4() || prefix.Bits() != 32 || prefix.Masked().Addr() != prefix.Addr() {
		return "", fmt.Errorf("%q is not a canonical IPv4 /32", value)
	}
	return prefix.String(), nil
}

func (f *GatewayDNSFirewall) equalTokens(left, right []string) bool {
	return f.canonicalRule(left) == f.canonicalRule(right)
}

// splitGatewayDNSFirewallIptablesLine handles the shell-style quoting emitted
// by iptables -S without invoking a shell or accepting shell expansion.
func splitGatewayDNSFirewallIptablesLine(line string) ([]string, error) {
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
