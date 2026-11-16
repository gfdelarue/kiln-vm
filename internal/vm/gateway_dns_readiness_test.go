package vm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gfdelarue/kiln-vm/internal/apierr"
	"github.com/gfdelarue/kiln-vm/internal/execution"
	"github.com/gfdelarue/kiln-vm/internal/machine"
	"github.com/gfdelarue/kiln-vm/internal/models"
)

func TestWaitReadyChecksGatewayDNSAfterProfileAndBeforeCapabilities(t *testing.T) {
	svc, base := setupCapabilityTestService(t, nil)
	events := []string{}
	checks := 0
	machines := &gatewayDNSCheckingMachineService{
		MachineService: base,
		exec: func(ctx context.Context, id, command string, stdin io.Reader) (*execution.BufferedResult, error) {
			switch {
			case strings.Contains(command, "kiln-vm-ready"):
				events = append(events, "profile")
			case strings.Contains(command, "docker-capability-ok"):
				events = append(events, "capability")
			}
			return base.Exec(ctx, id, command, stdin)
		},
		check: func(context.Context, string) (machine.GatewayDNSReadinessStatus, error) {
			checks++
			events = append(events, "gateway-dns")
			if checks == 1 {
				return machine.GatewayDNSReadinessStatus{
					Applicable:  true,
					FailureKind: "path",
					Transport:   "udp",
					Phase:       "read",
					Detail:      "gateway_dns_path_failed",
				}, machine.ErrGatewayDNSPath
			}
			return machine.GatewayDNSReadinessStatus{
				Applicable:   true,
				Ready:        true,
				PathOK:       true,
				ResolutionOK: true,
				Detail:       "ready",
			}, nil
		},
	}
	svc.machines = machines
	svc.gatewayDNSReadyTimeout = 100 * time.Millisecond
	svc.gatewayDNSReadyInterval = time.Millisecond

	if err := svc.WaitReady(context.Background(), "cap-vm"); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
	want := []string{"profile", "gateway-dns", "gateway-dns", "capability"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
}

func TestWaitReadyGatewayDNSFailureIsSanitizedStructuredProviderError(t *testing.T) {
	db := openVMServiceTestDB(t)
	base := &readinessMachineService{
		machine: gatewayDNSVMTestMachine(),
		execFunc: func(context.Context, string, string) (*execution.BufferedResult, error) {
			return &execution.BufferedResult{ExitCode: 0, Stdout: "kiln-vm-ready\n"}, nil
		},
	}
	const (
		rawName   = "secret-probe.example."
		rawServer = "172.16.0.5"
		rawPath   = "/run/kiln-vm/provider-runtime/dns-probe/kiln-vm-agent-secret"
		rawOutput = "raw probe stdout and resolver error"
	)
	machines := &gatewayDNSCheckingMachineService{
		MachineService: base,
		check: func(context.Context, string) (machine.GatewayDNSReadinessStatus, error) {
			return machine.GatewayDNSReadinessStatus{
				Applicable:  true,
				FailureKind: "path",
				Transport:   "udp",
				Phase:       "read",
				Detail:      "gateway_dns_path_failed",
				CheckedAt:   time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC),
			}, fmt.Errorf("%s %s %s %s", rawName, rawServer, rawPath, rawOutput)
		},
	}
	svc := NewService(db, nil, machines)
	svc.gatewayDNSReadyTimeout = 10 * time.Millisecond
	svc.gatewayDNSReadyInterval = time.Millisecond

	err := svc.WaitReady(context.Background(), "ready")
	if err == nil {
		t.Fatal("WaitReady succeeded, want gateway DNS failure")
	}
	var operation *apierr.OperationError
	if !errors.As(err, &operation) {
		t.Fatalf("WaitReady error = %T %v, want OperationError", err, err)
	}
	if operation.Code != "gateway_dns_not_ready" || operation.Phase != "gateway_dns_readiness" || operation.Resource != "ready" || operation.Action != "verify" || !operation.Retryable {
		t.Fatalf("operation = %+v", operation)
	}
	if !errors.Is(err, machine.ErrGatewayDNSPath) || !errors.Is(err, apierr.ErrConflict) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WaitReady error = %v, want path, conflict, and deadline sentinels", err)
	}
	if len(operation.Diagnostics) != 1 {
		t.Fatalf("diagnostics = %+v, want one", operation.Diagnostics)
	}
	diagnostic := operation.Diagnostics[0]
	if diagnostic.Category != "provider_network" || diagnostic.Phase != "gateway_dns_readiness" || diagnostic.MachineID != "machine-vm-ready" || diagnostic.State != "path" || diagnostic.Mode != "udp" || diagnostic.Step != "read" || diagnostic.Error != "gateway_dns_path_failed" || !diagnostic.Retryable {
		t.Fatalf("diagnostic = %+v", diagnostic)
	}
	exposedDiagnostic, marshalErr := json.Marshal(diagnostic)
	if marshalErr != nil {
		t.Fatalf("marshal diagnostic: %v", marshalErr)
	}
	exposed := err.Error() + string(exposedDiagnostic)
	for _, forbidden := range []string{rawName, rawServer, rawPath, rawOutput} {
		if strings.Contains(exposed, forbidden) {
			t.Fatalf("structured error exposed %q in %q", forbidden, exposed)
		}
	}
	if diagnostic.Path != "" || diagnostic.TAPDevice != "" || diagnostic.StdoutTail != "" || diagnostic.StderrTail != "" {
		t.Fatalf("diagnostic exposed private transport fields: %+v", diagnostic)
	}
}

func TestWaitReadyGatewayDNSSanitizesUnexpectedCheckerFields(t *testing.T) {
	db := openVMServiceTestDB(t)
	base := &readinessMachineService{
		machine: gatewayDNSVMTestMachine(),
		execFunc: func(context.Context, string, string) (*execution.BufferedResult, error) {
			return &execution.BufferedResult{ExitCode: 0, Stdout: "kiln-vm-ready\n"}, nil
		},
	}
	machines := &gatewayDNSCheckingMachineService{
		MachineService: base,
		check: func(context.Context, string) (machine.GatewayDNSReadinessStatus, error) {
			return machine.GatewayDNSReadinessStatus{
				Applicable:  true,
				FailureKind: "secret-kind",
				Transport:   "secret-transport",
				Phase:       "secret-phase",
				RCodeName:   "secret-rcode",
				Detail:      "secret-detail",
			}, errors.New("secret-checker-error")
		},
	}
	svc := NewService(db, nil, machines)
	svc.gatewayDNSReadyTimeout = 5 * time.Millisecond
	svc.gatewayDNSReadyInterval = time.Millisecond

	err := svc.WaitReady(context.Background(), "ready")
	var operation *apierr.OperationError
	if !errors.As(err, &operation) {
		t.Fatalf("WaitReady error = %T %v, want OperationError", err, err)
	}
	diagnostic := operation.Diagnostics[0]
	if diagnostic.State != "probe" || diagnostic.Mode != "" || diagnostic.Step != "" || diagnostic.Error != "probe_unavailable" {
		t.Fatalf("sanitized diagnostic = %+v", diagnostic)
	}
	exposed, _ := json.Marshal(diagnostic)
	if strings.Contains(err.Error()+string(exposed), "secret") {
		t.Fatalf("unexpected checker fields leaked: error=%v diagnostic=%s", err, exposed)
	}
}

func TestWaitReadyBypassesGatewayDNSForMachineServicesWithoutChecker(t *testing.T) {
	db := openVMServiceTestDB(t)
	base := &readinessMachineService{
		machine: gatewayDNSVMTestMachine(),
		execFunc: func(context.Context, string, string) (*execution.BufferedResult, error) {
			return &execution.BufferedResult{ExitCode: 0, Stdout: "kiln-vm-ready\n"}, nil
		},
	}
	svc := NewService(db, nil, base)
	if err := svc.WaitReady(context.Background(), "ready"); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
	if base.execCalls != 1 {
		t.Fatalf("profile exec calls = %d, want one and no extra private probe", base.execCalls)
	}
}

type gatewayDNSCheckingMachineService struct {
	machine.MachineService
	check func(context.Context, string) (machine.GatewayDNSReadinessStatus, error)
	exec  func(context.Context, string, string, io.Reader) (*execution.BufferedResult, error)
}

func (s *gatewayDNSCheckingMachineService) CheckGatewayDNSReadiness(ctx context.Context, machineID string) (machine.GatewayDNSReadinessStatus, error) {
	return s.check(ctx, machineID)
}

func (s *gatewayDNSCheckingMachineService) Exec(ctx context.Context, machineID, command string, stdin io.Reader) (*execution.BufferedResult, error) {
	if s.exec != nil {
		return s.exec(ctx, machineID, command, stdin)
	}
	result, err := machine.ExecuteBuffered(ctx, s.MachineService, machineID, command, stdin)
	if err != nil {
		return result, fmt.Errorf("execute readiness probe: %w", err)
	}
	return result, nil
}

func (s *gatewayDNSCheckingMachineService) Execute(ctx context.Context, machineID string, request execution.Request) execution.Outcome {
	return executeTestAdapter(ctx, request, func(ctx context.Context, command string, stdin io.Reader) (*execution.BufferedResult, error) {
		return s.Exec(ctx, machineID, command, stdin)
	})
}

func gatewayDNSVMTestMachine() *models.Machine {
	return &models.Machine{
		ID:       "machine-vm-ready",
		Name:     "vm-ready-machine",
		Backend:  "firecracker",
		State:    models.MachineStateRunning,
		VCPUs:    1,
		CPU:      models.CPUConfig{Kind: "shared", VCPUs: 1},
		MemoryMB: 256,
		IP:       "172.16.0.6",
		VSockCID: 50,
	}
}
