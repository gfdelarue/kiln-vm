package cli

import (
	"crypto/sha256"
	"debug/elf"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"

	"github.com/gfdelarue/kiln-vm/internal/config"
	"github.com/gfdelarue/kiln-vm/internal/version"
	vmservice "github.com/gfdelarue/kiln-vm/internal/vm"
)

var doctorEffectiveUID = os.Geteuid

type doctorOptions struct {
	EnvFile   string
	Profile   string
	MediaPath string
}

type doctorCheck struct {
	Name        string            `json:"name"`
	Status      string            `json:"status"`
	Detail      string            `json:"detail"`
	Warning     bool              `json:"warning,omitempty"`
	Remediation string            `json:"remediation,omitempty"`
	Data        map[string]string `json:"data,omitempty"`
}

type doctorConfigAuthority struct {
	Source        string
	Authoritative bool
	Detail        string
}

type doctorProfileTokenState struct {
	Token  string
	Source string
	File   string
	Err    error
}

var doctorCmd = newDoctorCommand("doctor")

func newDoctorCommand(use string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   use,
		Short: "Check local kiln-vm host/package prerequisites",
		RunE: func(cmd *cobra.Command, args []string) error {
			envFile, _ := cmd.Flags().GetString("env-file")
			profile, _ := cmd.Flags().GetString("profile")
			mediaPath, _ := cmd.Flags().GetString("media")
			jsonOutput, _ := cmd.Flags().GetBool("json")
			checks := runDoctorChecksWithOptions(doctorOptions{
				EnvFile:   envFile,
				Profile:   profile,
				MediaPath: mediaPath,
			})
			if jsonOutput {
				encoder := json.NewEncoder(cmd.OutOrStdout())
				encoder.SetIndent("", "  ")
				if err := encoder.Encode(checks); err != nil {
					return fmt.Errorf("encode doctor checks: %w", err)
				}
			} else {
				printDoctorChecks(cmd, checks)
			}
			for _, check := range checks {
				if check.Status == "fail" {
					return fmt.Errorf("doctor found failed checks")
				}
			}
			return nil
		},
	}
	cmd.Flags().String("env-file", "/etc/kiln-vm/kiln-vmd.env", "kiln-vm service environment file to inspect")
	cmd.Flags().String("profile", "", "Limit profile readiness checks to one VM profile")
	cmd.Flags().String("media", "", "Installer media path to validate for the selected profile")
	cmd.Flags().Bool("json", false, "Emit checks as JSON")
	return cmd
}

func runDoctorChecksWithOptions(options doctorOptions) []doctorCheck {
	envFile := options.EnvFile
	if strings.TrimSpace(envFile) == "" {
		envFile = "/etc/kiln-vm/kiln-vmd.env"
	}
	env, envCheck := readDoctorEnvFile(envFile)
	configAuthority := doctorConfigAuthorityFromEnvCheck(envCheck)
	lookup := func(key, fallback string) string {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			return value
		}
		if value := strings.TrimSpace(env[key]); value != "" {
			return value
		}
		return fallback
	}

	checks := []doctorCheck{}
	if envCheck != nil {
		checks = append(checks, *envCheck)
	}
	checks = append(checks, doctorCheck{
		Name:   "cli_version",
		Status: "ok",
		Detail: version.Version,
		Data: map[string]string{
			"version": version.Version,
		},
	})
	checks = append(checks, checkDaemonBinary("kiln-vmd"))
	checks = append(checks, checkHostArch())
	checks = append(checks, checkKVMAccess("/dev/kvm"))
	checks = append(checks, checkRequiredHostTools())

	firecrackerBin := lookup("KILN_VM_FIRECRACKER_BIN", config.DefaultFirecrackerBin)
	checks = append(checks, checkExecutable("firecracker", firecrackerBin, true))
	checks = append(checks, checkGuestKernel(
		lookup("KILN_VM_KERNEL_PATH", config.DefaultKernelPath),
		lookup("KILN_VM_GUEST_KERNEL_METADATA", "/usr/share/kiln-vm/guest-kernel.json"),
		runtime.GOARCH,
	))

	agentBin := lookup("KILN_VM_AGENT_BIN", config.DefaultAgentBinPath)
	checks = append(checks, checkELFArchitecture("kiln_vm_agent", agentBin, runtime.GOARCH))

	mode := lookup("KILN_VM_PROFILE_IMAGE_MODE", "auto")
	catalog := lookup("KILN_VM_PROFILE_IMAGE_CATALOG", "/usr/share/kiln-vm/profile-images.json")
	tokenValue := lookup("KILN_VM_PROFILE_IMAGE_TOKEN", "")
	tokenFile := lookup("KILN_VM_PROFILE_IMAGE_TOKEN_FILE", "")
	token, tokenSource, tokenErr := config.ResolveProfileImageToken(tokenValue, tokenFile)
	tokenState := doctorProfileTokenState{Token: token, Source: tokenSource, File: tokenFile, Err: tokenErr}
	if tokenValue != "" || tokenFile != "" || tokenErr != nil {
		checks = append(checks, checkProfileImageToken(tokenSource, tokenFile, tokenErr))
	}
	checks = append(checks, checkProfileCatalog(mode, catalog, tokenState, runtime.GOARCH, configAuthority))
	checks = append(checks, checkResourceLimits(lookup))
	checks = append(checks, checkHostResources(lookup("KILN_VM_DATA_DIR", "/var/lib/kiln-vm")))
	checks = append(checks, runDoctorProfileChecks(options, lookup, configAuthority)...)
	return checks
}

func doctorConfigAuthorityFromEnvCheck(envCheck *doctorCheck) doctorConfigAuthority {
	if envCheck == nil || envCheck.Status == "ok" {
		return doctorConfigAuthority{
			Source:        "env_file",
			Authoritative: true,
			Detail:        "service environment file was readable",
		}
	}
	return doctorConfigAuthority{
		Source:        "process_env_and_packaged_defaults",
		Authoritative: false,
		Detail:        envCheck.Detail,
	}
}

func (authority doctorConfigAuthority) annotate(data map[string]string) map[string]string {
	if data == nil {
		data = map[string]string{}
	}
	data["config_source"] = authority.Source
	if authority.Authoritative {
		data["daemon_config_authoritative"] = "true"
	} else {
		data["daemon_config_authoritative"] = "false"
	}
	return data
}

func readDoctorEnvFile(path string) (map[string]string, *doctorCheck) {
	values := map[string]string{}
	data, err := os.ReadFile(path)
	if err != nil {
		data := map[string]string{
			"path":                        path,
			"config_source":               "process_env_and_packaged_defaults",
			"daemon_config_authoritative": "false",
		}
		if os.IsNotExist(err) {
			return values, &doctorCheck{
				Name:    "env_file",
				Status:  "warn",
				Warning: true,
				Detail:  fmt.Sprintf("%s is missing; config-dependent checks use process environment and packaged defaults and are not daemon-authoritative", path),
				Data:    data,
			}
		}
		return values, &doctorCheck{
			Name:    "env_file",
			Status:  "warn",
			Warning: true,
			Detail:  fmt.Sprintf("%s is not readable by current user; config-dependent checks use process environment and packaged defaults and are not daemon-authoritative: %v", path, err),
			Data:    data,
		}
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		values[strings.TrimSpace(key)] = strings.Trim(strings.TrimSpace(value), `"'`)
	}
	return values, &doctorCheck{
		Name:   "env_file",
		Status: "ok",
		Detail: path,
		Data: map[string]string{
			"path":                        path,
			"config_source":               "env_file",
			"daemon_config_authoritative": "true",
		},
	}
}

func checkHostArch() doctorCheck {
	switch runtime.GOARCH {
	case "amd64", "arm64":
		return doctorCheck{Name: "host_arch", Status: "ok", Detail: runtime.GOARCH}
	default:
		return doctorCheck{Name: "host_arch", Status: "fail", Detail: fmt.Sprintf("unsupported host architecture %s", runtime.GOARCH)}
	}
}

func checkKVMAccess(path string) doctorCheck {
	if _, err := os.Stat(path); err != nil {
		return doctorCheck{Name: "kvm", Status: "fail", Detail: fmt.Sprintf("%s is missing: %v", path, err)}
	}
	if err := unix.Access(path, unix.R_OK|unix.W_OK); err != nil {
		if doctorEffectiveUID() != 0 {
			return doctorCheck{Name: "kvm", Status: "warn", Warning: true, Detail: fmt.Sprintf("current user cannot read/write %s; root-owned kiln-vmd can still be valid", path)}
		}
		return doctorCheck{Name: "kvm", Status: "fail", Detail: fmt.Sprintf("root cannot read/write %s: %v", path, err)}
	}
	return doctorCheck{Name: "kvm", Status: "ok", Detail: path}
}

func checkDaemonBinary(name string) doctorCheck {
	path, err := exec.LookPath(name)
	if err != nil {
		return doctorCheck{
			Name:        "daemon_binary",
			Status:      "warn",
			Warning:     true,
			Detail:      "kiln-vmd was not found in PATH",
			Remediation: "On apt-installed hosts, /usr/bin/kiln-vmd should be installed with the kiln-vm package.",
		}
	}
	out, err := exec.Command(path, "--version").CombinedOutput()
	if err != nil {
		return doctorCheck{
			Name:        "daemon_binary",
			Status:      "fail",
			Detail:      fmt.Sprintf("%s --version failed: %s: %v", path, strings.TrimSpace(string(out)), err),
			Remediation: "Reinstall the kiln-vm package so the daemon binary matches the CLI release.",
		}
	}
	return doctorCheck{
		Name:   "daemon_binary",
		Status: "ok",
		Detail: strings.TrimSpace(string(out)),
		Data: map[string]string{
			"path": path,
		},
	}
}

func checkRequiredHostTools() doctorCheck {
	tools := []string{"ip", "iptables", "resize2fs", "tar", "qemu-img", "virt-install", "virsh", "mcopy", "ssh", "ssh-keygen"}
	missing := []string{}
	found := []string{}
	for _, tool := range tools {
		if path, err := doctorLookPath(tool); err == nil {
			found = append(found, tool+"="+path)
		} else {
			missing = append(missing, tool)
		}
	}
	if len(missing) > 0 {
		return doctorCheck{
			Name:        "host_tools",
			Status:      "fail",
			Detail:      "missing required tools: " + strings.Join(missing, ", "),
			Remediation: "Install the kiln-vm Debian package dependencies or the NixOS kiln-vmd module package set.",
			Data: map[string]string{
				"missing": strings.Join(missing, ","),
				"found":   strings.Join(found, ","),
			},
		}
	}
	return doctorCheck{
		Name:   "host_tools",
		Status: "ok",
		Detail: strings.Join(found, ", "),
		Data: map[string]string{
			"found": strings.Join(found, ","),
		},
	}
}

func checkExecutable(name, configured string, runVersion bool) doctorCheck {
	path, err := resolveExecutable(configured)
	if err != nil {
		return doctorCheck{Name: name, Status: "fail", Detail: err.Error()}
	}
	if runVersion {
		out, err := exec.Command(path, "--version").CombinedOutput()
		if err != nil {
			return doctorCheck{Name: name, Status: "fail", Detail: fmt.Sprintf("%s --version failed: %s: %v", path, out, err)}
		}
		detail := strings.TrimSpace(string(out))
		if detail == "" {
			detail = path
		}
		return doctorCheck{Name: name, Status: "ok", Detail: detail}
	}
	return doctorCheck{Name: name, Status: "ok", Detail: path}
}

func resolveExecutable(configured string) (string, error) {
	if strings.TrimSpace(configured) == "" {
		return "", fmt.Errorf("path is empty")
	}
	if filepath.IsAbs(configured) {
		info, err := os.Stat(configured)
		if err != nil {
			return "", fmt.Errorf("%s is not available: %w", configured, err)
		}
		if info.IsDir() || info.Mode()&0111 == 0 {
			return "", fmt.Errorf("%s is not executable", configured)
		}
		return configured, nil
	}
	path, err := doctorLookPath(configured)
	if err != nil {
		return "", fmt.Errorf("%s was not found in PATH", configured)
	}
	return path, nil
}

type guestKernelMetadata struct {
	SchemaVersion   int      `json:"schema_version"`
	Version         string   `json:"kiln_vm_version"`
	DebArch         string   `json:"deb_arch"`
	GoArch          string   `json:"go_arch"`
	LinuxKernelArch string   `json:"linux_kernel_arch"`
	FirecrackerArch string   `json:"firecracker_arch"`
	KernelVersion   string   `json:"kernel_version"`
	SourceURL       string   `json:"source_url"`
	ConfigURL       string   `json:"config_url"`
	ConfigDeltas    []string `json:"config_deltas"`
	BuildTarget     string   `json:"build_target"`
	BuildOutput     string   `json:"build_output"`
	PackagedPath    string   `json:"packaged_path"`
	SHA256          string   `json:"sha256"`
}

func checkGuestKernel(kernelPath, metadataPath, arch string) doctorCheck {
	info, err := os.Stat(kernelPath)
	if err != nil {
		return doctorCheck{
			Name:        "guest_kernel",
			Status:      "fail",
			Detail:      fmt.Sprintf("%s is not available: %v", kernelPath, err),
			Remediation: "Install the official kiln-vm package or configure KILN_VM_KERNEL_PATH to a Firecracker-compatible guest kernel.",
		}
	}
	if info.IsDir() || info.Size() == 0 {
		return doctorCheck{
			Name:        "guest_kernel",
			Status:      "fail",
			Detail:      fmt.Sprintf("%s is not a non-empty file", kernelPath),
			Remediation: "Configure KILN_VM_KERNEL_PATH to the packaged /usr/lib/kiln-vm/vmlinux artifact.",
		}
	}

	kind, kindErr := detectGuestKernelKind(kernelPath, arch)
	if kindErr != nil {
		return doctorCheck{
			Name:        "guest_kernel",
			Status:      "fail",
			Detail:      kindErr.Error(),
			Remediation: "Use the kiln-vm-built guest kernel from the official package. On ARM64 this must be arch/arm64/boot/Image, not ELF vmlinux.",
		}
	}
	kernelSHA := sha256FileForDoctor(kernelPath)
	kernelVersion := probeKernelVersion(kernelPath)

	metadata, metadataErr := readGuestKernelMetadata(metadataPath)
	data := map[string]string{
		"path":    kernelPath,
		"kind":    kind,
		"sha256":  kernelSHA,
		"version": kernelVersion,
	}
	if metadataErr != nil {
		return doctorCheck{
			Name:        "guest_kernel",
			Status:      "warn",
			Warning:     true,
			Detail:      fmt.Sprintf("%s kind=%s version=%s sha256=%s; metadata unavailable: %v", kernelPath, kind, withDefaultString(kernelVersion, "unknown"), shortSHA(kernelSHA), metadataErr),
			Remediation: "Official apt packages should include /usr/share/kiln-vm/guest-kernel.json so kernel provenance is auditable.",
			Data:        data,
		}
	}
	data["metadata_path"] = metadataPath
	data["metadata_version"] = metadata.Version
	data["metadata_deb_arch"] = metadata.DebArch
	data["metadata_kernel_version"] = metadata.KernelVersion
	data["metadata_source_url"] = metadata.SourceURL
	data["metadata_config_url"] = metadata.ConfigURL
	data["metadata_build_output"] = metadata.BuildOutput
	data["metadata_config_deltas"] = strings.Join(metadata.ConfigDeltas, ",")
	if strings.TrimSpace(metadata.SourceURL) == "" || strings.TrimSpace(metadata.ConfigURL) == "" {
		return doctorCheck{
			Name:        "guest_kernel",
			Status:      "fail",
			Detail:      fmt.Sprintf("%s metadata is missing source/config provenance", metadataPath),
			Remediation: "Rebuild/reinstall kiln-vm from a release that records guest kernel source and config URLs.",
			Data:        data,
		}
	}
	if metadata.SHA256 != "" && metadata.SHA256 != kernelSHA {
		return doctorCheck{
			Name:        "guest_kernel",
			Status:      "fail",
			Detail:      fmt.Sprintf("%s sha256=%s does not match metadata %s sha256=%s", kernelPath, shortSHA(kernelSHA), metadataPath, shortSHA(metadata.SHA256)),
			Remediation: "Reinstall the kiln-vm package or remove stale KILN_VM_KERNEL_PATH overrides.",
			Data:        data,
		}
	}
	if metadata.GoArch != "" && metadata.GoArch != arch {
		return doctorCheck{
			Name:        "guest_kernel",
			Status:      "fail",
			Detail:      fmt.Sprintf("%s metadata go_arch=%s does not match host arch=%s", metadataPath, metadata.GoArch, arch),
			Remediation: "Install the kiln-vm package matching this host architecture.",
			Data:        data,
		}
	}
	for _, required := range []string{"CONFIG_IP_NF_RAW=y", "CONFIG_IP6_NF_RAW=y"} {
		if !containsString(metadata.ConfigDeltas, required) {
			return doctorCheck{
				Name:        "guest_kernel",
				Status:      "fail",
				Detail:      fmt.Sprintf("%s metadata is missing required Docker networking kernel delta %s", metadataPath, required),
				Remediation: "Rebuild/reinstall kiln-vm with the packaged guest kernel produced by scripts/build-guest-kernel.mjs.",
				Data:        data,
			}
		}
	}

	return doctorCheck{
		Name:   "guest_kernel",
		Status: "ok",
		Detail: fmt.Sprintf("%s kind=%s version=%s sha256=%s metadata=%s", kernelPath, kind, withDefaultString(kernelVersion, metadata.KernelVersion), shortSHA(kernelSHA), metadataPath),
		Data:   data,
	}
}

func detectGuestKernelKind(kernelPath, arch string) (string, error) {
	file, err := elf.Open(kernelPath)
	if err == nil {
		defer file.Close()
		if arch == "arm64" {
			return "", fmt.Errorf("%s is an ELF %s kernel artifact; arm64 Firecracker needs arch/arm64/boot/Image", kernelPath, file.FileHeader.Machine)
		}
		expected, ok := elfMachineForDoctorArch(arch)
		if ok && file.FileHeader.Machine != expected {
			return "", fmt.Errorf("%s is ELF %s, want %s for %s", kernelPath, file.FileHeader.Machine, expected, arch)
		}
		return "elf:" + file.FileHeader.Machine.String(), nil
	}
	header := make([]byte, 2)
	f, openErr := os.Open(kernelPath)
	if openErr != nil {
		return "", fmt.Errorf("open kernel artifact %q: %w", kernelPath, openErr)
	}
	defer f.Close()
	if _, readErr := io.ReadFull(f, header); readErr != nil {
		return "", fmt.Errorf("read kernel header: %w", readErr)
	}
	if header[0] == 0x1f && header[1] == 0x8b {
		return "", fmt.Errorf("%s looks gzip-compressed; configure KILN_VM_KERNEL_PATH to the unpacked Firecracker kernel image", kernelPath)
	}
	if arch == "arm64" {
		return "arm64-image", nil
	}
	return "raw", nil
}

func readGuestKernelMetadata(path string) (*guestKernelMetadata, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read guest kernel metadata %q: %w", path, err)
	}
	var metadata guestKernelMetadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		return nil, fmt.Errorf("parse guest kernel metadata %q: %w", path, err)
	}
	if metadata.SchemaVersion != 1 {
		return nil, fmt.Errorf("unsupported schema_version %d", metadata.SchemaVersion)
	}
	return &metadata, nil
}

func probeKernelVersion(path string) string {
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 64<<20))
	if err != nil {
		return ""
	}
	match := regexp.MustCompile(`Linux version ([^\x00\s]+)`).FindSubmatch(data)
	if len(match) != 2 {
		return ""
	}
	return string(match[1])
}

func checkELFArchitecture(name, path, arch string) doctorCheck {
	file, err := elf.Open(path)
	if err != nil {
		return doctorCheck{Name: name, Status: "fail", Detail: fmt.Sprintf("%s must be an ELF executable: %v", path, err)}
	}
	defer file.Close()

	expected, ok := elfMachineForDoctorArch(arch)
	if !ok {
		return doctorCheck{Name: name, Status: "fail", Detail: fmt.Sprintf("unsupported architecture %s", arch)}
	}
	if file.FileHeader.Machine != expected {
		return doctorCheck{Name: name, Status: "fail", Detail: fmt.Sprintf("%s is %s, want %s for %s", path, file.FileHeader.Machine, expected, arch)}
	}
	return doctorCheck{Name: name, Status: "ok", Detail: path}
}

func elfMachineForDoctorArch(arch string) (elf.Machine, bool) {
	switch arch {
	case "amd64", "x86_64":
		return elf.EM_X86_64, true
	case "arm64", "aarch64":
		return elf.EM_AARCH64, true
	default:
		return 0, false
	}
}

func checkProfileImageToken(source, tokenFile string, err error) doctorCheck {
	data := map[string]string{}
	if source != "" {
		data["source"] = source
	}
	if strings.TrimSpace(tokenFile) != "" {
		data["token_file"] = tokenFile
	}
	if err != nil {
		if strings.TrimSpace(tokenFile) != "" && doctorEffectiveUID() != 0 && os.IsPermission(err) {
			return doctorCheck{
				Name:        "profile_image_token",
				Status:      "warn",
				Warning:     true,
				Detail:      fmt.Sprintf("current user cannot read KILN_VM_PROFILE_IMAGE_TOKEN_FILE %s; root-owned kiln-vmd may still be valid: %v", tokenFile, err),
				Remediation: "Run `sudo kiln-vm doctor` for an authoritative token check, or make the token file readable by the daemon user only and keep using the root-owned service path.",
				Data:        data,
			}
		}
		return doctorCheck{
			Name:        "profile_image_token",
			Status:      "fail",
			Detail:      fmt.Sprintf("cannot read KILN_VM_PROFILE_IMAGE_TOKEN_FILE %s: %v", tokenFile, err),
			Remediation: "Write a root-readable profile image token file or remove KILN_VM_PROFILE_IMAGE_TOKEN_FILE when profile artifacts do not require private auth.",
			Data:        data,
		}
	}
	if source == "" {
		return doctorCheck{
			Name:        "profile_image_token",
			Status:      "warn",
			Warning:     true,
			Detail:      "profile image token configuration is present but empty",
			Remediation: "Write a non-empty token to KILN_VM_PROFILE_IMAGE_TOKEN_FILE or remove the empty token configuration.",
			Data:        data,
		}
	}
	return doctorCheck{
		Name:   "profile_image_token",
		Status: "ok",
		Detail: fmt.Sprintf("profile image token loaded from %s", source),
		Data:   data,
	}
}

func checkProfileCatalog(mode, location string, tokenState doctorProfileTokenState, arch string, authority doctorConfigAuthority) doctorCheck {
	if mode == vmservice.ProfileImageModeLocal {
		return doctorCheck{
			Name:        "profile_catalog",
			Status:      "warn",
			Warning:     true,
			Detail:      "local profile image mode; trusted profile pulls are disabled",
			Remediation: "Use KILN_VM_PROFILE_IMAGE_MODE=auto or pull with a trusted profile catalog on production-like hosts.",
			Data: authority.annotate(map[string]string{
				"mode": mode,
			}),
		}
	}
	catalog, err := vmservice.LoadProfileImageCatalogWithToken(location, tokenState.Token)
	if err != nil {
		data := authority.annotate(map[string]string{
			"mode":     mode,
			"location": location,
		})
		if !authority.Authoritative {
			return doctorCheck{
				Name:        "profile_catalog",
				Status:      "warn",
				Warning:     true,
				Detail:      fmt.Sprintf("profile catalog check is not daemon-authoritative because %s; attempted %s: %v", authority.Detail, location, err),
				Remediation: "Run `sudo kiln-vm doctor` or provide a readable --env-file before treating profile catalog findings as daemon-effective.",
				Data:        data,
			}
		}
		return doctorCheck{
			Name:        "profile_catalog",
			Status:      "fail",
			Detail:      err.Error(),
			Remediation: "Point KILN_VM_PROFILE_IMAGE_CATALOG at a readable local catalog or configured private artifact catalog.",
			Data:        data,
		}
	}
	missing := []string{}
	for _, profile := range vmservice.StandardPublishedProfileNames() {
		if !catalogHasProfileArch(catalog, profile, arch) {
			missing = append(missing, profile)
		}
	}
	if len(missing) > 0 {
		return doctorCheck{
			Name:        "profile_catalog",
			Status:      "fail",
			Detail:      fmt.Sprintf("%s is missing %s entries for: %s", location, arch, strings.Join(missing, ", ")),
			Remediation: "Publish/install a kiln-vm profile catalog that includes all standard profiles for this host architecture.",
			Data: authority.annotate(map[string]string{
				"mode":     mode,
				"location": location,
				"arch":     arch,
				"missing":  strings.Join(missing, ","),
			}),
		}
	}
	profiles := catalogProfilesForArch(catalog, arch)
	data := map[string]string{
		"mode":     mode,
		"location": location,
		"arch":     arch,
		"profiles": strings.Join(profiles, ","),
	}
	if tokenState.Source != "" {
		data["token_source"] = tokenState.Source
	}
	return doctorCheck{
		Name:   "profile_catalog",
		Status: "ok",
		Detail: fmt.Sprintf("%s mode=%s has %s entries for: %s", location, mode, arch, strings.Join(profiles, ", ")),
		Data:   authority.annotate(data),
	}
}

func catalogHasProfileArch(catalog *vmservice.ProfileImageCatalog, profile, arch string) bool {
	if catalog == nil {
		return false
	}
	for _, entry := range catalog.Images {
		if entry.Profile == profile && entry.Arch == arch && entry.Reference != "" && entry.LayerDigest != "" && entry.RootfsSHA256 != "" {
			return true
		}
	}
	return false
}

func catalogProfilesForArch(catalog *vmservice.ProfileImageCatalog, arch string) []string {
	seen := map[string]bool{}
	for _, entry := range catalog.Images {
		if entry.Arch == arch {
			seen[entry.Profile] = true
		}
	}
	profiles := make([]string, 0, len(seen))
	for profile := range seen {
		profiles = append(profiles, profile)
	}
	sort.Strings(profiles)
	return profiles
}

func checkResourceLimits(lookup func(string, string) string) doctorCheck {
	maxRunning := parseDoctorInt(lookup("KILN_VM_MAX_RUNNING_VMS", "8"))
	maxVCPUs := parseDoctorInt(lookup("KILN_VM_MAX_TOTAL_VCPUS", "8"))
	maxMemory := parseDoctorInt(lookup("KILN_VM_MAX_TOTAL_MEMORY_MB", "8192"))
	maxDisk := parseDoctorInt64(lookup("KILN_VM_MAX_VM_DISK_MB", strconv.FormatInt(config.DefaultMaxVMDiskMB, 10)))
	data := map[string]string{
		"max_running_vms":     strconv.Itoa(maxRunning),
		"max_total_vcpus":     strconv.Itoa(maxVCPUs),
		"max_total_memory_mb": strconv.Itoa(maxMemory),
		"max_vm_disk_mb":      strconv.FormatInt(maxDisk, 10),
	}
	warnings := []string{}
	if maxRunning < 1 {
		warnings = append(warnings, "max running VMs is below 1")
	}
	if maxVCPUs < 1 {
		warnings = append(warnings, "max total vCPUs is below 1")
	}
	if maxMemory < 128 {
		warnings = append(warnings, "max total memory is below 128 MiB")
	}
	detail := fmt.Sprintf("max_running_vms=%d max_total_vcpus=%d max_total_memory_mb=%d max_vm_disk_mb=%d", maxRunning, maxVCPUs, maxMemory, maxDisk)
	if len(warnings) > 0 {
		return doctorCheck{
			Name:        "resource_limits",
			Status:      "warn",
			Warning:     true,
			Detail:      detail + "; " + strings.Join(warnings, "; "),
			Remediation: "Raise the KILN_VM_MAX_* limits so at least one VM fits.",
			Data:        data,
		}
	}
	return doctorCheck{Name: "resource_limits", Status: "ok", Detail: detail, Data: data}
}

func checkHostResources(dataDir string) doctorCheck {
	diskAvailable, diskPath, diskErr := doctorDiskAvailable(dataDir)
	memAvailable := doctorMemAvailableBytes()
	data := map[string]string{
		"data_dir": dataDir,
	}
	details := []string{}
	warnings := []string{}
	if diskErr != nil {
		warnings = append(warnings, "disk availability unknown: "+diskErr.Error())
	} else {
		data["disk_path"] = diskPath
		data["disk_available_bytes"] = strconv.FormatUint(diskAvailable, 10)
		details = append(details, fmt.Sprintf("disk_available=%s at %s", formatBytes(diskAvailable), diskPath))
		if diskAvailable < 64*1024*1024*1024 {
			warnings = append(warnings, "less than 64GiB available for kiln-vm state")
		}
	}
	if memAvailable > 0 {
		data["mem_available_bytes"] = strconv.FormatUint(memAvailable, 10)
		details = append(details, "mem_available="+formatBytes(memAvailable))
		if memAvailable < 8*1024*1024*1024 {
			warnings = append(warnings, "less than 8GiB available memory")
		}
	}
	if len(details) == 0 {
		details = append(details, "host resource availability unknown")
	}
	if len(warnings) > 0 {
		return doctorCheck{
			Name:        "host_resources",
			Status:      "warn",
			Warning:     true,
			Detail:      strings.Join(details, " ") + "; " + strings.Join(warnings, "; "),
			Remediation: "Free host disk or memory, or create smaller VMs.",
			Data:        data,
		}
	}
	return doctorCheck{Name: "host_resources", Status: "ok", Detail: strings.Join(details, " "), Data: data}
}

func doctorDiskAvailable(dataDir string) (uint64, string, error) {
	path := dataDir
	for {
		if _, err := os.Stat(path); err == nil {
			var stat unix.Statfs_t
			if err := unix.Statfs(path, &stat); err != nil {
				return 0, path, fmt.Errorf("statfs %q: %w", path, err)
			}
			return stat.Bavail * uint64(stat.Bsize), path, nil
		}
		parent := filepath.Dir(path)
		if parent == path {
			return 0, path, fmt.Errorf("no existing parent for %s", dataDir)
		}
		path = parent
	}
}

func doctorMemAvailableBytes() uint64 {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "MemAvailable:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0
		}
		kb, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return 0
		}
		return kb * 1024
	}
	return 0
}

func parseDoctorInt(value string) int {
	parsed, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return 0
	}
	return parsed
}

func parseDoctorInt64(value string) int64 {
	parsed, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil {
		return 0
	}
	return parsed
}

func sha256FileForDoctor(filePath string) string {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func shortSHA(value string) string {
	if len(value) <= 12 {
		return value
	}
	return value[:12]
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func withDefaultString(value, fallback string) string {
	if strings.TrimSpace(value) != "" {
		return value
	}
	return fallback
}

func printDoctorChecks(cmd *cobra.Command, checks []doctorCheck) {
	fmt.Fprintln(cmd.OutOrStdout(), "kiln-vm doctor")
	for _, check := range checks {
		label := strings.ToUpper(check.Status)
		fmt.Fprintf(cmd.OutOrStdout(), "%-5s %-16s %s\n", label, check.Name, check.Detail)
		if strings.TrimSpace(check.Remediation) != "" {
			fmt.Fprintf(cmd.OutOrStdout(), "      remediation: %s\n", check.Remediation)
		}
	}
}
