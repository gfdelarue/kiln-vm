package machine

import (
	"context"
	"fmt"
	"io"

	"github.com/gfdelarue/kiln-vm/internal/execution"
	"github.com/gfdelarue/kiln-vm/internal/models"
)

// MachineService defines the interface for machine lifecycle operations.
type MachineService interface {
	Create(ctx context.Context, req *models.CreateMachineRequest) (*models.Machine, error)
	Get(id string) (*models.Machine, error)
	List() ([]models.Machine, error)
	Stop(id string) error
	Start(ctx context.Context, id string) (*models.Machine, error)
	Delete(id string) error
	Execute(ctx context.Context, id string, request execution.Request) execution.Outcome
	Upload(ctx context.Context, id, path string, mode uint32, size int64, reader io.Reader) error
	DialShell(ctx context.Context, id string) (io.ReadWriteCloser, error)
	DialForward(ctx context.Context, id string, port int) (io.ReadWriteCloser, error)
}

func ExecuteBuffered(
	ctx context.Context,
	service MachineService,
	id string,
	command string,
	stdin io.Reader,
) (*execution.BufferedResult, error) {
	result, err := execution.RunBuffered(ctx, execution.RunnerFunc(func(ctx context.Context, request execution.Request) execution.Outcome {
		return service.Execute(ctx, id, request)
	}), command, stdin)
	if err != nil {
		return nil, fmt.Errorf("execute machine command: %w", err)
	}
	return result, nil
}

// MachineStore defines database operations needed by the Manager.
type MachineStore interface {
	CreateMachine(m *models.Machine) error
	GetMachine(id string) (*models.Machine, error)
	ListMachines() ([]models.Machine, error)
	UpdateMachineState(id string, state models.MachineState, errMsg string) error
	UpdateMachineStateWithDiagnostics(id string, state models.MachineState, errMsg string, diagnostics []models.Diagnostic) error
	UpdateMachineNetwork(id, ip, gatewayIP, tapDevice string) error
	UpdateMachineNetworkIntent(machine *models.Machine) error
	UpdateMachineVSockCID(id string, cid uint32) error
	DeleteMachine(id string) error
	MaxAllocatedNetworkIndex() (uint32, error)
	MaxAllocatedVSockCID() (uint32, error)
	GetImage(id string) (*models.Image, error)
	ListImages() ([]models.Image, error)
}

// NetworkAllocator defines the network allocation interface.
type NetworkAllocator interface {
	// Plan reserves the next provider network identity in memory without
	// mutating physical-host state. Callers must durably persist the returned
	// tuple before passing it to Reserve.
	Plan(vmID string) (*Allocation, error)
	Allocate(vmID string) (*Allocation, error)
	Reserve(alloc *Allocation) (*Allocation, error)
	Release(alloc *Allocation) error
}
