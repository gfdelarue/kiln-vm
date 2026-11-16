package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gfdelarue/kiln-vm/internal/config"
	"github.com/gfdelarue/kiln-vm/internal/libvirtnet"
	"github.com/gfdelarue/kiln-vm/internal/models"
	vmservice "github.com/gfdelarue/kiln-vm/internal/vm"
)

var (
	doctorLookPath      = exec.LookPath
	doctorCommandOutput = runDoctorCommandOutput
	doctorListProfiles  = vmservice.ListProfiles
)

func runDoctorProfileChecks(options doctorOptions, lookup func(string, string) string, authority doctorConfigAuthority) []doctorCheck {
	profiles, err := doctorProfilesForOptions(options)
	if err != nil {
		return []doctorCheck{{
			Name:        doctorProfileCheckName(options.Profile, "contract"),
			Status:      "fail",
			Detail:      err.Error(),
			Remediation: "Run `kiln-vm profiles` to see available VM profiles.",
		}}
	}

	connectURI := lookup("KILN_VM_LIBVIRT_CONNECT_URI", "qemu:///system")
	libvirtNetwork := lookup("KILN_VM_LIBVIRT_NETWORK", config.DefaultLibvirtNetwork)
	checks := []doctorCheck{}
	for _, profile := range profiles {
		if strings.TrimSpace(options.Profile) == "" && profile.Backend != "libvirt-qemu" {
			continue
		}
		checks = append(checks, checkDoctorProfileContract(profile))
		if profile.Backend != "libvirt-qemu" {
			continue
		}
		checks = append(checks, checkDoctorProfileLibvirtTools(profile))
		checks = append(checks, checkDoctorProfileLibvirtNetwork(profile, connectURI, doctorProfileLibvirtNetworkSource(profile, libvirtNetwork), authority))
		if doctorProfileNeedsUEFI(profile) {
			checks = append(checks, checkDoctorProfileLibvirtFirmware(profile, connectURI, authority))
		}
		if profile.Libvirt != nil && profile.Libvirt.TPM != nil {
			checks = append(checks, checkDoctorProfileLibvirtTPM(profile))
		}
		checks = append(checks, checkDoctorProfilePreparationInputs(profile, options)...)
	}
	return checks
}

func doctorProfilesForOptions(options doctorOptions) ([]models.VMProfile, error) {
	profiles := doctorListProfiles()
	sort.Slice(profiles, func(i, j int) bool {
		return profiles[i].Name < profiles[j].Name
	})
	name := strings.TrimSpace(options.Profile)
	if name == "" {
		return profiles, nil
	}
	for _, profile := range profiles {
		if profile.Name == name {
			return []models.VMProfile{profile}, nil
		}
	}
	return nil, fmt.Errorf("unknown VM profile %q", name)
}

func checkDoctorProfileContract(profile models.VMProfile) doctorCheck {
	data := map[string]string{
		"profile":              profile.Name,
		"backend":              profile.Backend,
		"guest_os":             profile.GuestOS,
		"preparation":          string(profile.Preparation.Kind),
		"requires_preparation": strconv.FormatBool(profile.RequiresPreparation),
		"access":               string(profile.Access.Kind),
		"readiness":            string(profile.Readiness.Kind),
		"public_network":       string(profile.Networking.Public.Kind),
	}
	if profile.Libvirt != nil {
		data["libvirt_osinfo"] = profile.Libvirt.OSInfo
		data["libvirt_firmware"] = profile.Libvirt.Firmware
		data["libvirt_secure_boot"] = strconv.FormatBool(profile.Libvirt.SecureBoot)
		data["libvirt_disk_bus"] = profile.Libvirt.Disk.Bus
		data["libvirt_network_model"] = profile.Libvirt.Network.Model
		if profile.Libvirt.TPM != nil {
			data["libvirt_tpm"] = profile.Libvirt.TPM.Model
		}
	}
	detail := fmt.Sprintf(
		"%s backend=%s guest_os=%s preparation=%s access=%s readiness=%s",
		profile.Name,
		withDefaultString(profile.Backend, "unknown"),
		withDefaultString(profile.GuestOS, "unknown"),
		withDefaultString(string(profile.Preparation.Kind), "default"),
		withDefaultString(string(profile.Access.Kind), "default"),
		withDefaultString(string(profile.Readiness.Kind), "default"),
	)
	return doctorCheck{Name: doctorProfileCheckName(profile.Name, "contract"), Status: "ok", Detail: detail, Data: data}
}

func checkDoctorProfileLibvirtTools(profile models.VMProfile) doctorCheck {
	tools := []string{"virsh", "virt-install", "qemu-img"}
	if profile.Access.Kind == models.VMProfileAccessSSH {
		tools = append(tools, "ssh")
	}
	if profile.Libvirt != nil && profile.Libvirt.TPM != nil {
		tools = append(tools, "swtpm", "swtpm_setup")
	}
	missing := []string{}
	found := []string{}
	for _, tool := range tools {
		if path, err := doctorLookPath(tool); err == nil {
			found = append(found, tool+"="+path)
		} else {
			missing = append(missing, tool)
		}
	}
	data := map[string]string{
		"profile": profile.Name,
		"found":   strings.Join(found, ","),
	}
	if len(missing) > 0 {
		data["missing"] = strings.Join(missing, ",")
		return doctorCheck{
			Name:        doctorProfileCheckName(profile.Name, "libvirt_tools"),
			Status:      "fail",
			Detail:      fmt.Sprintf("%s missing libvirt profile tools: %s", profile.Name, strings.Join(missing, ", ")),
			Remediation: "Install the kiln-vm Debian package dependencies or enable the NixOS kiln-vmd module package set before preparing or creating Windows VMs.",
			Data:        data,
		}
	}
	return doctorCheck{
		Name:   doctorProfileCheckName(profile.Name, "libvirt_tools"),
		Status: "ok",
		Detail: fmt.Sprintf("%s libvirt tools available: %s", profile.Name, strings.Join(found, ", ")),
		Data:   data,
	}
}

func checkDoctorProfileLibvirtNetwork(profile models.VMProfile, connectURI, network string, authority doctorConfigAuthority) doctorCheck {
	data := authority.annotate(map[string]string{
		"profile":     profile.Name,
		"connect_uri": connectURI,
		"network":     network,
	})
	managed := network == libvirtnet.ManagedName
	data["managed"] = strconv.FormatBool(managed)
	out, err := doctorCommandOutput("virsh", "-c", connectURI, "net-info", network)
	if err != nil && managed {
		if _, listErr := doctorCommandOutput("virsh", "-c", connectURI, "net-list", "--all", "--name"); listErr == nil {
			return doctorCheck{
				Name:   doctorProfileCheckName(profile.Name, "libvirt_network"),
				Status: "ok",
				Detail: fmt.Sprintf("%s libvirt network %q does not exist yet; kiln-vmd creates it on the first Windows VM or profile preparation", profile.Name, network),
				Data:   data,
			}
		}
	}
	if err != nil {
		return doctorCheck{
			Name:        doctorProfileCheckName(profile.Name, "libvirt_network"),
			Status:      "fail",
			Detail:      fmt.Sprintf("%s libvirt network %q is not available via %s: %v", profile.Name, network, connectURI, err),
			Remediation: doctorLibvirtRemediation(err, connectURI, fmt.Sprintf("Start libvirtd, then either define and start %q with `virsh -c %s net-start %s` or unset KILN_VM_LIBVIRT_NETWORK so kiln-vmd manages the %q network.", network, connectURI, network, libvirtnet.ManagedName)),
			Data:        data,
		}
	}
	active := libvirtnet.InfoField(out, "Active")
	data["active"] = active
	if managed && !strings.EqualFold(active, "yes") {
		return doctorCheck{
			Name:   doctorProfileCheckName(profile.Name, "libvirt_network"),
			Status: "ok",
			Detail: fmt.Sprintf("%s libvirt network %q is inactive; kiln-vmd starts it on the first Windows VM or profile preparation", profile.Name, network),
			Data:   data,
		}
	}
	if !strings.EqualFold(active, "yes") {
		return doctorCheck{
			Name:        doctorProfileCheckName(profile.Name, "libvirt_network"),
			Status:      "fail",
			Detail:      fmt.Sprintf("%s libvirt network %q is not active via %s", profile.Name, network, connectURI),
			Remediation: fmt.Sprintf("Start the libvirt network with `virsh -c %s net-start %s` or update KILN_VM_LIBVIRT_NETWORK to an active NAT network.", connectURI, network),
			Data:        data,
		}
	}
	return doctorCheck{
		Name:   doctorProfileCheckName(profile.Name, "libvirt_network"),
		Status: "ok",
		Detail: fmt.Sprintf("%s libvirt network %q is active via %s", profile.Name, network, connectURI),
		Data:   data,
	}
}

func doctorLibvirtRemediation(err error, connectURI, fallback string) string {
	message := strings.ToLower(err.Error())
	for _, denied := range []string{"authentication unavailable", "authentication failed", "permission denied"} {
		if strings.Contains(message, denied) {
			return fmt.Sprintf("Your user cannot reach %s. Run `sudo kiln-vm doctor`, or add your user to the libvirt group.", connectURI)
		}
	}
	return fallback
}

func checkDoctorProfileLibvirtFirmware(profile models.VMProfile, connectURI string, authority doctorConfigAuthority) doctorCheck {
	data := authority.annotate(map[string]string{
		"profile":     profile.Name,
		"connect_uri": connectURI,
		"firmware":    profile.Libvirt.Firmware,
	})
	out, err := doctorCommandOutput("virsh", "-c", connectURI, "domcapabilities")
	if err != nil {
		return doctorCheck{
			Name:        doctorProfileCheckName(profile.Name, "libvirt_firmware"),
			Status:      "fail",
			Detail:      fmt.Sprintf("%s cannot inspect libvirt domain firmware capabilities via %s: %v", profile.Name, connectURI, err),
			Remediation: doctorLibvirtRemediation(err, connectURI, "Start libvirtd and install OVMF/UEFI firmware packages before creating Windows VMs."),
			Data:        data,
		}
	}
	if !doctorDomcapsSupportUEFI(out) {
		return doctorCheck{
			Name:        doctorProfileCheckName(profile.Name, "libvirt_firmware"),
			Status:      "fail",
			Detail:      fmt.Sprintf("%s requires UEFI firmware but libvirt domain capabilities do not advertise an OVMF/pflash loader", profile.Name),
			Remediation: "Install OVMF/UEFI firmware support and restart libvirtd before preparing or creating this profile.",
			Data:        data,
		}
	}
	return doctorCheck{
		Name:   doctorProfileCheckName(profile.Name, "libvirt_firmware"),
		Status: "ok",
		Detail: fmt.Sprintf("%s UEFI firmware support is advertised by libvirt", profile.Name),
		Data:   data,
	}
}

func checkDoctorProfileLibvirtTPM(profile models.VMProfile) doctorCheck {
	tools := []string{"swtpm", "swtpm_setup"}
	missing := []string{}
	found := []string{}
	for _, tool := range tools {
		if path, err := doctorLookPath(tool); err == nil {
			found = append(found, tool+"="+path)
		} else {
			missing = append(missing, tool)
		}
	}
	data := map[string]string{
		"profile": profile.Name,
		"found":   strings.Join(found, ","),
	}
	if profile.Libvirt != nil && profile.Libvirt.TPM != nil {
		data["backend_type"] = profile.Libvirt.TPM.BackendType
		data["backend_version"] = profile.Libvirt.TPM.BackendVersion
		data["model"] = profile.Libvirt.TPM.Model
	}
	if len(missing) > 0 {
		data["missing"] = strings.Join(missing, ",")
		return doctorCheck{
			Name:        doctorProfileCheckName(profile.Name, "libvirt_tpm"),
			Status:      "fail",
			Detail:      fmt.Sprintf("%s requires emulated TPM support but tools are missing: %s", profile.Name, strings.Join(missing, ", ")),
			Remediation: "Install swtpm and swtpm-tools, or enable libvirtd qemu swtpm support in the NixOS kiln-vmd module.",
			Data:        data,
		}
	}
	return doctorCheck{
		Name:   doctorProfileCheckName(profile.Name, "libvirt_tpm"),
		Status: "ok",
		Detail: fmt.Sprintf("%s emulated TPM tools are available: %s", profile.Name, strings.Join(found, ", ")),
		Data:   data,
	}
}

func checkDoctorProfilePreparationInputs(profile models.VMProfile, options doctorOptions) []doctorCheck {
	required := profile.Preparation.RequiredInputs
	if len(required) == 0 {
		return nil
	}
	checks := make([]doctorCheck, 0, len(required))
	for _, input := range required {
		path := doctorPreparationInputPath(input, options)
		name := doctorProfileCheckName(profile.Name, "input_"+input)
		data := map[string]string{
			"profile": profile.Name,
			"input":   input,
		}
		if strings.TrimSpace(path) == "" {
			checks = append(checks, doctorCheck{
				Name:        name,
				Status:      "warn",
				Warning:     true,
				Detail:      fmt.Sprintf("%s preparation strategy %s requires %s; no path was provided to doctor", profile.Name, profile.Preparation.Kind, input),
				Remediation: fmt.Sprintf("Run `kiln-vm doctor --profile %s --media /path/to/input` before `kiln-vm profiles prepare %s`.", profile.Name, profile.Name),
				Data:        data,
			})
			continue
		}
		data["path"] = path
		info, err := os.Stat(path)
		if err != nil {
			checks = append(checks, doctorCheck{
				Name:        name,
				Status:      "fail",
				Detail:      fmt.Sprintf("%s preparation input %s at %s is not readable: %v", profile.Name, input, path, err),
				Remediation: "Provide a readable non-empty media/image path before starting profile preparation.",
				Data:        data,
			})
			continue
		}
		if info.IsDir() || info.Size() == 0 {
			checks = append(checks, doctorCheck{
				Name:        name,
				Status:      "fail",
				Detail:      fmt.Sprintf("%s preparation input %s at %s is not a non-empty file", profile.Name, input, path),
				Remediation: "Provide a readable non-empty media/image file before starting profile preparation.",
				Data:        data,
			})
			continue
		}
		file, err := os.Open(path)
		if err != nil {
			checks = append(checks, doctorCheck{
				Name:        name,
				Status:      "fail",
				Detail:      fmt.Sprintf("%s preparation input %s at %s is not readable: %v", profile.Name, input, path, err),
				Remediation: "Provide a readable non-empty media/image file before starting profile preparation.",
				Data:        data,
			})
			continue
		}
		_ = file.Close()
		data["size_bytes"] = strconv.FormatInt(info.Size(), 10)
		checks = append(checks, doctorCheck{
			Name:   name,
			Status: "ok",
			Detail: fmt.Sprintf("%s preparation input %s is readable at %s", profile.Name, input, path),
			Data:   data,
		})
	}
	return checks
}

func doctorProfileLibvirtNetworkSource(profile models.VMProfile, fallback string) string {
	if profile.Networking.Public.Source != "" {
		return strings.TrimSpace(profile.Networking.Public.Source)
	}
	if profile.Libvirt != nil && profile.Libvirt.Network.Source != "" {
		return strings.TrimSpace(profile.Libvirt.Network.Source)
	}
	fallback = strings.TrimSpace(fallback)
	if fallback != "" {
		return fallback
	}
	return config.DefaultLibvirtNetwork
}

func doctorProfileNeedsUEFI(profile models.VMProfile) bool {
	return profile.Libvirt != nil && (strings.EqualFold(profile.Libvirt.Firmware, "uefi") || profile.Libvirt.SecureBoot)
}

func doctorDomcapsSupportUEFI(output string) bool {
	lower := strings.ToLower(output)
	return (strings.Contains(lower, "<loader supported='yes'") || strings.Contains(lower, "<loader supported=\"yes\"")) &&
		(strings.Contains(lower, "ovmf") || strings.Contains(lower, "pflash"))
}

func doctorPreparationInputPath(input string, options doctorOptions) string {
	if input == "media_path" {
		return strings.TrimSpace(options.MediaPath)
	}
	return ""
}

func doctorProfileCheckName(profile, suffix string) string {
	profile = strings.TrimSpace(profile)
	if profile == "" {
		profile = "unknown"
	}
	replacer := strings.NewReplacer("-", "_", ".", "_", "/", "_", " ", "_")
	return "profile_" + replacer.Replace(profile) + "_" + suffix
}

func runDoctorCommandOutput(name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if ctx.Err() != nil {
		return text, fmt.Errorf("%s %s timed out: %w", name, strings.Join(args, " "), ctx.Err())
	}
	if err != nil {
		if text != "" {
			return text, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, text)
		}
		return text, fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return text, nil
}
