//go:build system

package systemtest

import (
	"strings"
	"testing"

	"github.com/gfdelarue/kiln-vm/internal/version"
)

func TestCompatibleHealthErrorRejectsStaleDaemon(t *testing.T) {
	err := compatibleHealthError(healthResponse{
		Status:  "ok",
		Version: "0.0.0-stale",
	})
	if err == nil {
		t.Fatal("expected stale daemon version to be rejected")
	}
	if !strings.Contains(err.Error(), "rebuild/restart kiln-vmd") {
		t.Fatalf("error %q does not explain how to fix stale kiln-vmd", err)
	}
}

func TestCompatibleHealthErrorAcceptsMatchingDaemon(t *testing.T) {
	err := compatibleHealthError(healthResponse{
		Status:  "ok",
		Version: version.Version,
	})
	if err != nil {
		t.Fatalf("matching daemon rejected: %v", err)
	}
}
