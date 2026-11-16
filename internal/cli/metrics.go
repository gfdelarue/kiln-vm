package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/gfdelarue/kiln-vm/internal/models"
)

var metricsCmd = &cobra.Command{
	Use:   "metrics [name-or-id]",
	Short: "Show live metrics for the node, or for one VM",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		jsonOutput, _ := cmd.Flags().GetBool("json")
		c, err := NewClientFromConfig()
		if err != nil {
			return err
		}
		if len(args) == 1 {
			var metrics models.VMMetrics
			if err := c.Get("/vms/"+args[0]+"/metrics", &metrics); err != nil {
				return err
			}
			if jsonOutput {
				return json.NewEncoder(os.Stdout).Encode(metrics)
			}
			printVMMetrics(metrics)
			return nil
		}
		var summary models.MetricsSummary
		if err := c.Get("/metrics/summary", &summary); err != nil {
			return err
		}
		if jsonOutput {
			return json.NewEncoder(os.Stdout).Encode(summary)
		}
		printMetricsSummary(summary)
		return nil
	},
}

var nodeCmd = &cobra.Command{
	Use:   "node",
	Short: "Inspect kiln-vm node health and capacity",
}

var nodeCapacityCmd = &cobra.Command{
	Use:   "capacity",
	Short: "Show node capacity and allocation",
	RunE: func(cmd *cobra.Command, args []string) error {
		jsonOutput, _ := cmd.Flags().GetBool("json")
		c, err := NewClientFromConfig()
		if err != nil {
			return err
		}
		var capacity models.NodeCapacity
		if err := c.Get("/node/capacity", &capacity); err != nil {
			return err
		}
		if jsonOutput {
			return json.NewEncoder(os.Stdout).Encode(capacity)
		}
		printNodeCapacity(capacity)
		return nil
	},
}

var nodeHealthCmd = &cobra.Command{
	Use:   "health",
	Short: "Show live node health",
	RunE: func(cmd *cobra.Command, args []string) error {
		jsonOutput, _ := cmd.Flags().GetBool("json")
		c, err := NewClientFromConfig()
		if err != nil {
			return err
		}
		var health models.NodeMetrics
		if err := c.Get("/node/health", &health); err != nil {
			return err
		}
		if jsonOutput {
			return json.NewEncoder(os.Stdout).Encode(health)
		}
		printNodeHealth(health)
		return nil
	},
}

func printMetricsSummary(summary models.MetricsSummary) {
	printNodeHealth(summary.Node)
	if len(summary.Machines) > 0 {
		fmt.Println()
		printTable([]string{"MACHINE", "STATE", "HEALTH", "CPU", "MEMORY", "NET_RX", "NET_TX"}, machineMetricRows(summary.Machines))
		printGatewayDNSMetrics(summary.Machines)
	}
	if len(summary.VMs) > 0 {
		fmt.Println()
		var rows [][]string
		for _, vm := range summary.VMs {
			rows = append(rows, []string{
				vm.Name,
				string(vm.State),
				vm.Profile,
				vm.Machine.Name,
				string(vm.Power.Policy.Mode),
				vm.Machine.HealthStatus,
			})
		}
		printTable([]string{"VM", "STATE", "PROFILE", "MACHINE", "POWER", "HEALTH"}, rows)
	}
}

func printNodeHealth(health models.NodeMetrics) {
	fmt.Printf("Node: %s  sampled=%s\n", health.Status, health.SampledAt.Format("2006-01-02T15:04:05Z07:00"))
	for _, warning := range health.Warnings {
		fmt.Printf("Warning [%s]: machine=%s reason=%s message=%s\n",
			formatNodeWarningValue(warning.Code, "unknown", 64),
			formatNodeWarningValue(nodeWarningMachine(warning), "-", 96),
			formatNodeWarningValue(warning.Reason, "unknown", 64),
			formatGatewayDNSError(warning.Message),
		)
	}
	fmt.Printf("Host: cpu=%s load=%s memory=%s/%s network=rx:%s tx:%s\n",
		formatPercent(health.Host.CPUPercent),
		formatLoadAverage(health.Host.LoadAverage),
		formatBytes(health.Host.MemoryUsedBytes),
		formatBytes(health.Host.MemoryTotalBytes),
		formatByteRate(health.Host.NetworkRxBytesPerSecond),
		formatByteRate(health.Host.NetworkTxBytesPerSecond),
	)
	fmt.Printf("Disk: allocated=%s reclaimable=%s\n", formatBytes(health.Disk.AllocatedBytes), formatBytes(health.Disk.ReclaimableBytes))
	printNodeCapacity(health.Capacity)
}

func printNodeCapacity(capacity models.NodeCapacity) {
	if capacity.PooledCPULimitMillis > 0 {
		fmt.Printf("CPU pool: limit=%dm guests=%d guest-vcpus=%d; shared capacity remaining=%dm\n", capacity.PooledCPULimitMillis, capacity.PooledVMs, capacity.PooledVCPUs, capacity.AvailableCPUMillis)
	}
	fmt.Printf("Capacity: vms=%d/%d cpu=%dm/%dm memory=%s/%s\n",
		capacity.RunningVMs,
		capacity.MaxRunningVMs,
		capacity.AllocatedCPUMillis,
		capacity.MaxCPUMillis,
		formatBytes(capacity.AllocatedMemoryBytes),
		formatBytes(capacity.MaxMemoryBytes),
	)
}

func printVMMetrics(metrics models.VMMetrics) {
	printTable([]string{"VM", "STATE", "PROFILE", "MACHINE", "POWER", "HEALTH", "CPU", "MEMORY"}, [][]string{{
		metrics.Name,
		string(metrics.State),
		metrics.Profile,
		metrics.Machine.Name,
		string(metrics.Power.Policy.Mode),
		metrics.Machine.HealthStatus,
		formatPercent(metrics.Machine.CPUPercent),
		formatMachineMemory(metrics.Machine),
	}})
	printGatewayDNSMetrics([]models.MachineMetrics{metrics.Machine})
}

const maxGatewayDNSErrorDisplayRunes = 160

func nodeWarningMachine(warning models.NodeWarning) string {
	if strings.TrimSpace(warning.Machine) != "" {
		return warning.Machine
	}
	return warning.MachineID
}

func formatNodeWarningValue(value, fallback string, maxRunes int) string {
	value = cleanTableCell(value)
	if value == "" {
		return fallback
	}
	return truncateDisplayText(value, maxRunes)
}

func printGatewayDNSMetrics(metrics []models.MachineMetrics) {
	for _, metric := range metrics {
		dns := metric.GatewayDNS
		fmt.Printf("Gateway DNS [%s]: status=%s reason=%s path=%s external=%s rules=%d/%d in_flight=%d\n",
			gatewayDNSDisplayValue(metric.Name, metric.MachineID),
			gatewayDNSDisplayValue(dns.Status, "unknown"),
			gatewayDNSDisplayValue(dns.Reason, "-"),
			yesNo(dns.GatewayDNSPath),
			yesNo(dns.ExternalDNS),
			dns.InstalledRules,
			dns.DesiredRules,
			dns.InFlight,
		)
		fmt.Printf("  checked=%s success=%s error=%s\n",
			formatTimePtr(dns.LastCheckedAt),
			formatTimePtr(dns.LastSuccessAt),
			formatGatewayDNSError(dns.LastError),
		)
	}
}

func gatewayDNSDisplayValue(value, fallback string) string {
	value = cleanTableCell(value)
	if value == "" {
		return fallback
	}
	return value
}

func formatGatewayDNSError(value string) string {
	value = strings.Join(strings.Fields(value), " ")
	if value == "" {
		return "-"
	}
	return truncateDisplayText(value, maxGatewayDNSErrorDisplayRunes)
}

func machineMetricRows(metrics []models.MachineMetrics) [][]string {
	rows := make([][]string, 0, len(metrics))
	for _, metric := range metrics {
		rows = append(rows, []string{
			metric.Name,
			string(metric.State),
			metric.HealthStatus,
			formatPercent(metric.CPUPercent),
			formatMachineMemory(metric),
			formatByteRate(metric.NetworkRxBytesPerSecond),
			formatByteRate(metric.NetworkTxBytesPerSecond),
		})
	}
	return rows
}

func formatMachineMemory(metrics models.MachineMetrics) string {
	limit := formatBytes(metrics.MemoryLimitBytes)
	if !metrics.MemoryObserved {
		return "unavailable/" + limit
	}
	return formatBytes(metrics.MemoryUsedBytes) + "/" + limit
}

func formatPercent(value float64) string {
	return fmt.Sprintf("%.1f%%", value)
}

func formatByteRate(value float64) string {
	return formatBytes(uint64(value)) + "/s"
}

func formatLoadAverage(load []float64) string {
	if len(load) < 3 {
		return "-"
	}
	return fmt.Sprintf("%.2f,%.2f,%.2f", load[0], load[1], load[2])
}

func init() {
	nodeCmd.AddCommand(nodeCapacityCmd)
	nodeCmd.AddCommand(nodeHealthCmd)
	rootCmd.AddCommand(metricsCmd)
	rootCmd.AddCommand(nodeCmd)

	metricsCmd.Flags().Bool("json", false, "Print JSON output")
	nodeCapacityCmd.Flags().Bool("json", false, "Print JSON output")
	nodeHealthCmd.Flags().Bool("json", false, "Print JSON output")
}

func yesNo(value bool) string {
	if value {
		return "yes"
	}
	return "no"
}
