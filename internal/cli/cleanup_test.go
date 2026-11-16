package cli

import (
	"strings"
	"testing"

	"github.com/gfdelarue/kiln-vm/internal/models"
)

func TestPrintCleanupReportDryRunSummarizesTotalBeforeLedger(t *testing.T) {
	output := captureStdout(t, func() {
		printCleanupReport(models.CleanupReport{
			DryRun: true,
			Removed: []models.DiskUsageEntry{
				{Kind: "vm-profile-image", Path: "/var/lib/kiln-vm/images/old", Reason: "superseded-vm-profile-image-cache", ReclaimableBytes: 1536},
				{Kind: "temp", Path: "/var/lib/kiln-vm/tmp/a", Reason: "stale-temp", ReclaimableBytes: 512},
			},
		})
	})
	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) == 0 || lines[0] != "Would reclaim 2.0KiB across 2 entries." {
		t.Fatalf("first cleanup line = %q, output:\n%s", firstLine(output), output)
	}
	for _, want := range []string{
		"Would remove vm-profile-image /var/lib/kiln-vm/images/old (1.5KiB) reason=superseded-vm-profile-image-cache",
		"Would remove temp /var/lib/kiln-vm/tmp/a (512B) reason=stale-temp",
		"Summary by reason:",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("cleanup output = %q, want %q", output, want)
		}
	}
}

func TestPrintCleanupReportNoEntriesKeepsNoDataMessage(t *testing.T) {
	output := captureStdout(t, func() {
		printCleanupReport(models.CleanupReport{DryRun: true})
	})
	if strings.TrimSpace(output) != "No reclaimable kiln-vm data found." {
		t.Fatalf("cleanup output = %q", output)
	}
}

func TestPrintCleanupReasonSummaryGroupsEntries(t *testing.T) {
	output := captureStdout(t, func() {
		printCleanupReasonSummary([]models.DiskUsageEntry{
			{Reason: "profile-image-cache-budget-exceeded", ReclaimableBytes: 1024},
			{Reason: "profile-image-cache-budget-exceeded", ReclaimableBytes: 2048},
			{Reason: "stale-build-scratch", ReclaimableBytes: 512},
		})
	})
	for _, want := range []string{
		"Summary by reason:",
		"profile-image-cache-budget-exceeded: 2 entries, 3.0KiB reclaimable",
		"stale-build-scratch: 1 entries, 512B reclaimable",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("summary output = %q, want %q", output, want)
		}
	}
}

func firstLine(output string) string {
	line, _, _ := strings.Cut(output, "\n")
	return line
}
