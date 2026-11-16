package machine

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gfdelarue/kiln-vm/internal/apierr"
	"github.com/gfdelarue/kiln-vm/internal/libvirtdomain"
	"github.com/gfdelarue/kiln-vm/internal/models"
	"github.com/gfdelarue/kiln-vm/internal/naming"
)

func TestLibvirtDomainNameIsStableAndBounded(t *testing.T) {
	machine := &models.Machine{
		ID:      "12345678-1234-1234-1234-123456789abc",
		Name:    "Very_Long Windows VM Name With Spaces And Symbols !!!",
		Backend: libvirtBackend,
		GuestOS: windowsGuestOS,
	}
	name := libvirtDomainName(machine)
	if len(name) > naming.LibvirtDomainNameMaxBytes {
		t.Fatalf("domain name length = %d, want <= %d: %s", len(name), naming.LibvirtDomainNameMaxBytes, name)
	}
	if name != "kiln-vm-very-long-12345678" {
		t.Fatalf("domain name = %q", name)
	}
	if strings.ContainsAny(name, " _!") {
		t.Fatalf("domain name contains unsafe chars: %q", name)
	}
}

func TestNormalizeWindowsPathMapsLogicalPaths(t *testing.T) {
	cases := map[string]string{
		"/workspace/app":            "C:/kiln-vm/workspaces/app",
		"/tmp/archive.tar.gz":       "C:/kiln-vm/tmp/archive.tar.gz",
		"/var/lib/kiln-vm/services": "C:/kiln-vm/root/var/lib/kiln-vm/services",
		"C:/already/windows/path":   "C:/already/windows/path",
	}
	for input, want := range cases {
		if got := normalizeWindowsPath(input); got != want {
			t.Fatalf("normalizeWindowsPath(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestWindowsExecCommandRunsTheScriptInTheDefaultShell(t *testing.T) {
	script := "Write-Output \"café 🚀\""
	command := windowsExecCommand(script)
	if strings.Contains(command, "powershell.exe") || strings.Contains(command, "-EncodedCommand") {
		t.Fatalf("windows exec nests a second PowerShell, which turns stderr into CLIXML:\n%s", command)
	}
	for _, want := range []string{
		"$ProgressPreference = 'SilentlyContinue'; ",
		"[Console]::OutputEncoding = $OutputEncoding = New-Object Text.UTF8Encoding $false",
		"FromBase64String('" + base64.StdEncoding.EncodeToString(utf16LE(script+"\n$global:KilnVMExecOK = $?")) + "')",
		"; if ($global:KilnVMExecOK) { exit 0 }; if ($LASTEXITCODE) { exit $LASTEXITCODE }; exit 1",
	} {
		if !strings.Contains(command, want) {
			t.Fatalf("windows exec command missing %q:\n%s", want, command)
		}
	}
}

func TestUTF16LEEncodesSurrogatePairs(t *testing.T) {
	if got, want := utf16LE("é🚀"), []byte{0xe9, 0x00, 0x3d, 0xd8, 0x80, 0xde}; !bytes.Equal(got, want) {
		t.Fatalf("utf16LE = % x, want % x", got, want)
	}
}

func TestParseLibvirtIPv4IgnoresNonIPv4Text(t *testing.T) {
	output := `
 Name       MAC address          Protocol     Address
-------------------------------------------------------------------------------
 vnet0      52:54:00:aa:bb:cc    ipv4         192.168.122.42/24
`
	if got := parseLibvirtIPv4(output); got != "192.168.122.42" {
		t.Fatalf("parseLibvirtIPv4 = %q", got)
	}
}

func TestCreateLibvirtWindowsOverlayUsesBackingFile(t *testing.T) {
	if _, err := exec.LookPath("qemu-img"); err != nil {
		t.Skip("qemu-img is required for the real qcow2 overlay test")
	}
	root := t.TempDir()
	backing := filepath.Join(root, "base.qcow2")
	overlay := filepath.Join(root, "machine.qcow2")
	ctx := context.Background()
	if err := runHost(ctx, nil, "qemu-img", "create", "-f", "qcow2", backing, "64M"); err != nil {
		t.Fatalf("create backing qcow2: %v", err)
	}
	if err := createLibvirtQCOW2Overlay(ctx, backing, overlay); err != nil {
		t.Fatalf("createLibvirtQCOW2Overlay: %v", err)
	}
	out, err := runHostOutput(ctx, "qemu-img", "info", "--output=json", overlay)
	if err != nil {
		t.Fatalf("qemu-img info overlay: %v", err)
	}
	var info struct {
		Filename    string `json:"filename"`
		Format      string `json:"format"`
		BackingFile string `json:"backing-filename"`
	}
	if err := json.Unmarshal([]byte(out), &info); err != nil {
		t.Fatalf("decode qemu-img info: %v\n%s", err, out)
	}
	if info.Format != "qcow2" {
		t.Fatalf("overlay format = %q, want qcow2", info.Format)
	}
	if info.BackingFile != backing {
		t.Fatalf("overlay backing file = %q, want %q", info.BackingFile, backing)
	}
	if info.Filename != overlay {
		t.Fatalf("overlay filename = %q, want %q", info.Filename, overlay)
	}
}

func TestLibvirtVirtInstallArgsUseWindowsProfileSpec(t *testing.T) {
	machine := &models.Machine{VCPUs: 2, MemoryMB: 4096}
	spec, err := libvirtdomain.Normalize(nil)
	if err != nil {
		t.Fatalf("Normalize(nil): %v", err)
	}
	spec.Network.Source = "windows-net"
	args := libvirtVirtInstallArgs("qemu:///system", "kiln-vm-win", "/var/lib/kiln-vm/machines/m/disk.qcow2", machine, spec)

	assertArgValue(t, args, "--osinfo", "win11")
	assertArgValue(t, args, "--features", "smm.state=on")
	assertArgValue(t, args, "--disk", "path=/var/lib/kiln-vm/machines/m/disk.qcow2,bus=virtio,format=qcow2")
	assertArgValue(t, args, "--network", "network=windows-net,model=virtio")
	assertArgValue(t, args, "--channel", "unix,target.type=virtio,name=org.qemu.guest_agent.0")
	assertArgValue(t, args, "--graphics", "spice,listen=127.0.0.1")
	assertArgValue(t, args, "--tpm", "backend.type=emulator,backend.version=2.0,model=tpm-crb")
	assertArgValue(t, args, "--boot", "uefi,loader.secure=yes")
	if !hasArg(args, "--import") || !hasArg(args, "--noautoconsole") {
		t.Fatalf("virt-install args missing import/noautoconsole: %#v", args)
	}
}

func TestLibvirtNetworkSourceFallsBackToDaemonDefault(t *testing.T) {
	spec := models.LibvirtNetworkSpec{Model: "virtio"}
	if got := libvirtNetworkSource("daemon-net", spec); got != "daemon-net" {
		t.Fatalf("libvirtNetworkSource daemon fallback = %q, want daemon-net", got)
	}
	if got := libvirtNetworkArg(spec); got != "network=kiln-vm,model=virtio" {
		t.Fatalf("libvirtNetworkArg empty source = %q, want kiln-vm fallback", got)
	}
}

func TestLibvirtDomainSpecRejectsSecureBootWithoutUEFI(t *testing.T) {
	spec := libvirtdomain.Windows11()
	spec.Firmware = "bios"
	spec.SecureBoot = true

	_, err := libvirtdomain.Normalize(spec)
	if err == nil {
		t.Fatal("Normalize accepted secure boot with BIOS firmware")
	}
	if !errors.Is(err, apierr.ErrValidation) || !strings.Contains(err.Error(), "requires uefi firmware") {
		t.Fatalf("Normalize error = %v, want validation for secure boot firmware", err)
	}
}

func TestLibvirtDomainSpecRejectsInvalidNetworkSource(t *testing.T) {
	spec := libvirtdomain.Windows11()
	spec.Network.Source = "default,model=e1000e"

	_, err := libvirtdomain.Normalize(spec)
	if err == nil {
		t.Fatal("Normalize accepted invalid libvirt network source")
	}
	if !errors.Is(err, apierr.ErrValidation) || !strings.Contains(err.Error(), "libvirt.network.source") {
		t.Fatalf("Normalize error = %v, want validation for libvirt network source", err)
	}
}

func TestValidateCreateRequestRejectsLibvirtLinuxGuest(t *testing.T) {
	for _, guestOS := range []string{"", "linux"} {
		req := &models.CreateMachineRequest{
			Name:     "linux-vm",
			Backend:  libvirtBackend,
			GuestOS:  guestOS,
			ImageID:  "img-linux-qcow2",
			VCPUs:    2,
			MemoryMB: 2048,
		}
		err := validateCreateRequest(req)
		if !errors.Is(err, apierr.ErrValidation) || !strings.Contains(err.Error(), "libvirt-qemu backend requires windows") {
			t.Fatalf("validateCreateRequest(libvirt guest_os=%q) error = %v, want windows-only validation", guestOS, err)
		}
	}
}

func TestValidateCreateRequestAcceptsLibvirtWindowsGuest(t *testing.T) {
	req := &models.CreateMachineRequest{
		Name:     "win",
		Backend:  libvirtBackend,
		GuestOS:  windowsGuestOS,
		ImageID:  "img-win",
		VCPUs:    2,
		MemoryMB: 4096,
	}
	if err := validateCreateRequest(req); err != nil {
		t.Fatalf("validateCreateRequest(libvirt windows): %v", err)
	}
}

func assertArgValue(t *testing.T, args []string, flag, want string) {
	t.Helper()
	for i := 0; i < len(args)-1; i++ {
		if args[i] == flag {
			if args[i+1] != want {
				t.Fatalf("%s = %q, want %q in args %#v", flag, args[i+1], want, args)
			}
			return
		}
	}
	t.Fatalf("args missing %s=%q: %#v", flag, want, args)
}

func hasArg(args []string, flag string) bool {
	for _, arg := range args {
		if arg == flag {
			return true
		}
	}
	return false
}

type libvirtDispatchStore struct {
	image   *models.Image
	machine *models.Machine
	created int
}

func (s *libvirtDispatchStore) CreateMachine(*models.Machine) error {
	s.created++
	return nil
}

func (s *libvirtDispatchStore) GetMachine(id string) (*models.Machine, error) {
	if s.machine != nil && (id == s.machine.ID || id == s.machine.Name) {
		copy := *s.machine
		return &copy, nil
	}
	return nil, apierr.ErrNotFound
}

func (s *libvirtDispatchStore) ListMachines() ([]models.Machine, error) {
	if s.machine == nil {
		return nil, nil
	}
	return []models.Machine{*s.machine}, nil
}

func (s *libvirtDispatchStore) UpdateMachineState(id string, state models.MachineState, errMsg string) error {
	if s.machine != nil && s.machine.ID == id {
		s.machine.State = state
		s.machine.Error = errMsg
	}
	return nil
}

func (s *libvirtDispatchStore) UpdateMachineStateWithDiagnostics(id string, state models.MachineState, errMsg string, diagnostics []models.Diagnostic) error {
	if s.machine != nil && s.machine.ID == id {
		s.machine.State = state
		s.machine.Error = errMsg
		s.machine.Diagnostics = diagnostics
	}
	return nil
}

func (s *libvirtDispatchStore) UpdateMachineNetwork(id, ip, gatewayIP, tapDevice string) error {
	return nil
}

func (s *libvirtDispatchStore) UpdateMachineNetworkIntent(machine *models.Machine) error {
	if s.machine != nil && machine != nil && s.machine.ID == machine.ID {
		*s.machine = *machine
	}
	return nil
}

func (s *libvirtDispatchStore) UpdateMachineVSockCID(id string, cid uint32) error {
	if s.machine != nil && s.machine.ID == id {
		s.machine.VSockCID = cid
	}
	return nil
}

func (s *libvirtDispatchStore) DeleteMachine(id string) error {
	return nil
}

func (s *libvirtDispatchStore) MaxAllocatedNetworkIndex() (uint32, error) {
	return 0, nil
}

func (s *libvirtDispatchStore) MaxAllocatedVSockCID() (uint32, error) {
	return 0, nil
}

func (s *libvirtDispatchStore) GetImage(id string) (*models.Image, error) {
	if s.image != nil && id == s.image.ID {
		copy := *s.image
		return &copy, nil
	}
	return nil, apierr.ErrNotFound
}

func (s *libvirtDispatchStore) ListImages() ([]models.Image, error) {
	if s.image == nil {
		return nil, nil
	}
	return []models.Image{*s.image}, nil
}
