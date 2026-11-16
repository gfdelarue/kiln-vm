package machine

import (
	"testing"

	"github.com/gfdelarue/kiln-vm/internal/models"
)

func TestShouldCleanupRuntimeOnExit(t *testing.T) {
	tests := []struct {
		state models.MachineState
		want  bool
	}{
		{state: models.MachineStateCreating, want: true},
		{state: models.MachineStateRunning, want: true},
		{state: models.MachineStateStopping, want: false},
		{state: models.MachineStateStopped, want: false},
		{state: models.MachineStateError, want: false},
	}

	for _, tt := range tests {
		t.Run(string(tt.state), func(t *testing.T) {
			if got := shouldCleanupRuntimeOnExit(tt.state); got != tt.want {
				t.Fatalf("shouldCleanupRuntimeOnExit(%s) = %v, want %v", tt.state, got, tt.want)
			}
		})
	}
}
