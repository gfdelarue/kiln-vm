package cli

import (
	"strings"
	"testing"

	"github.com/gfdelarue/kiln-vm/internal/models"
)

func TestDiagnosticRowsRenderBootFailureDetailsWithinWidth(t *testing.T) {
	rows := diagnosticRows([]models.Diagnostic{{
		Category:    "machine_boot",
		Phase:       "process_start",
		Error:       "start firecracker: exit status 1",
		KernelType:  "missing",
		KernelPath:  "/very/long/path/to/a/missing/kernel/that/should/not/make/the/table/wrap/vmlinux",
		LogPath:     "/var/lib/kiln-vm/machines/very-long-machine-id/console.log",
		Cleanup:     "released runtime network artifacts",
		Remediation: "check the VM kernel path, rootfs, Firecracker binary, and host virtualization support",
	}})
	if rows[0][0] != "machine_boot" || rows[0][1] != "process_start" || rows[0][3] != "missing" {
		t.Fatalf("diagnostic row = %+v", rows[0])
	}
	output := renderTextTable(textTable{Headers: diagnosticHeaders(), Rows: rows}, textTableRenderOptions{MaxWidth: 96})

	if !strings.Contains(output, "missing") {
		t.Fatalf("output missing diagnostic identity: %q", output)
	}
	if !strings.Contains(output, "...") {
		t.Fatalf("expected long diagnostic fields to truncate: %q", output)
	}
	for _, line := range strings.Split(strings.TrimRight(output, "\n"), "\n") {
		if len([]rune(line)) > 96 {
			t.Fatalf("line exceeded width: len=%d line=%q output=%q", len([]rune(line)), line, output)
		}
	}
}
