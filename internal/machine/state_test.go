package machine

import (
	"testing"

	"github.com/gfdelarue/kiln-vm/internal/models"
)

func TestValidTransitions(t *testing.T) {
	tests := []struct {
		name string
		from models.MachineState
		to   models.MachineState
	}{
		{"creating to running", models.MachineStateCreating, models.MachineStateRunning},
		{"creating to stopping", models.MachineStateCreating, models.MachineStateStopping},
		{"creating to error", models.MachineStateCreating, models.MachineStateError},
		{"running to stopping", models.MachineStateRunning, models.MachineStateStopping},
		{"running to error", models.MachineStateRunning, models.MachineStateError},
		{"stopping to stopped", models.MachineStateStopping, models.MachineStateStopped},
		{"stopping to error", models.MachineStateStopping, models.MachineStateError},
		{"stopped to creating", models.MachineStateStopped, models.MachineStateCreating},
		{"error to creating", models.MachineStateError, models.MachineStateCreating},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateTransition(tt.from, tt.to)
			if err != nil {
				t.Errorf("validateTransition(%q, %q) returned error: %v", tt.from, tt.to, err)
			}
		})
	}
}

func TestInvalidTransitions(t *testing.T) {
	tests := []struct {
		name string
		from models.MachineState
		to   models.MachineState
	}{
		{"creating to stopped", models.MachineStateCreating, models.MachineStateStopped},
		{"running to creating", models.MachineStateRunning, models.MachineStateCreating},
		{"running to stopped", models.MachineStateRunning, models.MachineStateStopped},
		{"stopping to running", models.MachineStateStopping, models.MachineStateRunning},
		{"stopping to creating", models.MachineStateStopping, models.MachineStateCreating},
		{"stopped to running", models.MachineStateStopped, models.MachineStateRunning},
		{"stopped to stopping", models.MachineStateStopped, models.MachineStateStopping},
		{"stopped to error", models.MachineStateStopped, models.MachineStateError},
		{"error to running", models.MachineStateError, models.MachineStateRunning},
		{"error to stopping", models.MachineStateError, models.MachineStateStopping},
		{"error to stopped", models.MachineStateError, models.MachineStateStopped},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateTransition(tt.from, tt.to)
			if err == nil {
				t.Errorf("validateTransition(%q, %q) should have returned error", tt.from, tt.to)
			}
		})
	}
}

func TestUnknownState(t *testing.T) {
	err := validateTransition(models.MachineState("bogus"), models.MachineStateRunning)
	if err == nil {
		t.Error("validateTransition from unknown state should return error")
	}
}
