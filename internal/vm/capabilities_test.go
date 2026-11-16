package vm

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/gfdelarue/kiln-vm/internal/apierr"
	"github.com/gfdelarue/kiln-vm/internal/execution"
	"github.com/gfdelarue/kiln-vm/internal/models"
	"github.com/gfdelarue/kiln-vm/internal/store"
)

func TestVMGetSynthesizesPendingDeclaredCapability(t *testing.T) {
	svc, _ := setupCapabilityTestService(t, nil)

	vm, err := svc.Get("cap-vm")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(vm.Capabilities) != 1 {
		t.Fatalf("capabilities = %+v, want one", vm.Capabilities)
	}
	if vm.Capabilities[0].Capability != dockerCapability || vm.Capabilities[0].Status != models.VMCapabilityStatusPending {
		t.Fatalf("capability = %+v, want pending docker", vm.Capabilities[0])
	}
}

func TestVerifyCapabilitiesPersistsDockerSuccess(t *testing.T) {
	svc, machines := setupCapabilityTestService(t, func(command string) *execution.BufferedResult {
		if strings.Contains(command, "kiln-vm-ready") {
			return &execution.BufferedResult{ExitCode: 0, Stdout: "kiln-vm-ready\n"}
		}
		if strings.Contains(command, "docker-capability-ok") {
			return &execution.BufferedResult{ExitCode: 0, Stdout: "docker-capability-ok\n"}
		}
		return &execution.BufferedResult{ExitCode: 127, Stderr: "unexpected command: " + command}
	})

	conditions, err := svc.VerifyCapabilities(context.Background(), "cap-vm", &models.VerifyVMCapabilitiesRequest{Capability: dockerCapability})
	if err != nil {
		t.Fatalf("VerifyCapabilities: %v", err)
	}
	if len(conditions) != 1 || conditions[0].Status != models.VMCapabilityStatusOK {
		t.Fatalf("conditions = %+v, want ok", conditions)
	}
	if !strings.Contains(conditions[0].Detail, "docker-capability-ok") {
		t.Fatalf("detail = %q", conditions[0].Detail)
	}
	if machines.dockerExecs != 1 {
		t.Fatalf("docker execs = %d, want 1", machines.dockerExecs)
	}

	vm, err := svc.Get("cap-vm")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if vm.Capabilities[0].Status != models.VMCapabilityStatusOK || vm.Capabilities[0].CheckedAt == nil {
		t.Fatalf("persisted capability = %+v, want checked ok", vm.Capabilities[0])
	}
}

func TestWaitReadyVerifiesDeclaredCapabilityAndPersistsFailure(t *testing.T) {
	svc, _ := setupCapabilityTestService(t, func(command string) *execution.BufferedResult {
		if strings.Contains(command, "kiln-vm-ready") {
			return &execution.BufferedResult{ExitCode: 0, Stdout: "kiln-vm-ready\n"}
		}
		return &execution.BufferedResult{ExitCode: 1, Stderr: "iptables raw table missing"}
	})

	err := svc.WaitReady(context.Background(), "cap-vm")
	if err == nil {
		t.Fatal("WaitReady succeeded, want capability failure")
	}
	var operation *apierr.OperationError
	if !errors.As(err, &operation) {
		t.Fatalf("WaitReady error = %T %v, want OperationError", err, err)
	}
	if operation.Code != "vm_capability_failed" || operation.Phase != "docker_networking" || operation.Resource != "cap-vm" {
		t.Fatalf("operation = %+v", operation)
	}

	vm, getErr := svc.Get("cap-vm")
	if getErr != nil {
		t.Fatalf("Get: %v", getErr)
	}
	if vm.Capabilities[0].Status != models.VMCapabilityStatusFailed {
		t.Fatalf("capability = %+v, want failed", vm.Capabilities[0])
	}
	if !strings.Contains(vm.Capabilities[0].Detail, "iptables raw table missing") {
		t.Fatalf("detail = %q, want iptables failure", vm.Capabilities[0].Detail)
	}
}

func TestVerifyCapabilitiesSkipsOKUnlessForced(t *testing.T) {
	svc, machines := setupCapabilityTestService(t, func(command string) *execution.BufferedResult {
		if strings.Contains(command, "kiln-vm-ready") {
			return &execution.BufferedResult{ExitCode: 0, Stdout: "kiln-vm-ready\n"}
		}
		return &execution.BufferedResult{ExitCode: 0, Stdout: "docker-capability-ok\n"}
	})
	now := time.Now().UTC()
	if err := svc.db.UpsertVMCapabilityCondition("vm-cap", models.VMCapabilityCondition{
		Capability: dockerCapability,
		Status:     models.VMCapabilityStatusOK,
		Phase:      "docker_networking",
		Detail:     "previous ok",
		CheckedAt:  &now,
		UpdatedAt:  now,
	}); err != nil {
		t.Fatalf("seed ok condition: %v", err)
	}

	conditions, err := svc.VerifyCapabilities(context.Background(), "cap-vm", &models.VerifyVMCapabilitiesRequest{Capability: dockerCapability})
	if err != nil {
		t.Fatalf("VerifyCapabilities without force: %v", err)
	}
	if conditions[0].Detail != "previous ok" {
		t.Fatalf("conditions = %+v, want cached condition", conditions)
	}
	if machines.dockerExecs != 0 {
		t.Fatalf("docker execs without force = %d, want 0", machines.dockerExecs)
	}

	_, err = svc.VerifyCapabilities(context.Background(), "cap-vm", &models.VerifyVMCapabilitiesRequest{Capability: dockerCapability, Force: true})
	if err != nil {
		t.Fatalf("VerifyCapabilities with force: %v", err)
	}
	if machines.dockerExecs != 1 {
		t.Fatalf("docker execs with force = %d, want 1", machines.dockerExecs)
	}
}

func TestWaitReadyRerunsFailedCapabilityCondition(t *testing.T) {
	svc, machines := setupCapabilityTestService(t, func(command string) *execution.BufferedResult {
		if strings.Contains(command, "kiln-vm-ready") {
			return &execution.BufferedResult{ExitCode: 0, Stdout: "kiln-vm-ready\n"}
		}
		return &execution.BufferedResult{ExitCode: 0, Stdout: "docker-capability-ok\n"}
	})
	now := time.Now().UTC()
	if err := svc.db.UpsertVMCapabilityCondition("vm-cap", models.VMCapabilityCondition{
		Capability:  dockerCapability,
		Status:      models.VMCapabilityStatusFailed,
		Phase:       "docker_networking",
		Detail:      "previous failure",
		Remediation: "fix docker",
		CheckedAt:   &now,
		UpdatedAt:   now,
	}); err != nil {
		t.Fatalf("seed failed condition: %v", err)
	}

	if err := svc.WaitReady(context.Background(), "cap-vm"); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
	if machines.dockerExecs != 1 {
		t.Fatalf("docker execs = %d, want verifier to rerun failed condition", machines.dockerExecs)
	}
	vm, err := svc.Get("cap-vm")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if vm.Capabilities[0].Status != models.VMCapabilityStatusOK {
		t.Fatalf("capability = %+v, want ok after rerun", vm.Capabilities[0])
	}
}

func TestDockerCapabilityCommandUsesEphemeralPublishedPort(t *testing.T) {
	if strings.Contains(dockerCapabilityCommand, ":18081") || strings.Contains(dockerCapabilityCommand, "127.0.0.1:18081") {
		t.Fatalf("docker capability command should not hard-code host port 18081:\n%s", dockerCapabilityCommand)
	}
	for _, want := range []string{
		"-p 127.0.0.1::8080",
		"docker port \"$container\" 8080/tcp",
		"\"http://127.0.0.1:$host_port\"",
		"network=\"kiln-vm-cap-net-$suffix\"",
		"container=\"kiln-vm-cap-web-$suffix\"",
	} {
		if !strings.Contains(dockerCapabilityCommand, want) {
			t.Fatalf("docker capability command missing %q:\n%s", want, dockerCapabilityCommand)
		}
	}
}

func TestDockerCapabilityCommandUsesLocalSeedImageWithoutRegistryPulls(t *testing.T) {
	for _, forbidden := range []string{
		"alpine:3.20",
		"hello-world",
		"docker pull",
		"docker.io",
	} {
		if strings.Contains(dockerCapabilityCommand, forbidden) {
			t.Fatalf("docker capability command must not depend on %q:\n%s", forbidden, dockerCapabilityCommand)
		}
	}
	for _, want := range []string{
		`capability_image="` + dockerCapabilityImage + `"`,
		`capability_rootfs="` + dockerCapabilityRootfsTar + `"`,
		`docker image inspect "$capability_image"`,
		`docker import "$capability_rootfs" "$capability_image"`,
		`--pull=never "$capability_image"`,
		`kiln-vm Docker capability image seed is missing`,
	} {
		if !strings.Contains(dockerCapabilityCommand, want) {
			t.Fatalf("docker capability command missing %q:\n%s", want, dockerCapabilityCommand)
		}
	}
}

func setupCapabilityTestService(t *testing.T, exec func(command string) *execution.BufferedResult) (*Service, *capabilityMachineService) {
	t.Helper()
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	now := time.Now().UTC()
	if err := db.CreateImage(&models.Image{
		ID:         "image-cap",
		Name:       "cap-image",
		Tag:        "test",
		BootMode:   "persistent",
		SizeMB:     4096,
		RootfsPath: "/tmp/rootfs.ext4",
		CreatedAt:  now,
	}); err != nil {
		t.Fatalf("CreateImage: %v", err)
	}
	machineRecord := &models.Machine{
		ID:        "machine-cap",
		Name:      "vm-cap-vm",
		ImageID:   "image-cap",
		State:     models.MachineStateRunning,
		VCPUs:     4,
		CPU:       models.CPUConfig{Kind: "shared", VCPUs: 4},
		MemoryMB:  8192,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := db.CreateMachine(machineRecord); err != nil {
		t.Fatalf("CreateMachine: %v", err)
	}
	if err := db.CreateVM(&models.VMRecord{
		ID:         "vm-cap",
		Name:       "cap-vm",
		Profile:    "ubuntu-docker",
		MachineID:  machineRecord.ID,
		ImageID:    "image-cap",
		DiskSizeMB: 4096,
		CreatedAt:  now,
		UpdatedAt:  now,
	}); err != nil {
		t.Fatalf("CreateVM: %v", err)
	}
	machines := &capabilityMachineService{machine: machineRecord, exec: exec}
	return NewService(db, nil, machines), machines
}

type capabilityMachineService struct {
	readinessMachineService
	machine     *models.Machine
	exec        func(command string) *execution.BufferedResult
	dockerExecs int
}

func (s *capabilityMachineService) Get(id string) (*models.Machine, error) {
	if id != s.machine.ID {
		return nil, &apierr.NotFoundError{Resource: "machine", ID: id}
	}
	clone := *s.machine
	return &clone, nil
}

func (s *capabilityMachineService) Exec(ctx context.Context, id, command string, _ io.Reader) (*execution.BufferedResult, error) {
	if id != s.machine.ID {
		return nil, &apierr.NotFoundError{Resource: "machine", ID: id}
	}
	if strings.Contains(command, "docker-capability-ok") {
		s.dockerExecs++
	}
	if s.exec == nil {
		if strings.Contains(command, "kiln-vm-ready") {
			return &execution.BufferedResult{ExitCode: 0, Stdout: "kiln-vm-ready\n"}, nil
		}
		return &execution.BufferedResult{ExitCode: 0, Stdout: "docker-capability-ok\n"}, nil
	}
	return s.exec(command), nil
}

func (s *capabilityMachineService) Execute(ctx context.Context, id string, request execution.Request) execution.Outcome {
	return executeTestAdapter(ctx, request, func(ctx context.Context, command string, stdin io.Reader) (*execution.BufferedResult, error) {
		return s.Exec(ctx, id, command, stdin)
	})
}

func TestDockerCapabilityCommandChecksTheResponseItServes(t *testing.T) {
	served := `printf "HTTP/1.1 200 OK\r\nContent-Length: 14\r\n\r\nkiln-vm-net-ok"`
	if !strings.Contains(dockerCapabilityCommand, served) {
		t.Fatalf("docker capability command does not serve %s:\n%s", served, dockerCapabilityCommand)
	}
	if got := strings.Count(dockerCapabilityCommand, "grep -q kiln-vm-net-ok"); got != 3 {
		t.Fatalf("docker capability command checks the response %d times, want 3:\n%s", got, dockerCapabilityCommand)
	}
	if strings.Contains(dockerCapabilityCommand, "__") {
		t.Fatalf("docker capability command has an unreplaced placeholder:\n%s", dockerCapabilityCommand)
	}
}
