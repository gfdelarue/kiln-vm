package models

import "time"

type MetricsSummary struct {
	SampledAt time.Time        `json:"sampled_at"`
	Node      NodeMetrics      `json:"node"`
	Machines  []MachineMetrics `json:"machines"`
	VMs       []VMMetrics      `json:"vms"`
}

type NodeMetrics struct {
	SampledAt time.Time       `json:"sampled_at"`
	Status    string          `json:"status"`
	Warnings  []NodeWarning   `json:"warnings,omitempty"`
	Capacity  NodeCapacity    `json:"capacity"`
	Host      HostMetrics     `json:"host"`
	Disk      DiskUsageTotals `json:"disk"`
}

// NodeWarning is a bounded, operator-facing explanation for a node status.
// Message and Reason are classifications, never raw provider command output.
type NodeWarning struct {
	Code      string `json:"code"`
	Reason    string `json:"reason"`
	MachineID string `json:"machine_id,omitempty"`
	Machine   string `json:"machine,omitempty"`
	Message   string `json:"message"`
}

type NodeCapacity struct {
	MaxRunningVMs             int    `json:"max_running_vms"`
	RunningVMs                int    `json:"running_vms"`
	AvailableRunningVMs       int    `json:"available_running_vms"`
	MaxCPUMillis              int    `json:"max_cpu_millis"`
	AllocatedCPUMillis        int    `json:"allocated_cpu_millis"`
	AvailableCPUMillis        int    `json:"available_cpu_millis"`
	PooledCPULimitMillis      int    `json:"pooled_cpu_limit_millis"`
	PooledVMs                 int    `json:"pooled_vms"`
	PooledVCPUs               int    `json:"pooled_vcpus"`
	MaxMemoryBytes            uint64 `json:"max_memory_bytes"`
	AllocatedMemoryBytes      uint64 `json:"allocated_memory_bytes"`
	AvailableMemoryBytes      uint64 `json:"available_memory_bytes"`
	TotalMachines             int    `json:"total_machines"`
	RunningOrStartingMachines int    `json:"running_or_starting_machines"`
}

type HostMetrics struct {
	CPUPercent              float64   `json:"cpu_percent"`
	LoadAverage             []float64 `json:"load_average"`
	MemoryTotalBytes        uint64    `json:"memory_total_bytes"`
	MemoryUsedBytes         uint64    `json:"memory_used_bytes"`
	MemoryAvailableBytes    uint64    `json:"memory_available_bytes"`
	NetworkRxBytesPerSecond float64   `json:"network_rx_bytes_per_second"`
	NetworkTxBytesPerSecond float64   `json:"network_tx_bytes_per_second"`
}

// GatewayDNSStatus reports the provider-owned DNS path for one kiln-vm VM's
// public Firecracker network. It intentionally excludes the probe query name,
// upstream addresses, guest command output, and raw resolver errors.
type GatewayDNSStatus struct {
	Status         string     `json:"status"`
	Reason         string     `json:"reason,omitempty"`
	GatewayDNSPath bool       `json:"gateway_dns_path"`
	ExternalDNS    bool       `json:"external_dns"`
	DesiredRules   int        `json:"desired_rules"`
	InstalledRules int        `json:"installed_rules"`
	InFlight       int64      `json:"in_flight"`
	LastCheckedAt  *time.Time `json:"last_checked_at,omitempty"`
	LastSuccessAt  *time.Time `json:"last_success_at,omitempty"`
	LastError      string     `json:"last_error,omitempty"`
}

type MachineMetrics struct {
	MachineID               string           `json:"machine_id"`
	Name                    string           `json:"name"`
	Backend                 string           `json:"backend"`
	GuestOS                 string           `json:"guest_os"`
	State                   MachineState     `json:"state"`
	HealthStatus            string           `json:"health_status"`
	GatewayDNS              GatewayDNSStatus `json:"gateway_dns"`
	CPUPercent              float64          `json:"cpu_percent"`
	CPULimitMillis          int              `json:"cpu_limit_millis"`
	MemoryUsedBytes         uint64           `json:"memory_used_bytes"`
	MemoryLimitBytes        uint64           `json:"memory_limit_bytes"`
	MemoryObserved          bool             `json:"memory_observed"`
	MemoryStatus            string           `json:"memory_status"`
	MemoryError             string           `json:"memory_error,omitempty"`
	DiskReadBytesPerSecond  float64          `json:"disk_read_bytes_per_second"`
	DiskWriteBytesPerSecond float64          `json:"disk_write_bytes_per_second"`
	NetworkRxBytesPerSecond float64          `json:"network_rx_bytes_per_second"`
	NetworkTxBytesPerSecond float64          `json:"network_tx_bytes_per_second"`
	UptimeSeconds           int64            `json:"uptime_seconds"`
	RuntimeObserved         bool             `json:"runtime_observed"`
	RuntimePID              int              `json:"runtime_pid,omitempty"`
	IP                      string           `json:"ip,omitempty"`
	Error                   string           `json:"error,omitempty"`
	SampledAt               time.Time        `json:"sampled_at"`
}

type VMMetrics struct {
	ID        string         `json:"id"`
	Name      string         `json:"name"`
	Profile   string         `json:"profile"`
	MachineID string         `json:"machine_id"`
	State     MachineState   `json:"state"`
	Power     VMPower        `json:"power"`
	Machine   MachineMetrics `json:"machine"`
	SampledAt time.Time      `json:"sampled_at"`
}
