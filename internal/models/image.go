package models

import "time"

type Image struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Tag        string    `json:"tag"`
	BootMode   string    `json:"boot_mode"`
	SizeMB     int64     `json:"size_mb"`
	RootfsPath string    `json:"-"`
	CreatedAt  time.Time `json:"created_at"`
	// Set only on the result of a Dockerfile build; never stored.
	BaseImages map[string]string `json:"-"`
}

type BuildImageRequest struct {
	Name         string `json:"name"`
	Tag          string `json:"tag"`
	Dockerfile   string `json:"dockerfile"`
	Context      string `json:"context"` // base64-encoded tar
	RootfsSizeMB int64  `json:"rootfs_size_mb,omitempty"`
	BootMode     string `json:"boot_mode,omitempty"`
}

type ImportImageRequest struct {
	Name         string `json:"name"`
	Tag          string `json:"tag"`
	RootfsPath   string `json:"rootfs_path"`
	RootfsSizeMB int64  `json:"rootfs_size_mb,omitempty"`
	BootMode     string `json:"boot_mode,omitempty"`
}
