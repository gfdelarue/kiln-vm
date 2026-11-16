package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/gfdelarue/kiln-vm/internal/models"
)

var vmProfilesCmd = &cobra.Command{
	Use:   "profiles",
	Short: "List VM profiles",
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := NewClientFromConfig()
		if err != nil {
			return err
		}
		jsonOutput, _ := cmd.Flags().GetBool("json")
		var profiles []models.VMProfile
		if err := c.Get("/vm-profiles", &profiles); err != nil {
			return err
		}
		if jsonOutput {
			return json.NewEncoder(os.Stdout).Encode(profiles)
		}
		var rows [][]string
		for _, profile := range profiles {
			prepared := "-"
			if profile.RequiresPreparation {
				if profile.Prepared {
					prepared = "yes"
				} else {
					prepared = "no"
				}
			}
			rows = append(rows, []string{
				profile.Name,
				profile.Backend,
				profile.GuestOS,
				prepared,
				fmt.Sprintf("%dMB", profile.DefaultDiskSizeMB),
				fmt.Sprintf("%d", profile.DefaultVCPUs),
				fmt.Sprintf("%dMB", profile.DefaultMemoryMB),
				profile.Description,
			})
		}
		printTable([]string{"NAME", "BACKEND", "GUEST", "PREPARED", "DISK", "VCPUS", "MEMORY", "DESCRIPTION"}, rows)
		return nil
	},
}

var vmListCmd = &cobra.Command{
	Use:     "list",
	Aliases: []string{"ls"},
	Short:   "List persistent VMs",
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := NewClientFromConfig()
		if err != nil {
			return err
		}
		jsonOutput, _ := cmd.Flags().GetBool("json")
		var vms []models.VM
		if err := c.Get("/vms", &vms); err != nil {
			return err
		}
		if jsonOutput {
			return json.NewEncoder(os.Stdout).Encode(vms)
		}
		var rows [][]string
		for _, vm := range vms {
			rows = append(rows, []string{
				vm.Name,
				string(vm.State),
				vm.Profile,
				vm.IP,
				formatCPU(vm.CPU),
				fmt.Sprintf("%dMB", vm.MemoryMB),
				fmt.Sprintf("%dMB", vm.DiskSizeMB),
				string(vm.Power.Policy.Mode),
			})
		}
		printTable([]string{"NAME", "STATE", "PROFILE", "IP", "CPU", "MEMORY", "DISK", "POWER"}, rows)
		return nil
	},
}

var vmCreateCmd = &cobra.Command{
	Use:   "create <name>",
	Short: "Create a persistent VM",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		req, dryRun, jsonOutput, err := parseVMCreateCommand(cmd, args[0])
		if err != nil {
			return err
		}
		c, err := NewClientFromConfig()
		if err != nil {
			return err
		}
		if dryRun {
			var plan models.VMCreatePlan
			if err := c.Post("/vms/plan", &req, &plan); err != nil {
				return err
			}
			if jsonOutput {
				return json.NewEncoder(os.Stdout).Encode(plan)
			}
			printVMCreatePlan(plan)
			return nil
		}
		var vm models.VM
		if err := c.Post("/vms", &req, &vm); err != nil {
			return err
		}
		if jsonOutput {
			return json.NewEncoder(os.Stdout).Encode(vm)
		}
		fmt.Printf("Created VM %s (state: %s)\n", vm.Name, vm.State)
		return nil
	},
}

func parseVMCreateCommand(cmd *cobra.Command, name string) (models.CreateVMRequest, bool, bool, error) {
	profile, _ := cmd.Flags().GetString("profile")
	disk, _ := cmd.Flags().GetString("disk")
	networkPolicy, _ := cmd.Flags().GetString("network-policy")
	cpuFlag, _ := cmd.Flags().GetString("cpu")
	cpuKind, _ := cmd.Flags().GetString("cpu-kind")
	vcpus, _ := cmd.Flags().GetInt("vcpus")
	memoryMB, _ := cmd.Flags().GetInt("memory-mb")
	dryRun, _ := cmd.Flags().GetBool("dry-run")
	jsonOutput, _ := cmd.Flags().GetBool("json")
	power, err := parseCreatePowerPolicy(cmd)
	if err != nil {
		return models.CreateVMRequest{}, false, false, err
	}
	diskSizeMB, err := parseDiskMB(disk)
	if err != nil {
		return models.CreateVMRequest{}, false, false, err
	}
	cpu, err := parseCPUFlag(cpuFlag, cpuKind)
	if err != nil {
		return models.CreateVMRequest{}, false, false, err
	}
	if vcpus < 0 {
		return models.CreateVMRequest{}, false, false, fmt.Errorf("--vcpus must be positive")
	}
	if vcpus > 0 {
		if cpu == nil {
			cpu = &models.CPUConfig{Kind: strings.TrimSpace(cpuKind)}
		}
		cpu.VCPUs = vcpus
	}
	return models.CreateVMRequest{
		Name:          name,
		Profile:       profile,
		NetworkPolicy: networkPolicy,
		CPU:           cpu,
		MemoryMB:      memoryMB,
		DiskSizeMB:    diskSizeMB,
		Power:         power,
	}, dryRun, jsonOutput, nil
}

var vmGetCmd = &cobra.Command{
	Use:   "get <name-or-id>",
	Short: "Get persistent VM details",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := NewClientFromConfig()
		if err != nil {
			return err
		}
		jsonOutput, _ := cmd.Flags().GetBool("json")
		var vm models.VM
		if err := c.Get("/vms/"+args[0], &vm); err != nil {
			return err
		}
		if jsonOutput {
			return json.NewEncoder(os.Stdout).Encode(vm)
		}
		printTable([]string{"NAME", "STATE", "PROFILE", "IP", "CPU", "MEMORY", "DISK", "POWER", "IDLE"}, [][]string{{
			vm.Name,
			string(vm.State),
			vm.Profile,
			vm.IP,
			formatCPU(vm.CPU),
			fmt.Sprintf("%dMB", vm.MemoryMB),
			fmt.Sprintf("%dMB", vm.DiskSizeMB),
			string(vm.Power.Policy.Mode),
			formatIdleTimeout(vm.Power.Policy.IdleTimeoutSeconds),
		}})
		if len(vm.Capabilities) > 0 {
			fmt.Println()
			printCapabilityConditions(vm.Capabilities)
		}
		if len(vm.Diagnostics) > 0 {
			fmt.Println()
			printDiagnostics(vm.Diagnostics)
		}
		return nil
	},
}

var vmPowerCmd = &cobra.Command{
	Use:   "power <name-or-id>",
	Short: "Inspect or update a VM power policy",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := NewClientFromConfig()
		if err != nil {
			return err
		}
		jsonOutput, _ := cmd.Flags().GetBool("json")
		policy, updating, err := parsePowerCommandPolicy(cmd)
		if err != nil {
			return err
		}
		var vm models.VM
		if updating {
			if err := c.Patch("/vms/"+args[0]+"/power", &models.UpdateVMPowerRequest{Policy: *policy}, &vm); err != nil {
				return err
			}
		} else if err := c.Get("/vms/"+args[0], &vm); err != nil {
			return err
		}
		if jsonOutput {
			return json.NewEncoder(os.Stdout).Encode(vm.Power)
		}
		printTable([]string{"NAME", "MODE", "IDLE", "LAST_ACTIVITY", "LAST_STARTED", "LAST_STOPPED", "STOP_REASON"}, [][]string{{
			vm.Name,
			string(vm.Power.Policy.Mode),
			formatIdleTimeout(vm.Power.Policy.IdleTimeoutSeconds),
			formatTimePtr(vm.Power.LastActivityAt),
			formatTimePtr(vm.Power.LastStartedAt),
			formatTimePtr(vm.Power.LastStoppedAt),
			vm.Power.StopReason,
		}})
		return nil
	},
}

var vmVerifyCmd = &cobra.Command{
	Use:   "verify <name-or-id>",
	Short: "Verify declared VM profile capabilities",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := NewClientFromConfig()
		if err != nil {
			return err
		}
		capability, _ := cmd.Flags().GetString("capability")
		force, _ := cmd.Flags().GetBool("force")
		jsonOutput, _ := cmd.Flags().GetBool("json")
		req := models.VerifyVMCapabilitiesRequest{
			Capability: strings.TrimSpace(capability),
			Force:      force,
		}
		var response models.VerifyVMCapabilitiesResponse
		if err := c.Post("/vms/"+args[0]+"/verify", &req, &response); err != nil {
			return err
		}
		if jsonOutput {
			return json.NewEncoder(os.Stdout).Encode(response)
		}
		printCapabilityConditions(response.Conditions)
		return nil
	},
}

var vmStartCmd = &cobra.Command{
	Use:   "start <name-or-id>",
	Short: "Start a persistent VM",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := NewClientFromConfig()
		if err != nil {
			return err
		}
		jsonOutput, _ := cmd.Flags().GetBool("json")
		var vm models.VM
		if err := c.Post("/vms/"+args[0]+"/start", nil, &vm); err != nil {
			return err
		}
		if jsonOutput {
			return json.NewEncoder(os.Stdout).Encode(vm)
		}
		fmt.Printf("Started VM %s\n", vm.Name)
		return nil
	},
}

var vmStopCmd = &cobra.Command{
	Use:   "stop <name-or-id>",
	Short: "Stop a persistent VM",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := NewClientFromConfig()
		if err != nil {
			return err
		}
		if err := c.Post("/vms/"+args[0]+"/stop", nil, nil); err != nil {
			return err
		}
		fmt.Printf("Stopping VM %s\n", args[0])
		return nil
	},
}

var vmDeleteCmd = &cobra.Command{
	Use:     "delete <name-or-id>",
	Aliases: []string{"rm"},
	Short:   "Delete a persistent VM",
	Args:    cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := NewClientFromConfig()
		if err != nil {
			return err
		}
		if err := c.Delete("/vms/" + args[0]); err != nil {
			return err
		}
		fmt.Printf("Deleted VM %s\n", args[0])
		return nil
	},
}

var vmExecCmd = &cobra.Command{
	Use:   "exec <name-or-id> <command> [args...]",
	Short: "Execute a command in a persistent VM, waking it when policy allows",
	Args:  cobra.MinimumNArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := NewClientFromConfig()
		if err != nil {
			return err
		}
		command := args[1]
		if len(args) > 2 {
			var vm models.VM
			if err := c.Get("/vms/"+args[0], &vm); err != nil {
				return err
			}
			command = vmExecCommand(vm.GuestOS, args[1:])
		}
		return executeRemoteCommand(c, "/vms/"+args[0]+"/exec", command)
	},
}

var vmShellCmd = &cobra.Command{
	Use:   "shell <name-or-id>",
	Short: "Open a shell in a persistent VM, waking it when policy allows",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := NewClientFromConfig()
		if err != nil {
			return err
		}
		return openShell(c, "/vms/"+args[0]+"/shell")
	},
}

var vmImagesCmd = &cobra.Command{
	Use:   "images",
	Short: "Manage cached VM profile base images",
}

var vmImagesListCmd = &cobra.Command{
	Use:   "list",
	Short: "List cached VM profile base images",
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := NewClientFromConfig()
		if err != nil {
			return err
		}
		jsonOutput, _ := cmd.Flags().GetBool("json")
		var images []models.VMProfileImage
		if err := c.Get("/vm-profile-images", &images); err != nil {
			return err
		}
		if jsonOutput {
			return json.NewEncoder(os.Stdout).Encode(images)
		}
		wide, _ := cmd.Flags().GetBool("wide")
		profileImageIDs := make([]string, 0, len(images))
		imageIDs := make([]string, 0, len(images))
		for _, image := range images {
			profileImageIDs = append(profileImageIDs, image.ID)
			imageIDs = append(imageIDs, image.ImageID)
		}
		displayProfileImages := displayIDMap(profileImageIDs, wide)
		displayImages := displayIDMap(imageIDs, wide)

		var rows [][]string
		for _, image := range images {
			rows = append(rows, []string{
				displayProfileImages[image.ID],
				image.Profile,
				displayImages[image.ImageID],
				fmt.Sprintf("%dMB", image.RootfsSizeMB),
				image.Fingerprint,
				image.CreatedAt.Format("2006-01-02 15:04:05"),
			})
		}
		printTable([]string{"ID", "PROFILE", "IMAGE", "SIZE", "FINGERPRINT", "CREATED"}, rows)
		return nil
	},
}

var vmImagesEnsureCmd = &cobra.Command{
	Use:   "ensure <profile>",
	Short: "Build or reuse a cached VM profile base image",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return ensureVMImage(cmd, args[0], false)
	},
}

var vmImagesRefreshCmd = &cobra.Command{
	Use:   "refresh <profile>",
	Short: "Rebuild a cached VM profile base image",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return ensureVMImage(cmd, args[0], true)
	},
}

var vmImagesDeleteCmd = &cobra.Command{
	Use:   "delete <vm-profile-image-ref>",
	Short: "Delete a cached VM profile base image",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := NewClientFromConfig()
		if err != nil {
			return err
		}
		if err := c.Delete("/vm-profile-images/" + args[0]); err != nil {
			return err
		}
		fmt.Printf("Deleted VM profile image %s\n", args[0])
		return nil
	},
}

var vmImagesDeleteProfileCmd = &cobra.Command{
	Use:   "delete-profile <profile>",
	Short: "Delete cached VM profile base images for a profile",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := NewClientFromConfig()
		if err != nil {
			return err
		}
		if err := c.Delete("/vm-profile-images/profile/" + args[0]); err != nil {
			return err
		}
		fmt.Printf("Deleted cached VM profile images for %s\n", args[0])
		return nil
	},
}

func init() {
	vmImagesCmd.AddCommand(vmImagesListCmd)
	vmImagesCmd.AddCommand(vmImagesEnsureCmd)
	vmImagesCmd.AddCommand(vmImagesRefreshCmd)
	vmImagesCmd.AddCommand(vmImagesDeleteCmd)
	vmImagesCmd.AddCommand(vmImagesDeleteProfileCmd)

	rootCmd.AddCommand(
		vmProfilesCmd,
		vmImagesCmd,
		vmListCmd,
		vmCreateCmd,
		vmGetCmd,
		vmStartCmd,
		vmStopCmd,
		vmPowerCmd,
		vmVerifyCmd,
		vmDeleteCmd,
		vmExecCmd,
		vmShellCmd,
		vmForwardCmd,
	)

	for _, cmd := range []*cobra.Command{vmProfilesCmd, vmListCmd, vmGetCmd, vmStartCmd} {
		cmd.Flags().Bool("json", false, "Print JSON output")
	}
	vmExecCmd.Flags().SetInterspersed(false)
	vmImagesListCmd.Flags().Bool("json", false, "Print JSON output")
	vmImagesListCmd.Flags().Bool("wide", false, "Print full IDs")
	for _, cmd := range []*cobra.Command{vmImagesEnsureCmd, vmImagesRefreshCmd} {
		cmd.Flags().String("disk", "", "Root disk size in MiB, or with M/G suffix")
		cmd.Flags().Bool("json", false, "Print JSON output")
	}
	vmCreateCmd.Flags().String("profile", "", "VM profile (server default when omitted)")
	vmCreateCmd.Flags().String("disk", "", "Root disk size in MiB, or with M/G suffix")
	vmCreateCmd.Flags().String("network-policy", "", "Network policy: isolated-web restricts guests to public HTTP/HTTPS and gateway DNS")
	vmCreateCmd.Flags().String("cpu", "", "CPU allocation (for example 1, 0.25, or 250m; profile default when omitted)")
	vmCreateCmd.Flags().String("cpu-kind", "shared", "CPU kind: shared (per-VM quota), pooled (guest vCPUs sharing the host pool), or dedicated")
	vmCreateCmd.Flags().Int("vcpus", 0, "Guest vCPU count, which --cpu does not change (profile default when omitted)")
	vmCreateCmd.Flags().Int("memory-mb", 0, "Memory in MiB (profile default when omitted)")
	vmCreateCmd.Flags().String("power", "", "Power policy mode: manual, always, on_demand, or warm")
	vmCreateCmd.Flags().String("idle", "", "Idle timeout for on_demand or warm power modes (for example 10m)")
	vmCreateCmd.Flags().Bool("dry-run", false, "Show the resolved VM create plan without creating a VM")
	vmCreateCmd.Flags().Bool("json", false, "Print JSON output")
	vmPowerCmd.Flags().String("mode", "", "Power policy mode: manual, always, on_demand, or warm")
	vmPowerCmd.Flags().String("idle", "", "Idle timeout for on_demand or warm power modes (for example 10m)")
	vmPowerCmd.Flags().Bool("json", false, "Print JSON output")
	vmVerifyCmd.Flags().String("capability", "", "Verify only one declared capability")
	vmVerifyCmd.Flags().Bool("force", false, "Rerun verification even if the capability is already ok")
	vmVerifyCmd.Flags().Bool("json", false, "Print JSON output")
	vmForwardCmd.Flags().String("listen", defaultVMForwardListen, "Local address to listen on; the default is loopback with a free port")
}

func parseCreatePowerPolicy(cmd *cobra.Command) (*models.VMPowerPolicy, error) {
	mode, _ := cmd.Flags().GetString("power")
	idle, _ := cmd.Flags().GetString("idle")
	if strings.TrimSpace(mode) == "" && strings.TrimSpace(idle) == "" {
		return nil, nil
	}
	if strings.TrimSpace(mode) == "" {
		mode = string(models.VMPowerModeOnDemand)
	}
	policy, err := buildPowerPolicy(mode, idle)
	if err != nil {
		return nil, err
	}
	return &policy, nil
}

func parsePowerCommandPolicy(cmd *cobra.Command) (*models.VMPowerPolicy, bool, error) {
	mode, _ := cmd.Flags().GetString("mode")
	idle, _ := cmd.Flags().GetString("idle")
	if strings.TrimSpace(mode) == "" && strings.TrimSpace(idle) == "" {
		return nil, false, nil
	}
	if strings.TrimSpace(mode) == "" {
		return nil, false, fmt.Errorf("--mode is required when updating power policy")
	}
	policy, err := buildPowerPolicy(mode, idle)
	if err != nil {
		return nil, false, err
	}
	return &policy, true, nil
}

func buildPowerPolicy(mode, idle string) (models.VMPowerPolicy, error) {
	idleSeconds, err := parseIdleSeconds(idle)
	if err != nil {
		return models.VMPowerPolicy{}, err
	}
	return models.VMPowerPolicy{
		Mode:               models.VMPowerMode(strings.TrimSpace(mode)),
		IdleTimeoutSeconds: idleSeconds,
	}, nil
}

func parseIdleSeconds(value string) (int, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, nil
	}
	duration, err := time.ParseDuration(value)
	if err == nil {
		if duration < 0 {
			return 0, fmt.Errorf("--idle must be positive")
		}
		return int(duration.Seconds()), nil
	}
	seconds, parseErr := strconv.Atoi(value)
	if parseErr != nil || seconds < 0 {
		return 0, fmt.Errorf("--idle must be a duration like 10m or positive seconds")
	}
	return seconds, nil
}

func formatIdleTimeout(seconds int) string {
	if seconds <= 0 {
		return "-"
	}
	return (time.Duration(seconds) * time.Second).String()
}

func formatTimePtr(value *time.Time) string {
	if value == nil {
		return "-"
	}
	return value.UTC().Format(time.RFC3339)
}

func printCapabilityConditions(conditions []models.VMCapabilityCondition) {
	rows := make([][]string, 0, len(conditions))
	for _, condition := range conditions {
		checkedAt := "-"
		if condition.CheckedAt != nil {
			checkedAt = condition.CheckedAt.UTC().Format(time.RFC3339)
		}
		rows = append(rows, []string{
			condition.Capability,
			string(condition.Status),
			withDash(condition.Phase),
			withDash(condition.Detail),
			withDash(condition.Remediation),
			checkedAt,
		})
	}
	printTable([]string{"CAPABILITY", "STATUS", "PHASE", "DETAIL", "REMEDIATION", "CHECKED"}, rows)
}

func printVMCreatePlan(plan models.VMCreatePlan) {
	printTable([]string{"NAME", "PROFILE", "CPU", "MEMORY", "DISK", "POWER", "CAPABILITIES"}, [][]string{{
		plan.Name,
		plan.Profile,
		formatCPU(plan.CPU),
		fmt.Sprintf("%dMB", plan.MemoryMB),
		fmt.Sprintf("%dMB", plan.DiskSizeMB),
		string(plan.Power.Mode),
		formatStringList(plan.ProfileCapabilities),
	}})
}

func formatStringList(values []string) string {
	if len(values) == 0 {
		return "-"
	}
	return strings.Join(values, ",")
}

func withDash(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "-"
	}
	return value
}

func ensureVMImage(cmd *cobra.Command, profile string, refresh bool) error {
	c, err := NewClientFromConfig()
	if err != nil {
		return err
	}
	disk, _ := cmd.Flags().GetString("disk")
	jsonOutput, _ := cmd.Flags().GetBool("json")
	diskSizeMB, err := parseDiskMB(disk)
	if err != nil {
		return err
	}
	req := models.EnsureVMProfileImageRequest{
		Profile:      profile,
		RootfsSizeMB: diskSizeMB,
		Refresh:      refresh,
	}
	var image models.VMProfileImage
	if err := c.Post("/vm-profile-images/ensure", &req, &image); err != nil {
		return err
	}
	if jsonOutput {
		if err := json.NewEncoder(os.Stdout).Encode(image); err != nil {
			return fmt.Errorf("encode vm profile image: %w", err)
		}
		return nil
	}
	action := "Using"
	if refresh {
		action = "Refreshed"
	}
	fmt.Printf("%s cached %s VM profile image %s (image: %s, size: %dMB)\n", action, image.Profile, image.ID, image.ImageID, image.RootfsSizeMB)
	return nil
}

func parseDiskMB(raw string) (int64, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return 0, nil
	}
	multiplier := int64(1)
	last := value[len(value)-1]
	switch last {
	case 'g', 'G':
		multiplier = 1024
		value = value[:len(value)-1]
	case 'm', 'M':
		value = value[:len(value)-1]
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("invalid disk size %q", raw)
	}
	return parsed * multiplier, nil
}

func shellJoin(args []string) string {
	quoted := make([]string, 0, len(args))
	for _, arg := range args {
		quoted = append(quoted, shellQuote(arg))
	}
	return strings.Join(quoted, " ")
}

func vmExecCommand(guestOS string, args []string) string {
	if len(args) == 1 {
		return args[0]
	}
	if guestOS == "windows" {
		return powerShellJoin(args)
	}
	return shellJoin(args)
}

func powerShellJoin(args []string) string {
	quoted := make([]string, 0, len(args))
	for _, arg := range args {
		quoted = append(quoted, "'"+strings.ReplaceAll(arg, "'", "''")+"'")
	}
	return "& " + strings.Join(quoted, " ")
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}
