package vm

import (
	"fmt"

	"github.com/gfdelarue/kiln-vm/internal/apierr"
	"github.com/gfdelarue/kiln-vm/internal/models"
)

func machineDiagnosticsOperationError(vm *models.VM, stateErr *WaitReadyStateError) error {
	diagnostics := machineDiagnosticsToOperationDiagnostics(vm.Diagnostics)
	if len(diagnostics) == 0 {
		return stateErr
	}
	phase := diagnostics[0].Phase
	if phase == "" {
		phase = "machine_start"
	}
	code := "vm_start_failed"
	if vm.State == models.MachineStateError {
		code = "vm_boot_failed"
	}
	return &apierr.OperationError{
		Code:        code,
		StatusRef:   fmt.Sprintf("/vms/%s", vm.Name),
		Phase:       phase,
		Resource:    vm.Name,
		Action:      "start",
		Retryable:   vm.Diagnostics[0].Retryable,
		Diagnostics: diagnostics,
		Err:         stateErr,
	}
}

func machineDiagnosticsToOperationDiagnostics(values []models.Diagnostic) []apierr.OperationDiagnostic {
	if len(values) == 0 {
		return nil
	}
	diagnostics := make([]apierr.OperationDiagnostic, 0, len(values))
	for _, value := range values {
		diagnostics = append(diagnostics, apierr.OperationDiagnostic{
			Category:    value.Category,
			Phase:       value.Phase,
			Step:        value.Action,
			Machine:     value.Resource,
			MachineID:   value.MachineID,
			Backend:     value.Backend,
			KernelPath:  value.KernelPath,
			KernelType:  value.KernelType,
			RootfsPath:  value.RootfsPath,
			ConfigPath:  value.ConfigPath,
			SocketPath:  value.SocketPath,
			VSockPath:   value.VSockPath,
			TAPDevice:   value.TAPDevice,
			LogPath:     value.LogPath,
			Cleanup:     value.Cleanup,
			Remediation: value.Remediation,
			Error:       value.Error,
			StdoutTail:  value.StdoutTail,
			StderrTail:  value.StderrTail,
		})
	}
	return diagnostics
}
