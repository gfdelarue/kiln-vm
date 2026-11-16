package models

import "time"

type VMImageSource string

const (
	VMImageSourceVM         VMImageSource = "vm"
	VMImageSourceDockerfile VMImageSource = "dockerfile"
)

type VMImage struct {
	ID         string            `json:"id"`
	Name       string            `json:"name"`
	Profile    string            `json:"profile"`
	Source     VMImageSource     `json:"source"`
	ImageID    string            `json:"-"`
	DiskSizeMB int64             `json:"disk_size_mb"`
	Labels     map[string]string `json:"labels,omitempty"`
	Resolved   map[string]string `json:"resolved,omitempty"`
	CreatedAt  time.Time         `json:"created_at"`
}

type CreateVMImageRequest struct {
	Name       string            `json:"name"`
	FromVM     string            `json:"from_vm,omitempty"`
	Dockerfile string            `json:"dockerfile,omitempty"`
	Context    string            `json:"context,omitempty"`
	Profile    string            `json:"profile,omitempty"`
	DiskSizeMB int64             `json:"disk_size_mb,omitempty"`
	Labels     map[string]string `json:"labels,omitempty"`
}
