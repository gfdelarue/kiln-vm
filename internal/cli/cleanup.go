package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"

	"github.com/spf13/cobra"

	"github.com/gfdelarue/kiln-vm/internal/models"
)

var cleanupCmd = &cobra.Command{
	Use:   "cleanup",
	Short: "Clean orphaned kiln-vm data",
	RunE: func(cmd *cobra.Command, args []string) error {
		dryRun, _ := cmd.Flags().GetBool("dry-run")
		jsonOut, _ := cmd.Flags().GetBool("json")
		c, err := NewClientFromConfig()
		if err != nil {
			return err
		}
		var report models.CleanupReport
		if err := c.Post("/cleanup", models.CleanupRequest{DryRun: dryRun}, &report); err != nil {
			return err
		}
		if jsonOut {
			return json.NewEncoder(os.Stdout).Encode(report)
		}
		printCleanupReport(report)
		if len(report.Failed) > 0 {
			fmt.Fprintf(os.Stderr, "%d cleanup entries failed\n", len(report.Failed))
		}
		return nil
	},
}

func printCleanupReport(report models.CleanupReport) {
	action := "Removed"
	if report.DryRun {
		action = "Would remove"
	}
	if len(report.Removed) == 0 {
		fmt.Println("No reclaimable kiln-vm data found.")
		return
	}
	summaryAction := "Reclaimed"
	if report.DryRun {
		summaryAction = "Would reclaim"
	}
	bytes := cleanupRemovedBytes(report.Removed)
	fmt.Printf("%s %s across %d %s.\n\n", summaryAction, formatBytes(bytes), len(report.Removed), cleanupEntryLabel(len(report.Removed)))
	for _, entry := range report.Removed {
		fmt.Printf("%s %s %s (%s) reason=%s\n", action, entry.Kind, entry.Path, formatBytes(entry.ReclaimableBytes), entry.Reason)
	}
	printCleanupReasonSummary(report.Removed)
}

func printCleanupReasonSummary(entries []models.DiskUsageEntry) {
	type total struct {
		count int
		bytes uint64
	}
	byReason := map[string]total{}
	for _, entry := range entries {
		reason := entry.Reason
		if reason == "" {
			reason = "unspecified"
		}
		current := byReason[reason]
		current.count++
		current.bytes += entry.ReclaimableBytes
		byReason[reason] = current
	}
	if len(byReason) == 0 {
		return
	}
	reasons := make([]string, 0, len(byReason))
	for reason := range byReason {
		reasons = append(reasons, reason)
	}
	sort.Strings(reasons)
	fmt.Println("Summary by reason:")
	for _, reason := range reasons {
		total := byReason[reason]
		fmt.Printf("  %s: %d entries, %s reclaimable\n", reason, total.count, formatBytes(total.bytes))
	}
}

func cleanupRemovedBytes(entries []models.DiskUsageEntry) uint64 {
	var total uint64
	for _, entry := range entries {
		total += entry.ReclaimableBytes
	}
	return total
}

func cleanupEntryLabel(count int) string {
	if count == 1 {
		return "entry"
	}
	return "entries"
}

func init() {
	cleanupCmd.Flags().Bool("dry-run", false, "Show cleanup actions without deleting data")
	cleanupCmd.Flags().Bool("json", false, "Print JSON output")
}
