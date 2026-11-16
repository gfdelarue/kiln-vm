package models

import "time"

type VM struct {
	ID            string                  `json:"id"`
	Name          string                  `json:"name"`
	Profile       string                  `json:"profile"`
	Backend       string                  `json:"backend"`
	GuestOS       string                  `json:"guest_os,omitempty"`
	MachineID     string                  `json:"machine_id"`
	ImageID       string                  `json:"image_id"`
	Image         string                  `json:"image,omitempty"`
	Labels        map[string]string       `json:"labels,omitempty"`
	NetworkPolicy string                  `json:"network_policy,omitempty"`
	State         MachineState            `json:"state"`
	IP            string                  `json:"ip,omitempty"`
	GatewayIP     string                  `json:"gateway_ip,omitempty"`
	TAPDevice     string                  `json:"tap_device,omitempty"`
	VSockCID      uint32                  `json:"vsock_cid,omitempty"`
	VCPUs         int                     `json:"-"`
	CPU           CPUConfig               `json:"cpu"`
	MemoryMB      int                     `json:"memory_mb"`
	DiskSizeMB    int64                   `json:"disk_size_mb"`
	Power         VMPower                 `json:"power"`
	Capabilities  []VMCapabilityCondition `json:"capabilities,omitempty"`
	Error         string                  `json:"error,omitempty"`
	Diagnostics   []Diagnostic            `json:"diagnostics,omitempty"`
	CreatedAt     time.Time               `json:"created_at"`
	UpdatedAt     time.Time               `json:"updated_at"`
}

type VMRecord struct {
	ID         string
	Name       string
	Profile    string
	MachineID  string
	ImageID    string
	VMImage    string
	Labels     map[string]string
	DiskSizeMB int64
	Power      VMPower
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

type CreateVMRequest struct {
	Name          string            `json:"name"`
	Profile       string            `json:"profile,omitempty"`
	Image         string            `json:"image,omitempty"`
	Labels        map[string]string `json:"labels,omitempty"`
	NetworkPolicy string            `json:"network_policy,omitempty"`
	CPU           *CPUConfig        `json:"cpu,omitempty"`
	VCPUs         int               `json:"-"`
	MemoryMB      int               `json:"memory_mb,omitempty"`
	DiskSizeMB    int64             `json:"disk_size_mb,omitempty"`
	Power         *VMPowerPolicy    `json:"power,omitempty"`
}

type VMCreatePlan struct {
	Name                string            `json:"name"`
	Profile             string            `json:"profile"`
	Image               string            `json:"image,omitempty"`
	Labels              map[string]string `json:"labels,omitempty"`
	NetworkPolicy       string            `json:"network_policy,omitempty"`
	CPU                 CPUConfig         `json:"cpu"`
	MemoryMB            int               `json:"memory_mb"`
	DiskSizeMB          int64             `json:"disk_size_mb"`
	Power               VMPowerPolicy     `json:"power"`
	ProfileCapabilities []string          `json:"profile_capabilities,omitempty"`
}

type VMPowerMode string

const (
	VMPowerModeManual   VMPowerMode = "manual"
	VMPowerModeAlways   VMPowerMode = "always"
	VMPowerModeOnDemand VMPowerMode = "on_demand"
	VMPowerModeWarm     VMPowerMode = "warm"
)

type VMPowerPolicy struct {
	Mode               VMPowerMode `json:"mode"`
	IdleTimeoutSeconds int         `json:"idle_timeout_seconds,omitempty"`
}

func (p VMPowerPolicy) StartsWithDaemon() bool {
	return p.Mode == VMPowerModeAlways || p.Mode == VMPowerModeWarm
}

func (p VMPowerPolicy) WakesOnUse() bool {
	return p.Mode == VMPowerModeAlways || p.Mode == VMPowerModeOnDemand || p.Mode == VMPowerModeWarm
}

type VMPower struct {
	Policy         VMPowerPolicy `json:"policy"`
	ActiveLeases   int           `json:"active_leases,omitempty"`
	LastActivityAt *time.Time    `json:"last_activity_at,omitempty"`
	LastStartedAt  *time.Time    `json:"last_started_at,omitempty"`
	LastStoppedAt  *time.Time    `json:"last_stopped_at,omitempty"`
	StopReason     string        `json:"stop_reason,omitempty"`
}

type VMUseLease interface {
	Release()
}

type VMCapabilityStatus string

const (
	VMCapabilityStatusPending   VMCapabilityStatus = "pending"
	VMCapabilityStatusVerifying VMCapabilityStatus = "verifying"
	VMCapabilityStatusOK        VMCapabilityStatus = "ok"
	VMCapabilityStatusFailed    VMCapabilityStatus = "failed"
)

type VMCapabilityCondition struct {
	Capability  string             `json:"capability"`
	Status      VMCapabilityStatus `json:"status"`
	Phase       string             `json:"phase,omitempty"`
	Command     string             `json:"command,omitempty"`
	Detail      string             `json:"detail,omitempty"`
	Remediation string             `json:"remediation,omitempty"`
	CheckedAt   *time.Time         `json:"checked_at,omitempty"`
	UpdatedAt   time.Time          `json:"updated_at"`
}

type VerifyVMCapabilitiesRequest struct {
	Capability string `json:"capability,omitempty"`
	Force      bool   `json:"force,omitempty"`
}

type VerifyVMCapabilitiesResponse struct {
	Conditions []VMCapabilityCondition `json:"conditions"`
}

type UpdateVMPowerRequest struct {
	Policy VMPowerPolicy `json:"policy"`
}

type VMProfile struct {
	Name                string               `json:"name"`
	Description         string               `json:"description"`
	Backend             string               `json:"backend"`
	GuestOS             string               `json:"guest_os"`
	Libvirt             *LibvirtDomainSpec   `json:"libvirt,omitempty"`
	Networking          VMProfileNetworking  `json:"networking"`
	Access              VMProfileAccess      `json:"access"`
	Readiness           VMProfileReadiness   `json:"readiness"`
	Preparation         VMProfilePreparation `json:"preparation"`
	RequiresPreparation bool                 `json:"requires_preparation,omitempty"`
	Prepared            bool                 `json:"prepared,omitempty"`
	Capabilities        []string             `json:"capabilities,omitempty"`
	DefaultDiskSizeMB   int64                `json:"default_disk_size_mb"`
	DefaultVCPUs        int                  `json:"default_vcpus"`
	DefaultMemoryMB     int                  `json:"default_memory_mb"`
}

type VMProfilePublicNetworkKind string

const (
	VMProfilePublicNetworkFirecrackerNAT VMProfilePublicNetworkKind = "firecracker-nat"
	VMProfilePublicNetworkLibvirtNAT     VMProfilePublicNetworkKind = "libvirt-nat"
)

type VMProfileNetworking struct {
	Public VMProfilePublicNetwork `json:"public"`
}

type VMProfilePublicNetwork struct {
	Kind   VMProfilePublicNetworkKind `json:"kind"`
	Source string                     `json:"source,omitempty"`
}

type VMProfileAccessKind string

const (
	VMProfileAccessGuestAgent VMProfileAccessKind = "guest-agent"
	VMProfileAccessSSH        VMProfileAccessKind = "ssh"
)

type VMProfileAccess struct {
	Kind VMProfileAccessKind `json:"kind"`
	User string              `json:"user,omitempty"`
}

type VMProfileReadinessKind string

const (
	VMProfileReadinessGuestAgentExec VMProfileReadinessKind = "guest-agent-exec"
	VMProfileReadinessSSHExec        VMProfileReadinessKind = "ssh-exec"
)

type VMProfileReadiness struct {
	Kind                    VMProfileReadinessKind `json:"kind"`
	TimeoutSeconds          int                    `json:"timeout_seconds,omitempty"`
	ProbeIntervalMillis     int                    `json:"probe_interval_ms,omitempty"`
	Command                 string                 `json:"command,omitempty"`
	ExpectedStdoutSubstring string                 `json:"expected_stdout_substring,omitempty"`
}

type VMProfilePreparationKind string

const (
	VMProfilePreparationDockerRootfs      VMProfilePreparationKind = "docker-rootfs"
	VMProfilePreparationWindowsUnattended VMProfilePreparationKind = "windows-unattended"
)

type VMProfilePreparation struct {
	Kind           VMProfilePreparationKind `json:"kind"`
	Version        int                      `json:"version,omitempty"`
	RequiredInputs []string                 `json:"required_inputs,omitempty"`
	OptionalInputs []string                 `json:"optional_inputs,omitempty"`
	ImageBootMode  string                   `json:"image_boot_mode,omitempty"`
}

type VMProfileImage struct {
	ID           string    `json:"id"`
	Profile      string    `json:"profile"`
	ImageID      string    `json:"image_id"`
	Fingerprint  string    `json:"fingerprint"`
	RootfsSizeMB int64     `json:"rootfs_size_mb"`
	CreatedAt    time.Time `json:"created_at"`
}

type EnsureVMProfileImageRequest struct {
	Profile      string `json:"profile"`
	RootfsSizeMB int64  `json:"rootfs_size_mb,omitempty"`
	Refresh      bool   `json:"refresh,omitempty"`
}

type PrepareVMProfileRequest struct {
	Strategy           string `json:"strategy,omitempty"`
	MediaPath          string `json:"media_path,omitempty"`
	VirtIOMediaPath    string `json:"virtio_media_path,omitempty"`
	ProductKey         string `json:"product_key,omitempty"`
	ProductKeyFile     string `json:"product_key_file,omitempty"`
	ActivationMode     string `json:"activation_mode,omitempty"`
	Refresh            bool   `json:"refresh,omitempty"`
	WaitTimeoutSeconds int    `json:"wait_timeout_seconds,omitempty"`
}
