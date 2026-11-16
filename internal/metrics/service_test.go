package metrics

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gfdelarue/kiln-vm/internal/config"
	"github.com/gfdelarue/kiln-vm/internal/disk"
	"github.com/gfdelarue/kiln-vm/internal/image"
	"github.com/gfdelarue/kiln-vm/internal/machine"
	"github.com/gfdelarue/kiln-vm/internal/models"
	"github.com/gfdelarue/kiln-vm/internal/store"
	vmservice "github.com/gfdelarue/kiln-vm/internal/vm"
)

func TestSummaryUsesRealStoreAndFilesystem(t *testing.T) {
	dataDir := t.TempDir()
	db, err := store.Open(dataDir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	now := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	imageID := "img-metrics"
	machineID := "machine-metrics"
	if err := db.CreateImage(&models.Image{
		ID:         imageID,
		Name:       "metrics-image",
		Tag:        "test",
		SizeMB:     128,
		RootfsPath: filepath.Join(dataDir, "images", imageID, "rootfs.ext4"),
		CreatedAt:  now,
	}); err != nil {
		t.Fatalf("create image: %v", err)
	}
	if err := db.CreateMachine(&models.Machine{
		ID:        machineID,
		Name:      "metrics-machine",
		Backend:   "firecracker",
		GuestOS:   "linux",
		ImageID:   imageID,
		State:     models.MachineStateRunning,
		CPU:       models.CPUConfig{Kind: "shared", Millis: 1500, VCPUs: 2},
		VCPUs:     2,
		MemoryMB:  768,
		IP:        "172.16.0.2",
		TAPDevice: "kiln-vm-test0",
		CreatedAt: now,
		UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create machine: %v", err)
	}
	if err := db.CreateVM(&models.VMRecord{
		ID:         "vm-metrics",
		Name:       "metrics-vm",
		Profile:    "ubuntu",
		MachineID:  machineID,
		ImageID:    imageID,
		DiskSizeMB: 128,
		Power: models.VMPower{Policy: models.VMPowerPolicy{
			Mode: models.VMPowerModeAlways,
		}},
		CreatedAt: now,
		UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create vm: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dataDir, "machines", machineID), 0750); err != nil {
		t.Fatalf("create machine dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "machines", machineID, "rootfs.ext4"), []byte("rootfs"), 0640); err != nil {
		t.Fatalf("write rootfs: %v", err)
	}

	procRoot := t.TempDir()
	sysRoot := t.TempDir()
	writeTestFile(t, procRoot, "stat", "cpu  100 0 100 700 0 0 0 0 0 0\n")
	writeTestFile(t, procRoot, "meminfo", "MemTotal:       8192 kB\nMemAvailable:   4096 kB\n")
	writeTestFile(t, procRoot, "loadavg", "0.10 0.20 0.30 1/100 123\n")
	writeTestFile(t, procRoot, filepath.Join("net", "dev"), "Inter-| Receive | Transmit\n face |bytes packets errs drop fifo frame compressed multicast|bytes packets errs drop fifo colls carrier compressed\neth0: 1000 0 0 0 0 0 0 0 2000 0 0 0 0 0 0 0\n")
	writeTestFile(t, sysRoot, filepath.Join("class", "net", "kiln-vm-test0", "statistics", "rx_bytes"), "1234\n")
	writeTestFile(t, sysRoot, filepath.Join("class", "net", "kiln-vm-test0", "statistics", "tx_bytes"), "5678\n")

	cfg := &config.Config{
		DataDir:          dataDir,
		BridgeCIDR:       "172.16.0.0/16",
		MaxRunningVMs:    4,
		MaxTotalVCPUs:    4,
		MaxTotalMemoryMB: 4096,
	}
	mgr := machine.NewManager(cfg, db)
	imgService := image.NewService(db, dataDir, "")
	vmService := vmservice.NewService(db, imgService, mgr)
	diskService := disk.NewService(db, dataDir, "")
	service := NewService(cfg, mgr, vmService, diskService)
	service.procRoot = procRoot
	service.sysRoot = sysRoot
	service.sampleWindow = 0
	service.now = func() time.Time { return now }

	summary, err := service.Summary(context.Background())
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if summary.Node.Status != "ok" {
		t.Fatalf("node status = %q, want ok", summary.Node.Status)
	}
	if summary.Node.Capacity.RunningVMs != 1 || summary.Node.Capacity.AllocatedCPUMillis != 1500 {
		t.Fatalf("capacity = %+v, want one 1500m running VM", summary.Node.Capacity)
	}
	if summary.Node.Capacity.AllocatedMemoryBytes != 768*1024*1024 {
		t.Fatalf("allocated memory = %d, want 768MiB", summary.Node.Capacity.AllocatedMemoryBytes)
	}
	if len(summary.Machines) != 1 {
		t.Fatalf("machines len = %d, want 1", len(summary.Machines))
	}
	if summary.Machines[0].HealthStatus != "degraded" {
		t.Fatalf("machine health = %q, want degraded when DB says firecracker running but runtime is absent", summary.Machines[0].HealthStatus)
	}
	if summary.Machines[0].MemoryObserved || summary.Machines[0].MemoryStatus != memoryMetricUnavailable {
		t.Fatalf("machine memory availability = observed:%t status:%q, want unavailable when runtime is absent", summary.Machines[0].MemoryObserved, summary.Machines[0].MemoryStatus)
	}
	if summary.Machines[0].NetworkRxBytesPerSecond != 0 || summary.Machines[0].NetworkTxBytesPerSecond != 0 {
		t.Fatalf("network rates with zero sample window = rx %f tx %f, want 0", summary.Machines[0].NetworkRxBytesPerSecond, summary.Machines[0].NetworkTxBytesPerSecond)
	}
	if len(summary.VMs) != 1 || summary.VMs[0].Name != "metrics-vm" {
		t.Fatalf("VM metrics = %+v, want metrics-vm", summary.VMs)
	}
	if summary.VMs[0].Machine.MachineID != machineID {
		t.Fatalf("VM machine metrics = %+v, want machine %s", summary.VMs[0].Machine, machineID)
	}

	const rawCleanupError = "release public network: iptables stderr private.example 192.0.2.53"
	if err := db.UpdateMachineStateWithDiagnostics(machineID, models.MachineStateError, rawCleanupError, []models.Diagnostic{{
		Category:  "provider_network",
		Phase:     "cleanup",
		Retryable: true,
		Error:     rawCleanupError,
	}}); err != nil {
		t.Fatalf("record provider cleanup failure: %v", err)
	}

	degraded, err := service.Summary(context.Background())
	if err != nil {
		t.Fatalf("summary with provider cleanup warning: %v", err)
	}
	if degraded.Node.Status != "warn" || len(degraded.Node.Warnings) != 1 {
		t.Fatalf("node warning state = status:%q warnings:%+v, want one warning", degraded.Node.Status, degraded.Node.Warnings)
	}
	warning := degraded.Node.Warnings[0]
	if warning.Code != "gateway_dns_degraded" || warning.Reason != "cleanup_pending" || warning.MachineID != machineID {
		t.Fatalf("node warning = %+v, want cleanup_pending for %s", warning, machineID)
	}
	if len(degraded.Machines) != 1 || degraded.Machines[0].GatewayDNS.Reason != "cleanup_pending" {
		t.Fatalf("machine Gateway DNS metrics = %+v, want cleanup_pending", degraded.Machines)
	}
	encoded, err := json.Marshal(degraded)
	if err != nil {
		t.Fatalf("marshal degraded summary: %v", err)
	}
	for _, private := range []string{"iptables stderr", "private.example", "192.0.2.53"} {
		if strings.Contains(string(encoded), private) {
			t.Fatalf("degraded summary JSON leaked provider value %q: %s", private, encoded)
		}
	}
}

func TestMachineMetricsDistinguishesUnavailableAndMeasuredZeroMemory(t *testing.T) {
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	service := &Service{}
	vmMachine := models.Machine{
		ID:       "machine-memory",
		Name:     "memory-vm",
		Backend:  "firecracker",
		GuestOS:  "linux",
		State:    models.MachineStateRunning,
		CPU:      models.CPUConfig{Kind: "shared", Millis: 1000, VCPUs: 1},
		MemoryMB: 1024,
	}

	unavailable := service.machineMetrics(now, vmMachine, machineSample{}, machineSample{
		runtime:      machine.RuntimeSnapshot{MachineID: vmMachine.ID, Running: true, PID: 123, CgroupPath: "/missing"},
		memoryStatus: memoryMetricUnavailable,
		memoryError:  "open /missing/memory.current: no such file or directory",
	}, 1)
	if unavailable.MemoryObserved || unavailable.MemoryStatus != memoryMetricUnavailable || unavailable.MemoryError == "" {
		t.Fatalf("unavailable memory metrics = %+v, want unavailable with error", unavailable)
	}
	if unavailable.MemoryUsedBytes != 0 {
		t.Fatalf("unavailable memory used = %d, want zero value with observed=false", unavailable.MemoryUsedBytes)
	}

	measuredZero := service.machineMetrics(now, vmMachine, machineSample{}, machineSample{
		runtime:        machine.RuntimeSnapshot{MachineID: vmMachine.ID, Running: true, PID: 123, CgroupPath: "/sys/fs/cgroup/machine-memory"},
		memoryObserved: true,
		memoryStatus:   memoryMetricMeasured,
		memoryBytes:    0,
	}, 1)
	if !measuredZero.MemoryObserved || measuredZero.MemoryStatus != memoryMetricMeasured || measuredZero.MemoryUsedBytes != 0 || measuredZero.MemoryError != "" {
		t.Fatalf("measured-zero memory metrics = %+v, want observed measured zero", measuredZero)
	}
}

func TestNodeStatusWarnsForDegradedDesiredGatewayDNS(t *testing.T) {
	capacity := models.NodeCapacity{MaxRunningVMs: 4, MaxCPUMillis: 4000, MaxMemoryBytes: 4 << 30}
	host := models.HostMetrics{MemoryTotalBytes: 4 << 30, MemoryUsedBytes: 1 << 30}
	if got := nodeStatus(capacity, host, true); got != "warn" {
		t.Fatalf("nodeStatus with degraded gateway DNS = %q, want warn", got)
	}
	if got := nodeStatus(capacity, host, false); got != "ok" {
		t.Fatalf("nodeStatus with ready gateway DNS = %q, want ok", got)
	}

	overCapacity := capacity
	overCapacity.RunningVMs = 5
	if got := nodeStatus(overCapacity, host, true); got != "critical" {
		t.Fatalf("nodeStatus over capacity with degraded gateway DNS = %q, want critical", got)
	}
}

func TestMachineMetricsJSONRedactsProviderNetworkErrors(t *testing.T) {
	rawProviderError := "allocate network: iptables -S exposed resolver 192.0.2.53 query private.example " + strings.Repeat("secret-output ", 100)
	machineModel := models.Machine{
		ID:      "machine-provider-error",
		Name:    "provider-error-vm",
		Backend: "firecracker",
		Error:   rawProviderError,
		Diagnostics: []models.Diagnostic{
			{Category: "machine_boot", Phase: "network"},
			{Category: "provider_network", Phase: "cleanup", Retryable: true},
		},
	}

	metrics := (&Service{}).machineMetrics(time.Now(), machineModel, machineSample{}, machineSample{}, 1)
	if metrics.Error == "" || !strings.Contains(metrics.Error, "machine boot failed") || !strings.Contains(metrics.Error, "gateway DNS provider status is unavailable") {
		t.Fatalf("sanitized machine error = %q, want useful bounded classification", metrics.Error)
	}
	if len([]rune(metrics.Error)) > maxSanitizedProviderMetricsErrorRunes {
		t.Fatalf("sanitized machine error length = %d, max %d", len([]rune(metrics.Error)), maxSanitizedProviderMetricsErrorRunes)
	}
	encoded, err := json.Marshal(metrics)
	if err != nil {
		t.Fatalf("marshal machine metrics: %v", err)
	}
	serialized := string(encoded)
	for _, private := range []string{"iptables", "192.0.2.53", "private.example", "secret-output"} {
		if strings.Contains(serialized, private) {
			t.Fatalf("machine metrics JSON leaked provider value %q: %s", private, serialized)
		}
	}
}

func TestMachineMetricsPreservesNonProviderErrorSemantics(t *testing.T) {
	const nonProviderError = "rootfs missing at /expected/provider-visible/path"
	machineModel := models.Machine{
		ID:      "machine-rootfs-error",
		Backend: "firecracker",
		Error:   nonProviderError,
		Diagnostics: []models.Diagnostic{{
			Category: "machine_boot",
			Phase:    "rootfs",
		}},
	}

	metrics := (&Service{}).machineMetrics(time.Now(), machineModel, machineSample{}, machineSample{}, 1)
	if metrics.Error != nonProviderError {
		t.Fatalf("non-provider machine error = %q, want exact %q", metrics.Error, nonProviderError)
	}
}

func TestSanitizedMachineMetricsErrorUsesGatewayDNSClassification(t *testing.T) {
	machineModel := models.Machine{
		Error: "release public network: raw private host output",
		Diagnostics: []models.Diagnostic{{
			Category:  "provider_network",
			Phase:     "cleanup",
			Retryable: true,
		}},
	}
	got := sanitizedMachineMetricsError(machineModel, models.GatewayDNSStatus{Reason: "cleanup_pending"})
	if got != "gateway DNS provider cleanup is incomplete" {
		t.Fatalf("sanitized provider error = %q, want cleanup classification", got)
	}
	if strings.Contains(got, "raw private") {
		t.Fatalf("sanitized provider error leaked raw detail: %q", got)
	}
}

func TestGatewayDNSNodeWarningUsesAllowlistedReasonAndMessage(t *testing.T) {
	machineModel := models.Machine{ID: "machine-api", Name: "api-vm"}
	tests := []struct {
		name        string
		status      models.GatewayDNSStatus
		wantReason  string
		wantMessage string
	}{
		{
			name:        "known collision",
			status:      models.GatewayDNSStatus{Reason: "tap_ownership_collision", LastError: "raw ip link output secret"},
			wantReason:  "tap_ownership_collision",
			wantMessage: "physical-host TAP identity collides with an unowned link; inspect the conflicting link",
		},
		{
			name:        "unknown reason fails closed",
			status:      models.GatewayDNSStatus{Reason: "attacker-controlled-reason", LastError: "raw resolver output private.example"},
			wantReason:  "provider_status_unavailable",
			wantMessage: "gateway DNS provider status is unavailable",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			warning := gatewayDNSNodeWarning(machineModel, test.status)
			if warning.Code != "gateway_dns_degraded" || warning.Reason != test.wantReason || warning.Message != test.wantMessage {
				t.Fatalf("warning = %+v, want reason %s", warning, test.wantReason)
			}
			if warning.MachineID != machineModel.ID || warning.Machine != machineModel.Name {
				t.Fatalf("warning machine identity = %+v, want %s/%s", warning, machineModel.ID, machineModel.Name)
			}
			encoded, err := json.Marshal(warning)
			if err != nil {
				t.Fatalf("marshal warning: %v", err)
			}
			serialized := string(encoded)
			for _, private := range []string{"secret", "private.example", "attacker-controlled-reason", "raw resolver", "raw ip"} {
				if strings.Contains(serialized, private) {
					t.Fatalf("node warning leaked raw provider value %q: %s", private, serialized)
				}
			}
		})
	}
}

func TestKernelCounterParsers(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "cpu.stat", "usage_usec 250000\nuser_usec 100000\nsystem_usec 150000\n")
	usage, err := readCgroupCPUUsage(filepath.Join(dir, "cpu.stat"))
	if err != nil {
		t.Fatalf("readCgroupCPUUsage: %v", err)
	}
	if usage != 250000 {
		t.Fatalf("usage = %d, want 250000", usage)
	}

	writeTestFile(t, dir, "io.stat", "8:0 rbytes=1024 wbytes=2048 rios=1 wios=2\n8:16 rbytes=512 wbytes=256 rios=1 wios=1\n")
	readBytes, writeBytes, err := readCgroupIO(filepath.Join(dir, "io.stat"))
	if err != nil {
		t.Fatalf("readCgroupIO: %v", err)
	}
	if readBytes != 1536 || writeBytes != 2304 {
		t.Fatalf("io = read %d write %d, want 1536/2304", readBytes, writeBytes)
	}

	writeTestFile(t, dir, "dev", "Inter-| Receive | Transmit\n face |bytes packets errs drop fifo frame compressed multicast|bytes packets errs drop fifo colls carrier compressed\nlo: 100 0 0 0 0 0 0 0 200 0 0 0 0 0 0 0\neth0: 1000 0 0 0 0 0 0 0 2000 0 0 0 0 0 0 0\n")
	net, err := readProcNetDev(filepath.Join(dir, "dev"), true)
	if err != nil {
		t.Fatalf("readProcNetDev: %v", err)
	}
	if net.rxBytes != 1000 || net.txBytes != 2000 {
		t.Fatalf("net = %+v, want eth0 only", net)
	}

	startTicks, err := processStartTicksFromStat("123 (name with spaces) S 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 4242 20 21\n")
	if err != nil {
		t.Fatalf("processStartTicksFromStat: %v", err)
	}
	if startTicks != 4242 {
		t.Fatalf("start ticks = %d, want 4242", startTicks)
	}
}

func writeTestFile(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0640); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestPooledCPUMetricsUsePoolCapacity(t *testing.T) {
	s := &Service{cfg: &config.Config{PooledCPUs: 18}}
	m := models.Machine{VCPUs: 18, CPU: models.CPUConfig{Kind: "pooled", Millis: 18000, VCPUs: 18}}
	result := s.machineMetrics(time.Now(), m, machineSample{cpuUsageUsec: 1}, machineSample{cpuUsageUsec: 9000001}, 1)
	if result.CPULimitMillis != 18000 || result.CPUPercent != 50 {
		t.Fatalf("pooled metrics: %+v", result)
	}
}
