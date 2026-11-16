//go:build system && distro

package systemtest

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/gfdelarue/kiln-vm/internal/models"
)

func TestDistroUbuntuProfileSmoke(t *testing.T) {
	testDistroProfile(t, distroProfileTest{
		Label:       "ubuntu",
		Profile:     "ubuntu",
		DiskSizeMB:  8192,
		MemoryMB:    1024,
		VCPUs:       1,
		OSReleaseID: "ID=ubuntu",
		ToolCommand: "command -v apt-get",
		Timeout:     30 * time.Minute,
	})
}

func TestDistroKaliProfileSmoke(t *testing.T) {
	testDistroProfile(t, distroProfileTest{
		Label:       "kali",
		Profile:     "kali",
		DiskSizeMB:  32768,
		MemoryMB:    4096,
		VCPUs:       2,
		OSReleaseID: "ID=kali",
		ToolCommand: "command -v nmap",
		Timeout:     90 * time.Minute,
	})
}

func TestDistroUbuntuDockerProfileRunsDocker(t *testing.T) {
	client := requireClient(t)
	cleanupResourcesByPrefix(t, client)

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()
	profileImagesBefore := snapshotVMProfileImages(t, client, ctx)
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		cleanupNewVMProfileImages(t, client, cleanupCtx, profileImagesBefore)
	})

	name := uniqueName("distro-ubuntu-docker")
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		if err := client.deleteVM(cleanupCtx, name); err != nil {
			t.Logf("cleanup VM %s: %v", name, err)
		}
	})

	if _, err := client.createVM(ctx, models.CreateVMRequest{
		Name:       name,
		Profile:    "ubuntu-docker",
		DiskSizeMB: 32768,
		CPU:        &models.CPUConfig{VCPUs: 2},
		MemoryMB:   3072,
	}); err != nil {
		t.Fatalf("create ubuntu-docker VM: %v", err)
	}
	if _, err := client.waitForVMState(ctx, name, models.MachineStateRunning); err != nil {
		t.Fatalf("wait for ubuntu-docker VM running: %v", err)
	}
	vm, err := client.getVM(ctx, name)
	if err != nil {
		t.Fatalf("get ubuntu-docker VM: %v", err)
	}
	if err := waitForVMSSH(ctx, t, vm); err != nil {
		t.Fatal(err)
	}
	if err := waitForVMDocker(ctx, client, name); err != nil {
		t.Fatal(err)
	}
	result, err := client.execVM(ctx, name, "docker run --rm --pull=never kiln-vm/docker-capability:local /bin/sh -c 'echo kiln-vm-docker-ok'")
	if err != nil {
		t.Fatalf("run Docker container: %v", err)
	}
	if result.ExitCode != 0 || !strings.Contains(result.Stdout, "kiln-vm-docker-ok") {
		t.Fatalf("docker container mismatch: exit=%d stdout=%q stderr=%q", result.ExitCode, result.Stdout, result.Stderr)
	}
	assertDockerBridgeNetworking(ctx, t, client, name)
	result, err = client.execVM(ctx, name, "pkg-config --exists pixman-1 cairo pango pangocairo && python3 --version && g++ --version >/dev/null")
	if err != nil {
		t.Fatalf("check native Node build dependencies: %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("native build dependencies missing: exit=%d stdout=%q stderr=%q", result.ExitCode, result.Stdout, result.Stderr)
	}

	if err := client.stopVM(ctx, name); err != nil {
		t.Fatalf("stop ubuntu-docker VM: %v", err)
	}
	if _, err := client.waitForVMState(ctx, name, models.MachineStateStopped); err != nil {
		t.Fatalf("wait for ubuntu-docker VM stopped: %v", err)
	}
	if _, err := client.startVM(ctx, name); err != nil {
		t.Fatalf("restart ubuntu-docker VM: %v", err)
	}
	if _, err := client.waitForVMState(ctx, name, models.MachineStateRunning); err != nil {
		t.Fatalf("wait for restarted ubuntu-docker VM running: %v", err)
	}
	vm, err = client.getVM(ctx, name)
	if err != nil {
		t.Fatalf("get restarted ubuntu-docker VM: %v", err)
	}
	if err := waitForVMSSH(ctx, t, vm); err != nil {
		t.Fatal(err)
	}
	if err := waitForVMDocker(ctx, client, name); err != nil {
		t.Fatal(err)
	}
	assertDockerBridgeNetworking(ctx, t, client, name)
}

type distroProfileTest struct {
	Label       string
	Profile     string
	DiskSizeMB  int64
	MemoryMB    int
	VCPUs       int
	OSReleaseID string
	ToolCommand string
	Timeout     time.Duration
}

func waitForVMDocker(ctx context.Context, client *apiClient, name string) error {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	var last string
	for {
		result, err := client.execVM(ctx, name, "docker version --format '{{.Server.Version}}'")
		if err == nil && result.ExitCode == 0 && strings.TrimSpace(result.Stdout) != "" {
			return nil
		}
		if err != nil {
			last = err.Error()
		} else {
			last = fmt.Sprintf("exit=%d stdout=%q stderr=%q", result.ExitCode, result.Stdout, result.Stderr)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for Docker daemon in %s: %w; last: %s", name, ctx.Err(), last)
		case <-ticker.C:
		}
	}
}

func assertDockerBridgeNetworking(ctx context.Context, t *testing.T, client *apiClient, name string) {
	t.Helper()
	result, err := client.execVM(ctx, name, `
set -eu
cleanup() {
  docker rm -f kiln-vm-ci-web >/dev/null 2>&1 || true
  docker network rm kiln-vm-ci-net >/dev/null 2>&1 || true
}
cleanup
trap cleanup EXIT
docker network create kiln-vm-ci-net >/dev/null
docker image inspect kiln-vm/docker-capability:local >/dev/null
docker run -d --pull=never --name kiln-vm-ci-web --network kiln-vm-ci-net -p 18080:8080 kiln-vm/docker-capability:local /bin/sh -c 'while true; do printf "HTTP/1.1 200 OK\r\nContent-Length: 14\r\n\r\nkiln-vm-net-ok" | /bin/nc -l -p 8080; done' >/dev/null
service_ok=0
for _ in $(seq 1 40); do
  if docker run --rm --pull=never --network kiln-vm-ci-net kiln-vm/docker-capability:local /bin/wget -qO- http://kiln-vm-ci-web:8080 2>/dev/null | grep -q kiln-vm-net-ok; then
    service_ok=1
    break
  fi
  sleep 1
done
if [ "$service_ok" != 1 ]; then
  echo "service network DNS did not become ready" >&2
  exit 1
fi
host_ok=0
for _ in $(seq 1 20); do
  if python3 -c 'import urllib.request; print(urllib.request.urlopen("http://127.0.0.1:18080", timeout=3).read().decode())' 2>/dev/null | grep -q kiln-vm-net-ok; then
    host_ok=1
    break
  fi
  sleep 1
done
if [ "$host_ok" != 1 ]; then
  echo "published host port did not become ready" >&2
  exit 1
fi
echo docker-bridge-service-network-ok
`)
	if err != nil {
		t.Fatalf("run Docker bridge networking check: %v", err)
	}
	if result.ExitCode != 0 || !strings.Contains(result.Stdout, "docker-bridge-service-network-ok") {
		t.Fatalf("Docker bridge networking check failed: exit=%d stdout=%q stderr=%q", result.ExitCode, result.Stdout, result.Stderr)
	}
}

func waitForVMSSH(ctx context.Context, t *testing.T, vm *models.VM) error {
	t.Helper()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	var last string
	for {
		cmd := exec.CommandContext(ctx, "ssh-keyscan", "-T", "2", "-t", "ed25519", vm.IP)
		output, err := cmd.Output()
		if err == nil && strings.Contains(string(output), "ssh-ed25519") {
			return nil
		}
		last = strings.TrimSpace(string(output))
		if last == "" && err != nil {
			last = err.Error()
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for SSH in %s: %w; last: %s", vm.Name, ctx.Err(), last)
		case <-ticker.C:
		}
	}
}

func testDistroProfile(t *testing.T, tc distroProfileTest) {
	t.Helper()
	client := requireClient(t)
	cleanupResourcesByPrefix(t, client)

	ctx, cancel := context.WithTimeout(context.Background(), tc.Timeout)
	defer cancel()
	profileImagesBefore := snapshotVMProfileImages(t, client, ctx)
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		cleanupNewVMProfileImages(t, client, cleanupCtx, profileImagesBefore)
	})

	name := uniqueName("distro-" + tc.Label)
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		if err := client.deleteVM(cleanupCtx, name); err != nil {
			t.Logf("cleanup VM %s: %v", name, err)
		}
	})

	vm, err := client.createVM(ctx, models.CreateVMRequest{
		Name:       name,
		Profile:    tc.Profile,
		DiskSizeMB: tc.DiskSizeMB,
		CPU:        &models.CPUConfig{VCPUs: tc.VCPUs},
		MemoryMB:   tc.MemoryMB,
	})
	if err != nil {
		t.Fatalf("create %s VM: %v", tc.Profile, err)
	}
	if vm.ImageID == "" {
		t.Fatalf("expected cached image id for %s VM", tc.Profile)
	}
	vm, err = client.waitForVMState(ctx, name, models.MachineStateRunning)
	if err != nil {
		t.Fatalf("wait for %s VM running: %v", tc.Profile, err)
	}

	osRelease, err := client.execVM(ctx, name, "cat /etc/os-release")
	if err != nil {
		t.Fatalf("read /etc/os-release: %v", err)
	}
	if osRelease.ExitCode != 0 || !strings.Contains(osRelease.Stdout, tc.OSReleaseID) {
		t.Fatalf("unexpected /etc/os-release for %s: exit=%d stdout=%q stderr=%q", tc.Profile, osRelease.ExitCode, osRelease.Stdout, osRelease.Stderr)
	}
	tool, err := client.execVM(ctx, name, tc.ToolCommand)
	if err != nil {
		t.Fatalf("check %s tool command: %v", tc.Profile, err)
	}
	if tool.ExitCode != 0 {
		t.Fatalf("%s tool command failed: exit=%d stdout=%q stderr=%q", tc.Profile, tool.ExitCode, tool.Stdout, tool.Stderr)
	}

	marker := "kiln-vm-" + tc.Profile + "-persist"
	if _, err := client.execVM(ctx, name, "printf "+shellQuote(marker)+" >/root/distro-marker.txt && sync"); err != nil {
		t.Fatalf("write %s marker: %v", tc.Profile, err)
	}

	tap := vm.TAPDevice
	if err := client.stopVM(ctx, name); err != nil {
		t.Fatalf("stop %s VM: %v", tc.Profile, err)
	}
	if _, err := client.waitForVMState(ctx, name, models.MachineStateStopped); err != nil {
		t.Fatalf("wait for %s VM stopped: %v", tc.Profile, err)
	}
	if err := waitForNoTap(ctx, tap); err != nil {
		t.Fatal(err)
	}
	if _, err := client.startVM(ctx, name); err != nil {
		t.Fatalf("restart %s VM before reading marker: %v", tc.Profile, err)
	}
	if _, err := client.waitForVMState(ctx, name, models.MachineStateRunning); err != nil {
		t.Fatalf("wait for restarted %s VM running: %v", tc.Profile, err)
	}
	result, err := client.execVM(ctx, name, "cat /root/distro-marker.txt")
	if err != nil {
		t.Fatalf("read %s marker after restart: %v", tc.Profile, err)
	}
	if result.ExitCode != 0 || strings.TrimSpace(result.Stdout) != marker {
		t.Fatalf("%s marker mismatch: exit=%d stdout=%q stderr=%q", tc.Profile, result.ExitCode, result.Stdout, result.Stderr)
	}
}
