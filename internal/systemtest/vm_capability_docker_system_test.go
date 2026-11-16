//go:build system && distro

package systemtest

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gfdelarue/kiln-vm/internal/models"
)

func TestVMUbuntuDockerCapabilityVerification(t *testing.T) {
	client := requireClient(t)
	cleanupResourcesByPrefix(t, client)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	defer cancel()
	profileImagesBefore := snapshotVMProfileImages(t, client, ctx)
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		cleanupNewVMProfileImages(t, client, cleanupCtx, profileImagesBefore)
	})

	name := uniqueName("vm-capability-docker")
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		if err := client.deleteVM(cleanupCtx, name); err != nil {
			t.Logf("cleanup VM %s: %v", name, err)
		}
	})

	vm, err := client.createVM(ctx, models.CreateVMRequest{
		Name:       name,
		Profile:    "ubuntu-docker",
		DiskSizeMB: 32768,
		CPU:        &models.CPUConfig{VCPUs: 2},
		MemoryMB:   3072,
	})
	if err != nil {
		t.Fatalf("create ubuntu-docker VM: %v", err)
	}
	assertCapabilityStatus(t, vm.Capabilities, "docker", models.VMCapabilityStatusPending)
	if _, err := client.waitForVMState(ctx, name, models.MachineStateRunning); err != nil {
		t.Fatalf("wait for ubuntu-docker VM running: %v", err)
	}

	response, err := client.verifyVMCapabilities(ctx, name, models.VerifyVMCapabilitiesRequest{Capability: "docker", Force: true})
	if err != nil {
		t.Fatalf("verify docker capability: %v", err)
	}
	assertCapabilityStatus(t, response.Conditions, "docker", models.VMCapabilityStatusOK)

	vm, err = client.getVM(ctx, name)
	if err != nil {
		t.Fatalf("get VM after capability verification: %v", err)
	}
	assertCapabilityStatus(t, vm.Capabilities, "docker", models.VMCapabilityStatusOK)

	hello, err := client.execVM(ctx, name, "docker image inspect kiln-vm/docker-capability:local >/dev/null && docker run --rm --pull=never kiln-vm/docker-capability:local /bin/sh -c 'echo kiln-vm-docker-ok'")
	if err != nil {
		t.Fatalf("run local capability image after verification: %v", err)
	}
	if hello.ExitCode != 0 || !strings.Contains(hello.Stdout, "kiln-vm-docker-ok") {
		t.Fatalf("local capability image failed: exit=%d stdout=%q stderr=%q", hello.ExitCode, hello.Stdout, hello.Stderr)
	}
	images, err := client.execVM(ctx, name, "docker image ls --format '{{.Repository}}:{{.Tag}}'")
	if err != nil {
		t.Fatalf("list Docker images after capability verification: %v", err)
	}
	if strings.Contains(images.Stdout, "alpine:3.20") || strings.Contains(images.Stdout, "hello-world") {
		t.Fatalf("capability verification should not leave public test images: %q", images.Stdout)
	}

	if err := client.stopVM(ctx, name); err != nil {
		t.Fatalf("stop VM before warm reuse check: %v", err)
	}
	if _, err := client.waitForVMState(ctx, name, models.MachineStateStopped); err != nil {
		t.Fatalf("wait for stopped VM: %v", err)
	}
	if _, err := client.startVM(ctx, name); err != nil {
		t.Fatalf("start VM before warm reuse check: %v", err)
	}
	if _, err := client.waitForVMState(ctx, name, models.MachineStateRunning); err != nil {
		t.Fatalf("wait for restarted VM: %v", err)
	}
	response, err = client.verifyVMCapabilities(ctx, name, models.VerifyVMCapabilitiesRequest{Capability: "docker", Force: true})
	if err != nil {
		t.Fatalf("force verify docker capability after warm reuse: %v", err)
	}
	assertCapabilityStatus(t, response.Conditions, "docker", models.VMCapabilityStatusOK)

	if result, err := client.execVM(ctx, name, "docker image rm -f kiln-vm/docker-capability:local >/dev/null 2>&1 || true; mv /usr/share/kiln-vm/docker-capability-rootfs.tar /usr/share/kiln-vm/docker-capability-rootfs.tar.missing"); err != nil {
		t.Fatalf("remove local Docker capability seed: %v", err)
	} else if result.ExitCode != 0 {
		t.Fatalf("remove local Docker capability seed failed: exit=%d stdout=%q stderr=%q", result.ExitCode, result.Stdout, result.Stderr)
	}
	response, err = client.verifyVMCapabilities(ctx, name, models.VerifyVMCapabilitiesRequest{Capability: "docker", Force: true})
	if err == nil {
		t.Fatalf("verify docker capability without local seed succeeded, want failure: %+v", response.Conditions)
	}
	if !strings.Contains(err.Error(), "kiln-vm Docker capability image seed is missing") {
		t.Fatalf("missing seed error = %v, want explicit local seed failure", err)
	}
	vm, err = client.getVM(ctx, name)
	if err != nil {
		t.Fatalf("get VM after missing seed verification: %v", err)
	}
	assertCapabilityStatus(t, vm.Capabilities, "docker", models.VMCapabilityStatusFailed)
}

func assertCapabilityStatus(t *testing.T, conditions []models.VMCapabilityCondition, capability string, status models.VMCapabilityStatus) {
	t.Helper()
	for _, condition := range conditions {
		if condition.Capability != capability {
			continue
		}
		if condition.Status != status {
			t.Fatalf("%s capability status = %s, want %s; condition=%+v", capability, condition.Status, status, condition)
		}
		return
	}
	t.Fatalf("capability %s not found in %+v", capability, conditions)
}
