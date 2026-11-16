package models

import "time"

type MachineState string

const (
	MachineStateCreating MachineState = "creating"
	MachineStateRunning  MachineState = "running"
	MachineStateStopping MachineState = "stopping"
	MachineStateStopped  MachineState = "stopped"
	MachineStateError    MachineState = "error"
)

const (
	MachineStopReasonDaemonShutdown  = "daemon shutdown"
	MachineStopReasonDaemonRestarted = "daemon restarted"
)

type Machine struct {
	ID            string             `json:"id"`
	Name          string             `json:"name"`
	Backend       string             `json:"backend"`
	GuestOS       string             `json:"guest_os,omitempty"`
	Libvirt       *LibvirtDomainSpec `json:"libvirt,omitempty"`
	ImageID       string             `json:"image_id"`
	NetworkPolicy string             `json:"network_policy,omitempty"`
	State         MachineState       `json:"state"`
	VCPUs         int                `json:"-"`
	CPU           CPUConfig          `json:"cpu"`
	MemoryMB      int                `json:"memory_mb"`
	VSockCID      uint32             `json:"vsock_cid,omitempty"`
	IP            string             `json:"ip,omitempty"`
	GatewayIP     string             `json:"gateway_ip,omitempty"`
	TAPDevice     string             `json:"tap_device,omitempty"`
	NetworkCIDR   string             `json:"-"`
	NetworkIndex  uint32             `json:"-"`
	GuestMAC      string             `json:"-"`
	Netmask       string             `json:"-"`
	Error         string             `json:"error,omitempty"`
	Diagnostics   []Diagnostic       `json:"diagnostics,omitempty"`
	CreatedAt     time.Time          `json:"created_at"`
	UpdatedAt     time.Time          `json:"updated_at"`
}

type Diagnostic struct {
	Category     string `json:"category"`
	Phase        string `json:"phase,omitempty"`
	Resource     string `json:"resource,omitempty"`
	Action       string `json:"action,omitempty"`
	Retryable    bool   `json:"retryable"`
	Backend      string `json:"backend,omitempty"`
	MachineID    string `json:"machine_id,omitempty"`
	KernelPath   string `json:"kernel_path,omitempty"`
	KernelType   string `json:"kernel_type,omitempty"`
	RootfsPath   string `json:"rootfs_path,omitempty"`
	ConfigPath   string `json:"config_path,omitempty"`
	SocketPath   string `json:"socket_path,omitempty"`
	VSockPath    string `json:"vsock_path,omitempty"`
	TAPDevice    string `json:"tap_device,omitempty"`
	NetworkCIDR  string `json:"network_cidr,omitempty"`
	NetworkIndex uint32 `json:"network_index,omitempty"`
	GuestIP      string `json:"guest_ip,omitempty"`
	GatewayIP    string `json:"gateway_ip,omitempty"`
	GuestMAC     string `json:"guest_mac,omitempty"`
	Netmask      string `json:"netmask,omitempty"`
	TAPAlias     string `json:"tap_alias,omitempty"`
	LogPath      string `json:"log_path,omitempty"`
	Cleanup      string `json:"cleanup,omitempty"`
	Remediation  string `json:"remediation,omitempty"`
	Error        string `json:"error,omitempty"`
	StdoutTail   string `json:"stdout_tail,omitempty"`
	StderrTail   string `json:"stderr_tail,omitempty"`
}

type CreateMachineRequest struct {
	Name          string             `json:"name"`
	Backend       string             `json:"backend,omitempty"`
	GuestOS       string             `json:"guest_os,omitempty"`
	Libvirt       *LibvirtDomainSpec `json:"libvirt,omitempty"`
	ImageID       string             `json:"image_id"`
	NetworkPolicy string             `json:"network_policy,omitempty"`
	CPU           *CPUConfig         `json:"cpu,omitempty"`
	VCPUs         int                `json:"-"`
	MemoryMB      int                `json:"memory_mb"`
}

type CPUConfig struct {
	Kind   string `json:"kind,omitempty"`
	Millis int    `json:"millis,omitempty"`
	VCPUs  int    `json:"vcpus,omitempty"`
}

type LibvirtDomainSpec struct {
	OSInfo     string              `json:"osinfo,omitempty"`
	Firmware   string              `json:"firmware,omitempty"`
	SecureBoot bool                `json:"secure_boot,omitempty"`
	SMM        bool                `json:"smm,omitempty"`
	TPM        *LibvirtTPMSpec     `json:"tpm,omitempty"`
	Disk       LibvirtDiskSpec     `json:"disk"`
	Network    LibvirtNetworkSpec  `json:"network"`
	Graphics   LibvirtGraphicsSpec `json:"graphics"`
}

type LibvirtTPMSpec struct {
	BackendType    string `json:"backend_type,omitempty"`
	BackendVersion string `json:"backend_version,omitempty"`
	Model          string `json:"model,omitempty"`
}

type LibvirtDiskSpec struct {
	Bus    string `json:"bus,omitempty"`
	Format string `json:"format,omitempty"`
	Cache  string `json:"cache,omitempty"`
}

type LibvirtNetworkSpec struct {
	Source string `json:"source,omitempty"`
	Model  string `json:"model,omitempty"`
}

type LibvirtGraphicsSpec struct {
	Type   string `json:"type,omitempty"`
	Listen string `json:"listen,omitempty"`
}
