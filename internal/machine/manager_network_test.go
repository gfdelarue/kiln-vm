package machine

import (
	"errors"
	"testing"

	"github.com/gfdelarue/kiln-vm/internal/models"
)

type recordingNetworkAllocator struct {
	released *Allocation
	err      error
}

func (n *recordingNetworkAllocator) Plan(string) (*Allocation, error) {
	return nil, errors.New("not implemented")
}

func (n *recordingNetworkAllocator) Allocate(string) (*Allocation, error) {
	return nil, errors.New("not implemented")
}

func (n *recordingNetworkAllocator) Reserve(*Allocation) (*Allocation, error) {
	return nil, errors.New("not implemented")
}

func (n *recordingNetworkAllocator) Release(alloc *Allocation) error {
	if alloc != nil {
		copy := *alloc
		n.released = &copy
	}
	return n.err
}

func TestReleaseMachineRuntimePreservesGatewayIdentity(t *testing.T) {
	network := &recordingNetworkAllocator{}
	manager := &Manager{network: network}
	machine := canonicalLifecycleMachine(t, "machine-1", models.MachineStateStopping)

	if err := manager.releaseMachineRuntime(machine); err != nil {
		t.Fatalf("releaseMachineRuntime: %v", err)
	}
	if network.released == nil {
		t.Fatal("network allocation was not released")
	}
	if network.released.GatewayIP != machine.GatewayIP {
		t.Fatalf("released gateway = %q, want %q", network.released.GatewayIP, machine.GatewayIP)
	}
	if network.released.GuestIP != machine.IP || network.released.TAPDevice != machine.TAPDevice {
		t.Fatalf("released allocation = %+v, want machine public identity", network.released)
	}
}

func TestReleaseMachineRuntimePropagatesProviderCleanupFailure(t *testing.T) {
	want := errors.New("iptables cleanup failed")
	manager := &Manager{network: &recordingNetworkAllocator{err: want}}
	err := manager.releaseMachineRuntime(canonicalLifecycleMachine(t, "machine-1", models.MachineStateStopping))
	if !errors.Is(err, want) {
		t.Fatalf("releaseMachineRuntime error = %v, want wrapped %v", err, want)
	}
}

func TestRecordNetworkCleanupFailureKeepsMachineRecoverableAndDiagnostic(t *testing.T) {
	machine := &models.Machine{
		ID:        "machine-1",
		Name:      "app",
		Backend:   firecrackerBackend,
		State:     models.MachineStateStopping,
		IP:        "172.16.10.86",
		GatewayIP: "172.16.10.85",
		TAPDevice: "kv661",
	}
	store := &libvirtDispatchStore{machine: machine}
	manager := &Manager{db: store}
	want := errors.New("remove DNS admission: permission denied")

	err := manager.recordNetworkCleanupFailure(machine, "stop", want)
	if !errors.Is(err, want) {
		t.Fatalf("recordNetworkCleanupFailure error = %v, want wrapped %v", err, want)
	}
	if store.machine.State != models.MachineStateError {
		t.Fatalf("machine state = %q, want error", store.machine.State)
	}
	if len(store.machine.Diagnostics) != 1 {
		t.Fatalf("diagnostics = %+v, want one provider network diagnostic", store.machine.Diagnostics)
	}
	diagnostic := store.machine.Diagnostics[0]
	if diagnostic.Category != "provider_network" || diagnostic.Phase != "cleanup" || diagnostic.Action != "stop" || !diagnostic.Retryable {
		t.Fatalf("diagnostic = %+v", diagnostic)
	}
	if diagnostic.TAPDevice != machine.TAPDevice || diagnostic.MachineID != machine.ID {
		t.Fatalf("diagnostic lost machine identity: %+v", diagnostic)
	}
}
