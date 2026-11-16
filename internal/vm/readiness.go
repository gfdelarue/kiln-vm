package vm

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gfdelarue/kiln-vm/internal/apierr"
	"github.com/gfdelarue/kiln-vm/internal/execution"
	"github.com/gfdelarue/kiln-vm/internal/machine"
	"github.com/gfdelarue/kiln-vm/internal/models"
)

const (
	defaultGatewayDNSReadinessTimeout  = 30 * time.Second
	defaultGatewayDNSReadinessInterval = 500 * time.Millisecond
)

type gatewayDNSReadinessChecker interface {
	CheckGatewayDNSReadiness(context.Context, string) (machine.GatewayDNSReadinessStatus, error)
}

func profileAccess(profile Profile) models.VMProfileAccess {
	access := profile.Access
	if access.Kind != "" {
		return access
	}
	if profile.GuestOS == "windows" {
		return sshAccess("kiln-vm")
	}
	return guestAgentAccess()
}

func profileReadiness(profile Profile) models.VMProfileReadiness {
	readiness := profile.Readiness
	if readiness.Kind != "" {
		return readiness
	}
	if profileAccess(profile).Kind == models.VMProfileAccessSSH {
		return sshExecReadiness()
	}
	return guestAgentReadiness()
}

func (s *Service) waitProfileReady(ctx context.Context, record *models.VMRecord) error {
	profile, err := getProfile(record.Profile)
	if err != nil {
		return err
	}
	readiness := normalizeReadiness(profileReadiness(profile))
	switch readiness.Kind {
	case models.VMProfileReadinessGuestAgentExec:
		return s.waitExecReadiness(ctx, record, readiness, "vm guest agent")
	case models.VMProfileReadinessSSHExec:
		return s.waitExecReadiness(ctx, record, readiness, "vm ssh readiness")
	default:
		return &apierr.ValidationError{Field: "profile.readiness.kind", Message: fmt.Sprintf("unsupported readiness strategy %q", readiness.Kind)}
	}
}

func (s *Service) waitGatewayDNSReady(ctx context.Context, record *models.VMRecord) error {
	checker, ok := s.machines.(gatewayDNSReadinessChecker)
	if !ok {
		return nil
	}
	timeout := s.gatewayDNSReadyTimeout
	if timeout <= 0 {
		timeout = defaultGatewayDNSReadinessTimeout
	}
	interval := s.gatewayDNSReadyInterval
	if interval <= 0 {
		interval = defaultGatewayDNSReadinessInterval
	}
	// Gateway DNS is an additional provider gate after profile readiness. Cap
	// it independently even when the caller has a much longer operation deadline.
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	var lastErr error
	var lastStatus machine.GatewayDNSReadinessStatus
	for {
		status, err := checker.CheckGatewayDNSReadiness(ctx, record.MachineID)
		if err == nil && (!status.Applicable || status.Ready) {
			return nil
		}
		lastStatus = sanitizeGatewayDNSReadinessStatus(status)
		if err != nil {
			lastErr = err
		} else {
			lastErr = fmt.Errorf("gateway DNS readiness status was not ready: %w", machine.ErrGatewayDNSProbe)
		}
		if stateErr := s.waitReadyTerminalState(record, lastErr, "vm gateway DNS"); stateErr != nil {
			return gatewayDNSReadinessOperationError(record, lastStatus, stateErr)
		}
		select {
		case <-ctx.Done():
			return gatewayDNSReadinessOperationError(record, lastStatus, ctx.Err())
		case <-ticker.C:
		}
	}
}

func sanitizeGatewayDNSReadinessStatus(status machine.GatewayDNSReadinessStatus) machine.GatewayDNSReadinessStatus {
	safe := machine.GatewayDNSReadinessStatus{
		Applicable:    status.Applicable,
		Ready:         status.Ready,
		PathOK:        status.PathOK,
		ResolutionOK:  status.ResolutionOK,
		CheckedAt:     status.CheckedAt,
		LastSuccessAt: status.LastSuccessAt,
	}
	switch status.FailureKind {
	case "path", "resolution", "probe":
		safe.FailureKind = status.FailureKind
	default:
		safe.FailureKind = "probe"
	}
	switch status.Transport {
	case "udp", "tcp":
		safe.Transport = status.Transport
	}
	switch status.Phase {
	case "build_query", "dial", "set_deadline", "write", "read", "read_frame", "exchange", "validate_response", "resolve":
		safe.Phase = status.Phase
	}
	switch status.RCodeName {
	case "NOERROR", "FORMERR", "SERVFAIL", "NXDOMAIN", "NOTIMP", "REFUSED", "RCODE6", "RCODE7", "RCODE8", "RCODE9", "RCODE10", "RCODE11", "RCODE12", "RCODE13", "RCODE14", "RCODE15":
		safe.RCodeName = status.RCodeName
	}
	switch status.Detail {
	case "ready", "not_applicable", "probe_unavailable", "probe_output_invalid", "gateway_dns_path_failed", "gateway_dns_resolution_failed":
		safe.Detail = status.Detail
	default:
		safe.Detail = "probe_unavailable"
	}
	return safe
}

func gatewayDNSReadinessOperationError(record *models.VMRecord, status machine.GatewayDNSReadinessStatus, trigger error) error {
	status = sanitizeGatewayDNSReadinessStatus(status)
	kind := machine.ErrGatewayDNSProbe
	switch status.FailureKind {
	case "path":
		kind = machine.ErrGatewayDNSPath
	case "resolution":
		kind = machine.ErrGatewayDNSResolution
	}
	causes := []error{kind, apierr.ErrConflict}
	for _, candidate := range []error{context.Canceled, context.DeadlineExceeded, ErrVMStoppedBeforeReady, ErrVMErrorBeforeReady} {
		if errors.Is(trigger, candidate) {
			causes = append(causes, candidate)
		}
	}
	cause := fmt.Errorf("VM %s gateway DNS readiness failed (%s): %w", record.Name, status.Detail, errors.Join(causes...))
	diagnostic := apierr.OperationDiagnostic{
		Category:  "provider_network",
		Phase:     "gateway_dns_readiness",
		Step:      status.Phase,
		MachineID: record.MachineID,
		State:     status.FailureKind,
		Mode:      status.Transport,
		Retryable: true,
		Error:     status.Detail,
	}
	return &apierr.OperationError{
		Code:        "gateway_dns_not_ready",
		StatusRef:   fmt.Sprintf("/vms/%s", record.Name),
		Phase:       "gateway_dns_readiness",
		Resource:    record.Name,
		Action:      "verify",
		Retryable:   true,
		Diagnostics: []apierr.OperationDiagnostic{diagnostic},
		Err:         cause,
	}
}

func normalizeReadiness(readiness models.VMProfileReadiness) models.VMProfileReadiness {
	if readiness.TimeoutSeconds <= 0 {
		readiness.TimeoutSeconds = 120
	}
	if readiness.ProbeIntervalMillis <= 0 {
		readiness.ProbeIntervalMillis = 500
	}
	if strings.TrimSpace(readiness.Command) == "" {
		readiness.Command = "echo kiln-vm-ready"
	}
	if readiness.ExpectedStdoutSubstring == "" {
		readiness.ExpectedStdoutSubstring = "kiln-vm-ready"
	}
	return readiness
}

func (s *Service) waitExecReadiness(ctx context.Context, record *models.VMRecord, readiness models.VMProfileReadiness, probe string) error {
	ctx, cancel := contextWithDefaultTimeout(ctx, time.Duration(readiness.TimeoutSeconds)*time.Second)
	defer cancel()
	ticker := time.NewTicker(time.Duration(readiness.ProbeIntervalMillis) * time.Millisecond)
	defer ticker.Stop()

	var lastErr error
	for {
		result, err := machine.ExecuteBuffered(ctx, s.machines, record.MachineID, readiness.Command, nil)
		if err == nil && result.ExitCode == 0 && strings.Contains(result.Stdout, readiness.ExpectedStdoutSubstring) {
			return nil
		}
		if err != nil {
			var execErr *execution.Error
			if errors.As(err, &execErr) && execErr.Category == execution.CategoryRuntimeIncompatible {
				return fmt.Errorf("exec readiness runtime incompatible: %w", err)
			}
			lastErr = err
		} else {
			lastErr = fmt.Errorf("readiness command exit %d stdout=%q stderr=%q", result.ExitCode, result.Stdout, result.Stderr)
		}
		if stateErr := s.waitReadyTerminalState(record, lastErr, probe); stateErr != nil {
			return stateErr
		}
		select {
		case <-ctx.Done():
			return readinessTimeoutError(probe, ctx.Err(), lastErr)
		case <-ticker.C:
		}
	}
}

func readinessTimeoutError(probe string, err error, lastErr error) error {
	if lastErr != nil {
		return fmt.Errorf("wait for %s: %w; last error: %v", probe, err, lastErr)
	}
	return fmt.Errorf("wait for %s: %w", probe, err)
}
