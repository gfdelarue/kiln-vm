package cli

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/gfdelarue/kiln-vm/internal/models"
)

var diskCmd = &cobra.Command{
	Use:   "disk",
	Short: "Inspect kiln-vm disk usage",
}

var diskUsageCmd = &cobra.Command{
	Use:   "usage",
	Short: "Show kiln-vm disk usage",
	RunE: func(cmd *cobra.Command, args []string) error {
		jsonOut, _ := cmd.Flags().GetBool("json")
		c, err := NewClientFromConfig()
		if err != nil {
			return err
		}
		var report models.DiskUsageReport
		if err := c.Get("/disk/usage", &report); err != nil {
			return err
		}
		if jsonOut {
			return json.NewEncoder(os.Stdout).Encode(report)
		}
		printDiskUsageReport(report)
		return nil
	},
}

func printDiskUsageReport(report models.DiskUsageReport) {
	fmt.Printf("kiln-vm data: %s\n", report.DataDir)
	if report.BuildDir != "" && report.BuildDir != report.DataDir {
		fmt.Printf("kiln-vm build data: %s\n", report.BuildDir)
	}
	fmt.Printf("Allocated: %s  Reclaimable: %s\n", formatBytes(report.Totals.AllocatedBytes), formatBytes(report.Totals.ReclaimableBytes))
	for _, warning := range report.Warnings {
		fmt.Printf("Warning: %s (%s)\n", warning.Message, warning.Code)
	}
	for _, entry := range report.Entries {
		reclaimable := ""
		if entry.Reclaimable {
			reclaimable = " reclaimable=" + formatBytes(entry.ReclaimableBytes)
		}
		name := entry.Name
		if name == "" {
			name = entry.ID
		}
		if name == "" {
			name = "-"
		}
		fmt.Printf("%-14s %-12s allocated=%-8s apparent=%-8s%s %s\n", entry.Kind, name, formatBytes(entry.AllocatedBytes), formatBytes(entry.ApparentBytes), reclaimable, entry.Reason)
	}
}

func init() {
	diskCmd.AddCommand(diskUsageCmd)
	diskUsageCmd.Flags().Bool("json", false, "Print JSON output")
}
