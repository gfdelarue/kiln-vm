package machine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

type networkOperationRecorder struct {
	mu    sync.Mutex
	calls []string
}

func TestNetworkReconcileCannotRecreateReleasedDNSArtifactsFromStaleSnapshot(t *testing.T) {
	recorder := &networkOperationRecorder{}
	proxy := &blockingDNSProxyController{
		fakeDNSProxyController: fakeDNSProxyController{recorder: recorder},
		started:                make(chan struct{}),
		unblock:                make(chan struct{}),
	}
	firewall := &fakeGatewayDNSFirewallController{recorder: recorder}
	host := &fakePublicNetworkHostOperations{recorder: recorder, fail: map[string]error{}}
	nm := newNetworkManager("172.16.0.0/16", 1, proxy, true, firewall, host)
	alloc := mustNetworkAllocation(t, nm, 1)
	nm.setAllocationState(*alloc, networkAllocationActive, nil)
	nm.setAllocationHostStatus(*alloc, readyPublicNetworkHostStatus())

	reconcileDone := make(chan error, 1)
	go func() { reconcileDone <- nm.Reconcile() }()
	select {
	case <-proxy.started:
	case <-time.After(time.Second):
		t.Fatal("reconciler did not enter proxy start")
	}
	releaseDone := make(chan error, 1)
	go func() { releaseDone <- nm.Release(alloc) }()

	select {
	case err := <-releaseDone:
		t.Fatalf("Release completed while Reconcile still held lifecycle ownership: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	close(proxy.unblock)
	if err := <-reconcileDone; err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if err := <-releaseDone; err != nil {
		t.Fatalf("Release: %v", err)
	}
	if records := nm.allocationSnapshot(); len(records) != 0 {
		t.Fatalf("released allocation registry = %+v, want empty", records)
	}

	lastProxyStart := -1
	lastProxyStop := -1
	for index, operation := range recorder.snapshot() {
		switch operation {
		case "proxy:start":
			lastProxyStart = index
		case "proxy:stop":
			lastProxyStop = index
		}
	}
	if lastProxyStart < 0 || lastProxyStop <= lastProxyStart {
		t.Fatalf("provider operations = %v, want final proxy stop after reconcile start", recorder.snapshot())
	}
}

type blockingDNSProxyController struct {
	fakeDNSProxyController
	started chan struct{}
	unblock chan struct{}
	once    sync.Once
}

func (p *blockingDNSProxyController) Start(gatewayIP string) error {
	p.fakeDNSProxyController.recorder.add("proxy:start")
	p.once.Do(func() { close(p.started) })
	<-p.unblock
	return p.startErr
}

func (r *networkOperationRecorder) add(call string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, call)
}

func (r *networkOperationRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}

func (r *networkOperationRecorder) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = nil
}

type fakePublicNetworkHostOperations struct {
	recorder           *networkOperationRecorder
	fail               map[string]error
	createOwnedOnError bool
	create             func(string, string) (bool, error)
	reconcile          func(Allocation) (PublicNetworkHostStatus, error)
	reconcileStatus    PublicNetworkHostStatus
	reconcileErr       error
}

func (h *fakePublicNetworkHostOperations) call(name string) error {
	h.recorder.add(name)
	return h.fail[name]
}

func (h *fakePublicNetworkHostOperations) CreateTAP(alloc Allocation) (bool, error) {
	if h.create != nil {
		h.recorder.add("tap:create")
		return h.create(alloc.TAPDevice, alloc.GatewayIP)
	}
	err := h.call("tap:create")
	if err != nil {
		return h.createOwnedOnError, err
	}
	return true, nil
}

func (h *fakePublicNetworkHostOperations) SetupNAT(string, string) error {
	return h.call("nat:setup")
}

func (h *fakePublicNetworkHostOperations) TeardownNAT(string, string) error {
	return h.call("nat:teardown")
}

func (h *fakePublicNetworkHostOperations) DeleteTAP(Allocation) error {
	return h.call("tap:delete")
}

func (h *fakePublicNetworkHostOperations) ReconcileActive(alloc Allocation) (PublicNetworkHostStatus, error) {
	if h.reconcile != nil {
		h.recorder.add("host:reconcile")
		return h.reconcile(alloc)
	}
	callErr := h.call("host:reconcile")
	reconcileErr := errors.Join(callErr, h.reconcileErr)
	status := h.reconcileStatus
	if status == (PublicNetworkHostStatus{}) && reconcileErr == nil {
		status = readyPublicNetworkHostStatus()
	}
	if reconcileErr != nil {
		status.LastError = reconcileErr.Error()
	}
	return status, reconcileErr
}

func readyPublicNetworkHostStatus() PublicNetworkHostStatus {
	return PublicNetworkHostStatus{
		TAPPresent:          true,
		TAPIsTAP:            true,
		TAPOwned:            true,
		TAPAddressInstalled: true,
		TAPLinkUp:           true,
		IPv4Forwarding:      true,
		MasqueradeRules:     1,
		ForwardIngressRules: 1,
		ForwardEgressRules:  1,
		Ready:               true,
	}
}

type fakeDNSProxyController struct {
	mu                sync.Mutex
	recorder          *networkOperationRecorder
	startErr          error
	stopErr           error
	running           bool
	stopped           bool
	keepRunningOnStop bool
}

func (p *fakeDNSProxyController) Start(string) error {
	p.recorder.add("proxy:start")
	p.mu.Lock()
	p.running = true // Start failures may still leave one transport bound.
	p.stopped = false
	p.mu.Unlock()
	return p.startErr
}

func (p *fakeDNSProxyController) Stop(string) error {
	p.recorder.add("proxy:stop")
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopErr == nil && !p.keepRunningOnStop {
		p.running = false
		p.stopped = true
	}
	return p.stopErr
}

func (p *fakeDNSProxyController) Status(string) (DNSProxyStatus, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopped {
		return DNSProxyStatus{}, false
	}
	// A zero-value fake represents a pre-existing active listener in status-only
	// and reconciliation tests. Start explicitly transitions to the same state.
	return DNSProxyStatus{Running: true}, true
}

type fakeGatewayDNSFirewallController struct {
	mu            sync.Mutex
	recorder      *networkOperationRecorder
	guardErr      error
	activateErr   error
	deactivateErr error
	releaseErr    error
	reconcileErr  error
	reconciled    []fakeGatewayDNSFirewallSnapshot
}

type fakeGatewayDNSFirewallSnapshot struct {
	guarded  []Allocation
	admitted []Allocation
}

func (f *fakeGatewayDNSFirewallController) Guard(Allocation) error {
	f.recorder.add("admission:guard")
	return f.guardErr
}

func (f *fakeGatewayDNSFirewallController) Activate(Allocation) error {
	f.recorder.add("admission:activate")
	return f.activateErr
}

func (f *fakeGatewayDNSFirewallController) Deactivate(Allocation) error {
	f.recorder.add("admission:deactivate")
	return f.deactivateErr
}

func (f *fakeGatewayDNSFirewallController) Release(Allocation) error {
	f.recorder.add("admission:release")
	return f.releaseErr
}

func (f *fakeGatewayDNSFirewallController) ReconcileState(guarded, admitted []Allocation) error {
	f.recorder.add("admission:reconcile")
	f.mu.Lock()
	f.reconciled = append(f.reconciled, fakeGatewayDNSFirewallSnapshot{
		guarded:  append([]Allocation(nil), guarded...),
		admitted: append([]Allocation(nil), admitted...),
	})
	f.mu.Unlock()
	return f.reconcileErr
}

func (f *fakeGatewayDNSFirewallController) Status(Allocation) GatewayDNSFirewallStatus {
	return GatewayDNSFirewallStatus{
		Desired:           true,
		GuardDesired:      true,
		AdmissionDesired:  true,
		HookInstalled:     true,
		UDPInstalled:      true,
		TCPInstalled:      true,
		GuardUDPInstalled: true,
		GuardTCPInstalled: true,
		GuardReady:        true,
		AdmissionReady:    true,
		Ready:             true,
	}
}

func (f *fakeGatewayDNSFirewallController) lastReconcileCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.reconciled) == 0 {
		return -1
	}
	return len(f.reconciled[len(f.reconciled)-1].admitted)
}

func (f *fakeGatewayDNSFirewallController) lastReconcileSnapshot() fakeGatewayDNSFirewallSnapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.reconciled) == 0 {
		return fakeGatewayDNSFirewallSnapshot{}
	}
	snapshot := f.reconciled[len(f.reconciled)-1]
	return fakeGatewayDNSFirewallSnapshot{
		guarded:  append([]Allocation(nil), snapshot.guarded...),
		admitted: append([]Allocation(nil), snapshot.admitted...),
	}
}

func TestNewNetworkManagerWithDNSDataDir(t *testing.T) {
	nm := NewNetworkManagerWithDNSDataDir("172.16.0.0/16", 1, nil, "")
	if nm == nil {
		t.Fatal("NewNetworkManagerWithDNSDataDir returned nil")
	}
	if nm.baseCIDR != "172.16.0.0/16" {
		t.Errorf("baseCIDR = %q, want %q", nm.baseCIDR, "172.16.0.0/16")
	}
	if nm.nextIdx != 1 {
		t.Errorf("nextIdx = %d, want 1", nm.nextIdx)
	}
}

func TestGatewayDNSStatusUsesCachedComponentStateWithoutHostCommands(t *testing.T) {
	recorder := &networkOperationRecorder{}
	proxy := &fakeDNSProxyController{recorder: recorder}
	firewall := &fakeGatewayDNSFirewallController{recorder: recorder}
	host := &fakePublicNetworkHostOperations{recorder: recorder}
	nm := newNetworkManager("172.16.0.0/16", 1, proxy, true, firewall, host)
	alloc := mustNetworkAllocation(t, nm, 1)
	nm.setAllocationState(*alloc, networkAllocationActive, nil)
	nm.setAllocationHostStatus(*alloc, readyPublicNetworkHostStatus())

	recorder.calls = nil
	status := nm.GatewayDNSStatus(alloc)
	if !status.Desired || !status.Ready || !status.ProxyObserved || !status.Proxy.Running || !status.Firewall.Ready {
		t.Fatalf("GatewayDNSStatus() = %+v, want ready cached state", status)
	}
	if len(recorder.calls) != 0 {
		t.Fatalf("GatewayDNSStatus performed host mutation: %v", recorder.calls)
	}
}

func TestGatewayDNSStatusRejectsMismatchedAllocationIdentity(t *testing.T) {
	recorder := &networkOperationRecorder{}
	nm := newNetworkManager(
		"172.16.0.0/16",
		1,
		&fakeDNSProxyController{recorder: recorder},
		true,
		&fakeGatewayDNSFirewallController{recorder: recorder},
		&fakePublicNetworkHostOperations{recorder: recorder, fail: map[string]error{}},
	)
	alloc := mustNetworkAllocation(t, nm, 1)
	nm.setAllocationState(*alloc, networkAllocationActive, nil)
	mismatched := *alloc
	mismatched.GatewayIP = "172.16.0.9"

	status := nm.GatewayDNSStatus(&mismatched)
	if status.Desired || status.Ready {
		t.Fatalf("GatewayDNSStatus() = %+v, want mismatched identity rejected", status)
	}
}

func TestNetworkReserveRejectsNonCanonicalTupleBeforeHostMutation(t *testing.T) {
	base := mustNetworkAllocation(t, NewNetworkManagerWithDNSDataDir("172.16.0.0/16", 1, nil, ""), 1)
	tests := []struct {
		name   string
		mutate func(*Allocation)
	}{
		{name: "tap identity", mutate: func(alloc *Allocation) { alloc.TAPDevice = "kv01" }},
		{name: "guest address", mutate: func(alloc *Allocation) { alloc.GuestIP = "172.16.0.10" }},
		{name: "gateway address", mutate: func(alloc *Allocation) { alloc.GatewayIP = "172.16.0.9" }},
		{name: "guest mac", mutate: func(alloc *Allocation) { alloc.GuestMAC = "02:FC:00:00:00:FF" }},
		{name: "empty guest mac", mutate: func(alloc *Allocation) { alloc.GuestMAC = "" }},
		{name: "mask", mutate: func(alloc *Allocation) { alloc.Mask = "255.255.255.0" }},
		{name: "empty mask", mutate: func(alloc *Allocation) { alloc.Mask = "" }},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := &networkOperationRecorder{}
			host := &fakePublicNetworkHostOperations{recorder: recorder, fail: map[string]error{}}
			manager := newNetworkManager("172.16.0.0/16", 1, nil, false, nil, host)
			candidate := *base
			test.mutate(&candidate)

			reserved, err := manager.Reserve(&candidate)
			if err == nil || reserved != nil {
				t.Fatalf("Reserve(%+v) = %+v, %v; want nil and canonical validation error", candidate, reserved, err)
			}
			if calls := recorder.snapshot(); len(calls) != 0 {
				t.Fatalf("host calls after rejected reserve = %v, want none", calls)
			}
			if records := manager.allocationSnapshot(); len(records) != 0 {
				t.Fatalf("allocation registry after rejected reserve = %+v, want empty", records)
			}
		})
	}
}

func TestNetworkReleaseAcceptsVerifiedHistoricalAllocationAfterCIDRChange(t *testing.T) {
	oldAllocation, err := allocationForIndex("172.16.0.0/16", 41)
	if err != nil {
		t.Fatalf("old allocationForIndex: %v", err)
	}
	oldAllocation.OwnerID = "historical-machine"
	recorder := &networkOperationRecorder{}
	manager := newNetworkManager(
		"10.77.0.0/16",
		1,
		&fakeDNSProxyController{recorder: recorder},
		true,
		&fakeGatewayDNSFirewallController{recorder: recorder},
		&fakePublicNetworkHostOperations{recorder: recorder, fail: map[string]error{}},
	)

	if err := manager.Release(oldAllocation); err != nil {
		t.Fatalf("Release historical allocation: %v", err)
	}
	want := []string{"admission:deactivate", "proxy:stop", "nat:teardown", "tap:delete", "admission:release"}
	if got := recorder.snapshot(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("historical cleanup calls = %v, want %v", got, want)
	}
	if _, err := manager.Reserve(oldAllocation); err == nil {
		t.Fatal("Reserve accepted allocation from prior BridgeCIDR")
	}
}

func TestNetworkAllocatePreOwnershipCreateFailureReturnsNoAllocationOrCleanupIntent(t *testing.T) {
	recorder := &networkOperationRecorder{}
	host := &fakePublicNetworkHostOperations{
		recorder: recorder,
		fail:     map[string]error{"tap:create": errors.New("operation not permitted")},
	}
	manager := newNetworkManager("172.16.0.0/16", 1, nil, false, nil, host)

	alloc, err := manager.Allocate("vm-pre-ownership")
	if err == nil || alloc != nil {
		t.Fatalf("Allocate() = %+v, %v; want nil allocation and create failure", alloc, err)
	}
	if got := recorder.snapshot(); fmt.Sprint(got) != fmt.Sprint([]string{"tap:create"}) {
		t.Fatalf("host calls = %v, want no generic cleanup after pre-ownership failure", got)
	}
	if records := manager.allocationSnapshot(); len(records) != 0 {
		t.Fatalf("allocation registry = %+v, want no cleanup intent before ownership", records)
	}
}

func TestNetworkAllocateSkipsUnownedCollisionWithoutDeletingIt(t *testing.T) {
	recorder := &networkOperationRecorder{}
	host := &fakePublicNetworkHostOperations{recorder: recorder, fail: map[string]error{}}
	host.create = func(name, _ string) (bool, error) {
		if name == "kv1" {
			return false, errors.New("Device or resource busy")
		}
		return true, nil
	}
	manager := newNetworkManager("172.16.0.0/16", 1, nil, false, nil, host)

	alloc, err := manager.Allocate("vm-collision")
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if alloc == nil || alloc.TAPDevice != "kv2" {
		t.Fatalf("Allocate() = %+v, want canonical kv2 allocation", alloc)
	}
	for _, call := range recorder.snapshot() {
		if call == "tap:delete" {
			t.Fatalf("host calls = %v, must not delete unowned collision", recorder.snapshot())
		}
	}
}

func TestNetworkAllocateReturnsAttemptedAllocationWhenRollbackLeavesOwnedStages(t *testing.T) {
	recorder := &networkOperationRecorder{}
	host := &fakePublicNetworkHostOperations{
		recorder: recorder,
		fail: map[string]error{
			"nat:setup":  errors.New("iptables append failed"),
			"tap:delete": errors.New("link deletion failed"),
		},
	}
	manager := newNetworkManager("172.16.0.0/16", 1, nil, false, nil, host)

	alloc, err := manager.Allocate("vm-partial")
	if err == nil || alloc == nil || alloc.TAPDevice != "kv1" {
		t.Fatalf("Allocate() = %+v, %v; want attempted kv1 allocation and rollback error", alloc, err)
	}
	if !strings.Contains(err.Error(), "iptables append failed") || !strings.Contains(err.Error(), "link deletion failed") {
		t.Fatalf("Allocate error = %v, want primary and cleanup errors", err)
	}
	records := manager.allocationSnapshot()
	if len(records) != 1 || records[0].Allocation != *alloc || records[0].State != networkAllocationCleanupPending || records[0].LastError == "" {
		t.Fatalf("allocation registry = %+v, want owned cleanup_pending allocation", records)
	}
	wantCalls := []string{"tap:create", "nat:setup", "nat:teardown", "tap:delete"}
	if got := recorder.snapshot(); fmt.Sprint(got) != fmt.Sprint(wantCalls) {
		t.Fatalf("host calls = %v, want %v", got, wantCalls)
	}
}

func TestNetworkAllocateFullyRolledBackFailureReturnsNoAllocation(t *testing.T) {
	recorder := &networkOperationRecorder{}
	host := &fakePublicNetworkHostOperations{
		recorder: recorder,
		fail:     map[string]error{"nat:setup": errors.New("iptables append failed")},
	}
	manager := newNetworkManager("172.16.0.0/16", 1, nil, false, nil, host)

	alloc, err := manager.Allocate("vm-rolled-back")
	if err == nil || alloc != nil {
		t.Fatalf("Allocate() = %+v, %v; want nil allocation after complete rollback", alloc, err)
	}
	if records := manager.allocationSnapshot(); len(records) != 0 {
		t.Fatalf("allocation registry = %+v, want empty after complete rollback", records)
	}
}

func TestNetworkReconcileRepairsHostDriftAndClearsCachedError(t *testing.T) {
	recorder := &networkOperationRecorder{}
	drifted := true
	host := &fakePublicNetworkHostOperations{recorder: recorder, fail: map[string]error{}}
	host.reconcile = func(Allocation) (PublicNetworkHostStatus, error) {
		if drifted {
			status := PublicNetworkHostStatus{TAPPresent: true, TAPIsTAP: true, LastError: "host drift"}
			return status, errors.New("host drift")
		}
		return readyPublicNetworkHostStatus(), nil
	}
	proxy := &fakeDNSProxyController{recorder: recorder}
	firewall := &fakeGatewayDNSFirewallController{recorder: recorder}
	manager := newNetworkManager("172.16.0.0/16", 1, proxy, true, firewall, host)
	alloc := mustNetworkAllocation(t, manager, 1)
	manager.setAllocationState(*alloc, networkAllocationActive, errors.New("previous failure"))

	if err := manager.Reconcile(); err == nil || !strings.Contains(err.Error(), "host drift") {
		t.Fatalf("first Reconcile() error = %v, want host drift", err)
	}
	degraded := manager.GatewayDNSStatus(alloc)
	if !degraded.HostObserved || degraded.Host.Ready || degraded.LastError == "" || degraded.Ready {
		t.Fatalf("degraded GatewayDNSStatus = %+v", degraded)
	}

	drifted = false
	if err := manager.Reconcile(); err != nil {
		t.Fatalf("healing Reconcile: %v", err)
	}
	healed := manager.GatewayDNSStatus(alloc)
	if !healed.HostObserved || !healed.Host.Ready || healed.LastError != "" || !healed.Ready {
		t.Fatalf("healed GatewayDNSStatus = %+v, want ready and cleared error", healed)
	}
}

func TestNetworkReconcileHostCollisionDegradesWithoutGenericCleanup(t *testing.T) {
	recorder := &networkOperationRecorder{}
	host := &fakePublicNetworkHostOperations{recorder: recorder, fail: map[string]error{}}
	host.reconcile = func(Allocation) (PublicNetworkHostStatus, error) {
		return PublicNetworkHostStatus{TAPPresent: true, TAPOwnershipCollision: true}, errors.New("link collision: want tun/tap")
	}
	proxy := &fakeDNSProxyController{recorder: recorder}
	firewall := &fakeGatewayDNSFirewallController{recorder: recorder}
	manager := newNetworkManager(
		"172.16.0.0/16",
		1,
		proxy,
		true,
		firewall,
		host,
	)
	alloc := mustNetworkAllocation(t, manager, 1)
	manager.setAllocationState(*alloc, networkAllocationActive, nil)

	if err := manager.Reconcile(); err == nil || !strings.Contains(err.Error(), "link collision") {
		t.Fatalf("Reconcile() error = %v, want collision", err)
	}
	for _, call := range recorder.snapshot() {
		if call == "tap:delete" || call == "nat:teardown" {
			t.Fatalf("host calls = %v, active collision must not trigger generic cleanup", recorder.snapshot())
		}
	}
	if !containsNetworkOperation(recorder.snapshot(), "proxy:stop") {
		t.Fatalf("host collision operations = %v, want proxy admission closed", recorder.snapshot())
	}
	assertNetworkOperationBefore(t, recorder.snapshot(), "admission:deactivate", "proxy:stop")
	if got := firewall.lastReconcileCount(); got != 0 {
		t.Fatalf("firewall desired allocations after host collision = %d, want zero", got)
	}
	assertGatewayDNSFirewallSnapshot(t, firewall.lastReconcileSnapshot(), []Allocation{*alloc}, nil)
	status := manager.GatewayDNSStatus(alloc)
	if !status.HostObserved || !status.Host.TAPOwnershipCollision || status.Host.TAPIsTAP || status.Ready {
		t.Fatalf("GatewayDNSStatus = %+v, want cached host collision", status)
	}
}

func containsNetworkOperation(operations []string, want string) bool {
	for _, operation := range operations {
		if operation == want {
			return true
		}
	}
	return false
}

func TestSequentialAllocationsGetDifferentIPs(t *testing.T) {
	nm := NewNetworkManagerWithDNSDataDir("172.16.0.0/16", 1, nil, "")

	var allocs []*Allocation
	for i := 0; i < 5; i++ {
		nm.mu.Lock()
		idx := nm.nextIdx
		nm.nextIdx++
		nm.mu.Unlock()

		allocs = append(allocs, mustNetworkAllocation(t, nm, idx))
	}

	// Verify all guest IPs are distinct
	seen := make(map[string]bool)
	for _, a := range allocs {
		if seen[a.GuestIP] {
			t.Errorf("duplicate guest IP: %s", a.GuestIP)
		}
		seen[a.GuestIP] = true
	}

	// Verify all gateway IPs are distinct
	seenGW := make(map[string]bool)
	for _, a := range allocs {
		if seenGW[a.GatewayIP] {
			t.Errorf("duplicate gateway IP: %s", a.GatewayIP)
		}
		seenGW[a.GatewayIP] = true
	}

	// Verify all TAP device names are distinct
	seenTAP := make(map[string]bool)
	for _, a := range allocs {
		if seenTAP[a.TAPDevice] {
			t.Errorf("duplicate TAP device: %s", a.TAPDevice)
		}
		seenTAP[a.TAPDevice] = true
	}
}

func TestIPsAreIn172_16Range(t *testing.T) {
	nm := NewNetworkManagerWithDNSDataDir("172.16.0.0/16", 1, nil, "")

	for i := 0; i < 10; i++ {
		nm.mu.Lock()
		idx := nm.nextIdx
		nm.nextIdx++
		nm.mu.Unlock()

		alloc := mustNetworkAllocation(t, nm, idx)

		if !strings.HasPrefix(alloc.GuestIP, "172.16.") {
			t.Errorf("guest IP %q is not in 172.16.x.x range", alloc.GuestIP)
		}
		if !strings.HasPrefix(alloc.GatewayIP, "172.16.") {
			t.Errorf("gateway IP %q is not in 172.16.x.x range", alloc.GatewayIP)
		}
	}
}

func TestIPMathFirstAllocation(t *testing.T) {
	nm := NewNetworkManagerWithDNSDataDir("172.16.0.0/16", 1, nil, "")

	// First allocation: index=1, offset=4
	nm.mu.Lock()
	idx := nm.nextIdx
	nm.nextIdx++
	nm.mu.Unlock()

	if idx != 1 {
		t.Fatalf("expected first index = 1, got %d", idx)
	}

	alloc := mustNetworkAllocation(t, nm, idx)

	if alloc.GatewayIP != "172.16.0.5" {
		t.Errorf("first gateway IP = %q, want %q", alloc.GatewayIP, "172.16.0.5")
	}
	if alloc.GuestIP != "172.16.0.6" {
		t.Errorf("first guest IP = %q, want %q", alloc.GuestIP, "172.16.0.6")
	}
}

func TestNetworkIndexFromTAP(t *testing.T) {
	idx, ok := networkIndexFromTAP("kv42")
	if !ok || idx != 42 {
		t.Fatalf("networkIndexFromTAP(kv42) = %d, %v; want 42, true", idx, ok)
	}
	for _, name := range []string{"", "tap42", "kiln-vm", "kv0", "kilnabc"} {
		if idx, ok := networkIndexFromTAP(name); ok {
			t.Fatalf("networkIndexFromTAP(%q) = %d, true; want false", name, idx)
		}
	}
}

func TestNextIdxIncrements(t *testing.T) {
	nm := NewNetworkManagerWithDNSDataDir("172.16.0.0/16", 1, nil, "")

	if nm.nextIdx != 1 {
		t.Fatalf("initial nextIdx = %d, want 1", nm.nextIdx)
	}

	nm.mu.Lock()
	nm.nextIdx++
	nm.mu.Unlock()

	if nm.nextIdx != 2 {
		t.Errorf("after increment nextIdx = %d, want 2", nm.nextIdx)
	}
}

func TestMissingLinkOutputRecognizesIdempotentDeleteCases(t *testing.T) {
	for _, output := range []string{
		"Cannot find device \"kv1\"",
		"Device \"kv1\" does not exist.",
	} {
		if !missingLinkOutput(output) {
			t.Fatalf("missingLinkOutput(%q) = false, want true", output)
		}
	}

	if missingLinkOutput("permission denied") {
		t.Fatal("missingLinkOutput returned true for unrelated error")
	}
}

func TestTapAlreadyExistsErrorRecognizesStaleDeviceCases(t *testing.T) {
	for _, text := range []string{
		"[ip tuntap add dev kv2 mode tap]: ioctl(TUNSETIFF): Device or resource busy",
		"[ip tuntap add dev kv2 mode tap]: RTNETLINK answers: File exists",
	} {
		if !tapAlreadyExistsError(errors.New(text)) {
			t.Fatalf("tapAlreadyExistsError(%q) = false, want true", text)
		}
	}
}

func TestIPv4ForwardingEnabled(t *testing.T) {
	for _, value := range []string{"1", "1\n", " 1 \n"} {
		if !ipv4ForwardingEnabled(value) {
			t.Fatalf("ipv4ForwardingEnabled(%q) = false, want true", value)
		}
	}
	for _, value := range []string{"", "0", "2", "true"} {
		if ipv4ForwardingEnabled(value) {
			t.Fatalf("ipv4ForwardingEnabled(%q) = true, want false", value)
		}
	}
}

func TestRenderResolvConf(t *testing.T) {
	got := renderResolvConf([]string{" 1.1.1.1 ", "", "8.8.8.8"})
	want := "# Generated by kiln-vm.\nnameserver 1.1.1.1\nnameserver 8.8.8.8\n"
	if got != want {
		t.Fatalf("renderResolvConf = %q, want %q", got, want)
	}
}

func TestNetworkAllocationRollsBackInProviderOrderWhenDNSAdmissionFails(t *testing.T) {
	recorder := &networkOperationRecorder{}
	host := &fakePublicNetworkHostOperations{recorder: recorder, fail: map[string]error{}}
	proxy := &fakeDNSProxyController{recorder: recorder}
	firewall := &fakeGatewayDNSFirewallController{recorder: recorder, activateErr: errors.New("INPUT blocked")}
	manager := newNetworkManager("172.16.0.0/16", 1, proxy, true, firewall, host)

	_, err := manager.Allocate("vm-1")
	if err == nil || !strings.Contains(err.Error(), "INPUT blocked") {
		t.Fatalf("Allocate error = %v, want DNS admission failure", err)
	}
	want := []string{
		"tap:create",
		"nat:setup",
		"host:reconcile",
		"admission:guard",
		"proxy:start",
		"admission:activate",
		"admission:deactivate",
		"proxy:stop",
		"nat:teardown",
		"tap:delete",
		"admission:release",
	}
	if got := fmt.Sprint(recorder.calls); got != fmt.Sprint(want) {
		t.Fatalf("provider operation order = %v, want %v", recorder.calls, want)
	}
	if records := manager.allocationSnapshot(); len(records) != 0 {
		t.Fatalf("allocation registry after successful rollback = %+v, want empty", records)
	}
}

func TestNetworkActivationGuardsBeforeListenerAndAdmitsOnlyAfterListenerReady(t *testing.T) {
	recorder := &networkOperationRecorder{}
	host := &fakePublicNetworkHostOperations{recorder: recorder, fail: map[string]error{}}
	proxy := &fakeDNSProxyController{recorder: recorder}
	firewall := &fakeGatewayDNSFirewallController{recorder: recorder}
	manager := newNetworkManager("172.16.0.0/16", 1, proxy, true, firewall, host)

	alloc, err := manager.Allocate("vm-ordered-activation")
	if err != nil || alloc == nil {
		t.Fatalf("Allocate() = %+v, %v", alloc, err)
	}
	want := []string{
		"tap:create",
		"nat:setup",
		"host:reconcile",
		"admission:guard",
		"proxy:start",
		"admission:activate",
	}
	if got := recorder.snapshot(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("activation order = %v, want %v", got, want)
	}
	records := manager.allocationSnapshot()
	if len(records) != 1 || records[0].State != networkAllocationActive || records[0].Allocation != *alloc {
		t.Fatalf("allocation registry = %+v, want active allocation", records)
	}
}

func TestNetworkGuardFailureNeverStartsListener(t *testing.T) {
	recorder := &networkOperationRecorder{}
	host := &fakePublicNetworkHostOperations{recorder: recorder, fail: map[string]error{}}
	proxy := &fakeDNSProxyController{recorder: recorder}
	firewall := &fakeGatewayDNSFirewallController{recorder: recorder, guardErr: errors.New("guard installation failed")}
	manager := newNetworkManager("172.16.0.0/16", 1, proxy, true, firewall, host)

	alloc, err := manager.Allocate("vm-guard-failure")
	if err == nil || alloc != nil || !strings.Contains(err.Error(), firewall.guardErr.Error()) {
		t.Fatalf("Allocate() = %+v, %v; want fully rolled-back guard failure", alloc, err)
	}
	want := []string{
		"tap:create",
		"nat:setup",
		"host:reconcile",
		"admission:guard",
		"admission:deactivate",
		"nat:teardown",
		"tap:delete",
		"admission:release",
	}
	if got := recorder.snapshot(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("guard failure order = %v, want %v", got, want)
	}
	if containsNetworkOperation(recorder.snapshot(), "proxy:start") {
		t.Fatalf("guard failure operations = %v, listener must not start", recorder.snapshot())
	}
}

func TestNetworkAdmissionActivationRollbackFailureRemainsCleanupPending(t *testing.T) {
	recorder := &networkOperationRecorder{}
	host := &fakePublicNetworkHostOperations{recorder: recorder, fail: map[string]error{}}
	proxy := &fakeDNSProxyController{recorder: recorder}
	firewall := &fakeGatewayDNSFirewallController{
		recorder:    recorder,
		activateErr: errors.New("install admission failed and immediate rollback was incomplete"),
		releaseErr:  errors.New("admission cleanup still blocked"),
	}
	manager := newNetworkManager("172.16.0.0/16", 1, proxy, true, firewall, host)

	alloc, err := manager.Allocate("vm-1")
	if err == nil || alloc == nil || !strings.Contains(err.Error(), firewall.releaseErr.Error()) {
		t.Fatalf("Allocate = alloc=%+v err=%v, want durable admission cleanup failure", alloc, err)
	}
	records := manager.allocationSnapshot()
	if len(records) != 1 || records[0].State != networkAllocationCleanupPending || records[0].Allocation != *alloc {
		t.Fatalf("allocation registry = %+v, want cleanup_pending allocation", records)
	}
	want := []string{
		"tap:create",
		"nat:setup",
		"host:reconcile",
		"admission:guard",
		"proxy:start",
		"admission:activate",
		"admission:deactivate",
		"proxy:stop",
		"nat:teardown",
		"tap:delete",
		"admission:release",
	}
	if got := fmt.Sprint(recorder.calls); got != fmt.Sprint(want) {
		t.Fatalf("provider operation order = %v, want %v", recorder.calls, want)
	}
}

func TestNetworkProxyActivationRollbackFailureRemainsCleanupPending(t *testing.T) {
	recorder := &networkOperationRecorder{}
	host := &fakePublicNetworkHostOperations{recorder: recorder, fail: map[string]error{}}
	proxy := &fakeDNSProxyController{
		recorder: recorder,
		startErr: errors.New("TCP listener bind failed after UDP bind"),
		stopErr:  errors.New("partial listener cleanup blocked"),
	}
	manager := newNetworkManager("172.16.0.0/16", 1, proxy, true, nil, host)

	alloc, err := manager.Allocate("vm-1")
	if err == nil || alloc == nil || !strings.Contains(err.Error(), proxy.stopErr.Error()) {
		t.Fatalf("Allocate = alloc=%+v err=%v, want durable proxy cleanup failure", alloc, err)
	}
	records := manager.allocationSnapshot()
	if len(records) != 1 || records[0].State != networkAllocationCleanupPending || records[0].Allocation != *alloc {
		t.Fatalf("allocation registry = %+v, want cleanup_pending allocation", records)
	}
	want := []string{
		"tap:create",
		"nat:setup",
		"host:reconcile",
		"proxy:start",
		"proxy:stop",
	}
	if got := fmt.Sprint(recorder.calls); got != fmt.Sprint(want) {
		t.Fatalf("provider operation order = %v, want %v", recorder.calls, want)
	}
}

func TestNetworkReleaseDeactivateFailureDoesNotStopListener(t *testing.T) {
	recorder := &networkOperationRecorder{}
	host := &fakePublicNetworkHostOperations{recorder: recorder, fail: map[string]error{}}
	proxy := &fakeDNSProxyController{recorder: recorder}
	firewall := &fakeGatewayDNSFirewallController{recorder: recorder, deactivateErr: errors.New("admission close blocked")}
	manager := newNetworkManager("172.16.0.0/16", 1, proxy, true, firewall, host)
	alloc := mustNetworkAllocation(t, manager, 1)
	manager.setAllocationState(*alloc, networkAllocationActive, nil)

	err := manager.Release(alloc)
	if err == nil || !strings.Contains(err.Error(), firewall.deactivateErr.Error()) {
		t.Fatalf("Release() error = %v, want admission-close failure", err)
	}
	if got := recorder.snapshot(); fmt.Sprint(got) != fmt.Sprint([]string{"admission:deactivate"}) {
		t.Fatalf("failed Deactivate operations = %v, want listener untouched", got)
	}
	assertNetworkCleanupPending(t, manager, *alloc)

	recorder.reset()
	if err := manager.Reconcile(); err == nil || !strings.Contains(err.Error(), firewall.deactivateErr.Error()) {
		t.Fatalf("Reconcile() error = %v, want admission-close failure", err)
	}
	if got := recorder.snapshot(); fmt.Sprint(got) != fmt.Sprint([]string{"admission:deactivate", "admission:reconcile"}) {
		t.Fatalf("failed cleanup reconciliation = %v, want no listener stop", got)
	}
	assertGatewayDNSFirewallSnapshot(t, firewall.lastReconcileSnapshot(), []Allocation{*alloc}, nil)

	firewall.deactivateErr = nil
	recorder.reset()
	if err := manager.Reconcile(); err != nil {
		t.Fatalf("healing Reconcile: %v", err)
	}
	want := []string{"admission:deactivate", "proxy:stop", "nat:teardown", "tap:delete", "admission:release", "admission:reconcile"}
	if got := recorder.snapshot(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("healed cleanup order = %v, want %v", got, want)
	}
	if records := manager.allocationSnapshot(); len(records) != 0 {
		t.Fatalf("healed allocation registry = %+v, want empty", records)
	}
}

func TestNetworkReleaseStopFailureRetainsGuardAndOuterResources(t *testing.T) {
	recorder := &networkOperationRecorder{}
	host := &fakePublicNetworkHostOperations{recorder: recorder, fail: map[string]error{}}
	proxy := &fakeDNSProxyController{recorder: recorder, stopErr: errors.New("listener close blocked")}
	firewall := &fakeGatewayDNSFirewallController{recorder: recorder}
	manager := newNetworkManager("172.16.0.0/16", 1, proxy, true, firewall, host)
	alloc := mustNetworkAllocation(t, manager, 1)
	manager.setAllocationState(*alloc, networkAllocationActive, nil)

	err := manager.Release(alloc)
	if err == nil || !strings.Contains(err.Error(), proxy.stopErr.Error()) {
		t.Fatalf("Release() error = %v, want listener-stop failure", err)
	}
	if got := recorder.snapshot(); fmt.Sprint(got) != fmt.Sprint([]string{"admission:deactivate", "proxy:stop"}) {
		t.Fatalf("failed Stop operations = %v, want guard and outer resources retained", got)
	}
	assertNetworkCleanupPending(t, manager, *alloc)

	recorder.reset()
	if err := manager.Reconcile(); err == nil || !strings.Contains(err.Error(), proxy.stopErr.Error()) {
		t.Fatalf("Reconcile() error = %v, want listener-stop failure", err)
	}
	if containsNetworkOperation(recorder.snapshot(), "nat:teardown") || containsNetworkOperation(recorder.snapshot(), "tap:delete") || containsNetworkOperation(recorder.snapshot(), "admission:release") {
		t.Fatalf("failed Stop reconciliation crossed outer cleanup boundary: %v", recorder.snapshot())
	}
	assertGatewayDNSFirewallSnapshot(t, firewall.lastReconcileSnapshot(), []Allocation{*alloc}, nil)
}

func TestNetworkReleaseRequiresConfirmedListenerStop(t *testing.T) {
	recorder := &networkOperationRecorder{}
	host := &fakePublicNetworkHostOperations{recorder: recorder, fail: map[string]error{}}
	proxy := &fakeDNSProxyController{recorder: recorder, keepRunningOnStop: true}
	firewall := &fakeGatewayDNSFirewallController{recorder: recorder}
	manager := newNetworkManager("172.16.0.0/16", 1, proxy, true, firewall, host)
	alloc := mustNetworkAllocation(t, manager, 1)
	manager.setAllocationState(*alloc, networkAllocationActive, nil)

	err := manager.Release(alloc)
	if err == nil || !strings.Contains(err.Error(), "listener still reports running") {
		t.Fatalf("Release() error = %v, want unconfirmed-stop failure", err)
	}
	if got := recorder.snapshot(); fmt.Sprint(got) != fmt.Sprint([]string{"admission:deactivate", "proxy:stop"}) {
		t.Fatalf("unconfirmed Stop operations = %v, want no outer cleanup", got)
	}
	assertNetworkCleanupPending(t, manager, *alloc)

	proxy.keepRunningOnStop = false
	recorder.reset()
	if err := manager.Reconcile(); err != nil {
		t.Fatalf("healing Reconcile: %v", err)
	}
	if records := manager.allocationSnapshot(); len(records) != 0 {
		t.Fatalf("allocation registry after confirmed stop = %+v, want empty", records)
	}
}

func TestNetworkReleaseRetainsGuardUntilOuterCleanupCompletes(t *testing.T) {
	for _, failure := range []string{"nat:teardown", "tap:delete"} {
		t.Run(failure, func(t *testing.T) {
			recorder := &networkOperationRecorder{}
			hostErr := errors.New(failure + " blocked")
			host := &fakePublicNetworkHostOperations{recorder: recorder, fail: map[string]error{failure: hostErr}}
			proxy := &fakeDNSProxyController{recorder: recorder}
			firewall := &fakeGatewayDNSFirewallController{recorder: recorder}
			manager := newNetworkManager("172.16.0.0/16", 1, proxy, true, firewall, host)
			alloc := mustNetworkAllocation(t, manager, 1)
			manager.setAllocationState(*alloc, networkAllocationActive, nil)

			err := manager.Release(alloc)
			if err == nil || !strings.Contains(err.Error(), hostErr.Error()) {
				t.Fatalf("Release() error = %v, want %s failure", err, failure)
			}
			if containsNetworkOperation(recorder.snapshot(), "admission:release") {
				t.Fatalf("outer cleanup failure released guard: %v", recorder.snapshot())
			}
			assertNetworkCleanupPending(t, manager, *alloc)

			recorder.reset()
			if err := manager.Reconcile(); err == nil || !strings.Contains(err.Error(), hostErr.Error()) {
				t.Fatalf("Reconcile() error = %v, want %s failure", err, failure)
			}
			assertGatewayDNSFirewallSnapshot(t, firewall.lastReconcileSnapshot(), []Allocation{*alloc}, nil)
		})
	}
}

func TestNetworkReconcilePublishesCompleteGuardedAndAdmittedSnapshots(t *testing.T) {
	recorder := &networkOperationRecorder{}
	host := &fakePublicNetworkHostOperations{recorder: recorder, fail: map[string]error{}}
	proxy := &fakeDNSProxyController{recorder: recorder, stopErr: errors.New("cleanup listener still running")}
	firewall := &fakeGatewayDNSFirewallController{recorder: recorder}
	manager := newNetworkManager("172.16.0.0/16", 1, proxy, true, firewall, host)
	active := mustNetworkAllocation(t, manager, 1)
	cleanupPending := mustNetworkAllocation(t, manager, 2)
	manager.setAllocationState(*active, networkAllocationActive, nil)
	manager.setAllocationState(*cleanupPending, networkAllocationCleanupPending, errors.New("previous cleanup failure"))

	err := manager.Reconcile()
	if err == nil || !strings.Contains(err.Error(), proxy.stopErr.Error()) {
		t.Fatalf("Reconcile() error = %v, want cleanup listener failure", err)
	}
	snapshot := firewall.lastReconcileSnapshot()
	assertGatewayDNSFirewallSnapshot(t, snapshot, []Allocation{*active, *cleanupPending}, []Allocation{*active})
	for _, admitted := range snapshot.admitted {
		found := false
		for _, guarded := range snapshot.guarded {
			if admitted == guarded {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("admitted allocation %+v absent from guarded snapshot %+v", admitted, snapshot.guarded)
		}
	}
	operations := recorder.snapshot()
	assertNetworkOperationBefore(t, operations, "admission:guard", "proxy:start")
	assertNetworkOperationBefore(t, operations, "proxy:start", "admission:activate")
	assertNetworkOperationBefore(t, operations, "admission:deactivate", "proxy:stop")
	if len(operations) == 0 || operations[len(operations)-1] != "admission:reconcile" {
		t.Fatalf("reconcile operations = %v, want complete snapshot published last", operations)
	}
}

func TestNetworkReleaseFailureRemainsCleanupPendingUntilReconciled(t *testing.T) {
	recorder := &networkOperationRecorder{}
	host := &fakePublicNetworkHostOperations{recorder: recorder, fail: map[string]error{}}
	proxy := &fakeDNSProxyController{recorder: recorder}
	firewall := &fakeGatewayDNSFirewallController{recorder: recorder, releaseErr: errors.New("xtables lock failed")}
	manager := newNetworkManager("172.16.0.0/16", 1, proxy, true, firewall, host)
	alloc := mustNetworkAllocation(t, manager, 1)
	manager.setAllocationState(*alloc, networkAllocationActive, nil)

	err := manager.Release(alloc)
	if err == nil || !strings.Contains(err.Error(), "xtables lock failed") {
		t.Fatalf("Release error = %v, want firewall cleanup failure", err)
	}
	records := manager.allocationSnapshot()
	if len(records) != 1 || records[0].State != networkAllocationCleanupPending || records[0].LastError == "" {
		t.Fatalf("allocation registry = %+v, want cleanup_pending diagnostic", records)
	}

	firewall.releaseErr = nil
	if err := manager.Reconcile(); err != nil {
		t.Fatalf("Reconcile cleanup: %v", err)
	}
	if records := manager.allocationSnapshot(); len(records) != 0 {
		t.Fatalf("allocation registry after healed cleanup = %+v, want empty", records)
	}
}

func assertNetworkCleanupPending(t *testing.T, manager *NetworkManager, alloc Allocation) {
	t.Helper()
	records := manager.allocationSnapshot()
	if len(records) != 1 || records[0].State != networkAllocationCleanupPending || records[0].Allocation != alloc || records[0].LastError == "" {
		t.Fatalf("allocation registry = %+v, want cleanup_pending allocation %+v", records, alloc)
	}
}

func assertGatewayDNSFirewallSnapshot(t *testing.T, got fakeGatewayDNSFirewallSnapshot, guarded, admitted []Allocation) {
	t.Helper()
	assertAllocations := func(kind string, got, want []Allocation) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("%s snapshot = %+v, want %+v", kind, got, want)
		}
		remaining := append([]Allocation(nil), got...)
		for _, expected := range want {
			match := -1
			for index, candidate := range remaining {
				if candidate == expected {
					match = index
					break
				}
			}
			if match < 0 {
				t.Fatalf("%s snapshot = %+v, missing %+v", kind, got, expected)
			}
			remaining = append(remaining[:match], remaining[match+1:]...)
		}
	}
	assertAllocations("guarded", got.guarded, guarded)
	assertAllocations("admitted", got.admitted, admitted)
}

func assertNetworkOperationBefore(t *testing.T, operations []string, before, after string) {
	t.Helper()
	beforeIndex, afterIndex := -1, -1
	for index, operation := range operations {
		if operation == before && beforeIndex < 0 {
			beforeIndex = index
		}
		if operation == after && afterIndex < 0 {
			afterIndex = index
		}
	}
	if beforeIndex < 0 || afterIndex < 0 || beforeIndex >= afterIndex {
		t.Fatalf("operations = %v, want %q before %q", operations, before, after)
	}
}

type fakeNetworkCommandExitError struct {
	code int
}

func (e fakeNetworkCommandExitError) Error() string { return fmt.Sprintf("exit status %d", e.code) }
func (e fakeNetworkCommandExitError) ExitCode() int { return e.code }

type scriptedNetworkCommandResult struct {
	out []byte
	err error
}

type scriptedNetworkCommandCall struct {
	name        string
	args        []string
	hadDeadline bool
}

type scriptedNetworkHostRunner struct {
	mu      sync.Mutex
	results []scriptedNetworkCommandResult
	calls   []scriptedNetworkCommandCall
}

func (r *scriptedNetworkHostRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, hadDeadline := ctx.Deadline()
	r.calls = append(r.calls, scriptedNetworkCommandCall{
		name:        name,
		args:        append([]string(nil), args...),
		hadDeadline: hadDeadline,
	})
	if len(r.results) == 0 {
		return nil, errors.New("unexpected host command")
	}
	result := r.results[0]
	r.results = r.results[1:]
	return append([]byte(nil), result.out...), result.err
}

func (r *scriptedNetworkHostRunner) callsSnapshot() []scriptedNetworkCommandCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := make([]scriptedNetworkCommandCall, len(r.calls))
	copy(result, r.calls)
	return result
}

func newScriptedExecNetworkHost(runner networkHostCommandRunner) *execPublicNetworkHostOperations {
	return &execPublicNetworkHostOperations{
		runner:         runner,
		commandTimeout: time.Second,
		forwardingPath: "/test/ip_forward",
		readFile:       func(string) ([]byte, error) { return []byte("1\n"), nil },
		writeFile:      func(string, []byte, os.FileMode) error { return nil },
		now:            func() time.Time { return time.Unix(1, 0).UTC() },
	}
}

func TestExecPublicNetworkHostRecreatesMissingTAPWithExactState(t *testing.T) {
	manager := NewNetworkManagerWithDNSDataDir("172.16.0.0/16", 1, nil, "")
	alloc := mustNetworkAllocation(t, manager, 1)
	stagingName, err := publicNetworkTAPStagingName(*alloc)
	if err != nil {
		t.Fatalf("publicNetworkTAPStagingName: %v", err)
	}
	runner := &scriptedNetworkHostRunner{results: []scriptedNetworkCommandResult{
		{out: []byte(`Device "kv1" does not exist.`), err: fakeNetworkCommandExitError{code: 1}},
		{}, // ip tuntap add
		{}, // install kiln-vm ownership alias
		{}, // atomically rename owned staging TAP
		{}, // ip addr add
		{}, // ip link set up
		{out: []byte(`[ {"ifname":"kv1","ifalias":"kiln-vm-public-network:v1:kv1:172.16.0.5:172.16.0.6:02:fc:00:00:00:01","flags":["UP"],"linkinfo":{"info_kind":"tun","info_data":{"type":"tap"}}} ]`)},
		{out: []byte(`[ {"ifname":"kv1","addr_info":[{"family":"inet","local":"172.16.0.5","prefixlen":30}]} ]`)},
	}}
	host := newScriptedExecNetworkHost(runner)

	status, err := host.reconcileTAP(*alloc)
	if err != nil {
		t.Fatalf("reconcileTAP: %v", err)
	}
	if !status.TAPPresent || !status.TAPIsTAP || !status.TAPAddressInstalled || !status.TAPLinkUp {
		t.Fatalf("TAP status = %+v, want exact repaired state", status)
	}
	want := [][]string{
		{"-j", "-d", "link", "show", "dev", "kv1"},
		{"tuntap", "add", "dev", stagingName, "mode", "tap"},
		{"link", "set", "dev", stagingName, "alias", "kiln-vm-public-network:v1:kv1:172.16.0.5:172.16.0.6:02:fc:00:00:00:01"},
		{"link", "set", "dev", stagingName, "name", "kv1"},
		{"addr", "add", "172.16.0.5/30", "dev", "kv1"},
		{"link", "set", "kv1", "up"},
		{"-j", "-d", "link", "show", "dev", "kv1"},
		{"-j", "-4", "addr", "show", "dev", "kv1"},
	}
	assertScriptedNetworkCommands(t, runner.callsSnapshot(), "ip", want)
}

func TestExecPublicNetworkHostRepairsTAPAddressAndLinkDrift(t *testing.T) {
	manager := NewNetworkManagerWithDNSDataDir("172.16.0.0/16", 1, nil, "")
	alloc := mustNetworkAllocation(t, manager, 1)
	runner := &scriptedNetworkHostRunner{results: []scriptedNetworkCommandResult{
		{out: []byte(`[ {"ifname":"kv1","ifalias":"kiln-vm-public-network:v1:kv1:172.16.0.5:172.16.0.6:02:fc:00:00:00:01","flags":[],"linkinfo":{"info_kind":"tun","info_data":{"type":"tap"}}} ]`)},
		{out: []byte(`[ {"ifname":"kv1","addr_info":[{"family":"inet","local":"172.16.0.9","prefixlen":30}]} ]`)},
		{}, // remove stale address
		{}, // add exact address
		{}, // bring link up
		{out: []byte(`[ {"ifname":"kv1","ifalias":"kiln-vm-public-network:v1:kv1:172.16.0.5:172.16.0.6:02:fc:00:00:00:01","flags":["UP"],"linkinfo":{"info_kind":"tun","info_data":{"type":"tap"}}} ]`)},
		{out: []byte(`[ {"ifname":"kv1","addr_info":[{"family":"inet","local":"172.16.0.5","prefixlen":30}]} ]`)},
	}}
	host := newScriptedExecNetworkHost(runner)

	status, err := host.reconcileTAP(*alloc)
	if err != nil {
		t.Fatalf("reconcileTAP: %v", err)
	}
	if !status.TAPAddressInstalled || !status.TAPLinkUp || !status.TAPIsTAP {
		t.Fatalf("TAP status = %+v, want repaired address/link", status)
	}
	calls := runner.callsSnapshot()
	for _, call := range calls {
		if !call.hadDeadline {
			t.Fatalf("host command lacked deadline: %+v", call)
		}
	}
	if got := calls[2].args; !equalNetworkCommandTokens(got, []string{"addr", "del", "172.16.0.9/30", "dev", "kv1"}) {
		t.Fatalf("stale address mutation = %v", got)
	}
	if got := calls[3].args; !equalNetworkCommandTokens(got, []string{"addr", "add", "172.16.0.5/30", "dev", "kv1"}) {
		t.Fatalf("desired address mutation = %v", got)
	}
	if got := calls[4].args; !equalNetworkCommandTokens(got, []string{"link", "set", "kv1", "up"}) {
		t.Fatalf("link mutation = %v", got)
	}
}

func TestExecPublicNetworkHostRepairsAndVerifiesIPv4Forwarding(t *testing.T) {
	value := "0\n"
	writes := 0
	host := newScriptedExecNetworkHost(&scriptedNetworkHostRunner{})
	host.readFile = func(path string) ([]byte, error) {
		if path != "/test/ip_forward" {
			t.Fatalf("read forwarding path = %q", path)
		}
		return []byte(value), nil
	}
	host.writeFile = func(path string, data []byte, mode os.FileMode) error {
		if path != "/test/ip_forward" || string(data) != "1\n" || mode != 0644 {
			t.Fatalf("write forwarding = %q %q %#o", path, data, mode)
		}
		writes++
		value = string(data)
		return nil
	}

	enabled, err := host.ensureIPv4Forwarding()
	if err != nil || !enabled || writes != 1 {
		t.Fatalf("ensureIPv4Forwarding() = %t, %v writes=%d", enabled, err, writes)
	}
}

func TestExecPublicNetworkHostRejectsWrongTypeCollisionWithoutMutation(t *testing.T) {
	runner := &scriptedNetworkHostRunner{results: []scriptedNetworkCommandResult{{
		out: []byte(`[ {"ifname":"kv1","flags":["UP"],"linkinfo":{"info_kind":"bridge","info_data":{}}} ]`),
	}}}
	host := newScriptedExecNetworkHost(runner)
	alloc := mustNetworkAllocation(t, NewNetworkManagerWithDNSDataDir("172.16.0.0/16", 1, nil, ""), 1)

	status, err := host.ReconcileActive(*alloc)
	if err == nil || !strings.Contains(err.Error(), "link collision") {
		t.Fatalf("ReconcileActive() = %+v, %v; want wrong-type collision", status, err)
	}
	if !status.TAPPresent || !status.TAPOwnershipCollision || status.TAPIsTAP || status.Ready {
		t.Fatalf("collision status = %+v", status)
	}
	calls := runner.callsSnapshot()
	if len(calls) != 1 || calls[0].name != "ip" || !calls[0].hadDeadline {
		t.Fatalf("collision commands = %+v, want one bounded observation", calls)
	}
}

func TestExecPublicNetworkHostRejectsUnownedSameTypeTAPWithoutMutation(t *testing.T) {
	runner := &scriptedNetworkHostRunner{results: []scriptedNetworkCommandResult{{
		out: []byte(`[ {"ifname":"kv1","ifalias":"foreign-owner","flags":["UP"],"linkinfo":{"info_kind":"tun","info_data":{"type":"tap"}}} ]`),
	}}}
	host := newScriptedExecNetworkHost(runner)
	alloc := mustNetworkAllocation(t, NewNetworkManagerWithDNSDataDir("172.16.0.0/16", 1, nil, ""), 1)

	status, err := host.ReconcileActive(*alloc)
	if err == nil || !strings.Contains(err.Error(), "unowned TAP collision") {
		t.Fatalf("ReconcileActive() = %+v, %v; want same-type ownership collision", status, err)
	}
	if !status.TAPPresent || !status.TAPOwnershipCollision || !status.TAPIsTAP || status.TAPOwned || status.Ready {
		t.Fatalf("collision status = %+v", status)
	}
	if calls := runner.callsSnapshot(); len(calls) != 1 || calls[0].name != "ip" || !calls[0].hadDeadline {
		t.Fatalf("same-type collision commands = %+v, want one bounded observation", calls)
	}
}

func TestExecPublicNetworkHostDeleteRefusesWrongTypeCollision(t *testing.T) {
	runner := &scriptedNetworkHostRunner{results: []scriptedNetworkCommandResult{
		{out: []byte(`[ {"ifname":"kv1","flags":["UP"],"linkinfo":{"info_kind":"bridge","info_data":{}}} ]`)},
		{out: []byte(`Cannot find device`), err: fakeNetworkCommandExitError{code: 1}},
	}}
	host := newScriptedExecNetworkHost(runner)

	alloc := mustNetworkAllocation(t, NewNetworkManagerWithDNSDataDir("172.16.0.0/16", 1, nil, ""), 1)
	err := host.DeleteTAP(*alloc)
	if err == nil || !strings.Contains(err.Error(), "refuse to delete unowned link collision") {
		t.Fatalf("DeleteTAP() error = %v, want collision refusal", err)
	}
	if calls := runner.callsSnapshot(); len(calls) != 2 || calls[0].name != "ip" || calls[1].name != "ip" || !calls[0].hadDeadline || !calls[1].hadDeadline {
		t.Fatalf("DeleteTAP collision commands = %+v, want bounded final and staging observations", calls)
	}
}

func TestExecPublicNetworkHostDeleteRefusesUnownedSameTypeTAP(t *testing.T) {
	runner := &scriptedNetworkHostRunner{results: []scriptedNetworkCommandResult{
		{out: []byte(`[ {"ifname":"kv1","ifalias":"foreign-owner","flags":["UP"],"linkinfo":{"info_kind":"tun","info_data":{"type":"tap"}}} ]`)},
		{out: []byte(`Cannot find device`), err: fakeNetworkCommandExitError{code: 1}},
	}}
	host := newScriptedExecNetworkHost(runner)
	alloc := mustNetworkAllocation(t, NewNetworkManagerWithDNSDataDir("172.16.0.0/16", 1, nil, ""), 1)

	err := host.DeleteTAP(*alloc)
	if err == nil || !strings.Contains(err.Error(), "refuse to delete unowned TAP collision") {
		t.Fatalf("DeleteTAP() error = %v, want same-type ownership refusal", err)
	}
	if calls := runner.callsSnapshot(); len(calls) != 2 || calls[0].name != "ip" || calls[1].name != "ip" || !calls[0].hadDeadline || !calls[1].hadDeadline {
		t.Fatalf("DeleteTAP same-type collision commands = %+v, want bounded final and staging observations", calls)
	}
}

func TestExecPublicNetworkHostCreateReportsPreOwnershipAndResidualOwnership(t *testing.T) {
	t.Run("pre-ownership failure", func(t *testing.T) {
		runner := &scriptedNetworkHostRunner{results: []scriptedNetworkCommandResult{{
			out: []byte("Device or resource busy"),
			err: fakeNetworkCommandExitError{code: 1},
		}}}
		host := newScriptedExecNetworkHost(runner)
		alloc := mustNetworkAllocation(t, NewNetworkManagerWithDNSDataDir("172.16.0.0/16", 1, nil, ""), 1)
		owned, err := host.CreateTAP(*alloc)
		if err == nil || owned {
			t.Fatalf("CreateTAP() = owned=%t err=%v, want pre-ownership failure", owned, err)
		}
		if calls := runner.callsSnapshot(); len(calls) != 1 {
			t.Fatalf("pre-ownership commands = %+v, want no cleanup", calls)
		}
	})

	t.Run("partial create cleanup failure", func(t *testing.T) {
		runner := &scriptedNetworkHostRunner{results: []scriptedNetworkCommandResult{
			{},
			{},
			{},
			{out: []byte("address operation failed"), err: fakeNetworkCommandExitError{code: 2}},
			{out: []byte(`[ {"ifname":"kv1","ifalias":"kiln-vm-public-network:v1:kv1:172.16.0.5:172.16.0.6:02:fc:00:00:00:01","flags":[],"linkinfo":{"info_kind":"tun","info_data":{"type":"tap"}}} ]`)},
			{out: []byte("Cannot find device"), err: fakeNetworkCommandExitError{code: 1}},
			{out: []byte("delete denied"), err: fakeNetworkCommandExitError{code: 2}},
		}}
		host := newScriptedExecNetworkHost(runner)
		alloc := mustNetworkAllocation(t, NewNetworkManagerWithDNSDataDir("172.16.0.0/16", 1, nil, ""), 1)
		owned, err := host.CreateTAP(*alloc)
		if err == nil || !owned || !strings.Contains(err.Error(), "delete denied") {
			t.Fatalf("CreateTAP() = owned=%t err=%v, want residual ownership", owned, err)
		}
		if calls := runner.callsSnapshot(); len(calls) != 7 {
			t.Fatalf("partial create commands = %+v, want staging create/claim/rename, address failure, full preflight, and delete", calls)
		}
	})
}

func TestExecPublicNetworkHostNormalizesOnlyExactDuplicateRule(t *testing.T) {
	rule := publicNetworkRules("kv1", "172.16.0.6")[0]
	exact := renderPublicNetworkRuleForTest(rule)
	// Same packet match without kiln-vm's ownership comment is deliberately
	// foreign and must not be counted or mutated.
	foreign := "-A POSTROUTING -s 172.16.0.6/32 -j MASQUERADE\n"
	runner := &scriptedNetworkHostRunner{results: []scriptedNetworkCommandResult{
		{out: []byte(exact + foreign + exact + exact)},
		{},
		{},
		{out: []byte(foreign + exact)},
	}}
	host := newScriptedExecNetworkHost(runner)

	count, err := host.reconcileExactIptablesRule(rule)
	if err != nil || count != 1 {
		t.Fatalf("reconcileExactIptablesRule() = %d, %v; want 1", count, err)
	}
	calls := runner.callsSnapshot()
	if len(calls) != 4 {
		t.Fatalf("iptables calls = %+v, want inventory, two deletes, verification", calls)
	}
	for _, call := range calls {
		if call.name != "iptables" || len(call.args) < 3 || call.args[0] != "-w" || call.args[1] != "5" || !call.hadDeadline {
			t.Fatalf("iptables command is not bounded argv with -w 5: %+v", call)
		}
	}
	wantDelete := iptablesRuleArgs(rule, "-D")
	if !equalNetworkCommandTokens(calls[1].args, wantDelete) || !equalNetworkCommandTokens(calls[2].args, wantDelete) {
		t.Fatalf("duplicate mutations = %v / %v, want exact owned rule %v", calls[1].args, calls[2].args, wantDelete)
	}
}

func TestIptablesMissingRuleClassificationRejectsOperationalExitOne(t *testing.T) {
	tests := []struct {
		name string
		out  string
		err  error
		want bool
	}{
		{name: "empty exit one is ambiguous", err: fakeNetworkCommandExitError{code: 1}, want: false},
		{name: "bad rule", out: "iptables: Bad rule (does a matching rule exist in that chain?).", err: fakeNetworkCommandExitError{code: 1}, want: true},
		{name: "permission denied", out: "iptables: permission denied", err: fakeNetworkCommandExitError{code: 1}, want: false},
		{name: "xtables lock", out: "xtables lock", err: fakeNetworkCommandExitError{code: 4}, want: false},
		{name: "success", want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := iptablesRuleMissingFromResult([]byte(test.out), test.err); got != test.want {
				t.Fatalf("iptablesRuleMissingFromResult(%q, %v) = %t, want %t", test.out, test.err, got, test.want)
			}
		})
	}
}

func TestRemoveOwnedIptablesRulesPropagatesOperationalExitOne(t *testing.T) {
	rule := publicNetworkRules("kv1", "172.16.0.6")[1]
	exact := renderPublicNetworkRuleForTest(rule)
	runner := &scriptedNetworkHostRunner{results: []scriptedNetworkCommandResult{
		{out: []byte(exact)},
		{out: []byte("permission denied"), err: fakeNetworkCommandExitError{code: 1}},
	}}
	host := newScriptedExecNetworkHost(runner)

	err := host.removeAllOwnedIptablesRules(rule)
	if err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("removeAllOwnedIptablesRules() error = %v, want operational failure", err)
	}
}

func TestRemoveOwnedIptablesRulesFailsBeforeRemovalCapIsExceeded(t *testing.T) {
	rule := publicNetworkRules("kv1", "172.16.0.6")[2]
	exact := renderPublicNetworkRuleForTest(rule)
	runner := &scriptedNetworkHostRunner{results: []scriptedNetworkCommandResult{{
		out: []byte(strings.Repeat(exact, networkRuleRemovalLimit+1)),
	}}}
	host := newScriptedExecNetworkHost(runner)

	err := host.removeAllOwnedIptablesRules(rule)
	if err == nil || !strings.Contains(err.Error(), "cap exhausted") {
		t.Fatalf("removeAllOwnedIptablesRules() error = %v, want cap exhaustion", err)
	}
	if calls := runner.callsSnapshot(); len(calls) != 1 || !strings.Contains(strings.Join(calls[0].args, " "), "-S FORWARD") {
		t.Fatalf("commands after cap exhaustion = %+v, want inventory only", calls)
	}
}

func renderPublicNetworkRuleForTest(rule publicNetworkIptablesRule) string {
	return strings.Join(append([]string{"-A", rule.chain}, rule.spec...), " ") + "\n"
}

func assertScriptedNetworkCommands(t *testing.T, calls []scriptedNetworkCommandCall, name string, args [][]string) {
	t.Helper()
	if len(calls) != len(args) {
		t.Fatalf("commands = %+v, want %d", calls, len(args))
	}
	for index, want := range args {
		if calls[index].name != name || !equalNetworkCommandTokens(calls[index].args, want) || !calls[index].hadDeadline {
			t.Fatalf("command[%d] = %+v, want %s %v with deadline", index, calls[index], name, want)
		}
	}
}

func mustNetworkAllocation(t *testing.T, nm *NetworkManager, idx uint32) *Allocation {
	t.Helper()
	alloc, err := nm.allocationForIndex(idx)
	if err != nil {
		t.Fatalf("allocationForIndex(%d): %v", idx, err)
	}
	alloc.OwnerID = fmt.Sprintf("test-machine-%d", idx)
	return alloc
}
