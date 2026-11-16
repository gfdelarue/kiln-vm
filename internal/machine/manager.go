package machine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/google/uuid"

	"github.com/gfdelarue/kiln-vm/internal/apierr"
	"github.com/gfdelarue/kiln-vm/internal/config"
	"github.com/gfdelarue/kiln-vm/internal/execution"
	"github.com/gfdelarue/kiln-vm/internal/guestagent"
	"github.com/gfdelarue/kiln-vm/internal/hostfs"
	"github.com/gfdelarue/kiln-vm/internal/models"
	"github.com/gfdelarue/kiln-vm/internal/refid"
)

// VMProcess tracks a running Firecracker VM.
type VMProcess struct {
	CgroupPath   string
	Machine      *models.Machine
	Cmd          *exec.Cmd
	Cancel       func()        // cancel context to stop VM (SIGKILL)
	ExitCh       chan struct{} // closed when process exits
	ExpectedStop atomic.Bool
}

type RuntimeSnapshot struct {
	MachineID  string
	Running    bool
	PID        int
	CgroupPath string
}

type publicNetworkReconciler interface {
	Reconcile() error
}

// GracefulStop sends SIGTERM and waits up to timeout for the process to exit,
// falling back to SIGKILL (via context cancellation) if needed.
func (vp *VMProcess) GracefulStop(timeout time.Duration) {
	vp.ExpectedStop.Store(true)
	if vp.Cmd != nil && vp.Cmd.Process != nil {
		vp.Cmd.Process.Signal(syscall.SIGTERM)
	}
	select {
	case <-vp.ExitCh:
	case <-time.After(timeout):
		vp.Cancel()
		<-vp.ExitCh
	}
}

func (vp *VMProcess) Wait(timeout time.Duration) bool {
	select {
	case <-vp.ExitCh:
		return true
	case <-time.After(timeout):
		return false
	}
}

// Manager handles VM lifecycle.
type Manager struct {
	cfg                 *config.Config
	db                  MachineStore
	network             NetworkAllocator
	gatewayDNSReadiness *gatewayDNSReadinessController
	processes           sync.Map // map[string]*VMProcess
	lifecycleMu         sync.Mutex
	onStart             []func(machineID string)
	onStop              []func(machineID string)
	onDelete            []func(machineID string)
	vsockMu             sync.Mutex
	nextVSockCID        uint32
	limits              ResourceLimits
	beforeAsyncBoot     func(machineID string) // deterministic lifecycle-race test seam
	bootGenerations     map[string]uint64      // guarded by lifecycleMu
	nextBootGeneration  uint64                 // guarded by lifecycleMu
}

func NewManager(cfg *config.Config, db MachineStore) *Manager {
	// Query DB for the highest allocated network index so we don't
	// collide with TAP devices from a previous daemon run.
	maxIdx, err := db.MaxAllocatedNetworkIndex()
	if err != nil {
		slog.Error("failed to query max network index, starting from 1", "error", err)
		maxIdx = 0
	}

	maxVSockCID, err := db.MaxAllocatedVSockCID()
	if err != nil {
		slog.Error("failed to query max vsock cid, starting from 3", "error", err)
		maxVSockCID = 2
	}
	if maxVSockCID < 2 {
		maxVSockCID = 2
	}

	manager := &Manager{
		cfg:          cfg,
		db:           db,
		network:      NewNetworkManagerWithDNSDataDir(cfg.BridgeCIDR, maxIdx+1, NewDNSProxyManager(cfg.GuestDNSProxyEnabled, cfg.BridgeCIDR, cfg.GuestDNSProxyUpstreams), cfg.DataDir),
		nextVSockCID: maxVSockCID + 1,
		limits: ResourceLimits{
			MaxRunningVMs:    cfg.MaxRunningVMs,
			MaxTotalVCPUs:    cfg.MaxTotalVCPUs,
			PooledCPUs:       cfg.PooledCPUs,
			MaxTotalMemoryMB: cfg.MaxTotalMemoryMB,
		},
		bootGenerations: make(map[string]uint64),
	}
	manager.gatewayDNSReadiness = newGatewayDNSReadinessController(
		managerGatewayDNSRuntime{manager: manager},
		cfg.AgentBinPath,
		cfg.GuestDNSProbeName,
	)
	manager.gatewayDNSReadiness.bridgeCIDR = cfg.BridgeCIDR
	return manager
}

type managerGatewayDNSRuntime struct {
	manager *Manager
}

func (r managerGatewayDNSRuntime) Upload(
	ctx context.Context,
	id,
	path string,
	mode uint32,
	size int64,
	reader io.Reader,
) error {
	return r.manager.Upload(ctx, id, path, mode, size, reader)
}

func (r managerGatewayDNSRuntime) Exec(
	ctx context.Context,
	id,
	command string,
	stdin io.Reader,
) (*execution.BufferedResult, error) {
	return ExecuteBuffered(ctx, r.manager, id, command, stdin)
}

// beginBootGenerationLocked creates an in-process identity for exactly one
// asynchronous Firecracker boot. Callers hold lifecycleMu, which makes the
// generation handoff atomic with the durable creating transition.
func (m *Manager) beginBootGenerationLocked(machineID string) uint64 {
	if m.bootGenerations == nil {
		m.bootGenerations = make(map[string]uint64)
	}
	m.nextBootGeneration++
	if m.nextBootGeneration == 0 {
		m.nextBootGeneration++
	}
	m.bootGenerations[machineID] = m.nextBootGeneration
	return m.nextBootGeneration
}

func (m *Manager) bootGenerationCurrentLocked(machineID string, generation uint64) bool {
	return generation != 0 && m.bootGenerations[machineID] == generation
}

func (m *Manager) completeBootGenerationLocked(machineID string, generation uint64) {
	if m.bootGenerationCurrentLocked(machineID, generation) {
		delete(m.bootGenerations, machineID)
	}
}

func (m *Manager) invalidateBootGenerationLocked(machineID string) {
	delete(m.bootGenerations, machineID)
}

func (m *Manager) invalidateAllBootGenerationsLocked() {
	clear(m.bootGenerations)
}

func (m *Manager) RuntimeSnapshot(id string) RuntimeSnapshot {
	snapshot := RuntimeSnapshot{
		MachineID:  id,
		CgroupPath: MachineCgroupPath(id),
	}
	if proc, ok := m.processes.Load(id); ok {
		vp := proc.(*VMProcess)
		snapshot.CgroupPath = vp.CgroupPath
		if vp.Cmd != nil && vp.Cmd.Process != nil {
			snapshot.Running = true
			snapshot.PID = vp.Cmd.Process.Pid
		}
	}
	return snapshot
}

func (m *Manager) StartNetworkReconciler(ctx context.Context) {
	if _, ok := m.network.(publicNetworkReconciler); !ok {
		return
	}
	go func() {
		ticker := time.NewTicker(defaultNetworkReconcileEvery)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := m.reconcilePublicNetworkCycle(ctx); err != nil {
					if errors.Is(err, context.Canceled) && ctx.Err() != nil {
						return
					}
					slog.Error("failed to reconcile public network artifacts", "error", err)
				}
			}
		}
	}()
}

func (m *Manager) reconcilePublicNetworkCycle(ctx context.Context) error {
	reconciler, ok := m.network.(publicNetworkReconciler)
	if !ok {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("reconcile public network: %w", err)
	}

	m.lifecycleMu.Lock()
	defer m.lifecycleMu.Unlock()
	reconcileErr := reconciler.Reconcile()
	m.reconcileProviderCleanupRecordsLocked("background_reconcile")
	if reconcileErr != nil {
		return fmt.Errorf("reconcile public network artifacts: %w", reconcileErr)
	}
	return nil
}

// AddStartHook registers a function to be called after a machine reaches running state.
func (m *Manager) AddStartHook(fn func(machineID string)) {
	m.onStart = append(m.onStart, fn)
}

// AddStopHook registers a function to be called when a machine stops but is not deleted.
func (m *Manager) AddStopHook(fn func(machineID string)) {
	m.onStop = append(m.onStop, fn)
}

// AddDeleteHook registers a function to be called when a machine is deleted.
func (m *Manager) AddDeleteHook(fn func(machineID string)) {
	m.onDelete = append(m.onDelete, fn)
}

func (m *Manager) runStartHooks(machineID string) {
	for _, fn := range m.onStart {
		fn(machineID)
	}
}

func (m *Manager) runStopHooks(machineID string) {
	if m.gatewayDNSReadiness != nil {
		m.gatewayDNSReadiness.clearMachine(machineID, false)
	}
	for _, fn := range m.onStop {
		fn(machineID)
	}
}

func (m *Manager) runDeleteHooks(machineID string) {
	if m.gatewayDNSReadiness != nil {
		m.gatewayDNSReadiness.clearMachine(machineID, true)
	}
	for _, fn := range m.onDelete {
		fn(machineID)
	}
}

func validateCreateRequest(req *models.CreateMachineRequest) error {
	if req.Name == "" {
		return &apierr.ValidationError{Field: "name", Message: "is required"}
	}
	req.Backend = strings.TrimSpace(req.Backend)
	if req.Backend == "" {
		req.Backend = "firecracker"
	}
	req.GuestOS = strings.TrimSpace(req.GuestOS)
	if req.GuestOS == "" {
		req.GuestOS = "linux"
	}
	if req.Backend != "firecracker" && req.Backend != "libvirt-qemu" {
		return &apierr.ValidationError{Field: "backend", Message: "must be one of: firecracker, libvirt-qemu"}
	}
	if req.GuestOS != "linux" && req.GuestOS != "windows" {
		return &apierr.ValidationError{Field: "guest_os", Message: "must be one of: linux, windows"}
	}
	if req.Backend == "firecracker" && req.GuestOS != "linux" {
		return &apierr.ValidationError{Field: "guest_os", Message: "firecracker backend requires linux"}
	}
	if req.Backend == libvirtBackend && req.GuestOS != windowsGuestOS {
		return &apierr.ValidationError{Field: "guest_os", Message: "libvirt-qemu backend requires windows"}
	}
	if err := models.ValidateNetworkPolicy(req.NetworkPolicy, req.Backend); err != nil {
		return &apierr.ValidationError{Field: "network_policy", Message: err.Error(), Cause: err}
	}
	if req.ImageID == "" {
		return &apierr.ValidationError{Field: "image_id", Message: "is required"}
	}
	if err := normalizeCreateRequestCPU(req); err != nil {
		return err
	}
	if req.MemoryMB <= 0 {
		req.MemoryMB = 512
	}
	// Validate ranges
	if req.VCPUs > 32 {
		return &apierr.ValidationError{Field: "vcpus", Message: "must be between 1 and 32"}
	}
	if req.MemoryMB < 128 || req.MemoryMB > 32768 {
		return &apierr.ValidationError{Field: "memory_mb", Message: "must be between 128 and 32768"}
	}
	return nil
}

func normalizeCreateRequestCPU(req *models.CreateMachineRequest) error {
	cpu := req.CPU
	if cpu == nil {
		cpu = &models.CPUConfig{}
		req.CPU = cpu
	}
	if cpu.Kind == "" {
		cpu.Kind = "shared"
	}
	if cpu.Kind != "shared" && cpu.Kind != "pooled" && cpu.Kind != "dedicated" {
		return &apierr.ValidationError{Field: "cpu.kind", Message: `must be "shared", "pooled" or "dedicated"`}
	}
	if cpu.Kind == "pooled" && req.Backend != "" && req.Backend != "firecracker" {
		return &apierr.ValidationError{Field: "cpu.kind", Message: "pooled CPU requires the firecracker backend"}
	}
	if cpu.Kind == "dedicated" {
		return &apierr.ValidationError{Field: "cpu.kind", Message: "dedicated CPU pinning is not implemented yet"}
	}
	if cpu.Millis < 0 {
		return &apierr.ValidationError{Field: "cpu.millis", Message: "must be positive"}
	}
	if cpu.VCPUs < 0 {
		return &apierr.ValidationError{Field: "cpu.vcpus", Message: "must be positive"}
	}
	if cpu.Millis == 0 {
		if cpu.VCPUs > 0 {
			cpu.Millis = cpu.VCPUs * 1000
		} else if req.VCPUs > 0 {
			cpu.Millis = req.VCPUs * 1000
		} else {
			cpu.Millis = 1000
		}
	}
	if cpu.Millis < 1 || cpu.Millis > 32_000 {
		return &apierr.ValidationError{Field: "cpu.millis", Message: "must be between 1 and 32000"}
	}
	if cpu.VCPUs == 0 {
		cpu.VCPUs = ceilDiv(cpu.Millis, 1000)
	}
	if cpu.Kind == "pooled" && (cpu.Millis%1000 != 0 || cpu.VCPUs != cpu.Millis/1000) {
		return &apierr.ValidationError{Field: "cpu", Message: "pooled CPU must declare a whole number of guest vCPUs"}
	}
	req.VCPUs = cpu.VCPUs
	return nil
}

func ceilDiv(value, divisor int) int {
	return (value + divisor - 1) / divisor
}

func (m *Manager) checkResourceAdmission(excludeID string, req *models.CreateMachineRequest) error {
	machines, err := m.db.ListMachines()
	if err != nil {
		return fmt.Errorf("list machines for resource admission: %w", err)
	}
	machines = m.reconcileExitedStoppingMachines(machines)
	return checkResourceAdmission(machines, excludeID, req, m.limits)
}

func (m *Manager) reconcileExitedStoppingMachines(machines []models.Machine) []models.Machine {
	for i := range machines {
		machine := &machines[i]
		if machine.State != models.MachineStateStopping || isLibvirtMachine(machine) {
			continue
		}
		if _, ok := m.processes.Load(machine.ID); ok {
			continue
		}
		if cleanupErr := m.releaseMachineRuntime(machine); cleanupErr != nil {
			if err := m.recordNetworkCleanupFailure(machine, "stopping_recovery", cleanupErr); err != nil {
				slog.Warn("failed to reconcile exited stopping machine provider cleanup", "machine", machine.ID, "error", err)
			}
			machine.State = models.MachineStateError
			machine.Error = cleanupErr.Error()
			continue
		}
		m.runStopHooks(machine.ID)
		if err := m.db.UpdateMachineState(machine.ID, models.MachineStateStopped, ""); err != nil {
			slog.Warn("failed to reconcile exited stopping machine before resource admission", "machine", machine.ID, "error", err)
			continue
		}
		machine.State = models.MachineStateStopped
	}
	return machines
}

func (m *Manager) Create(ctx context.Context, req *models.CreateMachineRequest) (*models.Machine, error) {
	m.lifecycleMu.Lock()
	defer m.lifecycleMu.Unlock()

	if err := validateCreateRequest(req); err != nil {
		return nil, err
	}
	if err := m.checkResourceAdmission("", req); err != nil {
		return nil, err
	}

	img, err := m.resolveImage(req.ImageID)
	if err != nil {
		return nil, err
	}
	if req.Backend == libvirtBackend {
		return m.createLibvirtMachine(ctx, req, img)
	}
	imageID := img.ID

	now := time.Now().UTC()
	machine := &models.Machine{
		ID:            uuid.New().String(),
		Name:          req.Name,
		Backend:       req.Backend,
		GuestOS:       req.GuestOS,
		ImageID:       imageID,
		NetworkPolicy: req.NetworkPolicy,
		State:         models.MachineStateCreating,
		VCPUs:         req.VCPUs,
		CPU:           *req.CPU,
		MemoryMB:      req.MemoryMB,
		VSockCID:      m.allocateVSockCID(),
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	plannedAllocation, err := m.network.Plan(machine.ID)
	if err != nil {
		return nil, fmt.Errorf("plan public network: %w", err)
	}
	setMachinePublicAllocation(machine, plannedAllocation)

	// Create machine directory
	machineDir := filepath.Join(m.cfg.DataDir, "machines", machine.ID)
	if err := os.MkdirAll(machineDir, 0750); err != nil {
		return nil, fmt.Errorf("create machine dir: %w", err)
	}

	// Copy rootfs from image
	srcRootfs := filepath.Join(m.cfg.DataDir, "images", imageID, "rootfs.ext4")
	dstRootfs := filepath.Join(machineDir, "rootfs.ext4")
	if err := copyFile(ctx, srcRootfs, dstRootfs); err != nil {
		_ = os.RemoveAll(machineDir)
		return nil, fmt.Errorf("copy rootfs: %w", err)
	}

	alloc, persisted, err := m.commitAndActivatePublicNetwork(machine, plannedAllocation, true)
	if err != nil {
		if !persisted {
			_ = os.RemoveAll(machineDir)
			return nil, fmt.Errorf("save machine network intent: %w", err)
		}
		return nil, m.rollbackCreate(machine, machineDir, alloc, fmt.Errorf("activate planned network: %w", err))
	}

	if err := injectGuestNameConfig(dstRootfs, guestHostname(machine.Name), m.guestDNSServersForAllocation(alloc)); err != nil {
		return nil, m.rollbackCreate(machine, machineDir, alloc, fmt.Errorf("inject guest name config: %w", err))
	}

	generation := m.beginBootGenerationLocked(machine.ID)
	go m.startVM(machine, machineDir, alloc, generation)

	return machine, nil
}

func (m *Manager) rollbackCreate(machine *models.Machine, machineDir string, alloc *Allocation, primary error) error {
	cleanupErr := m.releaseAllocatedRuntime(alloc)
	if cleanupErr == nil {
		if err := m.db.DeleteMachine(machine.ID); err != nil {
			combined := errors.Join(primary, fmt.Errorf("delete rolled-back machine record: %w", err))
			_ = m.db.UpdateMachineState(machine.ID, models.MachineStateError, combined.Error())
			return combined
		}
		if err := os.RemoveAll(machineDir); err != nil {
			return errors.Join(primary, fmt.Errorf("remove rolled-back machine files: %w", err))
		}
		return primary
	}

	// Keep the machine and its files when host cleanup is incomplete; the
	// allocation was committed before activation so recovery can finish it.
	diagnostic := buildProviderNetworkCleanupDiagnosticForAllocation(machine, alloc, "create_rollback", cleanupErr)
	diagnostics := appendNetworkCleanupDiagnostic(machine.Diagnostics, diagnostic)
	combined := errors.Join(primary, cleanupErr)
	if err := m.db.UpdateMachineStateWithDiagnostics(machine.ID, models.MachineStateError, combined.Error(), diagnostics); err != nil {
		return errors.Join(combined, fmt.Errorf("record create rollback failure: %w", err))
	}
	return combined
}

func injectGuestNameConfig(rootfsPath, hostname string, servers []string) error {
	return withMountedRootfs(rootfsPath, func(root *os.Root) error {
		return writeGuestNameConfig(root, hostname, servers)
	})
}

func withMountedRootfs(rootfsPath string, fn func(*os.Root) error) (err error) {
	tmpDir, err := os.MkdirTemp(filepath.Dir(rootfsPath), ".rootfs-mount-*")
	if err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}
	mounted := false
	defer func() {
		if mounted {
			if unmountErr := unmountRootfsTempDir(tmpDir); unmountErr != nil {
				if err == nil {
					err = fmt.Errorf("umount rootfs %s: %w", rootfsPath, unmountErr)
				}
				slog.Warn("leaving rootfs mountpoint in place because unmount failed", "rootfs", rootfsPath, "mount_dir", tmpDir, "error", unmountErr)
				return
			}
		}
		if removeErr := os.RemoveAll(tmpDir); removeErr != nil && err == nil {
			err = fmt.Errorf("remove rootfs temp dir: %w", removeErr)
		}
	}()

	if out, err := exec.Command("mount", "-o", "loop,nodev,nosuid,noexec", rootfsPath, tmpDir).CombinedOutput(); err != nil {
		return fmt.Errorf("mount rootfs: %s: %w", out, err)
	}
	mounted = true
	root, err := os.OpenRoot(tmpDir)
	if err != nil {
		return fmt.Errorf("open rootfs: %w", err)
	}
	defer root.Close()
	return fn(root)
}

func unmountRootfsTempDir(mountDir string) error {
	if out, err := exec.Command("umount", mountDir).CombinedOutput(); err == nil {
		return nil
	} else if lazyOut, lazyErr := exec.Command("umount", "-l", mountDir).CombinedOutput(); lazyErr != nil {
		return fmt.Errorf("umount: %s: %w; lazy umount: %s: %v", out, err, lazyOut, lazyErr)
	}
	return nil
}

func renderResolvConf(servers []string) string {
	var b strings.Builder
	b.WriteString("# Generated by kiln-vm.\n")
	for _, server := range servers {
		trimmed := strings.TrimSpace(server)
		if trimmed == "" {
			continue
		}
		b.WriteString("nameserver ")
		b.WriteString(trimmed)
		b.WriteString("\n")
	}
	return b.String()
}

func (m *Manager) getMachine(id string) (*models.Machine, error) {
	canonicalID, err := m.resolveMachineID(id)
	if err != nil {
		return nil, err
	}
	machine, err := m.db.GetMachine(canonicalID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, &apierr.NotFoundError{Resource: "machine", ID: id}
		}
		return nil, fmt.Errorf("get machine: %w", err)
	}
	return machine, nil
}

func (m *Manager) resolveMachineID(ref string) (string, error) {
	machine, err := m.db.GetMachine(ref)
	if err == nil {
		return machine.ID, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("get machine: %w", err)
	}
	machines, err := m.db.ListMachines()
	if err != nil {
		return "", fmt.Errorf("list machines for ID resolution: %w", err)
	}
	candidates := make([]refid.Candidate, 0, len(machines))
	for _, machine := range machines {
		candidates = append(candidates, refid.Candidate{ID: machine.ID, Label: machine.Name})
	}
	candidate, err := refid.Resolve("machine", ref, candidates)
	if err != nil {
		return "", fmt.Errorf("resolve machine ID: %w", err)
	}
	return candidate.ID, nil
}

func (m *Manager) resolveImage(ref string) (*models.Image, error) {
	img, err := m.db.GetImage(ref)
	if err == nil {
		return img, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("get image: %w", err)
	}
	images, err := m.db.ListImages()
	if err != nil {
		return nil, fmt.Errorf("list images for ID resolution: %w", err)
	}
	candidates := make([]refid.Candidate, 0, len(images))
	for _, img := range images {
		candidates = append(candidates, refid.Candidate{ID: img.ID, Label: img.Name + ":" + img.Tag})
	}
	candidate, err := refid.Resolve("image", ref, candidates)
	if err != nil {
		return nil, fmt.Errorf("resolve image ID: %w", err)
	}
	img, err = m.db.GetImage(candidate.ID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, &apierr.NotFoundError{Resource: "image", ID: ref}
		}
		return nil, fmt.Errorf("get image: %w", err)
	}
	return img, nil
}

func (m *Manager) releaseMachineRuntime(machine *models.Machine) error {
	if machine == nil {
		return nil
	}
	var cleanupErrs []error
	alloc, identityErr := publicAllocationForCleanup(machine)
	if identityErr != nil {
		cleanupErrs = append(cleanupErrs, fmt.Errorf("resolve public network identity: %w", identityErr))
	} else if alloc != nil {
		if err := m.network.Release(alloc); err != nil {
			cleanupErrs = append(cleanupErrs, fmt.Errorf("release public network: %w", err))
		}
	}
	if len(cleanupErrs) == 0 {
		if err := releaseMachineIsolation(machine); err != nil {
			cleanupErrs = append(cleanupErrs, err)
		}
	}
	return errors.Join(cleanupErrs...)
}

func (m *Manager) bridgeCIDR() string {
	if m != nil && m.cfg != nil {
		return m.cfg.BridgeCIDR
	}
	return defaultBaseCIDR
}

// Rollback must remove per-owner guards before a freed TAP can be reused.
func (m *Manager) releaseIsolationRuntime(machine *models.Machine, alloc *Allocation) error {
	if err := m.releaseAllocatedRuntime(alloc); err != nil {
		return err
	}
	return releaseMachineIsolation(machine)
}

func (m *Manager) releaseAllocatedRuntime(alloc *Allocation) error {
	if alloc == nil {
		return nil
	}
	if err := m.network.Release(alloc); err != nil {
		return fmt.Errorf("release network: %w", err)
	}
	return nil
}

func (m *Manager) recordNetworkCleanupFailure(machine *models.Machine, action string, cleanupErr error) error {
	if cleanupErr == nil {
		return nil
	}
	if machine == nil {
		return cleanupErr
	}
	alloc, identityErr := publicAllocationForCleanup(machine)
	if identityErr != nil {
		cleanupErr = errors.Join(cleanupErr, fmt.Errorf("resolve public network identity for cleanup journal: %w", identityErr))
	}
	diagnostic := buildProviderNetworkCleanupDiagnosticForAllocation(machine, alloc, action, cleanupErr)
	diagnostics := appendNetworkCleanupDiagnostic(machine.Diagnostics, diagnostic)
	if err := m.db.UpdateMachineStateWithDiagnostics(machine.ID, models.MachineStateError, conciseDiagnosticError(diagnostic), diagnostics); err != nil {
		return errors.Join(cleanupErr, fmt.Errorf("record provider network cleanup failure: %w", err))
	}
	return cleanupErr
}

func appendNetworkCleanupDiagnostic(existing []models.Diagnostic, diagnostic models.Diagnostic) []models.Diagnostic {
	result := make([]models.Diagnostic, 0, len(existing)+1)
	for _, current := range existing {
		if current.Category == diagnostic.Category && current.Phase == diagnostic.Phase && current.Action == diagnostic.Action {
			// Retry attempts update the failure text, but an incomplete later
			// observation must never erase the fullest previously validated
			// immutable cleanup tuple or retarget it to a conflicting tuple.
			if cleanupDiagnosticIdentityRank(current) > cleanupDiagnosticIdentityRank(diagnostic) ||
				(cleanupDiagnosticIdentityRank(current) == 2 && cleanupDiagnosticIdentityRank(diagnostic) == 2 && !sameCleanupDiagnosticIdentity(current, diagnostic)) {
				copyCleanupDiagnosticIdentity(&diagnostic, current)
			}
			continue
		}
		result = append(result, current)
	}
	return append(result, diagnostic)
}

func cleanupDiagnosticIdentityRank(diagnostic models.Diagnostic) int {
	_, complete, err := allocationFromCleanupDiagnostic(diagnostic)
	if err == nil && complete {
		return 2
	}
	if strings.TrimSpace(diagnostic.TAPDevice) != "" {
		return 1
	}
	return 0
}

func sameCleanupDiagnosticIdentity(left, right models.Diagnostic) bool {
	leftAllocation, leftComplete, leftErr := allocationFromCleanupDiagnostic(left)
	rightAllocation, rightComplete, rightErr := allocationFromCleanupDiagnostic(right)
	return leftErr == nil && rightErr == nil && leftComplete && rightComplete && *leftAllocation == *rightAllocation
}

func copyCleanupDiagnosticIdentity(destination *models.Diagnostic, source models.Diagnostic) {
	if destination == nil {
		return
	}
	destination.MachineID = source.MachineID
	destination.TAPDevice = source.TAPDevice
	destination.NetworkCIDR = source.NetworkCIDR
	destination.NetworkIndex = source.NetworkIndex
	destination.GuestIP = source.GuestIP
	destination.GatewayIP = source.GatewayIP
	destination.GuestMAC = source.GuestMAC
	destination.Netmask = source.Netmask
	destination.TAPAlias = source.TAPAlias
}

func (m *Manager) handleVMExit(machineID string, generation uint64, exitErr error, diagnostic *models.Diagnostic) {
	m.lifecycleMu.Lock()
	defer m.lifecycleMu.Unlock()
	if !m.bootGenerationCurrentLocked(machineID, generation) {
		return
	}
	defer m.completeBootGenerationLocked(machineID, generation)

	current, getErr := m.db.GetMachine(machineID)
	if getErr != nil {
		return
	}
	if !shouldCleanupRuntimeOnExit(current.State) {
		return
	}

	cleanupErr := m.releaseMachineRuntime(current)
	m.runStopHooks(machineID)
	cleanup := "released runtime network artifacts"
	if cleanupErr != nil {
		cleanup = "provider network cleanup incomplete"
	}

	if exitErr == nil {
		if cleanupErr != nil {
			if err := m.recordNetworkCleanupFailure(current, "process_exit", cleanupErr); err != nil {
				slog.Error("failed to clean up provider network after VM exit", "machine", machineID, "error", err)
			}
			return
		}
		if err := m.db.UpdateMachineState(machineID, models.MachineStateStopped, ""); err != nil {
			slog.Error("failed to update machine state to stopped", "machine", machineID, "error", err)
		}
		return
	}
	errMsg := exitErr.Error()
	var diagnostics []models.Diagnostic
	if diagnostic != nil {
		diagnostic.Cleanup = cleanup
		errMsg = conciseDiagnosticError(*diagnostic)
		diagnostics = []models.Diagnostic{*diagnostic}
	}
	if cleanupErr != nil {
		cleanupAlloc, identityErr := publicAllocationForCleanup(current)
		if identityErr != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("resolve public network identity for process-exit journal: %w", identityErr))
		}
		networkDiagnostic := buildProviderNetworkCleanupDiagnosticForAllocation(current, cleanupAlloc, "process_exit", cleanupErr)
		diagnostics = append(diagnostics, networkDiagnostic)
		errMsg = errors.Join(exitErr, cleanupErr).Error()
	}
	if err := m.db.UpdateMachineStateWithDiagnostics(machineID, models.MachineStateError, errMsg, diagnostics); err != nil {
		slog.Error("failed to update machine state to error", "machine", machineID, "error", err)
	}
}

func shouldCleanupRuntimeOnExit(state models.MachineState) bool {
	return state == models.MachineStateCreating || state == models.MachineStateRunning
}

func (m *Manager) Get(id string) (*models.Machine, error) {
	return m.getMachine(id)
}

func (m *Manager) List() ([]models.Machine, error) {
	machines, err := m.db.ListMachines()
	if err != nil {
		return nil, fmt.Errorf("list machines: %w", err)
	}
	return machines, nil
}

func (m *Manager) Stop(id string) error {
	m.lifecycleMu.Lock()
	defer m.lifecycleMu.Unlock()

	machine, err := m.getMachine(id)
	if err != nil {
		return err
	}
	canonicalID := machine.ID

	if err := validateTransition(machine.State, models.MachineStateStopping); err != nil {
		return err
	}
	if isLibvirtMachine(machine) {
		return m.stopLibvirtMachine(canonicalID)
	}

	if err := m.db.UpdateMachineState(canonicalID, models.MachineStateStopping, ""); err != nil {
		return fmt.Errorf("update machine state: %w", err)
	}
	m.invalidateBootGenerationLocked(canonicalID)

	// Ask the guest to flush and power itself off before falling back to
	// terminating Firecracker from the host.
	if proc, ok := m.processes.Load(canonicalID); ok {
		vp := proc.(*VMProcess)
		vp.ExpectedStop.Store(true)
		if err := m.requestGuestShutdown(canonicalID); err != nil || !vp.Wait(10*time.Second) {
			if err != nil {
				slog.Warn("guest shutdown request failed; terminating VM process", "machine", canonicalID, "error", err)
			} else {
				slog.Warn("guest shutdown timed out; terminating VM process", "machine", canonicalID)
			}
			vp.GracefulStop(5 * time.Second)
		}
	}
	m.runStopHooks(canonicalID)
	if cleanupErr := m.releaseMachineRuntime(machine); cleanupErr != nil {
		return fmt.Errorf("stop machine with incomplete provider cleanup: %w", m.recordNetworkCleanupFailure(machine, "stop", cleanupErr))
	}

	if err := m.db.UpdateMachineState(canonicalID, models.MachineStateStopped, ""); err != nil {
		return fmt.Errorf("update machine state: %w", err)
	}

	return nil
}

func (m *Manager) Start(ctx context.Context, id string) (*models.Machine, error) {
	m.lifecycleMu.Lock()
	defer m.lifecycleMu.Unlock()

	machine, err := m.getMachine(id)
	if err != nil {
		return nil, err
	}
	canonicalID := machine.ID

	if err := validateTransition(machine.State, models.MachineStateCreating); err != nil {
		return nil, err
	}
	if m.gatewayDNSReadiness != nil {
		m.gatewayDNSReadiness.clearMachine(canonicalID, false)
	}
	if isLibvirtMachine(machine) {
		return m.startLibvirtMachine(ctx, machine)
	}

	if machine.VSockCID == 0 {
		machine.VSockCID = m.allocateVSockCID()
		if err := m.db.UpdateMachineVSockCID(canonicalID, machine.VSockCID); err != nil {
			return nil, fmt.Errorf("update machine vsock cid: %w", err)
		}
	}
	if err := m.checkResourceAdmission(canonicalID, &models.CreateMachineRequest{
		Name:     machine.Name,
		CPU:      &machine.CPU,
		ImageID:  machine.ImageID,
		VCPUs:    machine.VCPUs,
		MemoryMB: machine.MemoryMB,
	}); err != nil {
		return nil, err
	}

	// Verify rootfs still exists on disk
	machineDir := filepath.Join(m.cfg.DataDir, "machines", canonicalID)
	rootfsPath := filepath.Join(machineDir, "rootfs.ext4")
	if _, err := os.Stat(rootfsPath); err != nil {
		return nil, fmt.Errorf("rootfs not found: %w", err)
	}

	// Release old runtime devices before choosing the next public allocation.
	// Normally the durable identity is reused. If BridgeCIDR changed so the old
	// /30 no longer belongs to the configured range, exact historical cleanup
	// completes first and only then may a fresh current-CIDR intent be planned.
	plannedAllocation, err := m.prepareMachinePublicNetworkForStart(machine)
	if err != nil {
		return nil, err
	}
	alloc, persisted, err := m.commitAndActivatePublicNetwork(machine, plannedAllocation, false)
	if err != nil {
		if !persisted {
			return nil, fmt.Errorf("persist starting public-network intent: %w", err)
		}
		primary := fmt.Errorf("activate planned public network: %w", err)
		if alloc != nil {
			return nil, m.rollbackStartingAllocation(machine, alloc, primary)
		}
		if stateErr := m.db.UpdateMachineState(machine.ID, models.MachineStateError, primary.Error()); stateErr != nil {
			return nil, errors.Join(primary, fmt.Errorf("record failed network activation: %w", stateErr))
		}
		return nil, primary
	}

	if err := injectGuestNameConfig(rootfsPath, guestHostname(machine.Name), m.guestDNSServersForAllocation(alloc)); err != nil {
		m.markStartFailure(machine, alloc, err)
		return nil, fmt.Errorf("refresh guest name config: %w", err)
	}

	// Start the VM asynchronously
	generation := m.beginBootGenerationLocked(machine.ID)
	go m.startVM(machine, machineDir, alloc, generation)

	return machine, nil
}

// commitAndActivatePublicNetwork is the crash boundary for public networking:
// it atomically stores the complete allocation with creating intent and only
// then permits Reserve to mutate the physical host. persisted reports whether
// recovery owns the tuple when activation fails.
func (m *Manager) commitAndActivatePublicNetwork(machine *models.Machine, planned *Allocation, create bool) (*Allocation, bool, error) {
	if machine == nil || planned == nil {
		return nil, false, fmt.Errorf("commit public network intent: machine and allocation are required")
	}
	ownerID := strings.TrimSpace(machine.ID)
	if ownerID == "" {
		return nil, false, fmt.Errorf("commit public network intent: machine owner ID is required")
	}
	if candidateOwner := strings.TrimSpace(planned.OwnerID); candidateOwner != "" && candidateOwner != ownerID {
		return nil, false, fmt.Errorf("commit public network intent: allocation owner %q does not match machine %q", candidateOwner, ownerID)
	}
	planned.OwnerID = ownerID
	setMachinePublicAllocation(machine, planned)
	machine.State = models.MachineStateCreating
	machine.Error = ""
	machine.Diagnostics = nil
	durable, present, err := durablePublicAllocationFromMachine(machine)
	if err != nil || !present || durable == nil || *durable != *planned {
		if err == nil {
			err = fmt.Errorf("persisted public allocation does not match plan")
		}
		return nil, false, fmt.Errorf("validate public network intent: %w", err)
	}
	if create {
		err = m.db.CreateMachine(machine)
	} else {
		err = m.db.UpdateMachineNetworkIntent(machine)
	}
	if err != nil {
		return nil, false, fmt.Errorf("persist public network intent: %w", err)
	}
	reserved, err := m.network.Reserve(planned)
	if err != nil {
		return reserved, true, fmt.Errorf("activate public network allocation: %w", err)
	}
	return reserved, true, nil
}

func (m *Manager) rollbackStartingAllocation(machine *models.Machine, alloc *Allocation, primary error) error {
	cleanupErr := m.releaseAllocatedRuntime(alloc)
	combined := errors.Join(primary, cleanupErr)
	if cleanupErr != nil {
		diagnostic := buildProviderNetworkCleanupDiagnosticForAllocation(machine, alloc, "start_rollback", cleanupErr)
		diagnostics := appendNetworkCleanupDiagnostic(machine.Diagnostics, diagnostic)
		if err := m.db.UpdateMachineStateWithDiagnostics(machine.ID, models.MachineStateError, combined.Error(), diagnostics); err != nil {
			return errors.Join(combined, fmt.Errorf("record start rollback failure: %w", err))
		}
		return combined
	}
	if err := m.db.UpdateMachineState(machine.ID, models.MachineStateError, primary.Error()); err != nil {
		return errors.Join(primary, fmt.Errorf("record failed start transition: %w", err))
	}
	return primary
}

func (m *Manager) guestDNSServersForAllocation(alloc *Allocation) []string {
	if m.cfg.GuestDNSProxyEnabled && alloc != nil && strings.TrimSpace(alloc.GatewayIP) != "" {
		return []string{alloc.GatewayIP}
	}
	return m.cfg.GuestDNSServers
}

func (m *Manager) planMachinePublicNetwork(machine *models.Machine) (*Allocation, error) {
	alloc, identityErr := publicAllocationForCleanup(machine)
	if identityErr != nil {
		return nil, fmt.Errorf("restore public network identity: %w", identityErr)
	}
	if alloc != nil {
		if current, ok := currentPublicAllocation(alloc, m.bridgeCIDR()); ok {
			return current, nil
		}
		// The old allocation has already been cleaned by the caller. Prefer the
		// same provider index in the new allocation domain before consulting the
		// monotonic allocator. This preserves ownership continuity without making
		// deleted or foreign indexes globally reusable, and is required when a
		// smaller current BridgeCIDR has no slot above the historical high-water
		// mark.
		if current, err := allocationForIndex(m.bridgeCIDR(), alloc.NetworkIndex); err == nil {
			current.OwnerID = strings.TrimSpace(machine.ID)
			return current, nil
		}
	}
	planned, err := m.network.Plan(machine.ID)
	if err != nil {
		return nil, fmt.Errorf("plan public network for machine %s: %w", machine.ID, err)
	}
	return planned, nil
}

func (m *Manager) prepareMachinePublicNetworkForStart(machine *models.Machine) (*Allocation, error) {
	if cleanupErr := m.releaseMachineRuntime(machine); cleanupErr != nil {
		return nil, fmt.Errorf("clean previous machine runtime before start: %w", m.recordNetworkCleanupFailure(machine, "start", cleanupErr))
	}
	planned, err := m.planMachinePublicNetwork(machine)
	if err != nil {
		return nil, fmt.Errorf("plan public network: %w", err)
	}
	return planned, nil
}

func (m *Manager) markStartFailure(machine *models.Machine, alloc *Allocation, err error) {
	cleanupErr := m.releaseIsolationRuntime(machine, alloc)
	combined := errors.Join(err, cleanupErr)
	if cleanupErr != nil {
		diagnostic := buildProviderNetworkCleanupDiagnosticForAllocation(machine, alloc, "start_rollback", cleanupErr)
		diagnostics := appendNetworkCleanupDiagnostic(machine.Diagnostics, diagnostic)
		if recordErr := m.db.UpdateMachineStateWithDiagnostics(machine.ID, models.MachineStateError, combined.Error(), diagnostics); recordErr != nil {
			slog.Error("failed to record start rollback cleanup failure", "machine", machine.ID, "error", recordErr)
		}
		return
	}
	_ = m.db.UpdateMachineState(machine.ID, models.MachineStateError, combined.Error())
}

func (m *Manager) Delete(id string) error {
	m.lifecycleMu.Lock()
	defer m.lifecycleMu.Unlock()

	machine, err := m.getMachine(id)
	if err != nil {
		return err
	}
	canonicalID := machine.ID
	if isLibvirtMachine(machine) {
		return m.deleteLibvirtMachine(canonicalID)
	}
	m.invalidateBootGenerationLocked(canonicalID)

	// Stop if running and wait for the monitoring goroutine to finish
	if machine.State == models.MachineStateRunning || machine.State == models.MachineStateCreating {
		if proc, ok := m.processes.Load(canonicalID); ok {
			vp := proc.(*VMProcess)
			vp.GracefulStop(5 * time.Second)
		}
	}

	// Release network before deleting durable identity. A failed cleanup keeps the
	// record available so a retry or recovery pass can heal the owned artifacts.
	if cleanupErr := m.releaseMachineRuntime(machine); cleanupErr != nil {
		return fmt.Errorf("delete machine with incomplete provider cleanup: %w", m.recordNetworkCleanupFailure(machine, "delete", cleanupErr))
	}

	m.runDeleteHooks(canonicalID)

	// Clean up files
	machineDir := filepath.Join(m.cfg.DataDir, "machines", canonicalID)
	os.RemoveAll(machineDir)

	// Clean up log watchers

	// Remove from DB
	m.processes.Delete(canonicalID)
	if err = m.db.DeleteMachine(canonicalID); err != nil {
		return fmt.Errorf("delete machine: %w", err)
	}
	return nil
}

func (m *Manager) Execute(ctx context.Context, id string, request execution.Request) execution.Outcome {
	if err := execution.ValidateCommand(request.Command); err != nil {
		return execution.OutcomeFromError(&execution.Error{
			Category: execution.CategoryProtocolError,
			Message:  err.Error(),
			Cause:    err,
		})
	}
	machine, err := m.getMachine(id)
	if err != nil {
		return execution.OutcomeFromError(err)
	}

	if machine.State != models.MachineStateRunning {
		return execution.OutcomeFromError(fmt.Errorf("machine is not running (state: %s): %w", machine.State, apierr.ErrConflict))
	}
	if isLibvirtMachine(machine) {
		return m.executeLibvirtWindows(ctx, machine, request)
	}

	if machine.VSockCID == 0 {
		return execution.Outcome{
			Category: execution.CategoryRuntimeIncompatible,
			Message:  "guest exec runtime is unavailable; recreate the VM or rebuild its profile with the current kiln-vm release",
		}
	}

	return guestagent.Execute(
		ctx,
		filepath.Join(m.cfg.DataDir, "machines", machine.ID, "firecracker.vsock"),
		request,
	)
}

func (m *Manager) Upload(ctx context.Context, id, path string, mode uint32, size int64, reader io.Reader) error {
	machine, err := m.getMachine(id)
	if err != nil {
		return err
	}

	if machine.State != models.MachineStateRunning {
		return fmt.Errorf("machine is not running (state: %s): %w", machine.State, apierr.ErrConflict)
	}
	if isLibvirtMachine(machine) {
		return m.uploadLibvirtWindows(ctx, machine, path, mode, size, reader)
	}

	if machine.VSockCID == 0 {
		return &apierr.ValidationError{
			Field:   "image",
			Message: "guest agent is not available for this machine",
		}
	}

	if err := guestagent.Upload(ctx, filepath.Join(m.cfg.DataDir, "machines", machine.ID, "firecracker.vsock"), path, mode, size, reader); err != nil {
		if errors.Is(err, guestagent.ErrTransientTransport) {
			return fmt.Errorf("guest agent upload unavailable: %w", err)
		}
		return &apierr.ValidationError{
			Field:   "image",
			Message: fmt.Sprintf("guest agent upload unavailable: %v", err),
		}
	}
	return nil
}

func (m *Manager) requestGuestShutdown(id string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	result, err := execution.RunBuffered(ctx, execution.RunnerFunc(func(ctx context.Context, request execution.Request) execution.Outcome {
		return guestagent.Execute(ctx, filepath.Join(m.cfg.DataDir, "machines", id, "firecracker.vsock"), request)
	}), "sync; (poweroff -f || halt -f) >/dev/null 2>&1 &", nil)
	if err != nil {
		return fmt.Errorf("execute guest shutdown: %w", err)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("guest shutdown command failed: exit=%d stderr=%q", result.ExitCode, result.Stderr)
	}
	return nil
}

func (m *Manager) DialShell(ctx context.Context, id string) (io.ReadWriteCloser, error) {
	machine, err := m.getMachine(id)
	if err != nil {
		return nil, err
	}
	if machine.State != models.MachineStateRunning {
		return nil, fmt.Errorf("machine is not running (state: %s): %w", machine.State, apierr.ErrConflict)
	}
	if isLibvirtMachine(machine) {
		return m.shellLibvirtWindows(ctx, machine)
	}
	if machine.VSockCID == 0 {
		return nil, &apierr.ValidationError{
			Field:   "image",
			Message: "guest agent is not available for this machine",
		}
	}
	conn, err := guestagent.DialShell(ctx, filepath.Join(m.cfg.DataDir, "machines", machine.ID, "firecracker.vsock"))
	if err != nil {
		return nil, &apierr.ValidationError{
			Field:   "image",
			Message: fmt.Sprintf("guest agent shell unavailable: %v", err),
		}
	}
	return conn, nil
}

func (m *Manager) DialForward(ctx context.Context, id string, port int) (io.ReadWriteCloser, error) {
	machine, err := m.getMachine(id)
	if err != nil {
		return nil, err
	}
	if machine.State != models.MachineStateRunning {
		return nil, fmt.Errorf("machine is not running (state: %s): %w", machine.State, apierr.ErrConflict)
	}
	if isLibvirtMachine(machine) {
		return m.dialLibvirtForward(ctx, machine, port)
	}
	if machine.VSockCID == 0 {
		return nil, &apierr.ValidationError{
			Field:   "image",
			Message: "guest agent is not available for this machine",
		}
	}
	conn, err := guestagent.DialForward(ctx, filepath.Join(m.cfg.DataDir, "machines", machine.ID, "firecracker.vsock"), port)
	var targetErr *guestagent.ForwardTargetError
	if errors.As(err, &targetErr) {
		return nil, &apierr.ValidationError{
			Field:   "port",
			Message: fmt.Sprintf("guest port %d: %s", port, targetErr.Message),
		}
	}
	if err != nil {
		return nil, &apierr.ValidationError{
			Field:   "image",
			Message: fmt.Sprintf("guest agent forward unavailable: %v", err),
		}
	}
	return conn, nil
}

// startVM boots the Firecracker VM. Runs in a goroutine.
func (m *Manager) startVM(machine *models.Machine, machineDir string, alloc *Allocation, generation uint64) {
	if m.beforeAsyncBoot != nil {
		m.beforeAsyncBoot(machine.ID)
	}

	// Create and Start hand boot work to a goroutine. Serialize the handoff with
	// Stop/Delete and re-read durable intent so a completed lifecycle operation
	// can never be resurrected by a late-scheduled boot goroutine.
	m.lifecycleMu.Lock()
	defer m.lifecycleMu.Unlock()
	if !m.bootGenerationCurrentLocked(machine.ID, generation) {
		slog.Info("skipping stale asynchronous VM boot generation", "machine", machine.ID, "generation", generation)
		return
	}
	current, stateErr := m.db.GetMachine(machine.ID)
	if stateErr != nil {
		m.completeBootGenerationLocked(machine.ID, generation)
		cleanupErr := m.releaseIsolationRuntime(machine, alloc)
		if cleanupErr != nil {
			slog.Error("failed to release provider runtime after boot intent lookup failed", "machine", machine.ID, "error", cleanupErr)
		}
		return
	}
	if current.State != models.MachineStateCreating {
		slog.Info("skipping stale asynchronous VM boot", "machine", machine.ID, "state", current.State)
		m.completeBootGenerationLocked(machine.ID, generation)
		return
	}

	if err := applyMachineIsolation(current); err != nil {
		m.completeBootGenerationLocked(machine.ID, generation)
		m.markStartFailure(machine, alloc, fmt.Errorf("enforce network policy: %w", err))
		return
	}
	logWriter := consoleLogWriter(m.cfg.DataDir, machine.ID)

	socketPath := filepath.Join(machineDir, "firecracker.sock")
	rootfsPath := filepath.Join(machineDir, "rootfs.ext4")
	kernelPath := m.cfg.KernelPath

	vsockPath := filepath.Join(machineDir, "firecracker.vsock")
	paths := firecrackerStartPaths{
		SocketPath: socketPath,
		VSockPath:  vsockPath,
		ConfigPath: socketPath + ".json",
		KernelPath: kernelPath,
		RootfsPath: rootfsPath,
		LogPath:    machineLogPath(m.cfg.DataDir, machine.ID),
	}
	err := startFirecrackerVM(m.cfg.FirecrackerBin, socketPath, vsockPath, kernelPath, rootfsPath, machine, alloc, logWriter, &m.processes, m.cfg.PooledCPUs, func(exitErr error) {
		var diagnostic *models.Diagnostic
		if exitErr != nil {
			value := buildFirecrackerFailureDiagnostic(machine, "process_exit", paths, alloc, exitErr, "")
			diagnostic = &value
		}
		m.handleVMExit(machine.ID, generation, exitErr, diagnostic)
	})
	if err != nil {
		m.completeBootGenerationLocked(machine.ID, generation)
		slog.Error("failed to start VM", "machine", machine.ID, "error", err)
		_ = logWriter.Close()
		cleanupErr := m.releaseIsolationRuntime(machine, alloc)
		phase := firecrackerErrorPhase(err, "process_start")
		cleanup := "released runtime network artifacts"
		if cleanupErr != nil {
			cleanup = "provider network cleanup incomplete"
		}
		diagnostic := buildFirecrackerFailureDiagnostic(machine, phase, paths, alloc, err, cleanup)
		diagnostics := []models.Diagnostic{diagnostic}
		combined := errors.Join(err, cleanupErr)
		if cleanupErr != nil {
			diagnostics = append(diagnostics, buildProviderNetworkCleanupDiagnosticForAllocation(machine, alloc, "start_rollback", cleanupErr))
		}
		if dbErr := m.db.UpdateMachineStateWithDiagnostics(machine.ID, models.MachineStateError, combined.Error(), diagnostics); dbErr != nil {
			slog.Error("failed to update machine state to error", "machine", machine.ID, "error", dbErr)
		}
		return
	}

	// Mark as running
	if err := m.db.UpdateMachineState(machine.ID, models.MachineStateRunning, ""); err != nil {
		slog.Error("failed to update machine state to running", "machine", machine.ID, "error", err)
		if proc, ok := m.processes.Load(machine.ID); ok {
			proc.(*VMProcess).GracefulStop(5 * time.Second)
		}
		cleanupErr := m.releaseIsolationRuntime(machine, alloc)
		if cleanupErr != nil {
			if recordErr := m.recordNetworkCleanupFailure(machine, "start_rollback", cleanupErr); recordErr != nil {
				slog.Error("failed to record provider cleanup after state update failure", "machine", machine.ID, "error", recordErr)
			}
		}
		m.completeBootGenerationLocked(machine.ID, generation)
		return
	}
	m.runStartHooks(machine.ID)
}

// RecoverRunning checks for machines in running/creating state at startup
// and marks them as error (since Firecracker processes didn't survive daemon restart).
func (m *Manager) RecoverRunning() {
	m.lifecycleMu.Lock()
	defer m.lifecycleMu.Unlock()

	orphaned, err := terminateOrphanFirecrackerProcesses(m.cfg.DataDir, 5*time.Second)
	if err != nil {
		slog.Error("failed to terminate orphaned Firecracker processes", "error", err)
	} else {
		for _, process := range orphaned {
			slog.Warn("terminated orphaned Firecracker process", "pid", process.PID, "machine", process.MachineID)
		}
	}

	machines, err := m.db.ListMachines()
	if err != nil {
		slog.Error("failed to list machines for recovery", "error", err)
		return
	}

	for _, machine := range machines {
		if isLibvirtMachine(&machine) {
			state, err := libvirtDomainState(context.Background(), m.cfg.LibvirtConnectURI, libvirtDomainName(&machine))
			if err == nil && state == "running" {
				_ = m.db.UpdateMachineState(machine.ID, models.MachineStateRunning, "")
				m.runStartHooks(machine.ID)
				continue
			}
			if machine.State == models.MachineStateRunning || machine.State == models.MachineStateCreating || machine.State == models.MachineStateStopping {
				_ = m.db.UpdateMachineState(machine.ID, models.MachineStateStopped, models.MachineStopReasonDaemonRestarted)
				m.runStopHooks(machine.ID)
			}
			continue
		}
		if machineNeedsProviderRuntimeRecovery(&machine) {
			slog.Warn("marking orphaned VM as stopped", "machine", machine.ID, "prev_state", machine.State)
			cleanupErr := m.releaseMachineRuntime(&machine)
			m.runStopHooks(machine.ID)
			if cleanupErr != nil {
				if recordErr := m.recordNetworkCleanupFailure(&machine, "recovery", cleanupErr); recordErr != nil {
					slog.Error("failed to clean up orphaned VM provider network", "machine", machine.ID, "error", recordErr)
				}
				continue
			}
			if err := m.db.UpdateMachineState(machine.ID, models.MachineStateStopped, models.MachineStopReasonDaemonRestarted); err != nil {
				slog.Error("failed to mark orphaned VM stopped", "machine", machine.ID, "error", err)
			}
		}
	}
	if reconciler, ok := m.network.(publicNetworkReconciler); ok {
		if err := reconciler.Reconcile(); err != nil {
			slog.Error("failed to reconcile public network artifacts during recovery", "error", err)
		}
	}
	m.reconcileProviderCleanupRecordsLocked("recovery_reconcile")
}

func (m *Manager) reconcileProviderCleanupRecordsLocked(action string) {
	machines, err := m.db.ListMachines()
	if err != nil {
		slog.Error("failed to list provider cleanup records for reconciliation", "error", err)
		return
	}
	for index := range machines {
		machine := &machines[index]
		if machine.State != models.MachineStateError || !machineNeedsProviderRuntimeRecovery(machine) {
			continue
		}
		cleanupErr := m.releaseMachineRuntime(machine)
		if cleanupErr != nil {
			if recordErr := m.recordNetworkCleanupFailure(machine, action, cleanupErr); recordErr != nil {
				slog.Error("failed to reconcile provider cleanup record", "machine", machine.ID, "error", recordErr)
			}
			continue
		}
		m.runStopHooks(machine.ID)
		if err := m.db.UpdateMachineState(machine.ID, models.MachineStateStopped, "provider network cleanup reconciled"); err != nil {
			slog.Error("failed to mark reconciled provider cleanup record stopped", "machine", machine.ID, "error", err)
		}
	}
}

func machineNeedsProviderRuntimeRecovery(machine *models.Machine) bool {
	if machine == nil || isLibvirtMachine(machine) {
		return false
	}
	switch machine.State {
	case models.MachineStateRunning, models.MachineStateCreating, models.MachineStateStopping:
		return true
	case models.MachineStateError:
		for _, diagnostic := range machine.Diagnostics {
			if diagnostic.Category == "provider_network" && diagnostic.Phase == "cleanup" && diagnostic.Retryable {
				return true
			}
		}
	}
	return false
}

// StopAll cancels all running VMs and waits for them to exit.
func (m *Manager) StopAll() {
	m.lifecycleMu.Lock()
	defer m.lifecycleMu.Unlock()

	// Daemon shutdown cancels every pending asynchronous Firecracker boot,
	// including creating machines whose goroutine has not stored a process yet.
	// Exit callbacks from processes below are likewise stale after this point;
	// StopAll owns the single cleanup/state transition for them.
	m.invalidateAllBootGenerationsLocked()
	handledFirecracker := make(map[string]struct{})

	m.processes.Range(func(key, value any) bool {
		id := key.(string)
		handledFirecracker[id] = struct{}{}
		vp := value.(*VMProcess)
		slog.Info("stopping VM for shutdown", "machine", id)
		vp.ExpectedStop.Store(true)
		if err := m.requestGuestShutdown(id); err != nil || !vp.Wait(10*time.Second) {
			if err != nil {
				slog.Warn("guest shutdown request failed during daemon shutdown; terminating VM process", "machine", id, "error", err)
			} else {
				slog.Warn("guest shutdown timed out during daemon shutdown; terminating VM process", "machine", id)
			}
			vp.GracefulStop(5 * time.Second)
		}
		cleanupErr := m.releaseMachineRuntime(vp.Machine)
		m.runStopHooks(id)
		if cleanupErr != nil {
			if recordErr := m.recordNetworkCleanupFailure(vp.Machine, "daemon_shutdown", cleanupErr); recordErr != nil {
				slog.Error("failed to clean up provider network on shutdown", "machine", id, "error", recordErr)
			}
			return true
		}
		if err := m.db.UpdateMachineState(id, models.MachineStateStopped, models.MachineStopReasonDaemonShutdown); err != nil {
			slog.Error("failed to update machine state on shutdown", "machine", id, "error", err)
		}
		return true
	})
	if m.db == nil {
		return
	}
	machines, err := m.db.ListMachines()
	if err != nil {
		slog.Error("failed to list machines during shutdown", "error", err)
		return
	}
	for _, machine := range machines {
		if machine.State != models.MachineStateRunning && machine.State != models.MachineStateCreating && machine.State != models.MachineStateStopping {
			continue
		}
		if isLibvirtMachine(&machine) {
			continue
		}
		if _, handled := handledFirecracker[machine.ID]; handled {
			continue
		}

		// A creating Firecracker machine may not have reached processes.Store yet.
		// Its generation was invalidated above, so reclaim its already-allocated
		// provider runtime and persist the shutdown transition here.
		cleanupErr := m.releaseMachineRuntime(&machine)
		m.runStopHooks(machine.ID)
		if cleanupErr != nil {
			if recordErr := m.recordNetworkCleanupFailure(&machine, "daemon_shutdown", cleanupErr); recordErr != nil {
				slog.Error("failed to clean up pending Firecracker network on shutdown", "machine", machine.ID, "error", recordErr)
			}
			continue
		}
		if err := m.db.UpdateMachineState(machine.ID, models.MachineStateStopped, models.MachineStopReasonDaemonShutdown); err != nil {
			slog.Error("failed to update pending Firecracker state on shutdown", "machine", machine.ID, "error", err)
		}
	}
}

func (m *Manager) allocateVSockCID() uint32 {
	m.vsockMu.Lock()
	defer m.vsockMu.Unlock()

	if m.nextVSockCID < 3 {
		m.nextVSockCID = 3
	}
	cid := m.nextVSockCID
	m.nextVSockCID++
	return cid
}

func copyFile(ctx context.Context, src, dst string) error {
	if err := hostfs.CopyFilePreserveSparse(ctx, src, dst, 0640); err != nil {
		return fmt.Errorf("copy sparse file: %w", err)
	}
	return nil
}
