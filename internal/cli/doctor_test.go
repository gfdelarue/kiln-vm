package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/gfdelarue/kiln-vm/internal/libvirtdomain"
	"github.com/gfdelarue/kiln-vm/internal/models"
	vmservice "github.com/gfdelarue/kiln-vm/internal/vm"
)

func TestReadDoctorEnvFileParsesTheDaemonEnv(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kiln-vmd.env")
	if err := os.WriteFile(path, []byte(`
# comment
KILN_VM_FIRECRACKER_BIN=/usr/bin/firecracker
KILN_VM_KERNEL_PATH="/usr/lib/kiln-vm/vmlinux"
ignored
KILN_VM_PROFILE_IMAGE_MODE='pull'
KILN_VM_PROFILE_IMAGE_TOKEN_FILE=/etc/kiln-vm/profile-image-token
`), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	env, check := readDoctorEnvFile(path)
	if check == nil || check.Status != "ok" {
		t.Fatalf("env check = %#v, want ok", check)
	}
	if env["KILN_VM_FIRECRACKER_BIN"] != "/usr/bin/firecracker" {
		t.Fatalf("firecracker env = %q", env["KILN_VM_FIRECRACKER_BIN"])
	}
	if env["KILN_VM_KERNEL_PATH"] != "/usr/lib/kiln-vm/vmlinux" {
		t.Fatalf("kernel env = %q", env["KILN_VM_KERNEL_PATH"])
	}
	if env["KILN_VM_PROFILE_IMAGE_MODE"] != "pull" {
		t.Fatalf("profile mode env = %q", env["KILN_VM_PROFILE_IMAGE_MODE"])
	}
	if env["KILN_VM_PROFILE_IMAGE_TOKEN_FILE"] != "/etc/kiln-vm/profile-image-token" {
		t.Fatalf("profile token file env = %q", env["KILN_VM_PROFILE_IMAGE_TOKEN_FILE"])
	}
}

func TestReadDoctorEnvFileWarnsWhenMissing(t *testing.T) {
	env, check := readDoctorEnvFile(filepath.Join(t.TempDir(), "missing.env"))
	if len(env) != 0 {
		t.Fatalf("env = %#v, want empty", env)
	}
	if check == nil || check.Status != "warn" || !check.Warning {
		t.Fatalf("check = %#v, want warning", check)
	}
	if got := check.Data["daemon_config_authoritative"]; got != "false" {
		t.Fatalf("daemon_config_authoritative = %q, want false", got)
	}
}

func TestCatalogHasProfileArchRequiresCompleteEntry(t *testing.T) {
	catalog := &vmservice.ProfileImageCatalog{
		SchemaVersion: 1,
		Images: []vmservice.ProfileImageCatalogEntry{
			{Profile: "ubuntu", Arch: "amd64", Reference: "https://example.invalid/rootfs.ext4.gz", LayerDigest: "sha256:layer", RootfsSHA256: "sha256:rootfs"},
			{Profile: "ubuntu", Arch: "arm64", Reference: "", LayerDigest: "sha256:layer", RootfsSHA256: "sha256:rootfs"},
		},
	}
	if !catalogHasProfileArch(catalog, "ubuntu", "amd64") {
		t.Fatal("complete amd64 entry was not accepted")
	}
	if catalogHasProfileArch(catalog, "ubuntu", "arm64") {
		t.Fatal("incomplete arm64 entry was accepted")
	}
}

func TestCheckProfileCatalogWarnsForLocalMode(t *testing.T) {
	check := checkProfileCatalog("local", "", doctorProfileTokenState{}, "amd64", doctorConfigAuthority{Source: "explicit", Authoritative: true})
	if check.Status != "warn" || !check.Warning {
		t.Fatalf("check = %#v, want warning", check)
	}
	if !strings.Contains(check.Detail, "trusted profile pulls are disabled") {
		t.Fatalf("detail = %q", check.Detail)
	}
}

func TestCheckProfileCatalogReportsTokenSourceWithoutToken(t *testing.T) {
	catalogPath := writeDoctorCatalog(t, "https://images.example.test/kiln-vm/v0.1.0")
	check := checkProfileCatalog("pull", catalogPath, doctorProfileTokenState{
		Token:  "secret-token",
		Source: "KILN_VM_PROFILE_IMAGE_TOKEN_FILE",
	}, "amd64", doctorConfigAuthority{Source: "explicit", Authoritative: true})
	if check.Status != "ok" {
		t.Fatalf("check = %#v, want ok", check)
	}
	if got := check.Data["token_source"]; got != "KILN_VM_PROFILE_IMAGE_TOKEN_FILE" {
		t.Fatalf("token_source = %q", got)
	}
	if strings.Contains(fmt.Sprint(check), "secret-token") {
		t.Fatalf("check leaked token: %#v", check)
	}
}

func TestCheckProfileImageTokenDoesNotExposeSecret(t *testing.T) {
	check := checkProfileImageToken("KILN_VM_PROFILE_IMAGE_TOKEN_FILE", "/etc/kiln-vm/profile-image-token", nil)
	if check.Status != "ok" {
		t.Fatalf("check = %#v, want ok", check)
	}
	if strings.Contains(fmt.Sprint(check), "secret") {
		t.Fatalf("check leaked token-ish content: %#v", check)
	}
}

func TestCheckProfileImageTokenWarnsWhenOnlyRootCanRead(t *testing.T) {
	withDoctorEffectiveUID(t, 1000)
	check := checkProfileImageToken("KILN_VM_PROFILE_IMAGE_TOKEN_FILE", "/etc/kiln-vm/profile-image-token", os.ErrPermission)
	if check.Status != "warn" || !check.Warning {
		t.Fatalf("check = %#v, want warning", check)
	}
	if !strings.Contains(check.Detail, "root-owned kiln-vmd may still be valid") {
		t.Fatalf("detail = %q", check.Detail)
	}
}

func TestCheckProfileImageTokenFailsWhenRootCannotRead(t *testing.T) {
	withDoctorEffectiveUID(t, 0)
	check := checkProfileImageToken("KILN_VM_PROFILE_IMAGE_TOKEN_FILE", "/etc/kiln-vm/profile-image-token", os.ErrPermission)
	if check.Status != "fail" {
		t.Fatalf("check = %#v, want fail", check)
	}
}

func TestCheckGuestKernelRejectsArm64ELFKernel(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("Executable: %v", err)
	}
	check := checkGuestKernel(executable, filepath.Join(t.TempDir(), "missing.json"), "arm64")
	if check.Status != "fail" {
		t.Fatalf("check = %#v, want fail", check)
	}
	if !strings.Contains(check.Detail, "arm64 Firecracker needs arch/arm64/boot/Image") {
		t.Fatalf("detail = %q", check.Detail)
	}
}

func TestCheckGuestKernelAcceptsMetadataBackedArm64Image(t *testing.T) {
	dir := t.TempDir()
	kernelPath := filepath.Join(dir, "Image")
	kernel := []byte("synthetic arm64 image\x00Linux version 6.1.155-test\x00")
	if err := os.WriteFile(kernelPath, kernel, 0644); err != nil {
		t.Fatalf("WriteFile kernel: %v", err)
	}
	sum := sha256.Sum256(kernel)
	metadata := guestKernelMetadata{
		SchemaVersion:   1,
		Version:         "0.1.70",
		DebArch:         "arm64",
		GoArch:          "arm64",
		LinuxKernelArch: "arm64",
		FirecrackerArch: "aarch64",
		KernelVersion:   "6.1.155",
		SourceURL:       "https://cdn.kernel.org/pub/linux/kernel/v6.x/linux-6.1.155.tar.xz",
		ConfigURL:       "https://kernelconfig.io/config-arm64-6.1.155",
		ConfigDeltas:    []string{"CONFIG_IP_NF_RAW=y", "CONFIG_IP6_NF_RAW=y"},
		BuildTarget:     "Image",
		BuildOutput:     "arch/arm64/boot/Image",
		PackagedPath:    "/usr/lib/kiln-vm/vmlinux",
		SHA256:          hex.EncodeToString(sum[:]),
	}
	metadataBytes, err := json.Marshal(metadata)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	metadataPath := filepath.Join(dir, "guest-kernel.json")
	if err := os.WriteFile(metadataPath, metadataBytes, 0644); err != nil {
		t.Fatalf("WriteFile metadata: %v", err)
	}
	check := checkGuestKernel(kernelPath, metadataPath, "arm64")
	if check.Status != "ok" {
		t.Fatalf("check = %#v, want ok", check)
	}
	if check.Data["metadata_build_output"] != "arch/arm64/boot/Image" {
		t.Fatalf("metadata_build_output = %q", check.Data["metadata_build_output"])
	}
}

func TestCheckGuestKernelFailsWithoutSourceConfigProvenance(t *testing.T) {
	dir := t.TempDir()
	kernelPath := filepath.Join(dir, "Image")
	kernel := []byte("synthetic arm64 image\x00Linux version 6.1.155-test\x00")
	if err := os.WriteFile(kernelPath, kernel, 0644); err != nil {
		t.Fatalf("WriteFile kernel: %v", err)
	}
	sum := sha256.Sum256(kernel)
	metadata := guestKernelMetadata{
		SchemaVersion: 1,
		GoArch:        "arm64",
		KernelVersion: "6.1.155",
		ConfigDeltas:  []string{"CONFIG_IP_NF_RAW=y", "CONFIG_IP6_NF_RAW=y"},
		BuildOutput:   "arch/arm64/boot/Image",
		PackagedPath:  "/usr/lib/kiln-vm/vmlinux",
		SHA256:        hex.EncodeToString(sum[:]),
	}
	metadataBytes, err := json.Marshal(metadata)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	metadataPath := filepath.Join(dir, "guest-kernel.json")
	if err := os.WriteFile(metadataPath, metadataBytes, 0644); err != nil {
		t.Fatalf("WriteFile metadata: %v", err)
	}
	check := checkGuestKernel(kernelPath, metadataPath, "arm64")
	if check.Status != "fail" {
		t.Fatalf("check = %#v, want fail", check)
	}
	if !strings.Contains(check.Detail, "source/config provenance") {
		t.Fatalf("detail = %q", check.Detail)
	}
}

func TestCheckResourceLimitsWarnsWhenNoVMFits(t *testing.T) {
	check := checkResourceLimits(func(key, fallback string) string {
		values := map[string]string{
			"KILN_VM_MAX_RUNNING_VMS":     "1",
			"KILN_VM_MAX_TOTAL_VCPUS":     "0",
			"KILN_VM_MAX_TOTAL_MEMORY_MB": "64",
			"KILN_VM_MAX_VM_DISK_MB":      "65536",
		}
		if value, ok := values[key]; ok {
			return value
		}
		return fallback
	})
	if check.Status != "warn" || !check.Warning {
		t.Fatalf("check = %#v, want warning", check)
	}
	for _, want := range []string{"vCPUs is below 1", "memory is below 128 MiB"} {
		if !strings.Contains(check.Detail, want) {
			t.Fatalf("detail = %q, want %q", check.Detail, want)
		}
	}
}

func TestPrintDoctorChecksIncludesRemediation(t *testing.T) {
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	printDoctorChecks(cmd, []doctorCheck{{
		Name:        "guest_kernel",
		Status:      "fail",
		Detail:      "bad kernel",
		Remediation: "install the packaged guest kernel",
	}})
	got := out.String()
	if !strings.Contains(got, "FAIL") || !strings.Contains(got, "guest_kernel") {
		t.Fatalf("output = %q, want failed guest kernel line", got)
	}
	if !strings.Contains(got, "remediation: install the packaged guest kernel") {
		t.Fatalf("output = %q, want remediation", got)
	}
}

func TestDoctorProfileLibvirtReadinessReportsReadyHost(t *testing.T) {
	mediaPath := filepath.Join(t.TempDir(), "windows.iso")
	if err := os.WriteFile(mediaPath, []byte("synthetic installer media"), 0644); err != nil {
		t.Fatalf("WriteFile media: %v", err)
	}
	profile := doctorTestWindowsProfile()
	withDoctorProfiles(t, []models.VMProfile{profile})
	withDoctorLookPath(t, func(tool string) (string, error) {
		return "/usr/bin/" + tool, nil
	})
	withDoctorCommandOutput(t, func(name string, args ...string) (string, error) {
		joined := strings.Join(args, " ")
		switch {
		case name == "virsh" && strings.Contains(joined, "net-info kiln-vm"):
			return "Name: default\nActive: yes\nAutostart: yes\n", nil
		case name == "virsh" && strings.Contains(joined, "domcapabilities"):
			return "<domainCapabilities><os><loader supported='yes'><value>/usr/share/OVMF/OVMF_CODE.fd</value><enum name='type'><value>pflash</value></enum></loader></os></domainCapabilities>", nil
		default:
			t.Fatalf("unexpected command %s %s", name, joined)
			return "", nil
		}
	})

	checks := runDoctorProfileChecks(doctorOptions{Profile: "windows11", MediaPath: mediaPath}, doctorLookupDefaults, doctorAuthoritativeTest())
	byName := doctorChecksByName(checks)
	for _, name := range []string{
		"profile_windows11_contract",
		"profile_windows11_libvirt_tools",
		"profile_windows11_libvirt_network",
		"profile_windows11_libvirt_firmware",
		"profile_windows11_libvirt_tpm",
		"profile_windows11_input_media_path",
	} {
		check, ok := byName[name]
		if !ok {
			t.Fatalf("missing doctor check %s in %#v", name, checks)
		}
		if check.Status != "ok" {
			t.Fatalf("%s = %#v, want ok", name, check)
		}
	}
	if byName["profile_windows11_libvirt_network"].Data["daemon_config_authoritative"] != "true" {
		t.Fatalf("network check data = %#v, want authoritative marker", byName["profile_windows11_libvirt_network"].Data)
	}
}

func TestDoctorProfileLibvirtToolsReportsMissingTools(t *testing.T) {
	profile := doctorTestWindowsProfile()
	withDoctorLookPath(t, func(tool string) (string, error) {
		if tool == "qemu-img" {
			return "", errors.New("not found")
		}
		return "/usr/bin/" + tool, nil
	})

	check := checkDoctorProfileLibvirtTools(profile)
	if check.Status != "fail" {
		t.Fatalf("check = %#v, want fail", check)
	}
	if !strings.Contains(check.Detail, "qemu-img") || !strings.Contains(check.Data["missing"], "qemu-img") {
		t.Fatalf("check = %#v, want missing qemu-img diagnostic", check)
	}
}

func TestDoctorProfileLibvirtNetworkReportsMissingNetwork(t *testing.T) {
	profile := doctorTestWindowsProfile()
	withDoctorCommandOutput(t, func(name string, args ...string) (string, error) {
		return "", errors.New("network not found")
	})

	check := checkDoctorProfileLibvirtNetwork(profile, "qemu:///system", "missing-net", doctorAuthoritativeTest())
	if check.Status != "fail" {
		t.Fatalf("check = %#v, want fail", check)
	}
	if !strings.Contains(check.Detail, "missing-net") || !strings.Contains(check.Remediation, "net-start") {
		t.Fatalf("check = %#v, want actionable missing-network diagnostic", check)
	}
}

func TestDoctorProfileLibvirtNetworkAcceptsTheManagedNetworkBeforeKilnVMDCreatesIt(t *testing.T) {
	for _, tc := range []struct {
		name   string
		output func(args string) (string, error)
		detail string
	}{
		{name: "missing", detail: "does not exist yet", output: func(args string) (string, error) {
			if strings.Contains(args, "net-list") {
				return "default\n", nil
			}
			return "", errors.New("network not found")
		}},
		{name: "inactive", detail: "is inactive", output: func(string) (string, error) {
			return "Name: kiln-vm\nActive: no\nAutostart: no\n", nil
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withDoctorCommandOutput(t, func(name string, args ...string) (string, error) {
				return tc.output(strings.Join(args, " "))
			})

			check := checkDoctorProfileLibvirtNetwork(doctorTestWindowsProfile(), "qemu:///system", "kiln-vm", doctorAuthoritativeTest())

			if check.Status != "ok" || !strings.Contains(check.Detail, tc.detail) || check.Data["managed"] != "true" {
				t.Fatalf("check = %#v, want ok with %q", check, tc.detail)
			}
		})
	}
}

func TestDoctorProfileLibvirtNetworkFailsWhenLibvirtIsUnreachable(t *testing.T) {
	withDoctorCommandOutput(t, func(name string, args ...string) (string, error) {
		return "", errors.New("failed to connect to the hypervisor")
	})

	check := checkDoctorProfileLibvirtNetwork(doctorTestWindowsProfile(), "qemu:///system", "kiln-vm", doctorAuthoritativeTest())

	if check.Status != "fail" || !strings.Contains(check.Remediation, "Start libvirtd") {
		t.Fatalf("check = %#v, want fail for unreachable libvirt", check)
	}
}

func TestDoctorProfileLibvirtChecksPointUnprivilegedUsersAtSudo(t *testing.T) {
	withDoctorCommandOutput(t, func(name string, args ...string) (string, error) {
		return "", errors.New("error: authentication unavailable: no polkit agent available to authenticate action 'org.libvirt.unix.manage'")
	})

	checks := []doctorCheck{
		checkDoctorProfileLibvirtNetwork(doctorTestWindowsProfile(), "qemu:///system", "kiln-vm", doctorAuthoritativeTest()),
		checkDoctorProfileLibvirtFirmware(doctorTestWindowsProfile(), "qemu:///system", doctorAuthoritativeTest()),
	}
	for _, check := range checks {
		if check.Status != "fail" || !strings.Contains(check.Remediation, "sudo kiln-vm doctor") || strings.Contains(check.Remediation, "Start libvirtd") {
			t.Fatalf("check = %#v, want a fail that points at sudo or the libvirt group", check)
		}
	}
}

func TestDoctorProfileLibvirtFirmwareReportsMissingUEFI(t *testing.T) {
	profile := doctorTestWindowsProfile()
	withDoctorCommandOutput(t, func(name string, args ...string) (string, error) {
		return "<domainCapabilities><os><loader supported='no'></loader></os></domainCapabilities>", nil
	})

	check := checkDoctorProfileLibvirtFirmware(profile, "qemu:///system", doctorAuthoritativeTest())
	if check.Status != "fail" {
		t.Fatalf("check = %#v, want fail", check)
	}
	if !strings.Contains(check.Detail, "UEFI") || !strings.Contains(check.Remediation, "OVMF") {
		t.Fatalf("check = %#v, want UEFI/OVMF diagnostic", check)
	}
}

func TestDoctorProfileContractReportsWindowsAccessAndReadiness(t *testing.T) {
	check := checkDoctorProfileContract(doctorTestWindowsProfile())
	if check.Status != "ok" || check.Warning {
		t.Fatalf("check = %#v, want ok", check)
	}
	if check.Data["access"] != string(models.VMProfileAccessSSH) || check.Data["readiness"] != string(models.VMProfileReadinessSSHExec) {
		t.Fatalf("contract data = %#v, want access and readiness surfaced", check.Data)
	}
}

func TestDoctorProfilePreparationInputReportsInaccessibleMedia(t *testing.T) {
	profile := doctorTestWindowsProfile()
	checks := checkDoctorProfilePreparationInputs(profile, doctorOptions{MediaPath: filepath.Join(t.TempDir(), "missing.iso")})
	if len(checks) != 1 {
		t.Fatalf("checks = %#v, want one media check", checks)
	}
	check := checks[0]
	if check.Status != "fail" {
		t.Fatalf("check = %#v, want fail", check)
	}
	if !strings.Contains(check.Detail, "media_path") || !strings.Contains(check.Remediation, "readable non-empty") {
		t.Fatalf("check = %#v, want inaccessible media diagnostic", check)
	}
}

func writeDoctorCatalog(t *testing.T, baseURL string) string {
	t.Helper()
	catalog := vmservice.ProfileImageCatalog{SchemaVersion: 1}
	for _, profile := range vmservice.StandardPublishedProfileNames() {
		catalog.Images = append(catalog.Images, vmservice.ProfileImageCatalogEntry{
			Profile:        profile,
			ProfileVersion: 1,
			Fingerprint:    "fingerprint",
			Arch:           "amd64",
			RootfsSizeMB:   128,
			Reference:      baseURL + "/kiln-vm-profile-" + profile + "-v0.1.70-amd64-128mb-rootfs.ext4.gz",
			LayerDigest:    "sha256:layer",
			RootfsSHA256:   "sha256:rootfs",
			Source:         "https",
		})
	}
	data, err := json.Marshal(catalog)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	path := filepath.Join(t.TempDir(), "profile-images.json")
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

func withDoctorEffectiveUID(t *testing.T, uid int) {
	t.Helper()
	previous := doctorEffectiveUID
	doctorEffectiveUID = func() int { return uid }
	t.Cleanup(func() {
		doctorEffectiveUID = previous
	})
}

func withDoctorLookPath(t *testing.T, fn func(string) (string, error)) {
	t.Helper()
	previous := doctorLookPath
	doctorLookPath = fn
	t.Cleanup(func() {
		doctorLookPath = previous
	})
}

func withDoctorCommandOutput(t *testing.T, fn func(string, ...string) (string, error)) {
	t.Helper()
	previous := doctorCommandOutput
	doctorCommandOutput = fn
	t.Cleanup(func() {
		doctorCommandOutput = previous
	})
}

func withDoctorProfiles(t *testing.T, profiles []models.VMProfile) {
	t.Helper()
	previous := doctorListProfiles
	doctorListProfiles = func() []models.VMProfile {
		return append([]models.VMProfile(nil), profiles...)
	}
	t.Cleanup(func() {
		doctorListProfiles = previous
	})
}

func doctorTestWindowsProfile() models.VMProfile {
	return models.VMProfile{
		Name:                "windows11",
		Backend:             "libvirt-qemu",
		GuestOS:             "windows",
		Libvirt:             libvirtdomain.Windows11(),
		Networking:          models.VMProfileNetworking{Public: models.VMProfilePublicNetwork{Kind: models.VMProfilePublicNetworkLibvirtNAT}},
		Access:              models.VMProfileAccess{Kind: models.VMProfileAccessSSH, User: "kiln-vm"},
		Readiness:           models.VMProfileReadiness{Kind: models.VMProfileReadinessSSHExec},
		Preparation:         models.VMProfilePreparation{Kind: models.VMProfilePreparationWindowsUnattended, RequiredInputs: []string{"media_path"}, ImageBootMode: "bootable-qcow2"},
		RequiresPreparation: true,
	}
}

func doctorLookupDefaults(key, fallback string) string {
	values := map[string]string{
		"KILN_VM_LIBVIRT_CONNECT_URI": "qemu:///system",
		"KILN_VM_LIBVIRT_NETWORK":     "kiln-vm",
	}
	if value, ok := values[key]; ok {
		return value
	}
	return fallback
}

func doctorAuthoritativeTest() doctorConfigAuthority {
	return doctorConfigAuthority{Source: "env_file", Authoritative: true, Detail: "service environment file was readable"}
}

func doctorChecksByName(checks []doctorCheck) map[string]doctorCheck {
	result := map[string]doctorCheck{}
	for _, check := range checks {
		result[check.Name] = check
	}
	return result
}
