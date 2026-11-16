package metrics

import (
	"bufio"
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gfdelarue/kiln-vm/internal/config"
	"github.com/gfdelarue/kiln-vm/internal/disk"
	"github.com/gfdelarue/kiln-vm/internal/machine"
	"github.com/gfdelarue/kiln-vm/internal/models"
	vmservice "github.com/gfdelarue/kiln-vm/internal/vm"
)

const defaultSampleWindow = 100 * time.Millisecond

const maxSanitizedProviderMetricsErrorRunes = 256

const (
	memoryMetricMeasured    = "measured"
	memoryMetricUnavailable = "unavailable"
)

type Service struct {
	cfg          *config.Config
	machines     *machine.Manager
	vms          *vmservice.Service
	disks        *disk.Service
	procRoot     string
	sysRoot      string
	sampleWindow time.Duration
	now          func() time.Time
}

func NewService(cfg *config.Config, machines *machine.Manager, vms *vmservice.Service, disks *disk.Service) *Service {
	return &Service{
		cfg:          cfg,
		machines:     machines,
		vms:          vms,
		disks:        disks,
		procRoot:     "/proc",
		sysRoot:      "/sys",
		sampleWindow: defaultSampleWindow,
		now:          time.Now,
	}
}

func (s *Service) Summary(ctx context.Context) (*models.MetricsSummary, error) {
	sampledAt := s.now().UTC()
	machines, err := s.machines.List()
	if err != nil {
		return nil, fmt.Errorf("list machines for metrics: %w", err)
	}
	vms, err := s.vms.List()
	if err != nil {
		return nil, fmt.Errorf("list VMs for metrics: %w", err)
	}
	node, err := s.nodeMetrics(ctx, sampledAt, machines)
	if err != nil {
		return nil, err
	}
	machineMetrics, err := s.machineMetricsFor(ctx, sampledAt, machines)
	if err != nil {
		return nil, err
	}
	machineByID := make(map[string]models.MachineMetrics, len(machineMetrics))
	for _, metrics := range machineMetrics {
		machineByID[metrics.MachineID] = metrics
	}
	vmMetrics := make([]models.VMMetrics, 0, len(vms))
	for _, vm := range vms {
		vmMetrics = append(vmMetrics, vmMetricsFromVM(sampledAt, vm, machineByID[vm.MachineID]))
	}
	return &models.MetricsSummary{
		SampledAt: sampledAt,
		Node:      *node,
		Machines:  machineMetrics,
		VMs:       vmMetrics,
	}, nil
}

func (s *Service) NodeCapacity(ctx context.Context) (*models.NodeCapacity, error) {
	_ = ctx
	machines, err := s.machines.List()
	if err != nil {
		return nil, fmt.Errorf("list machines for capacity: %w", err)
	}
	capacity := s.capacity(machines)
	return &capacity, nil
}

func (s *Service) NodeHealth(ctx context.Context) (*models.NodeMetrics, error) {
	machines, err := s.machines.List()
	if err != nil {
		return nil, fmt.Errorf("list machines for node health: %w", err)
	}
	return s.nodeMetrics(ctx, s.now().UTC(), machines)
}

func (s *Service) MachineMetrics(ctx context.Context, ref string) (*models.MachineMetrics, error) {
	machine, err := s.machines.Get(ref)
	if err != nil {
		return nil, fmt.Errorf("get machine %s for metrics: %w", ref, err)
	}
	metrics, err := s.machineMetricsFor(ctx, s.now().UTC(), []models.Machine{*machine})
	if err != nil {
		return nil, fmt.Errorf("collect machine %s metrics: %w", machine.ID, err)
	}
	if len(metrics) != 1 {
		return nil, fmt.Errorf("collect machine metrics: empty result")
	}
	return &metrics[0], nil
}

func (s *Service) VMMetrics(ctx context.Context, ref string) (*models.VMMetrics, error) {
	vm, err := s.vms.Get(ref)
	if err != nil {
		return nil, fmt.Errorf("get VM %s for metrics: %w", ref, err)
	}
	machineMetrics, err := s.MachineMetrics(ctx, vm.MachineID)
	if err != nil {
		return nil, fmt.Errorf("collect backing machine metrics for VM %s: %w", vm.ID, err)
	}
	result := vmMetricsFromVM(s.now().UTC(), *vm, *machineMetrics)
	return &result, nil
}

func (s *Service) nodeMetrics(ctx context.Context, sampledAt time.Time, machines []models.Machine) (*models.NodeMetrics, error) {
	host, err := s.hostMetrics(ctx)
	if err != nil {
		return nil, err
	}
	usage, err := s.disks.Usage()
	if err != nil {
		return nil, fmt.Errorf("collect disk usage for node metrics: %w", err)
	}
	capacity := s.capacity(machines)
	warnings := make([]models.NodeWarning, 0)
	for index := range machines {
		status := s.machines.GatewayDNSProviderStatus(&machines[index])
		if status.Status == "degraded" && status.DesiredRules > 0 {
			warnings = append(warnings, gatewayDNSNodeWarning(machines[index], status))
		}
	}
	return &models.NodeMetrics{
		SampledAt: sampledAt,
		Status:    nodeStatus(capacity, host, len(warnings) > 0),
		Warnings:  warnings,
		Capacity:  capacity,
		Host:      host,
		Disk:      usage.Totals,
	}, nil
}

func gatewayDNSNodeWarning(machine models.Machine, status models.GatewayDNSStatus) models.NodeWarning {
	reason, message := sanitizedGatewayDNSReason(status.Reason)
	return models.NodeWarning{
		Code:      "gateway_dns_degraded",
		Reason:    reason,
		MachineID: machine.ID,
		Machine:   machine.Name,
		Message:   message,
	}
}

func sanitizedGatewayDNSReason(reason string) (string, string) {
	switch reason {
	case "cleanup_pending":
		return reason, "gateway DNS provider cleanup is incomplete"
	case "network_identity_invalid":
		return reason, "gateway DNS public network identity is incomplete or invalid"
	case "provider_status_unavailable":
		return reason, "gateway DNS provider status is unavailable"
	case "network_not_desired":
		return reason, "gateway DNS network intent is not active"
	case "host_status_unavailable":
		return reason, "physical-host network status has not been observed"
	case "tap_ownership_collision":
		return reason, "physical-host TAP identity collides with an unowned link; inspect the conflicting link"
	case "host_network_unready":
		return reason, "physical-host TAP, forwarding, or NAT state is incomplete"
	case "listener_missing":
		return reason, "gateway DNS listener is not running"
	case "firewall_ownership_collision":
		return reason, "gateway DNS firewall ownership namespace collides with unowned state; inspect the reserved chain"
	case "admission_missing":
		return reason, "gateway DNS admission rules are incomplete"
	case "network_reconciliation_failed":
		return reason, "gateway DNS network reconciliation is incomplete"
	case "path_failed":
		return reason, "guest could not reach gateway DNS over UDP and TCP"
	case "external_resolution_failed":
		return reason, "gateway DNS could not complete external recursive resolution"
	case "probe_output_invalid":
		return reason, "gateway DNS probe returned invalid structured output"
	case "probe_unavailable":
		return reason, "gateway DNS readiness probe is unavailable"
	default:
		return "provider_status_unavailable", "gateway DNS provider status is unavailable"
	}
}

func (s *Service) capacity(machines []models.Machine) models.NodeCapacity {
	capacity := models.NodeCapacity{
		MaxRunningVMs:        s.cfg.MaxRunningVMs,
		MaxCPUMillis:         s.cfg.MaxTotalVCPUs * 1000,
		PooledCPULimitMillis: s.cfg.PooledCPUs * 1000,
		MaxMemoryBytes: uint64(max(0, s.cfg.MaxTotalMemoryMB)) *
			1024 * 1024,
		TotalMachines: len(machines),
	}
	for _, machine := range machines {
		if machine.State != models.MachineStateRunning && machine.State != models.MachineStateCreating && machine.State != models.MachineStateStopping {
			continue
		}
		capacity.RunningVMs++
		if machine.State != models.MachineStateStopping {
			capacity.RunningOrStartingMachines++
		}
		capacity.AllocatedCPUMillis += machineCPUMillis(machine)
		if machine.CPU.Kind == "pooled" {
			capacity.PooledVMs++
			capacity.PooledVCPUs += machine.VCPUs
		}
		capacity.AllocatedMemoryBytes += uint64(max(0, machine.MemoryMB)) * 1024 * 1024
	}
	capacity.AvailableRunningVMs = max(0, capacity.MaxRunningVMs-capacity.RunningVMs)
	capacity.AvailableCPUMillis = max(0, capacity.MaxCPUMillis-capacity.AllocatedCPUMillis-capacity.PooledCPULimitMillis)
	if capacity.MaxMemoryBytes > capacity.AllocatedMemoryBytes {
		capacity.AvailableMemoryBytes = capacity.MaxMemoryBytes - capacity.AllocatedMemoryBytes
	}
	return capacity
}

func nodeStatus(capacity models.NodeCapacity, host models.HostMetrics, gatewayDNSDegraded ...bool) string {
	if capacity.MaxRunningVMs > 0 && capacity.RunningVMs > capacity.MaxRunningVMs {
		return "critical"
	}
	if capacity.MaxCPUMillis > 0 && capacity.AllocatedCPUMillis > capacity.MaxCPUMillis {
		return "critical"
	}
	if capacity.MaxMemoryBytes > 0 && capacity.AllocatedMemoryBytes > capacity.MaxMemoryBytes {
		return "critical"
	}
	if host.MemoryTotalBytes > 0 {
		usedRatio := float64(host.MemoryUsedBytes) / float64(host.MemoryTotalBytes)
		if usedRatio >= 0.95 {
			return "critical"
		}
		if usedRatio >= 0.85 {
			return "warn"
		}
	}
	if len(gatewayDNSDegraded) > 0 && gatewayDNSDegraded[0] {
		return "warn"
	}
	return "ok"
}

func (s *Service) hostMetrics(ctx context.Context) (models.HostMetrics, error) {
	firstCPU, err := readProcStat(filepath.Join(s.procRoot, "stat"))
	if err != nil {
		return models.HostMetrics{}, err
	}
	firstNet, err := readProcNetDev(filepath.Join(s.procRoot, "net", "dev"), true)
	if err != nil {
		return models.HostMetrics{}, err
	}
	started := s.now()
	if err := sleepContext(ctx, s.sampleWindow); err != nil {
		return models.HostMetrics{}, err
	}
	elapsed := s.now().Sub(started).Seconds()
	secondCPU, err := readProcStat(filepath.Join(s.procRoot, "stat"))
	if err != nil {
		return models.HostMetrics{}, err
	}
	secondNet, err := readProcNetDev(filepath.Join(s.procRoot, "net", "dev"), true)
	if err != nil {
		return models.HostMetrics{}, err
	}
	mem, err := readMemInfo(filepath.Join(s.procRoot, "meminfo"))
	if err != nil {
		return models.HostMetrics{}, err
	}
	load, err := readLoadAverage(filepath.Join(s.procRoot, "loadavg"))
	if err != nil {
		return models.HostMetrics{}, err
	}
	return models.HostMetrics{
		CPUPercent:              cpuPercent(firstCPU, secondCPU),
		LoadAverage:             load,
		MemoryTotalBytes:        mem.totalBytes,
		MemoryAvailableBytes:    mem.availableBytes,
		MemoryUsedBytes:         mem.usedBytes(),
		NetworkRxBytesPerSecond: rate(firstNet.rxBytes, secondNet.rxBytes, elapsed),
		NetworkTxBytesPerSecond: rate(firstNet.txBytes, secondNet.txBytes, elapsed),
	}, nil
}

func (s *Service) machineMetricsFor(ctx context.Context, sampledAt time.Time, machines []models.Machine) ([]models.MachineMetrics, error) {
	first := make(map[string]machineSample, len(machines))
	for _, machine := range machines {
		first[machine.ID] = s.sampleMachine(machine)
	}
	started := s.now()
	if err := sleepContext(ctx, s.sampleWindow); err != nil {
		return nil, err
	}
	elapsed := s.now().Sub(started).Seconds()
	result := make([]models.MachineMetrics, 0, len(machines))
	for _, machine := range machines {
		second := s.sampleMachine(machine)
		result = append(result, s.machineMetrics(sampledAt, machine, first[machine.ID], second, elapsed))
	}
	return result, nil
}

func (s *Service) sampleMachine(machine models.Machine) machineSample {
	sample := machineSample{
		runtime:      s.machines.RuntimeSnapshot(machine.ID),
		memoryStatus: memoryMetricUnavailable,
	}
	if machine.State != models.MachineStateRunning && machine.State != models.MachineStateCreating {
		return sample
	}
	if sample.runtime.Running {
		sample.cpuUsageUsec, _ = readCgroupCPUUsage(filepath.Join(sample.runtime.CgroupPath, "cpu.stat"))
		memoryBytes, err := readUintFile(filepath.Join(sample.runtime.CgroupPath, "memory.current"))
		if err == nil {
			sample.memoryBytes = memoryBytes
			sample.memoryObserved = true
			sample.memoryStatus = memoryMetricMeasured
		} else {
			sample.memoryError = err.Error()
		}
		sample.ioReadBytes, sample.ioWriteBytes, _ = readCgroupIO(filepath.Join(sample.runtime.CgroupPath, "io.stat"))
	}
	sample.netRxBytes, sample.netTxBytes = s.machineNetworkBytes(machine)
	return sample
}

func (s *Service) machineNetworkBytes(machine models.Machine) (uint64, uint64) {
	if machine.TAPDevice == "" {
		return 0, 0
	}
	stats, err := readSysNetStats(filepath.Join(s.sysRoot, "class", "net", machine.TAPDevice, "statistics"))
	if err != nil {
		return 0, 0
	}
	return stats.rxBytes, stats.txBytes
}

func (s *Service) machineMetrics(sampledAt time.Time, machine models.Machine, first, second machineSample, elapsed float64) models.MachineMetrics {
	cpuLimit := machineCPUMillis(machine)
	if machine.CPU.Kind == "pooled" {
		cpuLimit = min(machine.VCPUs, s.cfg.PooledCPUs) * 1000
	}
	metrics := models.MachineMetrics{
		MachineID:       machine.ID,
		Name:            machine.Name,
		Backend:         machine.Backend,
		GuestOS:         machine.GuestOS,
		State:           machine.State,
		HealthStatus:    machineHealth(machine, second.runtime),
		CPULimitMillis:  cpuLimit,
		MemoryUsedBytes: second.memoryBytes,
		MemoryLimitBytes: uint64(max(0, machine.MemoryMB)) *
			1024 * 1024,
		MemoryObserved:  second.memoryObserved,
		MemoryStatus:    second.memoryStatus,
		MemoryError:     second.memoryError,
		RuntimeObserved: second.runtime.Running,
		RuntimePID:      second.runtime.PID,
		IP:              machine.IP,
		SampledAt:       sampledAt,
	}
	if s.machines != nil {
		metrics.GatewayDNS = s.machines.GatewayDNSProviderStatus(&machine)
	} else {
		metrics.GatewayDNS = models.GatewayDNSStatus{Status: "unknown", Reason: "not_applicable"}
	}
	metrics.Error = sanitizedMachineMetricsError(machine, metrics.GatewayDNS)
	if first.cpuUsageUsec > 0 && second.cpuUsageUsec >= first.cpuUsageUsec && cpuLimit > 0 {
		elapsedUsec := elapsed * 1_000_000
		limitCores := float64(cpuLimit) / 1000
		metrics.CPUPercent = ((float64(second.cpuUsageUsec-first.cpuUsageUsec) / elapsedUsec) / limitCores) * 100
	}
	metrics.DiskReadBytesPerSecond = rate(first.ioReadBytes, second.ioReadBytes, elapsed)
	metrics.DiskWriteBytesPerSecond = rate(first.ioWriteBytes, second.ioWriteBytes, elapsed)
	metrics.NetworkRxBytesPerSecond = rate(first.netRxBytes, second.netRxBytes, elapsed)
	metrics.NetworkTxBytesPerSecond = rate(first.netTxBytes, second.netTxBytes, elapsed)
	if second.runtime.PID > 0 {
		if seconds, err := processUptimeSeconds(s.procRoot, second.runtime.PID); err == nil {
			metrics.UptimeSeconds = seconds
		}
	}
	return metrics
}

func sanitizedMachineMetricsError(machine models.Machine, gatewayDNS models.GatewayDNSStatus) string {
	if strings.TrimSpace(machine.Error) == "" {
		return ""
	}
	if !machineErrorHasProviderNetworkContext(machine) {
		// Preserve the established non-provider error contract. Provider-derived
		// errors take the classified path below and never cross the metrics API raw.
		return machine.Error
	}

	_, providerMessage := sanitizedGatewayDNSReason(gatewayDNS.Reason)
	message := providerMessage
	for _, diagnostic := range machine.Diagnostics {
		if diagnostic.Category == "provider_network" {
			continue
		}
		if diagnostic.Category == "machine_boot" {
			message = "machine boot failed; " + providerMessage
		} else {
			message = "machine lifecycle failed; " + providerMessage
		}
		break
	}
	return boundSanitizedProviderMetricsError(message)
}

func machineErrorHasProviderNetworkContext(machine models.Machine) bool {
	for _, diagnostic := range machine.Diagnostics {
		if diagnostic.Category == "provider_network" {
			return true
		}
	}
	lower := strings.ToLower(machine.Error)
	for _, marker := range []string{
		"gateway dns",
		"guest dns",
		"dns proxy",
		"provider network",
		"public network",
		"allocate network",
		"network cleanup",
		"setup nat",
		"teardown nat",
		"iptables",
		"tap collision",
		"link collision",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func boundSanitizedProviderMetricsError(value string) string {
	value = strings.Join(strings.Fields(value), " ")
	runes := []rune(value)
	if len(runes) <= maxSanitizedProviderMetricsErrorRunes {
		return value
	}
	return string(runes[:maxSanitizedProviderMetricsErrorRunes-3]) + "..."
}

func vmMetricsFromVM(sampledAt time.Time, vm models.VM, machineMetrics models.MachineMetrics) models.VMMetrics {
	return models.VMMetrics{
		ID:        vm.ID,
		Name:      vm.Name,
		Profile:   vm.Profile,
		MachineID: vm.MachineID,
		State:     vm.State,
		Power:     vm.Power,
		Machine:   machineMetrics,
		SampledAt: sampledAt,
	}
}

func machineHealth(machine models.Machine, runtime machine.RuntimeSnapshot) string {
	switch machine.State {
	case models.MachineStateRunning:
		if machine.Backend == "firecracker" && !runtime.Running {
			return "degraded"
		}
		return "running"
	case models.MachineStateCreating:
		if machine.Backend == "firecracker" && !runtime.Running {
			return "starting"
		}
		return "running"
	case models.MachineStateStopping:
		return "stopping"
	case models.MachineStateStopped:
		return "stopped"
	case models.MachineStateError:
		return "error"
	default:
		return string(machine.State)
	}
}

func machineCPUMillis(machine models.Machine) int {
	if machine.CPU.Kind == "pooled" {
		return 0
	}
	if machine.CPU.Millis > 0 {
		return machine.CPU.Millis
	}
	if machine.VCPUs > 0 {
		return machine.VCPUs * 1000
	}
	return 1000
}

type machineSample struct {
	runtime        machine.RuntimeSnapshot
	cpuUsageUsec   uint64
	memoryBytes    uint64
	ioReadBytes    uint64
	ioWriteBytes   uint64
	netRxBytes     uint64
	netTxBytes     uint64
	memoryObserved bool
	memoryStatus   string
	memoryError    string
}

type cpuSample struct {
	idle  uint64
	total uint64
}

func readProcStat(path string) (cpuSample, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return cpuSample{}, fmt.Errorf("read proc stat: %w", err)
	}
	line, _, _ := strings.Cut(string(data), "\n")
	fields := strings.Fields(line)
	if len(fields) < 5 || fields[0] != "cpu" {
		return cpuSample{}, fmt.Errorf("parse proc stat: missing aggregate cpu line")
	}
	var values []uint64
	for _, field := range fields[1:] {
		value, err := strconv.ParseUint(field, 10, 64)
		if err != nil {
			return cpuSample{}, fmt.Errorf("parse proc stat cpu field %q: %w", field, err)
		}
		values = append(values, value)
	}
	idle := values[3]
	if len(values) > 4 {
		idle += values[4]
	}
	var total uint64
	for _, value := range values {
		total += value
	}
	return cpuSample{idle: idle, total: total}, nil
}

func cpuPercent(first, second cpuSample) float64 {
	if second.total < first.total || second.idle < first.idle {
		return 0
	}
	totalDelta := second.total - first.total
	if totalDelta == 0 {
		return 0
	}
	idleDelta := second.idle - first.idle
	return (float64(totalDelta-idleDelta) / float64(totalDelta)) * 100
}

type memInfo struct {
	totalBytes     uint64
	availableBytes uint64
}

func (m memInfo) usedBytes() uint64 {
	if m.totalBytes > m.availableBytes {
		return m.totalBytes - m.availableBytes
	}
	return 0
}

func readMemInfo(path string) (memInfo, error) {
	file, err := os.Open(path)
	if err != nil {
		return memInfo{}, fmt.Errorf("read meminfo: %w", err)
	}
	defer file.Close()
	var info memInfo
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}
		value, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			continue
		}
		bytes := value * 1024
		switch fields[0] {
		case "MemTotal:":
			info.totalBytes = bytes
		case "MemAvailable:":
			info.availableBytes = bytes
		}
	}
	if err := scanner.Err(); err != nil {
		return memInfo{}, fmt.Errorf("scan meminfo: %w", err)
	}
	if info.totalBytes == 0 {
		return memInfo{}, fmt.Errorf("parse meminfo: missing MemTotal")
	}
	return info, nil
}

func readLoadAverage(path string) ([]float64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read loadavg: %w", err)
	}
	fields := strings.Fields(string(data))
	if len(fields) < 3 {
		return nil, fmt.Errorf("parse loadavg: expected at least 3 fields")
	}
	load := make([]float64, 0, 3)
	for _, field := range fields[:3] {
		value, err := strconv.ParseFloat(field, 64)
		if err != nil {
			return nil, fmt.Errorf("parse loadavg field %q: %w", field, err)
		}
		load = append(load, value)
	}
	return load, nil
}

type netBytes struct {
	rxBytes uint64
	txBytes uint64
}

func readProcNetDev(path string, excludeLoopback bool) (netBytes, error) {
	file, err := os.Open(path)
	if err != nil {
		return netBytes{}, fmt.Errorf("read net dev: %w", err)
	}
	defer file.Close()
	var total netBytes
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.Contains(line, ":") {
			continue
		}
		iface, rest, _ := strings.Cut(line, ":")
		iface = strings.TrimSpace(iface)
		if excludeLoopback && iface == "lo" {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) < 16 {
			continue
		}
		rx, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			continue
		}
		tx, err := strconv.ParseUint(fields[8], 10, 64)
		if err != nil {
			continue
		}
		total.rxBytes += rx
		total.txBytes += tx
	}
	if err := scanner.Err(); err != nil {
		return netBytes{}, fmt.Errorf("scan net dev: %w", err)
	}
	return total, nil
}

func readSysNetStats(dir string) (netBytes, error) {
	rx, err := readUintFile(filepath.Join(dir, "rx_bytes"))
	if err != nil {
		return netBytes{}, err
	}
	tx, err := readUintFile(filepath.Join(dir, "tx_bytes"))
	if err != nil {
		return netBytes{}, err
	}
	return netBytes{rxBytes: rx, txBytes: tx}, nil
}

func readCgroupCPUUsage(path string) (uint64, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("open cgroup cpu stat %s: %w", path, err)
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 2 && fields[0] == "usage_usec" {
			usage, err := strconv.ParseUint(fields[1], 10, 64)
			if err != nil {
				return 0, fmt.Errorf("parse cgroup cpu usage %s: %w", path, err)
			}
			return usage, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return 0, fmt.Errorf("scan cgroup cpu stat %s: %w", path, err)
	}
	return 0, nil
}

func readCgroupIO(path string) (uint64, uint64, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, 0, fmt.Errorf("open cgroup io stat %s: %w", path, err)
	}
	defer file.Close()
	var readBytes uint64
	var writeBytes uint64
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		for _, field := range strings.Fields(scanner.Text()) {
			key, value, ok := strings.Cut(field, "=")
			if !ok {
				continue
			}
			parsed, err := strconv.ParseUint(value, 10, 64)
			if err != nil {
				continue
			}
			switch key {
			case "rbytes":
				readBytes += parsed
			case "wbytes":
				writeBytes += parsed
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return 0, 0, fmt.Errorf("scan cgroup io stat %s: %w", path, err)
	}
	return readBytes, writeBytes, nil
}

func readUintFile(path string) (uint64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("read uint file %s: %w", path, err)
	}
	value, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse uint file %s: %w", path, err)
	}
	return value, nil
}

func processUptimeSeconds(procRoot string, pid int) (int64, error) {
	statPath := filepath.Join(procRoot, strconv.Itoa(pid), "stat")
	data, err := os.ReadFile(statPath)
	if err != nil {
		return 0, fmt.Errorf("read process stat %s: %w", statPath, err)
	}
	startTicks, err := processStartTicksFromStat(string(data))
	if err != nil {
		return 0, fmt.Errorf("parse process start ticks %s: %w", statPath, err)
	}
	uptimePath := filepath.Join(procRoot, "uptime")
	uptimeData, err := os.ReadFile(uptimePath)
	if err != nil {
		return 0, fmt.Errorf("read uptime %s: %w", uptimePath, err)
	}
	uptimeFields := strings.Fields(string(uptimeData))
	if len(uptimeFields) == 0 {
		return 0, fmt.Errorf("parse uptime: empty")
	}
	uptimeSeconds, err := strconv.ParseFloat(uptimeFields[0], 64)
	if err != nil {
		return 0, fmt.Errorf("parse uptime %s: %w", uptimePath, err)
	}
	const clockTicksPerSecond = 100.0
	seconds := uptimeSeconds - (float64(startTicks) / clockTicksPerSecond)
	if seconds < 0 || math.IsNaN(seconds) {
		return 0, nil
	}
	return int64(seconds), nil
}

func processStartTicksFromStat(data string) (uint64, error) {
	commEnd := strings.LastIndex(data, ")")
	if commEnd < 0 || commEnd+2 >= len(data) {
		return 0, fmt.Errorf("parse process stat: missing command field")
	}
	fields := strings.Fields(data[commEnd+2:])
	if len(fields) < 20 {
		return 0, fmt.Errorf("parse process stat: expected at least 20 fields after command")
	}
	startTicks, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse starttime field: %w", err)
	}
	return startTicks, nil
}

func rate(first, second uint64, elapsedSeconds float64) float64 {
	if elapsedSeconds <= 0 || second < first {
		return 0
	}
	return float64(second-first) / elapsedSeconds
}

func sleepContext(ctx context.Context, duration time.Duration) error {
	if duration <= 0 {
		return nil
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return fmt.Errorf("metrics sample canceled: %w", ctx.Err())
	case <-timer.C:
		return nil
	}
}
