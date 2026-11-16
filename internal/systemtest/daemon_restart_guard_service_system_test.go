//go:build system && service

package systemtest

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gfdelarue/kiln-vm/internal/models"
)

const sharedDaemonRestartOverrideEnv = "KILN_VM_SYSTEMTEST_ALLOW_SHARED_KILND_RESTART"

func requireDaemonRestartSupport(t *testing.T, client *apiClient) {
	t.Helper()
	if _, err := exec.LookPath("systemctl"); err != nil {
		t.Skip("systemctl unavailable")
	}
	if os.Geteuid() != 0 && os.Getenv("KILN_VM_SYSTEMTEST_ALLOW_UNPRIVILEGED_SYSTEMCTL") != "1" {
		t.Skip(serviceRestartSudoMessage)
	}
	requireExclusiveDaemonRestartHost(t, client)
}

func requireExclusiveDaemonRestartHost(t *testing.T, client *apiClient) {
	t.Helper()
	if os.Getenv(sharedDaemonRestartOverrideEnv) == "1" {
		t.Logf("allowing shared kiln-vmd restart because %s=1", sharedDaemonRestartOverrideEnv)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for snapshot := 1; snapshot <= 2; snapshot++ {
		blockers, err := daemonRestartBlockers(ctx, client)
		if err != nil {
			t.Fatalf("inspect kiln-vm resources before kiln-vmd restart (snapshot %d/2): %v", snapshot, err)
		}
		if len(blockers) != 0 {
			t.Fatalf(
				"refusing to restart shared kiln-vmd while provider resources have unsafe transient or recovery work, or active resources fall outside this test's exact ID allowlist:\n- %s\nwait for provider work to settle and stop active Testkit/kiln-vm consumers first, or set %s=1 to run this destructive restart suite deliberately",
				strings.Join(formatDaemonRestartBlockers(blockers), "\n- "),
				sharedDaemonRestartOverrideEnv,
			)
		}
		if snapshot == 1 {
			select {
			case <-ctx.Done():
				t.Fatalf("verify stable empty kiln-vm restart snapshot: %v", ctx.Err())
			case <-time.After(250 * time.Millisecond):
			}
		}
	}
}

type daemonRestartBlocker struct {
	Name          string
	ID            string
	State         string
	Reason        string
	StartOnDaemon bool
	ActiveLeases  int
}

func (b daemonRestartBlocker) String() string {
	return fmt.Sprintf(
		"vm %s (%s) state=%s start_on_daemon=%t active_leases=%d reason=%s",
		b.Name,
		b.ID,
		b.State,
		b.StartOnDaemon,
		b.ActiveLeases,
		b.Reason,
	)
}

func formatDaemonRestartBlockers(blockers []daemonRestartBlocker) []string {
	formatted := make([]string, 0, len(blockers))
	for _, blocker := range blockers {
		formatted = append(formatted, blocker.String())
	}
	return formatted
}

func daemonRestartBlockers(ctx context.Context, client *apiClient) ([]daemonRestartBlocker, error) {
	vms, err := client.listVMs(ctx)
	if err != nil {
		return nil, fmt.Errorf("list VMs: %w", err)
	}
	return daemonRestartBlockersFromVMs(vms, client.restartOwnedSnapshot()), nil
}

func daemonRestartBlockersFromVMs(vms []models.VM, owned map[string]struct{}) []daemonRestartBlocker {
	var blockers []daemonRestartBlocker
	for _, vm := range vms {
		if reason := vmRestartBlockReason(vm, restartIDOwned(owned, vm.ID)); reason != "" {
			blockers = append(blockers, daemonRestartBlocker{
				Name:          vm.Name,
				ID:            vm.ID,
				State:         string(vm.State),
				Reason:        reason,
				StartOnDaemon: vm.Power.Policy.StartsWithDaemon(),
				ActiveLeases:  vm.Power.ActiveLeases,
			})
		}
	}
	sort.Slice(blockers, func(i, j int) bool { return blockers[i].String() < blockers[j].String() })
	return blockers
}

func restartIDOwned(owned map[string]struct{}, id string) bool {
	_, ok := owned[id]
	return ok && strings.TrimSpace(id) != ""
}

func vmRestartBlockReason(vm models.VM, owned bool) string {
	if vm.Power.ActiveLeases > 0 {
		return "active VM leases"
	}
	for _, diagnostic := range vm.Diagnostics {
		if diagnostic.Category == "provider_network" && diagnostic.Phase == "cleanup" && diagnostic.Retryable {
			return "retryable provider network cleanup pending"
		}
	}
	switch vm.State {
	case models.MachineStateCreating:
		return "VM creation in progress"
	case models.MachineStateStopping:
		return "VM stop in progress"
	}
	if owned {
		return ""
	}
	if vm.State == models.MachineStateRunning || vm.Power.Policy.StartsWithDaemon() {
		return "active VM lifecycle or restart policy"
	}
	return ""
}

func TestDaemonRestartGuardClassifiesProviderRecoveryRisk(t *testing.T) {
	for _, test := range []struct {
		name string
		vm   models.VM
		want string
	}{
		{
			name: "active lifecycle",
			vm:   models.VM{State: models.MachineStateRunning},
			want: "active VM lifecycle or restart policy",
		},
		{
			name: "retryable provider cleanup",
			vm: models.VM{
				State: models.MachineStateError,
				Diagnostics: []models.Diagnostic{{
					Category:  "provider_network",
					Phase:     "cleanup",
					Retryable: true,
				}},
			},
			want: "retryable provider network cleanup pending",
		},
		{
			name: "nonretryable provider cleanup",
			vm: models.VM{
				State: models.MachineStateError,
				Diagnostics: []models.Diagnostic{{
					Category: "provider_network",
					Phase:    "cleanup",
				}},
			},
		},
		{
			name: "ordinary boot error",
			vm: models.VM{
				State: models.MachineStateError,
				Diagnostics: []models.Diagnostic{{
					Category:  "machine_boot",
					Phase:     "process_start",
					Retryable: true,
				}},
			},
		},
		{
			name: "stopped",
			vm:   models.VM{State: models.MachineStateStopped},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := vmRestartBlockReason(test.vm, false); got != test.want {
				t.Fatalf("vmRestartBlockReason(%+v) = %q, want %q", test.vm, got, test.want)
			}
		})
	}
}

func TestDaemonRestartGuardAllowsExactOwnedStableRestartFixtures(t *testing.T) {
	owned := map[string]struct{}{
		"vm-running-owned": {},
		"vm-stopped-owned": {},
	}
	blockers := daemonRestartBlockersFromVMs(
		[]models.VM{
			{
				ID:        "vm-running-owned",
				Name:      "stable-running-vm",
				MachineID: "machine-owned-through-vm",
				State:     models.MachineStateRunning,
				Power:     models.VMPower{Policy: models.VMPowerPolicy{Mode: models.VMPowerModeAlways}},
			},
			{
				ID:    "vm-stopped-owned",
				Name:  "stable-stopped-vm",
				State: models.MachineStateStopped,
				Power: models.VMPower{Policy: models.VMPowerPolicy{Mode: models.VMPowerModeAlways}},
			},
		},
		owned,
	)
	if len(blockers) != 0 {
		t.Fatalf("exact-owned stable restart fixtures unexpectedly blocked restart: %v", formatDaemonRestartBlockers(blockers))
	}
}

func TestDaemonRestartGuardKeepsUnownedStableResourcesBlocking(t *testing.T) {
	blockers := daemonRestartBlockersFromVMs(
		[]models.VM{
			{ID: "vm-running", Name: "unowned-running-vm", State: models.MachineStateRunning},
			{
				ID:    "vm-start-on-daemon",
				Name:  "unowned-start-on-daemon-vm",
				State: models.MachineStateStopped,
				Power: models.VMPower{Policy: models.VMPowerPolicy{Mode: models.VMPowerModeAlways}},
			},
		},
		nil,
	)
	want := map[string]string{
		"vm-running":         "active VM lifecycle or restart policy",
		"vm-start-on-daemon": "active VM lifecycle or restart policy",
	}
	if len(blockers) != len(want) {
		t.Fatalf("blockers = %v, want one blocker for each unowned stable VM", formatDaemonRestartBlockers(blockers))
	}
	for _, blocker := range blockers {
		if got := want[blocker.ID]; got == "" || blocker.Reason != got {
			t.Fatalf("unexpected unowned stable blocker: %+v; want reasons %v", blocker, want)
		}
	}
}

func TestDaemonRestartGuardExactOwnershipCannotBypassUnsafeWork(t *testing.T) {
	for _, test := range []struct {
		name       string
		vm         models.VM
		wantReason string
	}{
		{
			name: "VM active lease",
			vm: models.VM{
				ID: "vm-leased", Name: "owned-vm", State: models.MachineStateRunning,
				Power: models.VMPower{ActiveLeases: 1},
			},
			wantReason: "active VM leases",
		},
		{
			name: "VM retryable provider network cleanup",
			vm: models.VM{
				ID: "vm-cleanup", Name: "owned-vm", State: models.MachineStateRunning,
				Diagnostics: []models.Diagnostic{{Category: "provider_network", Phase: "cleanup", Retryable: true}},
			},
			wantReason: "retryable provider network cleanup pending",
		},
		{
			name:       "VM creating",
			vm:         models.VM{ID: "vm-creating", Name: "owned-vm", State: models.MachineStateCreating},
			wantReason: "VM creation in progress",
		},
		{
			name:       "VM stopping",
			vm:         models.VM{ID: "vm-stopping", Name: "owned-vm", State: models.MachineStateStopping},
			wantReason: "VM stop in progress",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			owned := map[string]struct{}{test.vm.ID: {}}
			blockers := daemonRestartBlockersFromVMs([]models.VM{test.vm}, owned)
			if len(blockers) != 1 {
				t.Fatalf("blockers = %v, want one exact-owned unsafe blocker", formatDaemonRestartBlockers(blockers))
			}
			blocker := blockers[0]
			if blocker.ID != test.vm.ID || blocker.State != string(test.vm.State) || blocker.Reason != test.wantReason {
				t.Fatalf("blocker = %+v, want id=%s state=%s reason=%q", blocker, test.vm.ID, test.vm.State, test.wantReason)
			}
			if formatted := blocker.String(); !strings.Contains(formatted, "reason="+test.wantReason) {
				t.Fatalf("structured blocker %q omits clear reason %q", formatted, test.wantReason)
			}
		})
	}
}

func TestDaemonRestartGuardUsesExactIDsNotNames(t *testing.T) {
	client := &apiClient{restartOwned: map[string]struct{}{}}
	client.rememberRestartOwned("owned-vm-id")
	owned := client.restartOwnedSnapshot()
	if !restartIDOwned(owned, "owned-vm-id") {
		t.Fatal("exact VM ID created by this client should be restart-owned")
	}
	if restartIDOwned(owned, resourcePrefix+"owned-vm-id") {
		t.Fatal("a systemtest-looking name must not be treated as restart-owned")
	}
	if restartIDOwned(owned, "owned-vm") {
		t.Fatal("a near-match must not be treated as restart-owned")
	}
}

func TestServiceDaemonRestartGuardRejectsActiveNonSystemtestVM(t *testing.T) {
	client := requireClient(t)
	cleanupResourcesByPrefix(t, client)

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	withSystemProfileImageCleanup(t, client, ctx)

	name := fmt.Sprintf("kiln-vm-restart-guard-non-systemtest-%d", time.Now().UnixNano())
	var created models.VM
	err := client.do(ctx, http.MethodPost, "/vms", models.CreateVMRequest{
		Name:     name,
		Profile:  "alpine",
		CPU:      &models.CPUConfig{VCPUs: 1},
		MemoryMB: 512,
		Power: &models.VMPowerPolicy{
			Mode: models.VMPowerModeAlways,
		},
	}, &created)
	if err != nil {
		t.Fatalf("create non-systemtest VM: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := client.deleteVM(cleanupCtx, name); err != nil && !isAPIStatus(err, 404) {
			t.Logf("cleanup non-systemtest VM %s: %v", name, err)
		}
	})
	if _, err := client.waitForVMState(ctx, name, models.MachineStateRunning); err != nil {
		t.Fatalf("wait for non-systemtest VM running: %v", err)
	}

	blockers, err := daemonRestartBlockers(ctx, client)
	if err != nil {
		t.Fatalf("inspect restart blockers: %v", err)
	}
	for _, blocker := range blockers {
		if blocker.Name == name && blocker.ID == created.ID && blocker.State == string(models.MachineStateRunning) && blocker.Reason == "active VM lifecycle or restart policy" {
			return
		}
	}
	t.Fatalf("restart blockers = %+v, want structured active untracked VM blocker for %s (%s)", blockers, name, created.ID)
}
