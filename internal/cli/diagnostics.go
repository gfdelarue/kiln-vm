package cli

import (
	"github.com/gfdelarue/kiln-vm/internal/models"
)

func printDiagnostics(diagnostics []models.Diagnostic) {
	if len(diagnostics) == 0 {
		return
	}
	printTable(diagnosticHeaders(), diagnosticRows(diagnostics))
}

func diagnosticHeaders() []string {
	return []string{"CATEGORY", "PHASE", "ERROR", "KERNEL", "KERNEL_PATH", "LOG", "CLEANUP", "REMEDIATION"}
}

func diagnosticRows(diagnostics []models.Diagnostic) [][]string {
	rows := make([][]string, 0, len(diagnostics))
	for _, diagnostic := range diagnostics {
		rows = append(rows, []string{
			diagnostic.Category,
			diagnostic.Phase,
			diagnostic.Error,
			diagnostic.KernelType,
			diagnostic.KernelPath,
			diagnostic.LogPath,
			diagnostic.Cleanup,
			diagnostic.Remediation,
		})
	}
	return rows
}
