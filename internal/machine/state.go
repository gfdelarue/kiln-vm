package machine

import (
	"fmt"

	"github.com/gfdelarue/kiln-vm/internal/apierr"
	"github.com/gfdelarue/kiln-vm/internal/models"
)

// ErrInvalidTransition is returned when a state transition is not allowed.
var ErrInvalidTransition = fmt.Errorf("invalid state transition: %w", apierr.ErrConflict)

// validTransitions defines the allowed state transitions.
var validTransitions = map[models.MachineState][]models.MachineState{
	models.MachineStateCreating: {models.MachineStateRunning, models.MachineStateStopping, models.MachineStateError},
	models.MachineStateRunning:  {models.MachineStateStopping, models.MachineStateError},
	models.MachineStateStopping: {models.MachineStateStopped, models.MachineStateError},
	models.MachineStateStopped:  {models.MachineStateCreating},
	models.MachineStateError:    {models.MachineStateCreating},
}

func validateTransition(from, to models.MachineState) error {
	allowed, ok := validTransitions[from]
	if !ok {
		return fmt.Errorf("unknown state: %s", from)
	}
	for _, s := range allowed {
		if s == to {
			return nil
		}
	}
	return fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, from, to)
}
