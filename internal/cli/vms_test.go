package cli

import (
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/gfdelarue/kiln-vm/internal/models"
)

func TestParseVMCreateCommandLeavesProfileToServer(t *testing.T) {
	cmd := newVMCreateParseTestCommand()
	if err := cmd.Flags().Set("dry-run", "true"); err != nil {
		t.Fatalf("set dry-run: %v", err)
	}
	if err := cmd.Flags().Set("json", "true"); err != nil {
		t.Fatalf("set json: %v", err)
	}

	req, dryRun, jsonOutput, err := parseVMCreateCommand(cmd, "runner")
	if err != nil {
		t.Fatalf("parseVMCreateCommand: %v", err)
	}
	if req.Name != "runner" || req.Profile != "" {
		t.Fatalf("request = %+v, want no client profile default", req)
	}
	if !dryRun || !jsonOutput {
		t.Fatalf("dryRun/json = %t/%t, want true/true", dryRun, jsonOutput)
	}
}

func TestParseVMCreateCommandCarriesExplicitOverrides(t *testing.T) {
	cmd := newVMCreateParseTestCommand()
	flags := map[string]string{
		"profile":        "alpine",
		"disk":           "2G",
		"network-policy": "isolated-web",
		"cpu":            "2",
		"cpu-kind":       "dedicated",
		"memory-mb":      "1024",
		"power":          "on_demand",
		"idle":           "30s",
	}
	for name, value := range flags {
		if err := cmd.Flags().Set(name, value); err != nil {
			t.Fatalf("set %s: %v", name, err)
		}
	}

	req, _, _, err := parseVMCreateCommand(cmd, "worker")
	if err != nil {
		t.Fatalf("parseVMCreateCommand: %v", err)
	}
	if req.Profile != "alpine" || req.NetworkPolicy != "isolated-web" || req.MemoryMB != 1024 || req.DiskSizeMB != 2048 {
		t.Fatalf("request overrides = %+v", req)
	}
	if req.CPU == nil || req.CPU.Kind != "dedicated" || req.CPU.Millis != 2000 {
		t.Fatalf("request CPU = %+v", req.CPU)
	}
	if req.Power == nil || req.Power.Mode != models.VMPowerModeOnDemand || req.Power.IdleTimeoutSeconds != 30 {
		t.Fatalf("request power = %+v", req.Power)
	}
}

func TestParseVMCreateCommandSetsVCPUs(t *testing.T) {
	tests := []struct {
		name  string
		flags map[string]string
		want  models.CPUConfig
	}{
		{
			name:  "with quota",
			flags: map[string]string{"cpu": "6", "vcpus": "6"},
			want:  models.CPUConfig{Kind: "shared", Millis: 6000, VCPUs: 6},
		},
		{
			name:  "without quota",
			flags: map[string]string{"cpu-kind": "dedicated", "vcpus": "2"},
			want:  models.CPUConfig{Kind: "dedicated", VCPUs: 2},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := newVMCreateParseTestCommand()
			for name, value := range tt.flags {
				if err := cmd.Flags().Set(name, value); err != nil {
					t.Fatalf("set %s: %v", name, err)
				}
			}

			req, _, _, err := parseVMCreateCommand(cmd, "worker")
			if err != nil {
				t.Fatalf("parseVMCreateCommand: %v", err)
			}
			if req.CPU == nil || *req.CPU != tt.want {
				t.Fatalf("request CPU = %+v, want %+v", req.CPU, tt.want)
			}
		})
	}
}

func TestParseVMCreateCommandRejectsNegativeVCPUs(t *testing.T) {
	cmd := newVMCreateParseTestCommand()
	if err := cmd.Flags().Set("vcpus", "-1"); err != nil {
		t.Fatalf("set vcpus: %v", err)
	}

	if _, _, _, err := parseVMCreateCommand(cmd, "worker"); err == nil || !strings.Contains(err.Error(), "--vcpus") {
		t.Fatalf("parseVMCreateCommand error = %v, want --vcpus error", err)
	}
}

func TestVMCreatePlanOutputShowsResolvedShape(t *testing.T) {
	output := captureStdout(t, func() {
		printVMCreatePlan(models.VMCreatePlan{
			Name:                "runner",
			Profile:             "ubuntu-docker",
			CPU:                 models.CPUConfig{Kind: "shared", VCPUs: 4},
			MemoryMB:            8192,
			DiskSizeMB:          262144,
			Power:               models.VMPowerPolicy{Mode: models.VMPowerModeAlways},
			ProfileCapabilities: []string{"docker"},
		})
	})
	for _, want := range []string{"runner", "ubuntu-docker", "8192MB", "262144MB", "always", "docker"} {
		if !strings.Contains(output, want) {
			t.Fatalf("plan output missing %q: %q", want, output)
		}
	}
}

func newVMCreateParseTestCommand() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Flags().String("profile", "", "")
	cmd.Flags().String("disk", "", "")
	cmd.Flags().String("network-policy", "", "")
	cmd.Flags().String("cpu", "", "")
	cmd.Flags().String("cpu-kind", "shared", "")
	cmd.Flags().Int("vcpus", 0, "")
	cmd.Flags().Int("memory-mb", 0, "")
	cmd.Flags().String("power", "", "")
	cmd.Flags().String("idle", "", "")
	cmd.Flags().Bool("dry-run", false, "")
	cmd.Flags().Bool("json", false, "")
	return cmd
}

func TestVMExecCommandQuotesArgumentsForTheGuestShell(t *testing.T) {
	args := []string{"powershell", "-Command", "Write-Output 'it''s'"}
	for _, tc := range []struct {
		guestOS string
		want    string
	}{
		{"linux", `'powershell' '-Command' 'Write-Output '"'"'it'"'"''"'"'s'"'"''`},
		{"windows", `& 'powershell' '-Command' 'Write-Output ''it''''s'''`},
	} {
		if got := vmExecCommand(tc.guestOS, args); got != tc.want {
			t.Errorf("vmExecCommand(%q) = %s, want %s", tc.guestOS, got, tc.want)
		}
	}
	if got := vmExecCommand("windows", []string{"Get-Process | Select -First 1"}); got != "Get-Process | Select -First 1" {
		t.Errorf("single-argument command = %q, want it unchanged", got)
	}
}

func TestVMExecPassesFlagsAfterTheVMToTheGuestCommand(t *testing.T) {
	cmd, args, err := rootCmd.Find([]string{"exec", "dev", "sh", "-c", "exit 3"})
	if err != nil {
		t.Fatalf("find exec: %v", err)
	}
	if err := cmd.ParseFlags(args); err != nil {
		t.Fatalf("parse exec flags: %v", err)
	}
	if got, want := cmd.Flags().Args(), []string{"dev", "sh", "-c", "exit 3"}; !slices.Equal(got, want) {
		t.Fatalf("exec args = %q, want %q", got, want)
	}
}
