package machine

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gfdelarue/kiln-vm/internal/config"
	"github.com/gfdelarue/kiln-vm/internal/dnsprobe"
	"github.com/gfdelarue/kiln-vm/internal/execution"
	"github.com/gfdelarue/kiln-vm/internal/models"
)

const (
	gatewayDNSProbeOutputLimit      = 64 * 1024
	gatewayDNSAgentSizeLimit        = 64 * 1024 * 1024
	gatewayDNSProbeRuntimeDir       = "/run/kiln-vm/provider-runtime/dns-probe"
	gatewayDNSProbeRuntimePath      = gatewayDNSProbeRuntimeDir + "/kiln-vm-agent"
	gatewayDNSProbeOperationTimeout = 15 * time.Second

	gatewayDNSDetailReady            = "ready"
	gatewayDNSDetailNotApplicable    = "not_applicable"
	gatewayDNSDetailProbeUnavailable = "probe_unavailable"
	gatewayDNSDetailProbeInvalid     = "probe_output_invalid"
	gatewayDNSDetailPathFailed       = "gateway_dns_path_failed"
	gatewayDNSDetailResolutionFailed = "gateway_dns_resolution_failed"
)

var (
	ErrGatewayDNSPath       = errors.New("gateway DNS path failed")
	ErrGatewayDNSResolution = errors.New("gateway DNS resolution failed")
	ErrGatewayDNSProbe      = errors.New("gateway DNS readiness probe failed")
	errGatewayDNSCheckStale = errors.New("gateway DNS readiness check was superseded")
)

// GatewayDNSReadinessStatus is the sanitized, in-memory provider view of the
// most recent guest-side gateway DNS readiness check. It deliberately excludes
// the query name, gateway address, staged executable, command, and raw process
// output. Those values are private implementation details rather than public
// readiness state.
type GatewayDNSReadinessStatus struct {
	Applicable    bool
	Ready         bool
	PathOK        bool
	ResolutionOK  bool
	FailureKind   string
	Transport     string
	Phase         string
	RCodeName     string
	Detail        string
	CheckedAt     time.Time
	LastSuccessAt time.Time
}

type gatewayDNSReadinessError struct {
	kind   error
	detail string
}

func (e *gatewayDNSReadinessError) Error() string {
	switch e.detail {
	case gatewayDNSDetailPathFailed:
		return "gateway DNS readiness: guest could not reach the gateway DNS service"
	case gatewayDNSDetailResolutionFailed:
		return "gateway DNS readiness: gateway DNS could not resolve the external probe name"
	case gatewayDNSDetailProbeInvalid:
		return "gateway DNS readiness: probe returned invalid structured output"
	default:
		return "gateway DNS readiness: probe unavailable"
	}
}

func (e *gatewayDNSReadinessError) Unwrap() error { return e.kind }

type gatewayDNSProbeRuntime interface {
	Upload(context.Context, string, string, uint32, int64, io.Reader) error
	Exec(context.Context, string, string, io.Reader) (*execution.BufferedResult, error)
}

type gatewayDNSStageKey struct {
	machineID  string
	agentHash  string
	generation uint64
}

type gatewayDNSStageAttempt struct {
	done    chan struct{}
	err     error
	version uint64
}

type gatewayDNSAgentLoadAttempt struct {
	done chan struct{}
	err  error
}

type gatewayDNSCheckToken struct {
	generation uint64
	sequence   uint64
}

type gatewayDNSReadinessController struct {
	runtime          gatewayDNSProbeRuntime
	agentPath        string
	probeName        string
	bridgeCIDR       string
	operationTimeout time.Duration
	now              func() time.Time
	loadAgent        func() ([]byte, error)

	agentMu      sync.Mutex
	agentPayload []byte
	agentHash    string
	agentLoading *gatewayDNSAgentLoadAttempt

	stageMu          sync.Mutex
	staged           map[gatewayDNSStageKey]uint64
	staging          map[gatewayDNSStageKey]*gatewayDNSStageAttempt
	nextStageVersion uint64

	statusMu          sync.RWMutex
	status            map[string]GatewayDNSReadinessStatus
	lastSuccess       map[string]time.Time
	generations       map[string]uint64
	latestSequence    map[string]uint64
	nextGeneration    uint64
	nextCheckSequence uint64
}

func newGatewayDNSReadinessController(runtime gatewayDNSProbeRuntime, agentPath, probeName string) *gatewayDNSReadinessController {
	if strings.TrimSpace(agentPath) == "" {
		agentPath = config.DefaultAgentBinPath
	}
	probeName = strings.ToLower(strings.TrimSpace(probeName))
	if probeName == "" {
		probeName = dnsprobe.DefaultName
	} else if !strings.HasSuffix(probeName, ".") {
		probeName += "."
	}
	controller := &gatewayDNSReadinessController{
		runtime:          runtime,
		agentPath:        agentPath,
		probeName:        probeName,
		bridgeCIDR:       defaultBaseCIDR,
		operationTimeout: gatewayDNSProbeOperationTimeout,
		now:              func() time.Time { return time.Now().UTC() },
		staged:           make(map[gatewayDNSStageKey]uint64),
		staging:          make(map[gatewayDNSStageKey]*gatewayDNSStageAttempt),
		status:           make(map[string]GatewayDNSReadinessStatus),
		lastSuccess:      make(map[string]time.Time),
		generations:      make(map[string]uint64),
		latestSequence:   make(map[string]uint64),
	}
	controller.loadAgent = func() ([]byte, error) {
		return readGatewayDNSAgent(controller.agentPath)
	}
	return controller
}

// CheckGatewayDNSReadiness checks gateway DNS from the guest network namespace
// using the kiln-vm-agent shipped with kiln-vmd, staged into the guest through
// Upload and Exec so the probe can change without rebuilding images.
func (m *Manager) CheckGatewayDNSReadiness(ctx context.Context, machineID string) (GatewayDNSReadinessStatus, error) {
	machine, err := m.getMachine(machineID)
	if err != nil {
		return GatewayDNSReadinessStatus{}, err
	}
	if m.gatewayDNSReadiness == nil {
		return GatewayDNSReadinessStatus{}, &gatewayDNSReadinessError{kind: ErrGatewayDNSProbe, detail: gatewayDNSDetailProbeUnavailable}
	}
	enabled := m.cfg != nil && m.cfg.GuestDNSProxyEnabled
	return m.gatewayDNSReadiness.check(ctx, machine, enabled)
}

// GatewayDNSReadinessStatus returns only the sanitized cached provider state.
// The cache is intentionally process-local: VM WaitReady is the authority that
// refreshes it for each running instance.
func (m *Manager) GatewayDNSReadinessStatus(machineID string) (GatewayDNSReadinessStatus, bool) {
	if m.gatewayDNSReadiness == nil {
		return GatewayDNSReadinessStatus{}, false
	}
	return m.gatewayDNSReadiness.cachedStatus(machineID)
}

func (c *gatewayDNSReadinessController) check(ctx context.Context, machine *models.Machine, proxyEnabled bool) (GatewayDNSReadinessStatus, error) {
	id := machineID(machine)
	token := c.beginCheck(id)
	checkedAt := c.now().UTC()
	if !gatewayDNSProbeApplies(machine, proxyEnabled) {
		status := GatewayDNSReadinessStatus{
			Ready:     true,
			Detail:    gatewayDNSDetailNotApplicable,
			CheckedAt: checkedAt,
		}
		return c.finishCheck(id, token, status)
	}

	base := GatewayDNSReadinessStatus{Applicable: true, CheckedAt: checkedAt}
	if !gatewayDNSPublicIdentityValid(machine, c.bridgeCIDR) || machine.State != models.MachineStateRunning {
		base.FailureKind = "probe"
		base.Detail = gatewayDNSDetailProbeUnavailable
		return c.finishCheck(machine.ID, token, base)
	}

	payload, agentHash, err := c.cachedAgent(ctx)
	if err != nil {
		base.FailureKind = "probe"
		base.Detail = gatewayDNSDetailProbeUnavailable
		return c.finishCheck(machine.ID, token, base)
	}
	stageKey := gatewayDNSStageKey{machineID: machine.ID, agentHash: agentHash, generation: token.generation}
	stagedPath := gatewayDNSProbeRuntimePath

	operationTimeout := c.operationTimeout
	if operationTimeout <= 0 {
		operationTimeout = gatewayDNSProbeOperationTimeout
	}
	operationCtx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	stageVersion, err := c.ensureStaged(operationCtx, stageKey, stagedPath, payload)
	if err != nil {
		base.FailureKind = "probe"
		base.Detail = gatewayDNSDetailProbeUnavailable
		return c.finishCheck(machine.ID, token, base)
	}

	command := gatewayDNSProbeCommand(stagedPath, machine.GatewayIP, c.probeName)
	probeResult, failureDetail := c.executeProbe(
		operationCtx,
		machine.ID,
		token,
		stageKey,
		stageVersion,
		stagedPath,
		payload,
		command,
		net.JoinHostPort(machine.GatewayIP, dnsprobe.DefaultPort),
		c.probeName,
	)
	if failureDetail != "" {
		base.FailureKind = "probe"
		base.Detail = failureDetail
		return c.finishCheck(machine.ID, token, base)
	}

	status := gatewayDNSStatusFromResult(probeResult, checkedAt)
	return c.finishCheck(machine.ID, token, status)
}

func gatewayDNSProbeApplies(machine *models.Machine, proxyEnabled bool) bool {
	return proxyEnabled && machine != nil && machine.Backend == firecrackerBackend
}

func gatewayDNSPublicIdentityValid(machine *models.Machine, bridgeCIDR string) bool {
	if machine == nil || strings.TrimSpace(machine.TAPDevice) == "" || strings.TrimSpace(machine.IP) == "" || strings.TrimSpace(machine.GatewayIP) == "" {
		return false
	}
	alloc, err := publicAllocationForCleanup(machine)
	if err != nil || alloc == nil {
		return false
	}
	idx, ok := networkIndexFromTAP(alloc.TAPDevice)
	if !ok || alloc.TAPDevice != fmt.Sprintf("kv%d", idx) {
		return false
	}
	expected, err := allocationForIndex(bridgeCIDR, idx)
	return err == nil && alloc.GuestIP == expected.GuestIP && alloc.GatewayIP == expected.GatewayIP
}

func machineID(machine *models.Machine) string {
	if machine == nil {
		return ""
	}
	return machine.ID
}

func (c *gatewayDNSReadinessController) cachedAgent(ctx context.Context) ([]byte, string, error) {
	for {
		c.agentMu.Lock()
		if len(c.agentPayload) > 0 && c.agentHash != "" {
			payload, hash := c.agentPayload, c.agentHash
			c.agentMu.Unlock()
			return payload, hash, nil
		}
		if attempt := c.agentLoading; attempt != nil {
			c.agentMu.Unlock()
			select {
			case <-ctx.Done():
				return nil, "", fmt.Errorf("wait for gateway DNS agent load: %w", ctx.Err())
			case <-attempt.done:
				if attempt.err != nil {
					return nil, "", attempt.err
				}
				continue
			}
		}

		attempt := &gatewayDNSAgentLoadAttempt{done: make(chan struct{})}
		c.agentLoading = attempt
		load := c.loadAgent
		c.agentMu.Unlock()

		payload, err := load()
		var hash string
		if err == nil && (len(payload) == 0 || len(payload) > gatewayDNSAgentSizeLimit) {
			err = fmt.Errorf("invalid gateway DNS agent payload")
		}
		if err == nil {
			// The configured agent is immutable for one kiln-vmd process. Copy custom
			// loader output before caching so callers cannot mutate the staged bytes.
			payload = append([]byte(nil), payload...)
			hashBytes := sha256.Sum256(payload)
			hash = hex.EncodeToString(hashBytes[:])
		}

		c.agentMu.Lock()
		if err == nil {
			c.agentPayload = payload
			c.agentHash = hash
		}
		attempt.err = err
		c.agentLoading = nil
		close(attempt.done)
		c.agentMu.Unlock()
		if err != nil {
			return nil, "", err
		}
		return payload, hash, nil
	}
}

func (c *gatewayDNSReadinessController) ensureStaged(ctx context.Context, key gatewayDNSStageKey, path string, payload []byte) (uint64, error) {
	return c.stage(ctx, key, path, payload, false, 0)
}

func (c *gatewayDNSReadinessController) restage(ctx context.Context, key gatewayDNSStageKey, path string, payload []byte, observedVersion uint64) (uint64, error) {
	return c.stage(ctx, key, path, payload, true, observedVersion)
}

func (c *gatewayDNSReadinessController) stage(
	ctx context.Context,
	key gatewayDNSStageKey,
	path string,
	payload []byte,
	force bool,
	observedVersion uint64,
) (uint64, error) {
	c.stageMu.Lock()
	if version, ok := c.staged[key]; ok {
		if !force || version != observedVersion {
			c.stageMu.Unlock()
			return version, nil
		}
	}
	if attempt, ok := c.staging[key]; ok {
		c.stageMu.Unlock()
		select {
		case <-ctx.Done():
			return 0, fmt.Errorf("wait for gateway DNS agent staging: %w", ctx.Err())
		case <-attempt.done:
			return attempt.version, attempt.err
		}
	}
	delete(c.staged, key)
	attempt := &gatewayDNSStageAttempt{done: make(chan struct{})}
	c.staging[key] = attempt
	c.stageMu.Unlock()

	// The guest Upload protocol writes a sibling temporary file and atomically
	// renames it into place. A stable target therefore bounds guest retention to
	// one executable per machine without exposing partially written binaries.
	err := c.runtime.Upload(ctx, key.machineID, path, 0700, int64(len(payload)), bytes.NewReader(payload))

	// Generation is read before the stage lock so clearMachine uses the same
	// status -> stage lock order and an old upload can never republish itself.
	c.statusMu.RLock()
	current := c.generations[key.machineID] == key.generation
	c.stageMu.Lock()
	if err == nil && current {
		c.nextStageVersion++
		if c.nextStageVersion == 0 {
			c.nextStageVersion++
		}
		attempt.version = c.nextStageVersion
		c.staged[key] = attempt.version
	} else if err == nil {
		err = errGatewayDNSCheckStale
	}
	attempt.err = err
	delete(c.staging, key)
	close(attempt.done)
	c.stageMu.Unlock()
	c.statusMu.RUnlock()
	return attempt.version, err
}

func (c *gatewayDNSReadinessController) executeProbe(
	ctx context.Context,
	machineID string,
	token gatewayDNSCheckToken,
	stageKey gatewayDNSStageKey,
	stageVersion uint64,
	stagedPath string,
	payload []byte,
	command string,
	expectedServer string,
	expectedName string,
) (dnsprobe.Result, string) {
	for attempt := 0; attempt < 2; attempt++ {
		if !c.checkGenerationCurrent(machineID, token.generation) {
			return dnsprobe.Result{}, gatewayDNSDetailProbeUnavailable
		}
		result, execErr := c.runtime.Exec(ctx, machineID, command, nil)
		failureDetail := gatewayDNSDetailProbeUnavailable
		if execErr == nil && result != nil {
			if result.ExitCode != 126 && result.ExitCode != 127 {
				decoded, decodeErr := decodeGatewayDNSProbeResult(result.Stdout, result.Stderr, result.ExitCode, expectedServer, expectedName)
				if decodeErr == nil {
					return decoded, ""
				}
				failureDetail = gatewayDNSDetailProbeInvalid
			}
		}
		if attempt == 1 {
			return dnsprobe.Result{}, failureDetail
		}
		var stageErr error
		stageVersion, stageErr = c.restage(ctx, stageKey, stagedPath, payload, stageVersion)
		if stageErr != nil {
			return dnsprobe.Result{}, gatewayDNSDetailProbeUnavailable
		}
	}
	return dnsprobe.Result{}, gatewayDNSDetailProbeUnavailable
}

func (c *gatewayDNSReadinessController) beginCheck(machineID string) gatewayDNSCheckToken {
	c.statusMu.Lock()
	defer c.statusMu.Unlock()
	if c.generations[machineID] == 0 {
		c.generations[machineID] = c.nextGenerationLocked()
	}
	c.nextCheckSequence++
	if c.nextCheckSequence == 0 {
		c.nextCheckSequence++
	}
	return gatewayDNSCheckToken{
		generation: c.generations[machineID],
		sequence:   c.nextCheckSequence,
	}
}

func (c *gatewayDNSReadinessController) nextGenerationLocked() uint64 {
	c.nextGeneration++
	if c.nextGeneration == 0 {
		c.nextGeneration++
	}
	return c.nextGeneration
}

func (c *gatewayDNSReadinessController) checkGenerationCurrent(machineID string, generation uint64) bool {
	c.statusMu.RLock()
	defer c.statusMu.RUnlock()
	return generation != 0 && c.generations[machineID] == generation
}

func (c *gatewayDNSReadinessController) clearMachine(machineID string, removeHistory bool) {
	// Advance the epoch before deleting stage entries. Holding statusMu while
	// taking stageMu is the common lock order used by stage publication too.
	c.statusMu.Lock()
	c.generations[machineID] = c.nextGenerationLocked()
	delete(c.status, machineID)
	delete(c.latestSequence, machineID)
	if removeHistory {
		delete(c.lastSuccess, machineID)
	}
	c.stageMu.Lock()
	for key := range c.staged {
		if key.machineID == machineID {
			delete(c.staged, key)
		}
	}
	c.stageMu.Unlock()
	c.statusMu.Unlock()
}

func (c *gatewayDNSReadinessController) finishCheck(machineID string, token gatewayDNSCheckToken, candidate GatewayDNSReadinessStatus) (GatewayDNSReadinessStatus, error) {
	status, current := c.cacheStatus(machineID, token, candidate)
	if !current {
		status = GatewayDNSReadinessStatus{
			Applicable:    candidate.Applicable,
			FailureKind:   "probe",
			Detail:        gatewayDNSDetailProbeUnavailable,
			CheckedAt:     candidate.CheckedAt,
			LastSuccessAt: status.LastSuccessAt,
		}
		return status, &gatewayDNSReadinessError{kind: ErrGatewayDNSProbe, detail: status.Detail}
	}
	if !status.Applicable || status.Ready {
		return status, nil
	}
	switch status.FailureKind {
	case "path":
		return status, &gatewayDNSReadinessError{kind: ErrGatewayDNSPath, detail: status.Detail}
	case "resolution":
		return status, &gatewayDNSReadinessError{kind: ErrGatewayDNSResolution, detail: status.Detail}
	default:
		return status, &gatewayDNSReadinessError{kind: ErrGatewayDNSProbe, detail: status.Detail}
	}
}

func (c *gatewayDNSReadinessController) cacheStatus(machineID string, token gatewayDNSCheckToken, status GatewayDNSReadinessStatus) (GatewayDNSReadinessStatus, bool) {
	c.statusMu.Lock()
	defer c.statusMu.Unlock()
	if c.generations[machineID] != token.generation {
		status.LastSuccessAt = c.lastSuccess[machineID]
		return status, false
	}
	if latest := c.latestSequence[machineID]; latest > token.sequence {
		current, ok := c.status[machineID]
		if ok {
			return current, true
		}
		status.LastSuccessAt = c.lastSuccess[machineID]
		return status, false
	}
	c.latestSequence[machineID] = token.sequence
	if status.Applicable && status.Ready && (c.lastSuccess[machineID].IsZero() || status.CheckedAt.After(c.lastSuccess[machineID])) {
		c.lastSuccess[machineID] = status.CheckedAt
	}
	status.LastSuccessAt = c.lastSuccess[machineID]
	c.status[machineID] = status
	return status, true
}

func (c *gatewayDNSReadinessController) cachedStatus(machineID string) (GatewayDNSReadinessStatus, bool) {
	c.statusMu.RLock()
	defer c.statusMu.RUnlock()
	status, ok := c.status[machineID]
	return status, ok
}

func readGatewayDNSAgent(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open gateway DNS agent %s: %w", path, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspect gateway DNS agent %s: %w", path, err)
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > gatewayDNSAgentSizeLimit {
		return nil, fmt.Errorf("invalid gateway DNS agent payload")
	}
	payload, err := io.ReadAll(io.LimitReader(file, gatewayDNSAgentSizeLimit+1))
	if err != nil {
		return nil, fmt.Errorf("read gateway DNS agent %s: %w", path, err)
	}
	if len(payload) == 0 || len(payload) > gatewayDNSAgentSizeLimit {
		return nil, fmt.Errorf("invalid gateway DNS agent payload")
	}
	return payload, nil
}

func gatewayDNSProbeCommand(path, gatewayIP, probeName string) string {
	args := []string{
		path,
		"dns-probe",
		"--server", net.JoinHostPort(gatewayIP, dnsprobe.DefaultPort),
		"--name", probeName,
		"--timeout", dnsprobe.DefaultTotalTimeout.String(),
		"--transport-timeout", dnsprobe.DefaultTransportTimeout.String(),
	}
	quoted := make([]string, len(args))
	for i, arg := range args {
		quoted[i] = gatewayDNSShellQuote(arg)
	}
	return strings.Join(quoted, " ")
}

func gatewayDNSShellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

func decodeGatewayDNSProbeResult(stdout, stderr string, exitCode int, expectedServer, expectedName string) (dnsprobe.Result, error) {
	if len(stdout) == 0 || len(stdout) > gatewayDNSProbeOutputLimit || strings.TrimSpace(stderr) != "" {
		return dnsprobe.Result{}, fmt.Errorf("invalid probe output")
	}
	decoder := json.NewDecoder(io.LimitReader(strings.NewReader(stdout), gatewayDNSProbeOutputLimit+1))
	decoder.DisallowUnknownFields()
	var result dnsprobe.Result
	if err := decoder.Decode(&result); err != nil {
		return dnsprobe.Result{}, fmt.Errorf("decode probe output: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return dnsprobe.Result{}, fmt.Errorf("probe output contained trailing data")
	}
	if err := validateGatewayDNSProbeResult(result, exitCode, expectedServer, expectedName); err != nil {
		return dnsprobe.Result{}, err
	}
	return result, nil
}

func validateGatewayDNSProbeResult(result dnsprobe.Result, exitCode int, expectedServer, expectedName string) error {
	if result.SchemaVersion != dnsprobe.SchemaVersion || result.Server != expectedServer || result.Name != expectedName || result.QueryType != "A" || result.Error != "" {
		return fmt.Errorf("probe result identity mismatch")
	}
	if err := validateGatewayDNSTransport(result.UDP, "udp"); err != nil {
		return err
	}
	if err := validateGatewayDNSTransport(result.TCP, "tcp"); err != nil {
		return err
	}
	wantPath := result.UDP.PathOK && result.TCP.PathOK
	wantResolution := result.UDP.ResolutionOK && result.TCP.ResolutionOK
	if result.PathOK != wantPath || result.ResolutionOK != wantResolution || (result.ResolutionOK && !result.PathOK) {
		return fmt.Errorf("probe aggregate result mismatch")
	}
	wantFailure := ""
	wantExit := dnsprobe.ExitOK
	if !result.PathOK {
		wantFailure = "path"
		wantExit = dnsprobe.ExitPath
	} else if !result.ResolutionOK {
		wantFailure = "resolution"
		wantExit = dnsprobe.ExitResolution
	}
	if result.FailureKind != wantFailure || exitCode != wantExit || result.ExitCode() != wantExit {
		return fmt.Errorf("probe result classification mismatch")
	}
	return nil
}

func validateGatewayDNSTransport(result dnsprobe.TransportResult, transport string) error {
	if result.Transport != transport || result.DurationMS < 0 || result.DurationMS > 60_000 || result.AnswerCount < 0 || result.AddressCount < 0 || result.AddressCount > result.AnswerCount {
		return fmt.Errorf("invalid %s transport result", transport)
	}
	if result.ResolutionOK && !result.PathOK {
		return fmt.Errorf("invalid %s resolution state", transport)
	}
	if !gatewayDNSProbePhaseAllowed(result.Phase) {
		return fmt.Errorf("invalid %s phase", transport)
	}
	if !result.PathOK {
		if result.FailureKind != "path" || result.Phase == "" || result.RCode != nil || result.RCodeName != "" || result.AnswerCount != 0 || result.AddressCount != 0 || result.Error == "" {
			return fmt.Errorf("invalid %s path failure", transport)
		}
		return nil
	}
	if result.RCode == nil || *result.RCode < 0 || *result.RCode > 15 || result.RCodeName != gatewayDNSRCodeName(*result.RCode) {
		return fmt.Errorf("invalid %s response code", transport)
	}
	if result.ResolutionOK {
		if result.FailureKind != "" || result.Phase != "" || *result.RCode != 0 || result.AddressCount == 0 || result.Error != "" {
			return fmt.Errorf("invalid %s success", transport)
		}
		return nil
	}
	if result.FailureKind != "resolution" || result.Phase != "resolve" || (*result.RCode == 0 && result.AddressCount != 0) || result.Error == "" {
		return fmt.Errorf("invalid %s resolution failure", transport)
	}
	return nil
}

func gatewayDNSProbePhaseAllowed(phase string) bool {
	switch phase {
	case "", "build_query", "dial", "set_deadline", "write", "read", "read_frame", "exchange", "validate_response", "resolve":
		return true
	default:
		return false
	}
}

func gatewayDNSRCodeName(code int) string {
	switch code {
	case 0:
		return "NOERROR"
	case 1:
		return "FORMERR"
	case 2:
		return "SERVFAIL"
	case 3:
		return "NXDOMAIN"
	case 4:
		return "NOTIMP"
	case 5:
		return "REFUSED"
	default:
		return fmt.Sprintf("RCODE%d", code)
	}
}

func gatewayDNSStatusFromResult(result dnsprobe.Result, checkedAt time.Time) GatewayDNSReadinessStatus {
	status := GatewayDNSReadinessStatus{
		Applicable:   true,
		Ready:        result.PathOK && result.ResolutionOK,
		PathOK:       result.PathOK,
		ResolutionOK: result.ResolutionOK,
		FailureKind:  result.FailureKind,
		CheckedAt:    checkedAt,
	}
	if status.Ready {
		status.Detail = gatewayDNSDetailReady
		return status
	}
	failed := result.UDP
	if (result.FailureKind == "path" && result.UDP.PathOK) || (result.FailureKind == "resolution" && result.UDP.ResolutionOK) {
		failed = result.TCP
	}
	status.Transport = failed.Transport
	status.Phase = failed.Phase
	status.RCodeName = failed.RCodeName
	if result.FailureKind == "path" {
		status.Detail = gatewayDNSDetailPathFailed
	} else {
		status.Detail = gatewayDNSDetailResolutionFailed
	}
	return status
}
