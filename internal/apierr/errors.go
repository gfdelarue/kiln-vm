package apierr

import (
	"errors"
	"fmt"
)

// Sentinel errors for domain-level error classification.
var (
	ErrNotFound          = errors.New("not found")
	ErrValidation        = errors.New("validation error")
	ErrConflict          = errors.New("conflict")
	ErrResourceExhausted = errors.New("resource exhausted")
)

// NotFoundError indicates a resource was not found.
type NotFoundError struct {
	Resource string
	ID       string
}

func (e *NotFoundError) Error() string {
	return fmt.Sprintf("%s %q not found", e.Resource, e.ID)
}

func (e *NotFoundError) Unwrap() error {
	return ErrNotFound
}

// AmbiguousReferenceError indicates a shortened resource reference matches
// multiple canonical IDs.
type AmbiguousReferenceError struct {
	Resource string
	Ref      string
	Matches  []string
}

func (e *AmbiguousReferenceError) Error() string {
	msg := fmt.Sprintf("%s ID prefix %q is ambiguous", e.Resource, e.Ref)
	if len(e.Matches) > 0 {
		msg += "; matches:"
		for _, match := range e.Matches {
			msg += "\n  " + match
		}
		msg += "\nprovide more characters"
	}
	return msg
}

func (e *AmbiguousReferenceError) Unwrap() error {
	return ErrConflict
}

// ValidationError indicates invalid input.
type ValidationError struct {
	Field   string
	Message string
	Cause   error
}

func (e *ValidationError) Error() string {
	if e.Field != "" {
		return fmt.Sprintf("validation: %s: %s", e.Field, e.Message)
	}
	return fmt.Sprintf("validation: %s", e.Message)
}

func (e *ValidationError) Unwrap() []error {
	if e.Cause == nil {
		return []error{ErrValidation}
	}
	return []error{ErrValidation, e.Cause}
}

type OperationError struct {
	Code        string
	StatusRef   string
	Phase       string
	Resource    string
	Action      string
	Retryable   bool
	Diagnostics []OperationDiagnostic
	Err         error
}

type OperationDiagnostic struct {
	Category         string  `json:"category"`
	Phase            string  `json:"phase,omitempty"`
	Scope            string  `json:"scope,omitempty"`
	Service          string  `json:"service,omitempty"`
	Step             string  `json:"step,omitempty"`
	Resource         string  `json:"resource,omitempty"`
	Path             string  `json:"path,omitempty"`
	Profile          string  `json:"profile,omitempty"`
	ProfileVersion   int     `json:"profile_version,omitempty"`
	Arch             string  `json:"arch,omitempty"`
	Mode             string  `json:"mode,omitempty"`
	Machine          string  `json:"machine,omitempty"`
	MachineID        string  `json:"machine_id,omitempty"`
	State            string  `json:"state,omitempty"`
	Backend          string  `json:"backend,omitempty"`
	KernelPath       string  `json:"kernel_path,omitempty"`
	KernelType       string  `json:"kernel_type,omitempty"`
	RootfsPath       string  `json:"rootfs_path,omitempty"`
	ConfigPath       string  `json:"config_path,omitempty"`
	SocketPath       string  `json:"socket_path,omitempty"`
	VSockPath        string  `json:"vsock_path,omitempty"`
	TAPDevice        string  `json:"tap_device,omitempty"`
	LogDir           string  `json:"log_dir,omitempty"`
	LogPath          string  `json:"log_path,omitempty"`
	CacheKey         string  `json:"cache_key,omitempty"`
	Cleanup          string  `json:"cleanup,omitempty"`
	Remediation      string  `json:"remediation,omitempty"`
	Retryable        bool    `json:"retryable,omitempty"`
	Error            string  `json:"error,omitempty"`
	AvailableBytes   *uint64 `json:"available_bytes,omitempty"`
	RequiredBytes    *uint64 `json:"required_bytes,omitempty"`
	ReclaimableBytes *uint64 `json:"reclaimable_bytes,omitempty"`
	ArchiveBytes     *uint64 `json:"archive_bytes,omitempty"`
	ExtractedBytes   *uint64 `json:"extracted_bytes,omitempty"`
	UploadCompleted  *bool   `json:"upload_completed,omitempty"`
	RequestedMB      int64   `json:"requested_mb,omitempty"`
	MinimumMB        int64   `json:"minimum_mb,omitempty"`
	StdoutTail       string  `json:"stdout_tail,omitempty"`
	StderrTail       string  `json:"stderr_tail,omitempty"`
	Lines            int     `json:"lines,omitempty"`
}

func (e *OperationError) Error() string {
	if e.Err != nil {
		return e.Err.Error()
	}
	if e.Code != "" {
		return e.Code
	}
	return "operation error"
}

func (e *OperationError) Unwrap() error {
	return e.Err
}
