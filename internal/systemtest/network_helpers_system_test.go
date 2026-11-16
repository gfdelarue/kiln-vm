//go:build system

package systemtest

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"

	"github.com/gfdelarue/kiln-vm/internal/models"
)

func stopVMAndWait(t *testing.T, ctx context.Context, client *apiClient, ref string) *models.VM {
	t.Helper()
	if err := client.stopVM(ctx, ref); err != nil {
		t.Fatalf("stop VM %s: %v", ref, err)
	}
	vm, err := client.waitForVMState(ctx, ref, models.MachineStateStopped)
	if err != nil {
		t.Fatalf("wait VM %s stopped: %v", ref, err)
	}
	return vm
}

func startVMAndWait(t *testing.T, ctx context.Context, client *apiClient, ref string) *models.VM {
	t.Helper()
	if _, err := client.startVM(ctx, ref); err != nil {
		t.Fatalf("start VM %s: %v", ref, err)
	}
	vm, err := client.waitForVMState(ctx, ref, models.MachineStateRunning)
	if err != nil {
		t.Fatalf("wait VM %s running: %v", ref, err)
	}
	return vm
}

func linkExistsForTest(name string) (bool, error) {
	out, err := exec.Command("ip", "link", "show", name).CombinedOutput()
	return linkExistsFromResultForTest(name, out, err)
}

func linkExistsFromResultForTest(name string, out []byte, err error) (bool, error) {
	if err == nil {
		return true, nil
	}
	if linkMissingOutputForTest(out) {
		return false, nil
	}
	return false, commandError(fmt.Sprintf("inspect link %s", name), out, err)
}

func linkMissingOutputForTest(out []byte) bool {
	text := string(out)
	return strings.Contains(text, "Cannot find device") || strings.Contains(text, "does not exist")
}

func commandError(action string, out []byte, err error) error {
	return fmt.Errorf("%s: %s: %w", action, out, err)
}
