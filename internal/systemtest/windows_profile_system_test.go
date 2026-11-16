//go:build system && windowsprofile

package systemtest

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gfdelarue/kiln-vm/internal/guestagent"
	"github.com/gfdelarue/kiln-vm/internal/models"
)

func TestWindowsProfileColdWarmStopStart(t *testing.T) {
	isoPath := os.Getenv("KILN_VM_WINDOWS_ISO")
	if strings.TrimSpace(isoPath) == "" {
		t.Skip("KILN_VM_WINDOWS_ISO is required for the real Windows profile system test")
	}
	client := requireClient(t)
	cleanupResourcesByPrefix(t, client)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
	defer cancel()

	profileImagesBefore := snapshotVMProfileImages(t, client, ctx)
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		cleanupNewVMProfileImages(t, client, cleanupCtx, profileImagesBefore)
	})

	image, err := client.prepareVMProfile(ctx, "windows11", models.PrepareVMProfileRequest{
		MediaPath:          isoPath,
		ActivationMode:     "deactivated",
		WaitTimeoutSeconds: 5400,
	})
	if err != nil {
		t.Fatalf("prepare windows11 profile: %v", err)
	}
	if image.Profile != "windows11" || image.ImageID == "" {
		t.Fatalf("prepared image = %+v", image)
	}

	name := uniqueName("windows")
	_, err = client.createVM(ctx, models.CreateVMRequest{
		Name:    name,
		Profile: "windows11",
		Power: &models.VMPowerPolicy{
			Mode: models.VMPowerModeManual,
		},
	})
	if err != nil {
		t.Fatalf("create windows VM: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		if err := client.deleteVM(cleanupCtx, name); err != nil {
			t.Logf("cleanup Windows VM %s: %v", name, err)
		}
	})

	vm, err := client.waitForVMState(ctx, name, models.MachineStateRunning)
	if err != nil {
		t.Fatalf("wait for windows VM running: %v", err)
	}
	if vm.Backend != "libvirt-qemu" || vm.GuestOS != "windows" || vm.IP == "" {
		t.Fatalf("windows VM provider details = %+v", vm)
	}

	result, err := client.execVM(ctx, name, "Write-Output 'kiln-vm-windows-ok'; (Get-Service QEMU-GA).Status; (Get-NetAdapter -Physical).InterfaceDescription; (Get-PhysicalDisk).FriendlyName")
	if err != nil {
		t.Fatalf("exec windows VM: %v", err)
	}
	if result.ExitCode != 0 || !strings.Contains(result.Stdout, "kiln-vm-windows-ok") || !strings.Contains(result.Stdout, "Running") || !strings.Contains(result.Stdout, "VirtIO Ethernet") || !strings.Contains(result.Stdout, "Red Hat VirtIO SCSI") {
		t.Fatalf("unexpected windows exec result: exit=%d stdout=%q stderr=%q", result.ExitCode, result.Stdout, result.Stderr)
	}
	result, err = client.execVM(ctx, name, `$env:COMPUTERNAME; @(Get-Content C:\ProgramData\ssh\administrators_authorized_keys | Where-Object { $_ }).Count`)
	if err != nil {
		t.Fatalf("read windows identity: %v", err)
	}
	if identity := strings.Fields(result.Stdout); result.ExitCode != 0 || len(identity) != 2 || strings.EqualFold(identity[0], "kiln-vm-win") || identity[1] != "1" {
		t.Fatalf("windows identity = %q, want a per-VM computer name and one authorized key", result.Stdout)
	}
	result, err = client.execVM(ctx, name, "Write-Error 'kiln-vm-stderr'; cmd /c exit 7")
	if err != nil {
		t.Fatalf("exec failing windows command: %v", err)
	}
	if result.ExitCode != 7 || !strings.Contains(result.Stderr, "kiln-vm-stderr") || strings.Contains(result.Stderr, "CLIXML") {
		t.Fatalf("failing windows command: exit=%d stderr=%q, want exit 7 and plain-text stderr", result.ExitCode, result.Stderr)
	}

	if err := client.uploadVM(ctx, name, `C:\kiln-vm\uploads\hello.txt`, []byte("kiln-vm-upload-ok")); err != nil {
		t.Fatalf("upload to windows VM: %v", err)
	}
	result, err = client.execVM(ctx, name, `Get-Content -LiteralPath C:\kiln-vm\uploads\hello.txt`)
	if err != nil || result.ExitCode != 0 || !strings.Contains(result.Stdout, "kiln-vm-upload-ok") {
		t.Fatalf("read uploaded file: result=%+v err=%v", result, err)
	}
	assertWindowsShellRuns(t, ctx, client, name)
	assertWindowsForwardReachesSSH(t, ctx, client, name)

	result, err = client.execVM(ctx, name, "Set-Content -LiteralPath C:\\kiln-vm\\warm-marker.txt -Value 'warm-ok'")
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("write warm marker: result=%+v err=%v", result, err)
	}
	if err := client.stopVM(ctx, name); err != nil {
		t.Fatalf("stop windows VM: %v", err)
	}
	if _, err := client.waitForVMState(ctx, name, models.MachineStateStopped); err != nil {
		t.Fatalf("wait for windows VM stopped: %v", err)
	}
	if _, err := client.startVM(ctx, name); err != nil {
		t.Fatalf("warm start windows VM: %v", err)
	}
	if _, err := client.waitForVMState(ctx, name, models.MachineStateRunning); err != nil {
		t.Fatalf("wait for warm windows VM running: %v", err)
	}
	result, err = client.execVM(ctx, name, "Get-Content -LiteralPath C:\\kiln-vm\\warm-marker.txt")
	if err != nil {
		t.Fatalf("read warm marker: %v", err)
	}
	if result.ExitCode != 0 || !strings.Contains(result.Stdout, "warm-ok") {
		t.Fatalf("warm marker mismatch: exit=%d stdout=%q stderr=%q", result.ExitCode, result.Stdout, result.Stderr)
	}
}

func assertWindowsShellRuns(t *testing.T, ctx context.Context, client *apiClient, name string) {
	t.Helper()
	conn, _, err := client.dialVMShell(ctx, name)
	if err != nil {
		t.Fatalf("dial windows shell: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if err := conn.WriteJSON(guestagent.ShellFrame{Type: "start", Version: guestagent.ProtocolVersion, Cols: 120, Rows: 40}); err != nil {
		t.Fatalf("start windows shell: %v", err)
	}
	if err := conn.WriteJSON(guestagent.ShellFrame{Type: "stdin", Data: []byte("Write-Output ('kiln-vm-' + 'shell-ok')\r\n")}); err != nil {
		t.Fatalf("write windows shell input: %v", err)
	}
	var output strings.Builder
	for !strings.Contains(output.String(), "kiln-vm-shell-ok") {
		if err := conn.SetReadDeadline(time.Now().Add(2 * time.Minute)); err != nil {
			t.Fatal(err)
		}
		var frame guestagent.ShellFrame
		if err := conn.ReadJSON(&frame); err != nil {
			t.Fatalf("read windows shell: %v; output so far %q", err, output.String())
		}
		switch frame.Type {
		case "stdout", "stderr":
			output.Write(frame.Data)
		case "error", "exit":
			t.Fatalf("windows shell ended early: %+v; output %q", frame, output.String())
		}
	}
}

func assertWindowsForwardReachesSSH(t *testing.T, ctx context.Context, client *apiClient, name string) {
	t.Helper()
	conn, _, err := client.dialVMForward(ctx, name, 22)
	if err != nil {
		t.Fatalf("dial windows forward: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if err := conn.SetReadDeadline(time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	_, banner, err := conn.ReadMessage()
	if err != nil || !strings.HasPrefix(string(banner), "SSH-2.0-") {
		t.Fatalf("forwarded windows port 22 banner = %q, err %v; want an SSH banner", banner, err)
	}
}
