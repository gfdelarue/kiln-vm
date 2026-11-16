package windowsprofile

import (
	"context"
	"encoding/xml"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gfdelarue/kiln-vm/internal/machine"
	"github.com/gfdelarue/kiln-vm/internal/naming"
)

func TestInstallBootKeysOutlastSlowFirmware(t *testing.T) {
	var total time.Duration
	for i, delay := range installBootKeyDelays() {
		if delay <= 0 || delay > 2*time.Second {
			t.Fatalf("delay %d = %s, want a press inside every 5s CD prompt", i, delay)
		}
		total += delay
	}
	if total < 45*time.Second {
		t.Fatalf("install boot key window = %s, want >= 45s", total)
	}
}

func TestShouldRestartInstallDomainOnlyForBoundedShutoffStates(t *testing.T) {
	if !shouldRestartInstallDomain("shut off", 0) {
		t.Fatal("expected shut off domain to be restartable")
	}
	if !shouldRestartInstallDomain("shutoff", maxInstallDomainStarts-1) {
		t.Fatal("expected final allowed shutoff restart to be restartable")
	}
	if shouldRestartInstallDomain("shut off", maxInstallDomainStarts) {
		t.Fatal("expected restart limit to be enforced")
	}
	if shouldRestartInstallDomain("crashed", 0) {
		t.Fatal("expected crashed domain not to be restarted as normal install progress")
	}
}

func TestWindowsReadinessCommandReportsStatusAndLogs(t *testing.T) {
	command := windowsReadinessCommand()
	for _, want := range []string{
		"kiln-vm-ready",
		"kiln-vm-not-ready",
		"C:\\kiln-vm\\provision.complete",
		"C:\\kiln-vm\\provision.log",
		"C:\\kiln-vm\\bootstrap.log",
		"QEMU-GA",
	} {
		if !strings.Contains(command, want) {
			t.Fatalf("readiness command missing %q:\n%s", want, command)
		}
	}
}

func TestAutounattendSkipsOOBEAndInteractivePrompts(t *testing.T) {
	for _, want := range []string{
		"<HideEULAPage>true</HideEULAPage>",
		"<HideLocalAccountScreen>true</HideLocalAccountScreen>",
		"<HideOEMRegistrationScreen>true</HideOEMRegistrationScreen>",
		"<HideOnlineAccountScreens>true</HideOnlineAccountScreens>",
		"<HideWirelessSetupInOOBE>true</HideWirelessSetupInOOBE>",
		"<SkipMachineOOBE>true</SkipMachineOOBE>",
		"<SkipUserOOBE>true</SkipUserOOBE>",
		"<ProtectYourPC>3</ProtectYourPC>",
	} {
		if !strings.Contains(autounattendTemplate, want) {
			t.Fatalf("autounattend template missing %q", want)
		}
	}
}

func TestSetupCompleteRunsProvisioningBeforeRetryTasks(t *testing.T) {
	synchronousProvision := `powershell.exe -NoProfile -ExecutionPolicy Bypass -File "%PROVISION%" -AuthorizedKeysPath "%AUTHORIZED_KEYS%" -LogPath "%PROVISION_LOG%"`
	retryTasks := `echo Synchronous provisioning did not complete; creating retry tasks`
	startupTask := `schtasks.exe /Create /TN "%TASK_STARTUP%"`

	for _, want := range []string{
		`copy /Y "%PROVISION_DRIVE%\provision.ps1" "%PROVISION%"`,
		`copy /Y "%PROVISION_DRIVE%\authorized_keys" "%AUTHORIZED_KEYS%"`,
		`if not exist "%PROVISION%" exit /b 1`,
		`if not exist "%AUTHORIZED_KEYS%" exit /b 1`,
		synchronousProvision,
		`if exist C:\kiln-vm\provision.complete (`,
		`echo Provisioning completed synchronously`,
		retryTasks,
		startupTask,
		`schtasks.exe /Run /TN "%TASK_STARTUP%"`,
	} {
		if !strings.Contains(setupCompleteTemplate, want) {
			t.Fatalf("SetupComplete template missing %q", want)
		}
	}

	provisionIndex := strings.Index(setupCompleteTemplate, synchronousProvision)
	retryIndex := strings.Index(setupCompleteTemplate, retryTasks)
	taskIndex := strings.Index(setupCompleteTemplate, startupTask)
	if provisionIndex < 0 || retryIndex < 0 || taskIndex < 0 {
		t.Fatal("SetupComplete ordering markers were not found")
	}
	if !(provisionIndex < retryIndex && retryIndex < taskIndex) {
		t.Fatalf("SetupComplete ordering is wrong: provision=%d retry=%d task=%d", provisionIndex, retryIndex, taskIndex)
	}
}

func TestProvisionTaskCleanupIgnoresMissingScheduledTasks(t *testing.T) {
	for _, want := range []string{
		"function Remove-ProvisionTasks",
		`$previousErrorActionPreference = $ErrorActionPreference`,
		`$ErrorActionPreference = "Continue"`,
		`schtasks.exe /Delete /TN $taskName /F *> $null`,
		`$ErrorActionPreference = $previousErrorActionPreference`,
	} {
		if !strings.Contains(provisionTemplate, want) {
			t.Fatalf("provision template missing %q", want)
		}
	}
	if strings.Contains(provisionTemplate, `schtasks.exe /Delete /TN $taskName /F 2>$null | Out-Null`) {
		t.Fatal("provision task cleanup still uses throwing scheduled-task deletion")
	}
}

func TestPreparePrivateKeyPathUsesStagingKey(t *testing.T) {
	keyPair := &machine.SSHKeyPair{PrivateKeyPath: "/tmp/staging-key"}
	if got := preparePrivateKeyPath(keyPair, "/tmp/final-key"); got != "/tmp/staging-key" {
		t.Fatalf("preparePrivateKeyPath = %q, want staging key", got)
	}
	if got := preparePrivateKeyPath(nil, "/tmp/final-key"); got != "/tmp/final-key" {
		t.Fatalf("preparePrivateKeyPath nil = %q, want final fallback", got)
	}
}

func TestPromotePreparedFileMovesInsteadOfCopying(t *testing.T) {
	root := t.TempDir()
	staging := filepath.Join(root, "staging")
	final := filepath.Join(root, "final")
	if err := os.MkdirAll(staging, 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(final, 0750); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(staging, "base.qcow2")
	target := filepath.Join(final, "base.qcow2")
	if err := os.WriteFile(source, []byte("disk"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := promotePreparedFile(source, target, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(source); !os.IsNotExist(err) {
		t.Fatalf("source still exists after promotion: %v", err)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("target mode = %o, want 0600", info.Mode().Perm())
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "disk" {
		t.Fatalf("target content = %q, want disk", data)
	}
}

func TestCopyFileWithContextStagesISOWithoutMutatingSource(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "Win11.iso")
	target := filepath.Join(root, "staging", "install.iso")
	if err := os.MkdirAll(filepath.Dir(target), 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("iso"), 0600); err != nil {
		t.Fatal(err)
	}

	if err := copyFileWithContext(context.Background(), source, target, 0644); err != nil {
		t.Fatal(err)
	}

	sourceInfo, err := os.Stat(source)
	if err != nil {
		t.Fatal(err)
	}
	if got := sourceInfo.Mode().Perm(); got != 0600 {
		t.Fatalf("source mode = %o, want unchanged 0600", got)
	}
	targetInfo, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if got := targetInfo.Mode().Perm(); got != 0644 {
		t.Fatalf("target mode = %o, want 0644", got)
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "iso" {
		t.Fatalf("target content = %q, want iso", data)
	}
}

func TestStageISOForLibvirtHardlinksInternalSource(t *testing.T) {
	workDir := t.TempDir()
	source := filepath.Join(workDir, "cache", "Win11.iso")
	target := filepath.Join(workDir, "staging", "install.iso")
	if err := os.MkdirAll(filepath.Dir(source), 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("iso"), 0600); err != nil {
		t.Fatal(err)
	}

	if err := stageISOForLibvirt(context.Background(), source, target, workDir, 0644); err != nil {
		t.Fatal(err)
	}

	sourceInfo, err := os.Stat(source)
	if err != nil {
		t.Fatal(err)
	}
	targetInfo, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(sourceInfo, targetInfo) {
		t.Fatal("expected internal ISO staging to hardlink instead of copying")
	}
}

func TestStageISOForLibvirtCopiesExternalSource(t *testing.T) {
	external := t.TempDir()
	workDir := t.TempDir()
	source := filepath.Join(external, "Win11.iso")
	target := filepath.Join(workDir, "staging", "install.iso")
	if err := os.MkdirAll(filepath.Dir(target), 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("iso"), 0600); err != nil {
		t.Fatal(err)
	}

	if err := stageISOForLibvirt(context.Background(), source, target, workDir, 0644); err != nil {
		t.Fatal(err)
	}

	sourceInfo, err := os.Stat(source)
	if err != nil {
		t.Fatal(err)
	}
	targetInfo, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(sourceInfo, targetInfo) {
		t.Fatal("expected external ISO staging to copy rather than hardlink")
	}
	if got := sourceInfo.Mode().Perm(); got != 0600 {
		t.Fatalf("external source mode = %o, want unchanged 0600", got)
	}
	if got := targetInfo.Mode().Perm(); got != 0644 {
		t.Fatalf("staged ISO mode = %o, want 0644", got)
	}
}

func TestVirtInstallInstallsOntoVirtIOWithStagedMedia(t *testing.T) {
	args := virtInstallArgs(PrepareRequest{
		ConnectURI: "qemu:///system",
		MemoryMB:   4096,
		VCPUs:      2,
		Network:    "kiln-vm",
	}, "domain", "52:54:00:00:00:01", "/tmp/base.qcow2", "/tmp/config.img", "/tmp/payload.iso", installMedia{
		Windows: "/var/lib/kiln-vm/windows-profiles/windows11-prepare-1/install.iso",
		VirtIO:  "/var/lib/kiln-vm/windows-profiles/windows11-prepare-1/virtio-win.iso",
	})

	for flag, want := range map[string]string{
		"--cdrom":   "/var/lib/kiln-vm/windows-profiles/windows11-prepare-1/install.iso",
		"--network": "network=kiln-vm,model=virtio,mac=52:54:00:00:00:01",
		"--channel": "unix,target.type=virtio,name=org.qemu.guest_agent.0",
	} {
		if got := argValues(args, flag); !slices.Equal(got, []string{want}) {
			t.Fatalf("%s values = %q, want %q", flag, got, want)
		}
	}
	wantDisks := []string{
		"path=/tmp/base.qcow2,bus=virtio,format=qcow2",
		"path=/var/lib/kiln-vm/windows-profiles/windows11-prepare-1/virtio-win.iso,device=cdrom,bus=sata,readonly=on",
		"path=/tmp/payload.iso,device=cdrom,bus=sata,readonly=on",
		"path=/tmp/config.img,device=floppy,readonly=on",
	}
	if got := argValues(args, "--disk"); !slices.Equal(got, wantDisks) {
		t.Fatalf("--disk values = %q, want %q", got, wantDisks)
	}
}

func argValues(args []string, flag string) []string {
	var values []string
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag {
			values = append(values, args[i+1])
		}
	}
	return values
}

func TestAutounattendLoadsVirtIODriversInWindowsPE(t *testing.T) {
	rendered := renderTemplate(autounattendTemplate, templateValues{AdminUser: "kiln-vm", AdminPassword: "pw", ProductKey: "KEY", ComputerName: "kiln-vm-win"})

	if strings.Contains(rendered, "{{") {
		t.Fatalf("rendered autounattend has unreplaced placeholders:\n%s", rendered)
	}
	decoder := xml.NewDecoder(strings.NewReader(rendered))
	for {
		if _, err := decoder.Token(); err == io.EOF {
			break
		} else if err != nil {
			t.Fatalf("rendered autounattend is not well-formed XML: %v", err)
		}
	}
	for _, want := range []string{
		`<component name="Microsoft-Windows-PnpCustomizationsWinPE"`,
		`wcm:keyValue="1"><Path>D:\viostor\w11\amd64</Path>`,
		`wcm:keyValue="2"><Path>D:\NetKVM\w11\amd64</Path>`,
		`wcm:keyValue="10"><Path>H:\NetKVM\w11\amd64</Path>`,
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("rendered autounattend missing %q", want)
		}
	}
	if pe, oobe := strings.Index(rendered, "PnpCustomizationsWinPE"), strings.Index(rendered, `pass="specialize"`); pe > oobe {
		t.Fatal("virtio driver paths are outside the windowsPE pass")
	}
	if strings.Contains(rendered, "RunSynchronous") {
		t.Fatal("autounattend runs provisioning before oobeSystem creates the admin account")
	}
}

func TestProvisionInstallsGuestToolsAndNoNode(t *testing.T) {
	rendered := renderProvisionTemplate(provisionTemplate)

	for _, want := range []string{
		`cert\Virtio_Win_Red_Hat_CA.cer`,
		`guest-agent\qemu-ga-x86_64.msi`,
		`"/qn", "/norestart"`,
		"pnputil.exe",
		"WaitForExit($TimeoutSeconds * 1000)",
		"Start-Service -Name QEMU-GA",
		"Install-VirtIOGuestSupport",
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("provision script missing %q", want)
		}
	}
	for _, unwanted := range []string{"{{", "node", "npm"} {
		if strings.Contains(strings.ToLower(rendered), unwanted) {
			t.Fatalf("provision script still contains %q", unwanted)
		}
	}
}

func TestLocalSSHKeyErrorClassifiesMissingAndPermission(t *testing.T) {
	if !localSSHKeyError(os.ErrNotExist) {
		t.Fatal("expected missing key to be local SSH key error")
	}
	if !localSSHKeyError(os.ErrPermission) {
		t.Fatal("expected permission error to be local SSH key error")
	}
	if localSSHKeyError(context.Canceled) {
		t.Fatal("expected context cancellation not to be local SSH key error")
	}
}

func TestIPProvisionGraceScalesWithOperationTimeout(t *testing.T) {
	tests := []struct {
		name    string
		timeout time.Duration
		want    time.Duration
	}{
		{name: "default", timeout: 0, want: 80 * time.Minute},
		{name: "short operation", timeout: 15 * time.Minute, want: 15 * time.Minute},
		{name: "near minimum", timeout: 25 * time.Minute, want: 20 * time.Minute},
		{name: "default cli timeout", timeout: 90 * time.Minute, want: 80 * time.Minute},
		{name: "explicit long timeout", timeout: 120 * time.Minute, want: 110 * time.Minute},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ipProvisionGrace(tt.timeout); got != tt.want {
				t.Fatalf("ipProvisionGrace(%s) = %s, want %s", tt.timeout, got, tt.want)
			}
		})
	}
}

func TestTailForError(t *testing.T) {
	if got := tailForError("abcdef", 3); got != "def" {
		t.Fatalf("tailForError = %q, want def", got)
	}
	if got := tailForError("abc", 10); got != "abc" {
		t.Fatalf("tailForError short = %q, want abc", got)
	}
}

func TestInstallDomainNameFitsTheSwtpmLimit(t *testing.T) {
	name := installDomainName()
	if len(name) > naming.LibvirtDomainNameMaxBytes || !strings.HasPrefix(name, "kiln-vm-") {
		t.Fatalf("install domain %q must start with kiln-vm- and fit in %d bytes", name, naming.LibvirtDomainNameMaxBytes)
	}
}
