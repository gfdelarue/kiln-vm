package machine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gfdelarue/kiln-vm/internal/config"
	"github.com/gfdelarue/kiln-vm/internal/models"
)

func TestPublicAllocationForCleanupUsesOnlyRecordedAllocation(t *testing.T) {
	recorded := canonicalLifecycleMachine(t, "machine-1", models.MachineStateStopped)
	want, err := allocationForIndex("172.16.0.0/16", recorded.NetworkIndex)
	if err != nil {
		t.Fatalf("allocationForIndex: %v", err)
	}
	want.OwnerID = recorded.ID

	got, err := publicAllocationForCleanup(recorded)
	if err != nil {
		t.Fatalf("publicAllocationForCleanup(recorded): %v", err)
	}
	if got == nil || *got != *want {
		t.Fatalf("allocation = %+v, want %+v", got, want)
	}

	if got, err := publicAllocationForCleanup(&models.Machine{ID: "empty"}); err != nil || got != nil {
		t.Fatalf("publicAllocationForCleanup(empty) = %+v, %v; want nil, nil", got, err)
	}

	mismatched := cloneLifecycleMachine(recorded)
	mismatched.IP = "172.16.10.211"
	failing := map[string]*models.Machine{
		"TAP alone":           {ID: "m", TAPDevice: want.TAPDevice},
		"guest IP alone":      {ID: "m", IP: want.GuestIP},
		"gateway alone":       {ID: "m", GatewayIP: want.GatewayIP},
		"mismatched guest IP": mismatched,
	}
	for name, machine := range failing {
		t.Run(name, func(t *testing.T) {
			if got, err := publicAllocationForCleanup(machine); err == nil || got != nil {
				t.Fatalf("publicAllocationForCleanup = %+v, %v; want fail-closed error", got, err)
			}
		})
	}
}

func TestPublicAllocationForCleanupRejectsDiagnosticFromDifferentMachine(t *testing.T) {
	machine := canonicalLifecycleMachine(t, "machine-owner", models.MachineStateError)
	diagnostic := buildProviderNetworkCleanupDiagnostic(machine, "stop", errors.New("cleanup failed"))
	diagnostic.MachineID = "different-machine"
	machine.Diagnostics = []models.Diagnostic{diagnostic}

	if alloc, err := publicAllocationForCleanup(machine); err == nil || alloc != nil {
		t.Fatalf("publicAllocationForCleanup = %+v, %v; want fail-closed MachineID mismatch", alloc, err)
	}
}

func TestAppendNetworkCleanupDiagnosticPreservesFullestValidImmutableTuple(t *testing.T) {
	machine := canonicalLifecycleMachine(t, "machine-journal", models.MachineStateError)
	full := buildProviderNetworkCleanupDiagnostic(machine, "recovery", errors.New("first failure"))
	incomplete := buildProviderNetworkCleanupDiagnosticForAllocation(machine, nil, "recovery", errors.New("latest failure"))
	result := appendNetworkCleanupDiagnostic([]models.Diagnostic{full}, incomplete)
	if len(result) != 1 {
		t.Fatalf("diagnostics = %+v, want one bounded journal record", result)
	}
	got := result[0]
	if got.Error != "latest failure" {
		t.Fatalf("diagnostic error = %q, want latest failure text", got.Error)
	}
	if got.TAPDevice != full.TAPDevice || got.NetworkCIDR != full.NetworkCIDR || got.NetworkIndex != full.NetworkIndex ||
		got.GuestIP != full.GuestIP || got.GatewayIP != full.GatewayIP || got.GuestMAC != full.GuestMAC ||
		got.Netmask != full.Netmask || got.TAPAlias != full.TAPAlias || got.MachineID != full.MachineID {
		t.Fatalf("diagnostic lost immutable tuple: got %+v, want identity from %+v", got, full)
	}
}

func TestReleaseMachineRuntimeRejectsMalformedDurablePublicIdentity(t *testing.T) {
	network := &lifecycleContractNetwork{}
	manager := &Manager{
		cfg:     &config.Config{BridgeCIDR: "172.16.0.0/16"},
		network: network,
	}

	err := manager.releaseMachineRuntime(&models.Machine{
		ID:        "machine-1",
		TAPDevice: "kv661",
		IP:        "172.16.99.99",
	})
	if err == nil {
		t.Fatal("releaseMachineRuntime error = nil, want malformed identity failure")
	}
	if network.releaseCount() != 0 {
		t.Fatalf("Release calls = %d, want zero for an unverified identity", network.releaseCount())
	}
}

func TestReconcileExitedStoppingMachineReleasesProviderNetworkBeforeStopped(t *testing.T) {
	events := &lifecycleEventLog{}
	machine := canonicalLifecycleMachine(t, "machine-1", models.MachineStateStopping)
	store := newLifecycleContractStore(machine)
	store.events = events
	network := &lifecycleContractNetwork{events: events}
	manager := newLifecycleContractManager(t, store, network)

	machines, err := store.ListMachines()
	if err != nil {
		t.Fatalf("ListMachines: %v", err)
	}
	got := manager.reconcileExitedStoppingMachines(machines)

	assertLifecycleEvents(t, events.snapshot(), "network.release:"+machine.TAPDevice, "state.stopped:"+machine.ID)
	if len(got) != 1 || got[0].State != models.MachineStateStopped {
		t.Fatalf("reconciled machines = %+v, want stopped", got)
	}
	persisted := store.mustMachine(t, machine.ID)
	if persisted.State != models.MachineStateStopped {
		t.Fatalf("persisted state = %q, want stopped", persisted.State)
	}
}

func TestReconcileExitedStoppingMachineRetainsCleanupFailure(t *testing.T) {
	events := &lifecycleEventLog{}
	cleanupErr := errors.New("remove gateway DNS admission: permission denied")
	machine := canonicalLifecycleMachine(t, "machine-1", models.MachineStateStopping)
	store := newLifecycleContractStore(machine)
	store.events = events
	network := &lifecycleContractNetwork{events: events, releaseErr: cleanupErr}
	manager := newLifecycleContractManager(t, store, network)

	machines, err := store.ListMachines()
	if err != nil {
		t.Fatalf("ListMachines: %v", err)
	}
	got := manager.reconcileExitedStoppingMachines(machines)

	eventList := events.snapshot()
	if containsLifecycleEvent(eventList, "state.stopped") {
		t.Fatalf("events = %v, must not mark stopped after failed cleanup", eventList)
	}
	if len(eventList) == 0 || !strings.HasPrefix(eventList[0], "network.release") {
		t.Fatalf("events = %v, want provider release first", eventList)
	}
	persisted := store.mustMachine(t, machine.ID)
	assertProviderCleanupFailure(t, persisted, cleanupErr)
	if len(got) != 1 || got[0].State == models.MachineStateStopped {
		t.Fatalf("reconciled machines = %+v, must retain non-stopped state", got)
	}
}

func TestRecoverRunningRetriesAllProviderCleanupPendingStates(t *testing.T) {
	events := &lifecycleEventLog{}
	machines := []*models.Machine{
		canonicalLifecycleMachine(t, "running", models.MachineStateRunning),
		canonicalLifecycleMachine(t, "creating", models.MachineStateCreating),
		canonicalLifecycleMachine(t, "stopping", models.MachineStateStopping),
		canonicalLifecycleMachine(t, "cleanup-error", models.MachineStateError),
		canonicalLifecycleMachine(t, "plain-error", models.MachineStateError),
		canonicalLifecycleMachine(t, "already-stopped", models.MachineStateStopped),
	}
	machines[3].Diagnostics = []models.Diagnostic{buildProviderNetworkCleanupDiagnostic(machines[3], "stop", errors.New("previous cleanup failure"))}
	machines[4].Diagnostics = []models.Diagnostic{{Category: "machine_boot", Phase: "process_start", Error: "boot failed"}}
	store := newLifecycleContractStore(machines...)
	store.events = events
	network := &lifecycleContractNetwork{events: events}
	manager := newLifecycleContractManager(t, store, network)

	manager.RecoverRunning()

	if got := network.releaseCount(); got != 4 {
		t.Fatalf("Release calls = %d, want running, creating, stopping, and cleanup-error", got)
	}
	for _, id := range []string{"running", "creating", "stopping", "cleanup-error"} {
		machine := store.mustMachine(t, id)
		if machine.State != models.MachineStateStopped {
			t.Fatalf("machine %s state = %q, want stopped after successful cleanup", id, machine.State)
		}
	}
	if got := store.mustMachine(t, "plain-error").State; got != models.MachineStateError {
		t.Fatalf("plain error state = %q, want untouched error", got)
	}
	if got := store.mustMachine(t, "already-stopped").State; got != models.MachineStateStopped {
		t.Fatalf("already-stopped state = %q, want stopped", got)
	}

	eventList := events.snapshot()
	for _, id := range []string{"running", "creating", "stopping", "cleanup-error"} {
		machine := store.mustMachine(t, id)
		assertEventBefore(t, eventList, "network.release:"+machine.TAPDevice, "state.stopped:"+id)
	}
}

func TestRecoverRunningKeepsCleanupFailureRecoverable(t *testing.T) {
	states := []models.MachineState{
		models.MachineStateRunning,
		models.MachineStateCreating,
		models.MachineStateStopping,
		models.MachineStateError,
	}
	for _, state := range states {
		t.Run(string(state), func(t *testing.T) {
			cleanupErr := errors.New("delete TAP: device busy")
			machine := canonicalLifecycleMachine(t, "machine-1", state)
			if state == models.MachineStateError {
				machine.Diagnostics = []models.Diagnostic{buildProviderNetworkCleanupDiagnostic(machine, "stop", errors.New("previous failure"))}
			}
			store := newLifecycleContractStore(machine)
			network := &lifecycleContractNetwork{releaseErr: cleanupErr}
			manager := newLifecycleContractManager(t, store, network)

			manager.RecoverRunning()

			persisted := store.mustMachine(t, machine.ID)
			assertProviderCleanupFailure(t, persisted, cleanupErr)
			if persisted.State == models.MachineStateStopped {
				t.Fatal("failed cleanup was incorrectly marked stopped")
			}
		})
	}
}

func TestPrepareMachinePublicNetworkForStartReusesHistoricalIndexOnlyAfterCleanup(t *testing.T) {
	machine := canonicalLifecycleMachine(t, "stopped-historical", models.MachineStateStopped)
	oldAllocation, present, err := durablePublicAllocationFromMachine(machine)
	if err != nil || !present {
		t.Fatalf("durable old allocation = %+v, present=%t, error=%v", oldAllocation, present, err)
	}
	newAllocation, err := allocationForIndex("10.77.0.0/16", oldAllocation.NetworkIndex)
	if err != nil {
		t.Fatalf("new allocationForIndex: %v", err)
	}
	newAllocation.OwnerID = machine.ID
	events := &lifecycleEventLog{}
	store := newLifecycleContractStore(machine)
	store.events = events
	network := &lifecycleContractNetwork{events: events}
	manager := newLifecycleContractManager(t, store, network)
	manager.cfg.BridgeCIDR = "10.77.0.0/16"

	planned, err := manager.prepareMachinePublicNetworkForStart(machine)
	if err != nil {
		t.Fatalf("prepareMachinePublicNetworkForStart: %v", err)
	}
	if *planned != *newAllocation {
		t.Fatalf("planned allocation = %+v, want fresh current-CIDR allocation %+v", planned, newAllocation)
	}
	assertLifecycleEvents(t, events.snapshot(),
		"network.release:"+oldAllocation.TAPDevice,
	)
}

func TestPrepareMachinePublicNetworkForStartReusesHistoricalIndexBeforeMonotonicPlan(t *testing.T) {
	const newCIDR = "10.77.0.0/29"
	machine := &models.Machine{
		ID:       "historical-small-range",
		Name:     "historical-small-range",
		Backend:  firecrackerBackend,
		State:    models.MachineStateStopped,
		VCPUs:    1,
		CPU:      models.CPUConfig{Kind: "shared", Millis: 1000, VCPUs: 1},
		MemoryMB: 128,
	}
	oldAllocation := canonicalLifecycleAllocation(t, "172.16.0.0/16", machine.ID, 1)
	setMachinePublicAllocation(machine, oldAllocation)
	events := &lifecycleEventLog{}
	store := newLifecycleContractStore(machine)
	store.events = events
	network := &lifecycleContractNetwork{
		allocateErr: errors.New("base CIDR exhausted at monotonic index 2"),
		events:      events,
	}
	manager := newLifecycleContractManager(t, store, network)
	manager.cfg.BridgeCIDR = newCIDR

	planned, err := manager.prepareMachinePublicNetworkForStart(machine)
	if err != nil {
		t.Fatalf("prepareMachinePublicNetworkForStart: %v", err)
	}
	want := canonicalLifecycleAllocation(t, newCIDR, machine.ID, 1)
	if *planned != *want {
		t.Fatalf("planned allocation = %+v, want same-index current allocation %+v", planned, want)
	}
	if got := events.snapshot(); len(got) != 1 || got[0] != "network.release:"+oldAllocation.TAPDevice {
		t.Fatalf("events = %v, want exact historical cleanup without monotonic Plan", got)
	}
}

func TestPrepareMachinePublicNetworkForStartCanonicalizesCompatibleNarrowedCIDRWithoutAddressChurn(t *testing.T) {
	machine := canonicalLifecycleMachine(t, "stopped-compatible", models.MachineStateStopped)
	// This test needs an index whose /30 remains inside the narrowed /24.
	oldAllocation := canonicalLifecycleAllocation(t, "172.16.0.0/16", machine.ID, 7)
	setMachinePublicAllocation(machine, oldAllocation)
	events := &lifecycleEventLog{}
	store := newLifecycleContractStore(machine)
	store.events = events
	network := &lifecycleContractNetwork{events: events}
	manager := newLifecycleContractManager(t, store, network)
	manager.cfg.BridgeCIDR = "172.16.0.0/24"

	planned, err := manager.prepareMachinePublicNetworkForStart(machine)
	if err != nil {
		t.Fatalf("prepareMachinePublicNetworkForStart: %v", err)
	}
	if planned.TAPDevice != oldAllocation.TAPDevice || planned.GuestIP != oldAllocation.GuestIP ||
		planned.GatewayIP != oldAllocation.GatewayIP || planned.NetworkIndex != oldAllocation.NetworkIndex {
		t.Fatalf("compatible narrowed CIDR changed allocation coordinates: got %+v want %+v", planned, oldAllocation)
	}
	if planned.BaseCIDR != "172.16.0.0/24" {
		t.Fatalf("canonical source CIDR = %q, want narrowed current range", planned.BaseCIDR)
	}
	for _, event := range events.snapshot() {
		if event == "network.plan:"+machine.ID {
			t.Fatalf("compatible historical allocation needlessly consumed a new index: %v", events.snapshot())
		}
	}
}

func TestPrepareMachinePublicNetworkForStartNeverPlansBeforeHistoricalCleanupSucceeds(t *testing.T) {
	machine := canonicalLifecycleMachine(t, "stopped-cleanup-pending", models.MachineStateStopped)
	events := &lifecycleEventLog{}
	store := newLifecycleContractStore(machine)
	store.events = events
	network := &lifecycleContractNetwork{
		allocation: canonicalLifecycleAllocation(t, "10.77.0.0/16", machine.ID, lifecycleIndexForID(machine.ID)+1),
		events:     events,
		releaseErr: errors.New("old TAP is busy"),
	}
	manager := newLifecycleContractManager(t, store, network)
	manager.cfg.BridgeCIDR = "10.77.0.0/16"

	planned, err := manager.prepareMachinePublicNetworkForStart(machine)
	if err == nil || planned != nil {
		t.Fatalf("prepareMachinePublicNetworkForStart = %+v, %v; want cleanup failure", planned, err)
	}
	if got := events.snapshot(); len(got) == 0 || got[0] != "network.release:"+machine.TAPDevice {
		t.Fatalf("events = %v, want historical release first", got)
	}
	for _, event := range events.snapshot() {
		if event == "network.plan:"+machine.ID {
			t.Fatalf("fresh allocation planned before old cleanup succeeded: %v", events.snapshot())
		}
	}
}

func TestReconcileProviderCleanupRecordMarksStoppedAndClearsDiagnostics(t *testing.T) {
	cleanupDiagnostic := buildProviderNetworkCleanupDiagnostic(nil, "stop", errors.New("previous transient cleanup failure"))
	machine := canonicalLifecycleMachine(t, "machine-1", models.MachineStateError)
	cleanupDiagnostic.MachineID = machine.ID
	cleanupDiagnostic.TAPDevice = machine.TAPDevice
	machine.Diagnostics = []models.Diagnostic{
		{Category: "machine_boot", Phase: "process_exit", Error: "process exited"},
		cleanupDiagnostic,
	}
	store := newLifecycleContractStore(machine)
	network := &lifecycleContractNetwork{}
	manager := newLifecycleContractManager(t, store, network)

	manager.reconcileProviderCleanupRecordsLocked("background_reconcile")

	if network.releaseCount() != 1 {
		t.Fatalf("Release calls = %d, want one cleanup retry", network.releaseCount())
	}
	persisted := store.mustMachine(t, machine.ID)
	if persisted.State != models.MachineStateStopped {
		t.Fatalf("machine state = %q, want stopped", persisted.State)
	}
	if persisted.Error != "provider network cleanup reconciled" {
		t.Fatalf("machine error = %q, want reconciliation status", persisted.Error)
	}
	if len(persisted.Diagnostics) != 0 {
		t.Fatalf("diagnostics = %+v, want cleared after successful provider cleanup", persisted.Diagnostics)
	}
}

func TestReconcileProviderCleanupRecordBoundsSameActionDiagnosticOnContinuingFailure(t *testing.T) {
	cleanupErr := errors.New("delete TAP: device busy")
	machine := canonicalLifecycleMachine(t, "machine-1", models.MachineStateError)
	machine.Diagnostics = []models.Diagnostic{
		buildProviderNetworkCleanupDiagnostic(machine, "stop", errors.New("initial stop cleanup failure")),
		buildProviderNetworkCleanupDiagnostic(machine, "background_reconcile", errors.New("earlier reconcile failure")),
	}
	store := newLifecycleContractStore(machine)
	network := &lifecycleContractNetwork{releaseErr: cleanupErr}
	manager := newLifecycleContractManager(t, store, network)

	for range 3 {
		manager.reconcileProviderCleanupRecordsLocked("background_reconcile")
	}

	if network.releaseCount() != 3 {
		t.Fatalf("Release calls = %d, want one retry per pass", network.releaseCount())
	}
	persisted := store.mustMachine(t, machine.ID)
	if persisted.State != models.MachineStateError {
		t.Fatalf("machine state = %q, want recoverable error", persisted.State)
	}
	var sameAction int
	for _, diagnostic := range persisted.Diagnostics {
		if diagnostic.Category == "provider_network" && diagnostic.Phase == "cleanup" && diagnostic.Action == "background_reconcile" {
			sameAction++
			if !diagnostic.Retryable || !strings.Contains(diagnostic.Error, cleanupErr.Error()) {
				t.Fatalf("latest reconciliation diagnostic = %+v", diagnostic)
			}
		}
	}
	if sameAction != 1 {
		t.Fatalf("diagnostics = %+v, want exactly one same-action cleanup diagnostic", persisted.Diagnostics)
	}
}

func TestRecoverRunningHealsTransientCleanupFailureAfterNetworkReconcile(t *testing.T) {
	events := &lifecycleEventLog{}
	cleanupErr := errors.New("transient DNS admission teardown failure")
	machine := canonicalLifecycleMachine(t, "machine-1", models.MachineStateRunning)
	store := newLifecycleContractStore(machine)
	store.events = events
	transientFailure := true
	network := &lifecycleContractNetwork{events: events}
	network.releaseHook = func(_ int, _ *Allocation) error {
		if transientFailure {
			return cleanupErr
		}
		return nil
	}
	reconciler := &lifecycleContractReconciler{
		lifecycleContractNetwork: network,
		events:                   events,
		reconcileHook: func() error {
			transientFailure = false
			return nil
		},
	}
	manager := newLifecycleContractManager(t, store, reconciler)

	manager.RecoverRunning()

	if network.releaseCount() != 2 {
		t.Fatalf("Release calls = %d, want initial failure and post-reconcile retry", network.releaseCount())
	}
	if reconciler.reconcileCalls != 1 {
		t.Fatalf("Reconcile calls = %d, want one startup reconcile", reconciler.reconcileCalls)
	}
	persisted := store.mustMachine(t, machine.ID)
	if persisted.State != models.MachineStateStopped {
		t.Fatalf("machine state = %q, want stopped after same-startup healing", persisted.State)
	}
	if len(persisted.Diagnostics) != 0 {
		t.Fatalf("diagnostics = %+v, want cleared after same-startup healing", persisted.Diagnostics)
	}
	assertLifecycleEvents(t, events.snapshot(),
		"network.release:"+machine.TAPDevice,
		"diagnostic.error:"+machine.ID,
		"network.reconcile",
		"network.release:"+machine.TAPDevice,
		"state.stopped:"+machine.ID,
	)
}

func TestMarkStartFailureSeparatesPrimaryAndCleanupErrors(t *testing.T) {
	primaryErr := errors.New("prepare guest rootfs: invalid image")
	cleanupErr := errors.New("remove gateway DNS admission: permission denied")
	machine := canonicalLifecycleMachine(t, "machine-1", models.MachineStateCreating)
	store := newLifecycleContractStore(machine)
	network := &lifecycleContractNetwork{releaseErr: cleanupErr}
	manager := newLifecycleContractManager(t, store, network)
	alloc, err := publicAllocationForCleanup(machine)
	if err != nil {
		t.Fatalf("publicAllocationForCleanup: %v", err)
	}

	manager.markStartFailure(machine, alloc, primaryErr)

	persisted := store.mustMachine(t, machine.ID)
	if !strings.Contains(persisted.Error, primaryErr.Error()) {
		t.Fatalf("machine error = %q, want original failure", persisted.Error)
	}
	diagnostic := providerCleanupDiagnostic(t, persisted.Diagnostics)
	if !strings.Contains(diagnostic.Error, cleanupErr.Error()) {
		t.Fatalf("cleanup diagnostic error = %q, want %q", diagnostic.Error, cleanupErr)
	}
	if strings.Contains(diagnostic.Error, primaryErr.Error()) {
		t.Fatalf("cleanup diagnostic error = %q, must not misclassify original start failure", diagnostic.Error)
	}
}

func TestCommitAndActivatePublicNetworkPersistsCompleteCreatingIntentBeforeHostMutation(t *testing.T) {
	for _, create := range []bool{true, false} {
		name := "start"
		if create {
			name = "create"
		}
		t.Run(name, func(t *testing.T) {
			machine := &models.Machine{ID: "machine-1", Name: "machine-1", Backend: firecrackerBackend, State: models.MachineStateStopped}
			store := newLifecycleContractStore()
			if !create {
				store = newLifecycleContractStore(machine)
			}
			events := &lifecycleEventLog{}
			store.events = events
			alloc, err := allocationForIndex("172.16.0.0/16", 700)
			if err != nil {
				t.Fatalf("allocationForIndex: %v", err)
			}
			network := &lifecycleContractNetwork{events: events}
			network.reserveHook = func(candidate *Allocation) error {
				persisted := store.mustMachine(t, machine.ID)
				if persisted.State != models.MachineStateCreating {
					t.Fatalf("state at first host mutation = %q, want creating", persisted.State)
				}
				assertPersistedPublicIdentity(t, persisted, candidate)
				return nil
			}
			manager := newLifecycleContractManager(t, store, network)

			reserved, persisted, err := manager.commitAndActivatePublicNetwork(machine, alloc, create)
			if err != nil || !persisted || reserved == nil || *reserved != *alloc {
				t.Fatalf("commitAndActivatePublicNetwork = reserved:%+v persisted:%t error:%v", reserved, persisted, err)
			}
			intentEvent := "network.intent:" + machine.ID
			if create {
				intentEvent = "machine.create-intent:" + machine.ID
			}
			assertLifecycleEvents(t, events.snapshot(), intentEvent, "network.reserve:"+alloc.TAPDevice)
		})
	}
}

func TestCommitAndActivatePublicNetworkNeverMutatesHostWhenIntentWriteFails(t *testing.T) {
	machine := &models.Machine{ID: "missing-machine", Backend: firecrackerBackend, State: models.MachineStateStopped}
	store := newLifecycleContractStore()
	alloc, err := allocationForIndex("172.16.0.0/16", 701)
	if err != nil {
		t.Fatalf("allocationForIndex: %v", err)
	}
	network := &lifecycleContractNetwork{}
	manager := newLifecycleContractManager(t, store, network)

	reserved, persisted, err := manager.commitAndActivatePublicNetwork(machine, alloc, false)
	if err == nil || persisted || reserved != nil {
		t.Fatalf("commitAndActivatePublicNetwork = reserved:%+v persisted:%t error:%v, want pre-mutation persistence failure", reserved, persisted, err)
	}
	if network.reserveCount() != 0 {
		t.Fatalf("Reserve calls = %d, want zero", network.reserveCount())
	}
}

func TestLateAsyncBootCannotResurrectStoppedMachine(t *testing.T) {
	machine := canonicalLifecycleMachine(t, "machine-stop", models.MachineStateCreating)
	store := newLifecycleContractStore(machine)
	network := &lifecycleContractNetwork{}
	manager := newLifecycleContractManager(t, store, network)
	entered, release := installBlockingAsyncBootHook(manager)
	defer release()
	machineDir := filepath.Join(manager.cfg.DataDir, "machines", machine.ID)
	if err := os.MkdirAll(machineDir, 0750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	manager.cfg.FirecrackerBin = filepath.Join(manager.cfg.DataDir, "must-not-execute-firecracker")
	alloc, err := publicAllocationForCleanup(machine)
	if err != nil {
		t.Fatalf("publicAllocationForCleanup: %v", err)
	}
	done := make(chan struct{})
	manager.lifecycleMu.Lock()
	generation := manager.beginBootGenerationLocked(machine.ID)
	manager.lifecycleMu.Unlock()
	go func() {
		defer close(done)
		manager.startVM(machine, machineDir, alloc, generation)
	}()
	waitForLifecycleSignal(t, entered, "asynchronous boot hook")

	if err := manager.Stop(machine.ID); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	release()
	waitForLifecycleSignal(t, done, "stale asynchronous boot return")

	persisted := store.mustMachine(t, machine.ID)
	if persisted.State != models.MachineStateStopped {
		t.Fatalf("machine state = %q, want stopped", persisted.State)
	}
	assertFirecrackerWasNotPrepared(t, machineDir)
}

func TestLateAsyncBootCannotResurrectDeletedMachine(t *testing.T) {
	machine := canonicalLifecycleMachine(t, "machine-delete", models.MachineStateCreating)
	store := newLifecycleContractStore(machine)
	network := &lifecycleContractNetwork{}
	manager := newLifecycleContractManager(t, store, network)
	entered, release := installBlockingAsyncBootHook(manager)
	defer release()
	machineDir := filepath.Join(manager.cfg.DataDir, "machines", machine.ID)
	if err := os.MkdirAll(machineDir, 0750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	manager.cfg.FirecrackerBin = filepath.Join(manager.cfg.DataDir, "must-not-execute-firecracker")
	alloc, err := publicAllocationForCleanup(machine)
	if err != nil {
		t.Fatalf("publicAllocationForCleanup: %v", err)
	}
	done := make(chan struct{})
	manager.lifecycleMu.Lock()
	generation := manager.beginBootGenerationLocked(machine.ID)
	manager.lifecycleMu.Unlock()
	go func() {
		defer close(done)
		manager.startVM(machine, machineDir, alloc, generation)
	}()
	waitForLifecycleSignal(t, entered, "asynchronous boot hook")

	if err := manager.Delete(machine.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	release()
	waitForLifecycleSignal(t, done, "stale asynchronous boot return")

	if store.hasMachine(machine.ID) {
		t.Fatal("deleted machine was recreated by stale asynchronous boot")
	}
	assertFirecrackerWasNotPrepared(t, machineDir)
}

func TestStaleFirecrackerExitCannotCleanUpReplacementGeneration(t *testing.T) {
	machine := canonicalLifecycleMachine(t, "machine-restarted", models.MachineStateRunning)
	store := newLifecycleContractStore(machine)
	network := &lifecycleContractNetwork{}
	manager := newLifecycleContractManager(t, store, network)

	manager.lifecycleMu.Lock()
	staleGeneration := manager.beginBootGenerationLocked(machine.ID)
	currentGeneration := manager.beginBootGenerationLocked(machine.ID)
	manager.lifecycleMu.Unlock()

	manager.handleVMExit(machine.ID, staleGeneration, nil, nil)
	if network.releaseCount() != 0 {
		t.Fatalf("stale exit release calls = %d, want zero", network.releaseCount())
	}
	if persisted := store.mustMachine(t, machine.ID); persisted.State != models.MachineStateRunning {
		t.Fatalf("state after stale exit = %q, want running", persisted.State)
	}

	manager.handleVMExit(machine.ID, currentGeneration, nil, nil)
	if network.releaseCount() != 1 {
		t.Fatalf("current exit release calls = %d, want one", network.releaseCount())
	}
	if persisted := store.mustMachine(t, machine.ID); persisted.State != models.MachineStateStopped {
		t.Fatalf("state after current exit = %q, want stopped", persisted.State)
	}
}

func TestStopAllCleansCreatingFirecrackerWithoutStoredProcess(t *testing.T) {
	machine := canonicalLifecycleMachine(t, "machine-pending-boot", models.MachineStateCreating)
	store := newLifecycleContractStore(machine)
	network := &lifecycleContractNetwork{}
	manager := newLifecycleContractManager(t, store, network)

	manager.lifecycleMu.Lock()
	generation := manager.beginBootGenerationLocked(machine.ID)
	manager.lifecycleMu.Unlock()

	manager.StopAll()

	if network.releaseCount() != 1 {
		t.Fatalf("network release calls = %d, want one", network.releaseCount())
	}
	if persisted := store.mustMachine(t, machine.ID); persisted.State != models.MachineStateStopped {
		t.Fatalf("state after StopAll = %q, want stopped", persisted.State)
	}
	manager.lifecycleMu.Lock()
	current := manager.bootGenerationCurrentLocked(machine.ID, generation)
	manager.lifecycleMu.Unlock()
	if current {
		t.Fatal("StopAll left pending Firecracker boot generation active")
	}
}

// lifecycleContractStore is intentionally small but stateful enough to assert
// ordering and durable recovery behavior without SQLite or physical-host work.
type lifecycleContractStore struct {
	mu sync.Mutex

	machines map[string]*models.Machine
	images   map[string]*models.Image
	events   *lifecycleEventLog

	updateStateHook   func(id string, state models.MachineState, errMsg string) error
	updateNetworkHook func(id, ip, gatewayIP, tapDevice string) error
}

func newLifecycleContractStore(machines ...*models.Machine) *lifecycleContractStore {
	store := &lifecycleContractStore{
		machines: make(map[string]*models.Machine, len(machines)),
		images:   make(map[string]*models.Image),
	}
	for _, machine := range machines {
		if machine != nil {
			store.machines[machine.ID] = cloneLifecycleMachine(machine)
		}
	}
	return store
}

func (s *lifecycleContractStore) CreateMachine(machine *models.Machine) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.events != nil {
		s.events.add("machine.create-intent:" + machine.ID)
	}
	s.machines[machine.ID] = cloneLifecycleMachine(machine)
	return nil
}

func (s *lifecycleContractStore) GetMachine(id string) (*models.Machine, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	machine, ok := s.machines[id]
	if !ok {
		return nil, sql.ErrNoRows
	}
	return cloneLifecycleMachine(machine), nil
}

func (s *lifecycleContractStore) ListMachines() ([]models.Machine, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]string, 0, len(s.machines))
	for id := range s.machines {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	machines := make([]models.Machine, 0, len(ids))
	for _, id := range ids {
		machines = append(machines, *cloneLifecycleMachine(s.machines[id]))
	}
	return machines, nil
}

func (s *lifecycleContractStore) UpdateMachineState(id string, state models.MachineState, errMsg string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.events != nil {
		s.events.add(fmt.Sprintf("state.%s:%s", state, id))
	}
	if s.updateStateHook != nil {
		if err := s.updateStateHook(id, state, errMsg); err != nil {
			return err
		}
	}
	machine, ok := s.machines[id]
	if !ok {
		return sql.ErrNoRows
	}
	machine.State = state
	machine.Error = errMsg
	machine.Diagnostics = nil
	return nil
}

func (s *lifecycleContractStore) UpdateMachineStateWithDiagnostics(id string, state models.MachineState, errMsg string, diagnostics []models.Diagnostic) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.events != nil {
		s.events.add(fmt.Sprintf("diagnostic.%s:%s", state, id))
	}
	machine, ok := s.machines[id]
	if !ok {
		return sql.ErrNoRows
	}
	machine.State = state
	machine.Error = errMsg
	machine.Diagnostics = append([]models.Diagnostic(nil), diagnostics...)
	return nil
}

func (s *lifecycleContractStore) UpdateMachineNetwork(id, ip, gatewayIP, tapDevice string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.events != nil {
		s.events.add("network.persist:" + id)
	}
	if s.updateNetworkHook != nil {
		if err := s.updateNetworkHook(id, ip, gatewayIP, tapDevice); err != nil {
			return err
		}
	}
	machine, ok := s.machines[id]
	if !ok {
		return sql.ErrNoRows
	}
	machine.IP = ip
	machine.GatewayIP = gatewayIP
	machine.TAPDevice = tapDevice
	return nil
}

func (s *lifecycleContractStore) UpdateMachineNetworkIntent(machine *models.Machine) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if machine == nil {
		return errors.New("nil machine intent")
	}
	if s.events != nil {
		s.events.add("network.intent:" + machine.ID)
	}
	current, ok := s.machines[machine.ID]
	if !ok {
		return sql.ErrNoRows
	}
	copyMachinePublicNetwork(current, machine)
	current.State = machine.State
	current.Error = machine.Error
	current.Diagnostics = append([]models.Diagnostic(nil), machine.Diagnostics...)
	return nil
}

func copyMachinePublicNetwork(destination, source *models.Machine) {
	destination.IP = source.IP
	destination.GatewayIP = source.GatewayIP
	destination.TAPDevice = source.TAPDevice
	destination.NetworkCIDR = source.NetworkCIDR
	destination.NetworkIndex = source.NetworkIndex
	destination.GuestMAC = source.GuestMAC
	destination.Netmask = source.Netmask
}

func (s *lifecycleContractStore) UpdateMachineVSockCID(id string, cid uint32) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	machine, ok := s.machines[id]
	if !ok {
		return sql.ErrNoRows
	}
	machine.VSockCID = cid
	return nil
}

func (s *lifecycleContractStore) DeleteMachine(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.machines, id)
	if s.events != nil {
		s.events.add("machine.delete:" + id)
	}
	return nil
}

func (s *lifecycleContractStore) MaxAllocatedNetworkIndex() (uint32, error) { return 0, nil }
func (s *lifecycleContractStore) MaxAllocatedVSockCID() (uint32, error)     { return 2, nil }

func (s *lifecycleContractStore) GetImage(id string) (*models.Image, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	image, ok := s.images[id]
	if !ok {
		return nil, sql.ErrNoRows
	}
	copy := *image
	return &copy, nil
}

func (s *lifecycleContractStore) ListImages() ([]models.Image, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	images := make([]models.Image, 0, len(s.images))
	for _, image := range s.images {
		images = append(images, *image)
	}
	return images, nil
}

func (s *lifecycleContractStore) mustMachine(t *testing.T, id string) *models.Machine {
	t.Helper()
	machine, err := s.GetMachine(id)
	if err != nil {
		t.Fatalf("GetMachine(%s): %v", id, err)
	}
	return machine
}

func (s *lifecycleContractStore) hasMachine(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.machines[id]
	return ok
}

type lifecycleContractNetwork struct {
	mu sync.Mutex

	allocation  *Allocation
	allocateErr error
	reserveErr  error
	reserveHook func(*Allocation) error
	reserves    []Allocation
	releaseErr  error
	releaseHook func(call int, alloc *Allocation) error
	releases    []Allocation
	events      *lifecycleEventLog
}

func (n *lifecycleContractNetwork) Plan(ownerID string) (*Allocation, error) {
	if n.events != nil {
		n.events.add("network.plan:" + ownerID)
	}
	return n.Allocate(ownerID)
}

func (n *lifecycleContractNetwork) Allocate(string) (*Allocation, error) {
	if n.allocateErr != nil {
		return nil, n.allocateErr
	}
	if n.allocation == nil {
		return nil, errors.New("test allocation is not configured")
	}
	copy := *n.allocation
	return &copy, nil
}

func (n *lifecycleContractNetwork) Reserve(alloc *Allocation) (*Allocation, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.reserveErr != nil {
		return nil, n.reserveErr
	}
	if alloc == nil {
		return nil, errors.New("nil reservation")
	}
	copy := *alloc
	n.reserves = append(n.reserves, copy)
	if n.events != nil {
		n.events.add("network.reserve:" + copy.TAPDevice)
	}
	if n.reserveHook != nil {
		if err := n.reserveHook(&copy); err != nil {
			return &copy, err
		}
	}
	return &copy, nil
}

func (n *lifecycleContractNetwork) Release(alloc *Allocation) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if alloc != nil {
		n.releases = append(n.releases, *alloc)
		if n.events != nil {
			n.events.add("network.release:" + alloc.TAPDevice)
		}
	}
	if n.releaseHook != nil {
		return n.releaseHook(len(n.releases), alloc)
	}
	return n.releaseErr
}

func (n *lifecycleContractNetwork) releaseCount() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.releases)
}

func (n *lifecycleContractNetwork) reserveCount() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.reserves)
}

type lifecycleContractReconciler struct {
	*lifecycleContractNetwork
	events         *lifecycleEventLog
	reconcileHook  func() error
	reconcileCalls int
}

func (r *lifecycleContractReconciler) Reconcile() error {
	r.reconcileCalls++
	if r.events != nil {
		r.events.add("network.reconcile")
	}
	if r.reconcileHook != nil {
		return r.reconcileHook()
	}
	return nil
}

func (*lifecycleContractReconciler) StartReconciler(context.Context, time.Duration) {}

// lifecycleEventLog is shared by the store and network fake so lifecycle
// ordering assertions do not rely on sleeps.
type lifecycleEventLog struct {
	mu     sync.Mutex
	events []string
}

func (l *lifecycleEventLog) add(event string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, event)
}

func (l *lifecycleEventLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.events...)
}

func newLifecycleContractManager(t *testing.T, store MachineStore, network NetworkAllocator) *Manager {
	t.Helper()
	dataDir := t.TempDir()
	return &Manager{
		cfg: &config.Config{
			DataDir:    dataDir,
			BridgeCIDR: "172.16.0.0/16",
		},
		db:              store,
		network:         network,
		bootGenerations: make(map[string]uint64),
	}
}

func canonicalLifecycleMachine(t *testing.T, id string, state models.MachineState) *models.Machine {
	t.Helper()
	index := lifecycleIndexForID(id)
	alloc, err := allocationForIndex("172.16.0.0/16", index)
	if err != nil {
		t.Fatalf("allocationForIndex(%d): %v", index, err)
	}
	alloc.OwnerID = id
	machine := &models.Machine{
		ID:        id,
		Name:      id,
		Backend:   firecrackerBackend,
		State:     state,
		VCPUs:     1,
		CPU:       models.CPUConfig{Kind: "shared", Millis: 1000, VCPUs: 1},
		MemoryMB:  128,
		IP:        alloc.GuestIP,
		GatewayIP: alloc.GatewayIP,
		TAPDevice: alloc.TAPDevice,
	}
	setMachinePublicAllocation(machine, alloc)
	return machine
}

func canonicalLifecycleAllocation(t *testing.T, bridgeCIDR, ownerID string, index uint32) *Allocation {
	t.Helper()
	alloc, err := allocationForIndex(bridgeCIDR, index)
	if err != nil {
		t.Fatalf("allocationForIndex(%q, %d): %v", bridgeCIDR, index, err)
	}
	alloc.OwnerID = ownerID
	return alloc
}

func lifecycleIndexForID(id string) uint32 {
	var total uint32 = 1
	for _, r := range id {
		total = (total*33 + uint32(r)) % 10000
	}
	return total + 1
}

func cloneLifecycleMachine(machine *models.Machine) *models.Machine {
	if machine == nil {
		return nil
	}
	copy := *machine
	copy.Diagnostics = append([]models.Diagnostic(nil), machine.Diagnostics...)
	return &copy
}

func assertPersistedPublicIdentity(t *testing.T, machine *models.Machine, alloc *Allocation) {
	t.Helper()
	if machine.IP != alloc.GuestIP || machine.GatewayIP != alloc.GatewayIP || machine.TAPDevice != alloc.TAPDevice ||
		machine.NetworkCIDR != alloc.BaseCIDR || machine.NetworkIndex != alloc.NetworkIndex ||
		machine.GuestMAC != alloc.GuestMAC || machine.Netmask != alloc.Mask || machine.ID != alloc.OwnerID {
		t.Fatalf("durable public identity = ip %q gateway %q TAP %q, want %+v", machine.IP, machine.GatewayIP, machine.TAPDevice, alloc)
	}
}

func assertProviderCleanupFailure(t *testing.T, machine *models.Machine, want error) {
	t.Helper()
	if machine.State != models.MachineStateError {
		t.Fatalf("machine state = %q, want recoverable error", machine.State)
	}
	diagnostic := providerCleanupDiagnosticContaining(t, machine.Diagnostics, want.Error())
	if !diagnostic.Retryable || diagnostic.Phase != "cleanup" {
		t.Fatalf("provider cleanup diagnostic = %+v", diagnostic)
	}
	if !strings.Contains(diagnostic.Error, want.Error()) {
		t.Fatalf("provider cleanup diagnostic error = %q, want %q", diagnostic.Error, want)
	}
}

func providerCleanupDiagnosticContaining(t *testing.T, diagnostics []models.Diagnostic, want string) models.Diagnostic {
	t.Helper()
	for _, diagnostic := range diagnostics {
		if diagnostic.Category == "provider_network" && diagnostic.Phase == "cleanup" && strings.Contains(diagnostic.Error, want) {
			return diagnostic
		}
	}
	t.Fatalf("diagnostics = %+v, want provider_network cleanup diagnostic containing %q", diagnostics, want)
	return models.Diagnostic{}
}

func providerCleanupDiagnostic(t *testing.T, diagnostics []models.Diagnostic) models.Diagnostic {
	t.Helper()
	for _, diagnostic := range diagnostics {
		if diagnostic.Category == "provider_network" && diagnostic.Phase == "cleanup" {
			return diagnostic
		}
	}
	t.Fatalf("diagnostics = %+v, want provider_network cleanup diagnostic", diagnostics)
	return models.Diagnostic{}
}

func assertLifecycleEvents(t *testing.T, got []string, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("events = %v, want %v", got, want)
		}
	}
}

func assertEventBefore(t *testing.T, events []string, first, second string) {
	t.Helper()
	firstIndex, secondIndex := -1, -1
	for i, event := range events {
		if event == first && firstIndex == -1 {
			firstIndex = i
		}
		if event == second && secondIndex == -1 {
			secondIndex = i
		}
	}
	if firstIndex == -1 || secondIndex == -1 || firstIndex >= secondIndex {
		t.Fatalf("events = %v, want %q before %q", events, first, second)
	}
}

func containsLifecycleEvent(events []string, want string) bool {
	for _, event := range events {
		if event == want || strings.HasPrefix(event, want+":") {
			return true
		}
	}
	return false
}

func installBlockingAsyncBootHook(manager *Manager) (<-chan struct{}, func()) {
	entered := make(chan struct{})
	gate := make(chan struct{})
	var once sync.Once
	manager.beforeAsyncBoot = func(string) {
		close(entered)
		<-gate
	}
	return entered, func() { once.Do(func() { close(gate) }) }
}

func waitForLifecycleSignal(t *testing.T, signal <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", description)
	}
}

func assertFirecrackerWasNotPrepared(t *testing.T, machineDir string) {
	t.Helper()
	configPath := filepath.Join(machineDir, "firecracker.sock.json")
	if _, err := os.Stat(configPath); err == nil {
		t.Fatalf("stale boot prepared Firecracker config %s", configPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stat Firecracker config: %v", err)
	}
}

func buildProviderNetworkCleanupDiagnostic(machine *models.Machine, action string, err error) models.Diagnostic {
	var alloc *Allocation
	if durable, present, durableErr := durablePublicAllocationFromMachine(machine); durableErr == nil && present {
		alloc = durable
	}
	if machine != nil && strings.TrimSpace(machine.TAPDevice) != "" && strings.TrimSpace(machine.IP) != "" && strings.TrimSpace(machine.GatewayIP) != "" {
		if index, ok := networkIndexFromTAP(strings.TrimSpace(machine.TAPDevice)); alloc == nil && ok {
			alloc = &Allocation{
				TAPDevice:    strings.TrimSpace(machine.TAPDevice),
				GuestIP:      strings.TrimSpace(machine.IP),
				GatewayIP:    strings.TrimSpace(machine.GatewayIP),
				GuestMAC:     guestMACForNetworkIndex(index),
				Mask:         "255.255.255.252",
				NetworkIndex: index,
			}
		}
	}
	return buildProviderNetworkCleanupDiagnosticForAllocation(machine, alloc, action, err)
}
