package vm

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/gfdelarue/kiln-vm/internal/apierr"
	"github.com/gfdelarue/kiln-vm/internal/execution"
	"github.com/gfdelarue/kiln-vm/internal/machine"
	"github.com/gfdelarue/kiln-vm/internal/models"
)

const dockerCapability = "docker"
const dockerCapabilityImage = "kiln-vm/docker-capability:local"
const dockerCapabilityRootfsTar = "/usr/share/kiln-vm/docker-capability-rootfs.tar"
const dockerCapabilityResponse = "kiln-vm-net-ok"

type capabilityVerifier struct {
	Capability  string
	Phase       string
	Command     string
	Remediation string
	Run         func(context.Context, machine.MachineService, string) (*execution.BufferedResult, error)
}

var capabilityVerifiers = map[string]capabilityVerifier{
	dockerCapability: {
		Capability:  dockerCapability,
		Phase:       "docker_networking",
		Command:     dockerCapabilityCommand,
		Remediation: "Use a kiln-vm profile/kernel that supports Docker bridge networking, including iptables raw and nat tables.",
		Run: func(ctx context.Context, machines machine.MachineService, machineID string) (*execution.BufferedResult, error) {
			return machine.ExecuteBuffered(ctx, machines, machineID, dockerCapabilityCommand, nil)
		},
	},
}

var dockerCapabilityCommand = strings.NewReplacer(
	"__CAPABILITY_IMAGE__", dockerCapabilityImage,
	"__CAPABILITY_ROOTFS_TAR__", dockerCapabilityRootfsTar,
	"__RESPONSE_LENGTH__", fmt.Sprint(len(dockerCapabilityResponse)),
	"__RESPONSE__", dockerCapabilityResponse,
).Replace(`set -eu
if ! command -v docker >/dev/null 2>&1; then
  echo "docker CLI not found" >&2
  exit 127
fi
for i in $(seq 1 60); do
  if docker version --format '{{.Server.Version}}' >/tmp/kiln-vm-docker-version 2>/tmp/kiln-vm-docker-version.err; then
    break
  fi
  sleep 1
done
if ! docker version --format '{{.Server.Version}}' >/tmp/kiln-vm-docker-version 2>/tmp/kiln-vm-docker-version.err; then
  echo "docker daemon not ready" >&2
  cat /tmp/kiln-vm-docker-version.err >&2 || true
  tail -n 80 /var/log/dockerd.log >&2 || true
  exit 1
fi
iptables -t nat -S >/dev/null
iptables -t raw -S >/dev/null
capability_image="__CAPABILITY_IMAGE__"
capability_rootfs="__CAPABILITY_ROOTFS_TAR__"
if ! docker image inspect "$capability_image" >/dev/null 2>&1; then
  if [ ! -f "$capability_rootfs" ]; then
    echo "kiln-vm Docker capability image seed is missing at $capability_rootfs" >&2
    echo "Use a refreshed ubuntu-docker profile image built by a kiln-vm release that includes the local Docker capability image." >&2
    exit 1
  fi
  docker import "$capability_rootfs" "$capability_image" >/dev/null
fi
suffix="$(date +%s)-$$"
network="kiln-vm-cap-net-$suffix"
container="kiln-vm-cap-web-$suffix"
cleanup() {
  docker rm -f "$container" >/dev/null 2>&1 || true
  docker network rm "$network" >/dev/null 2>&1 || true
}
trap cleanup EXIT
docker network create "$network" >/dev/null
docker run --rm --pull=never "$capability_image" /bin/sh -c 'echo kiln-vm-docker-container-ok' | grep -q kiln-vm-docker-container-ok
docker run -d --pull=never --name "$container" --network "$network" -p 127.0.0.1::8080 "$capability_image" /bin/sh -c 'while true; do printf "HTTP/1.1 200 OK\r\nContent-Length: __RESPONSE_LENGTH__\r\n\r\n__RESPONSE__" | /bin/nc -l -p 8080; done' >/dev/null
host_port="$(docker port "$container" 8080/tcp | sed -n 's/.*://p' | head -n1)"
if [ -z "$host_port" ]; then
  echo "Docker did not publish a host port for the capability container" >&2
  docker port "$container" 8080/tcp >&2 || true
  exit 1
fi
for i in $(seq 1 30); do
  if docker run --rm --pull=never --network "$network" "$capability_image" /bin/wget -qO- "http://$container:8080" 2>/dev/null | grep -q __RESPONSE__; then
    break
  fi
  sleep 1
done
docker run --rm --pull=never --network "$network" "$capability_image" /bin/wget -qO- "http://$container:8080" 2>/dev/null | grep -q __RESPONSE__
for i in $(seq 1 30); do
  if wget -qO- "http://127.0.0.1:$host_port" 2>/dev/null | grep -q __RESPONSE__; then
    echo docker-capability-ok
    exit 0
  fi
  sleep 1
done
echo "published Docker port was not reachable inside guest" >&2
exit 1`)

func (s *Service) initializeVMCapabilityConditions(vmID string, capabilities []string) error {
	for _, capability := range capabilities {
		condition := pendingCapabilityCondition(capability, "waiting for verification")
		if err := s.db.UpsertVMCapabilityCondition(vmID, condition); err != nil {
			return fmt.Errorf("initialize VM capability condition %q for %s: %w", capability, vmID, err)
		}
	}
	return nil
}

func pendingCapabilityCondition(capability, detail string) models.VMCapabilityCondition {
	now := time.Now().UTC()
	return models.VMCapabilityCondition{
		Capability: capability,
		Status:     models.VMCapabilityStatusPending,
		Detail:     detail,
		UpdatedAt:  now,
	}
}

func (s *Service) declaredCapabilityConditions(record *models.VMRecord) ([]models.VMCapabilityCondition, error) {
	profile, err := getProfile(record.Profile)
	if err != nil {
		return nil, err
	}
	stored, err := s.db.ListVMCapabilityConditions(record.ID)
	if err != nil {
		return nil, fmt.Errorf("list VM capability conditions: %w", err)
	}
	return mergeDeclaredCapabilityConditions(profile.Capabilities, stored), nil
}

func mergeDeclaredCapabilityConditions(declared []string, stored []models.VMCapabilityCondition) []models.VMCapabilityCondition {
	byCapability := map[string]models.VMCapabilityCondition{}
	for _, condition := range stored {
		byCapability[condition.Capability] = condition
	}
	conditions := make([]models.VMCapabilityCondition, 0, len(declared))
	for _, capability := range declared {
		if condition, ok := byCapability[capability]; ok {
			conditions = append(conditions, condition)
			continue
		}
		conditions = append(conditions, pendingCapabilityCondition(capability, "waiting for verification"))
	}
	return conditions
}

func (s *Service) VerifyCapabilities(ctx context.Context, ref string, req *models.VerifyVMCapabilitiesRequest) ([]models.VMCapabilityCondition, error) {
	if req == nil {
		req = &models.VerifyVMCapabilitiesRequest{}
	}
	record, err := s.getRecord(ref)
	if err != nil {
		return nil, err
	}
	if err := s.waitProfileReady(ctx, record); err != nil {
		return nil, err
	}
	conditions, err := s.verifyProfileCapabilities(ctx, record, strings.TrimSpace(req.Capability), req.Force)
	if err != nil {
		return conditions, err
	}
	return conditions, nil
}

func (s *Service) verifyDeclaredCapabilities(ctx context.Context, record *models.VMRecord) error {
	_, err := s.verifyProfileCapabilities(ctx, record, "", false)
	return err
}

func (s *Service) verifyProfileCapabilities(ctx context.Context, record *models.VMRecord, only string, force bool) ([]models.VMCapabilityCondition, error) {
	profile, err := getProfile(record.Profile)
	if err != nil {
		return nil, err
	}
	capabilities := profile.Capabilities
	if only != "" {
		if !containsCapability(capabilities, only) {
			return nil, &apierr.ValidationError{
				Field:   "capability",
				Message: fmt.Sprintf("profile %q does not declare capability %q", profile.Name, only),
			}
		}
		capabilities = []string{only}
	}
	if len(capabilities) > 0 && !profileAccessSupported(profileAccess(profile)) {
		return nil, unsupportedProfileAccessError(profile, "capability verification")
	}

	stored, err := s.db.ListVMCapabilityConditions(record.ID)
	if err != nil {
		return nil, fmt.Errorf("list VM capability conditions: %w", err)
	}
	current := map[string]models.VMCapabilityCondition{}
	for _, condition := range stored {
		current[condition.Capability] = condition
	}

	var verified []models.VMCapabilityCondition
	for _, capability := range capabilities {
		condition := current[capability]
		if condition.Status == models.VMCapabilityStatusOK && !force {
			verified = append(verified, condition)
			continue
		}
		condition, err = s.verifyCapability(ctx, record, capability)
		verified = append(verified, condition)
		if err != nil {
			return verified, err
		}
	}
	if len(capabilities) == 0 {
		return []models.VMCapabilityCondition{}, nil
	}
	return verified, nil
}

func (s *Service) verifyCapability(ctx context.Context, record *models.VMRecord, capability string) (models.VMCapabilityCondition, error) {
	verifier, ok := capabilityVerifiers[capability]
	if !ok {
		condition := failedCapabilityCondition(capability, "unsupported", "", "kiln-vm has no verifier for declared capability "+capability, "Upgrade kiln-vm or remove the unsupported capability from the profile.")
		_ = s.db.UpsertVMCapabilityCondition(record.ID, condition)
		return condition, capabilityOperationError(record, condition, nil, nil)
	}

	now := time.Now().UTC()
	verifying := models.VMCapabilityCondition{
		Capability:  capability,
		Status:      models.VMCapabilityStatusVerifying,
		Phase:       verifier.Phase,
		Command:     verifier.Command,
		Detail:      "verification running",
		Remediation: verifier.Remediation,
		UpdatedAt:   now,
	}
	if err := s.db.UpsertVMCapabilityCondition(record.ID, verifying); err != nil {
		return verifying, fmt.Errorf("record VM capability verifying condition: %w", err)
	}

	result, execErr := verifier.Run(ctx, s.machines, record.MachineID)
	if execErr != nil {
		condition := failedCapabilityCondition(capability, verifier.Phase, verifier.Command, execErr.Error(), verifier.Remediation)
		if err := s.db.UpsertVMCapabilityCondition(record.ID, condition); err != nil {
			return condition, fmt.Errorf("record VM capability failure: %w", err)
		}
		return condition, capabilityOperationError(record, condition, result, execErr)
	}
	if result == nil {
		condition := failedCapabilityCondition(capability, verifier.Phase, verifier.Command, "capability verifier returned no result", verifier.Remediation)
		if err := s.db.UpsertVMCapabilityCondition(record.ID, condition); err != nil {
			return condition, fmt.Errorf("record VM capability failure: %w", err)
		}
		return condition, capabilityOperationError(record, condition, result, nil)
	}
	if result.ExitCode != 0 {
		detail := fmt.Sprintf("command exited %d", result.ExitCode)
		if strings.TrimSpace(result.Stderr) != "" {
			detail += ": " + tailString(result.Stderr, 2000)
		}
		condition := failedCapabilityCondition(capability, verifier.Phase, verifier.Command, detail, verifier.Remediation)
		if err := s.db.UpsertVMCapabilityCondition(record.ID, condition); err != nil {
			return condition, fmt.Errorf("record VM capability failure: %w", err)
		}
		return condition, capabilityOperationError(record, condition, result, nil)
	}

	checkedAt := time.Now().UTC()
	condition := models.VMCapabilityCondition{
		Capability:  capability,
		Status:      models.VMCapabilityStatusOK,
		Phase:       verifier.Phase,
		Command:     verifier.Command,
		Detail:      strings.TrimSpace(withDefault(result.Stdout, capability+" capability verified")),
		Remediation: "",
		CheckedAt:   &checkedAt,
		UpdatedAt:   checkedAt,
	}
	if err := s.db.UpsertVMCapabilityCondition(record.ID, condition); err != nil {
		return condition, fmt.Errorf("record VM capability success: %w", err)
	}
	return condition, nil
}

func failedCapabilityCondition(capability, phase, command, detail, remediation string) models.VMCapabilityCondition {
	checkedAt := time.Now().UTC()
	return models.VMCapabilityCondition{
		Capability:  capability,
		Status:      models.VMCapabilityStatusFailed,
		Phase:       phase,
		Command:     command,
		Detail:      detail,
		Remediation: remediation,
		CheckedAt:   &checkedAt,
		UpdatedAt:   checkedAt,
	}
}

func capabilityOperationError(record *models.VMRecord, condition models.VMCapabilityCondition, result *execution.BufferedResult, cause error) error {
	detail := condition.Detail
	if cause != nil {
		detail = cause.Error()
	}
	err := fmt.Errorf("VM %s capability %s failed: %s: %w", record.Name, condition.Capability, detail, apierr.ErrConflict)
	diagnostic := apierr.OperationDiagnostic{
		Category: "vm_capability",
		Machine:  record.MachineID,
		Step:     condition.Capability,
		State:    string(condition.Status),
		Error:    detail,
	}
	if result != nil {
		diagnostic.StdoutTail = tailString(result.Stdout, 2000)
		diagnostic.StderrTail = tailString(result.Stderr, 2000)
	}
	return &apierr.OperationError{
		Code:        "vm_capability_failed",
		Phase:       condition.Phase,
		Resource:    record.Name,
		Action:      "verify",
		Retryable:   true,
		Diagnostics: []apierr.OperationDiagnostic{diagnostic},
		Err:         err,
	}
}

func containsCapability(capabilities []string, target string) bool {
	for _, capability := range capabilities {
		if capability == target {
			return true
		}
	}
	return false
}

func tailString(value string, limit int) string {
	if limit <= 0 || len(value) <= limit {
		return value
	}
	return value[len(value)-limit:]
}

func withDefault(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}
