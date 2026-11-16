package libvirtdomain

import (
	"strings"

	"github.com/gfdelarue/kiln-vm/internal/apierr"
	"github.com/gfdelarue/kiln-vm/internal/models"
)

func Windows11() *models.LibvirtDomainSpec {
	return &models.LibvirtDomainSpec{
		OSInfo:     "win11",
		Firmware:   "uefi",
		SecureBoot: true,
		SMM:        true,
		TPM: &models.LibvirtTPMSpec{
			BackendType:    "emulator",
			BackendVersion: "2.0",
			Model:          "tpm-crb",
		},
		Disk: models.LibvirtDiskSpec{
			Bus:    "virtio",
			Format: "qcow2",
		},
		Network: models.LibvirtNetworkSpec{
			Model: "virtio",
		},
		Graphics: models.LibvirtGraphicsSpec{
			Type:   "spice",
			Listen: "127.0.0.1",
		},
	}
}

func Clone(spec *models.LibvirtDomainSpec) *models.LibvirtDomainSpec {
	if spec == nil {
		return nil
	}
	clone := *spec
	if spec.TPM != nil {
		tpm := *spec.TPM
		clone.TPM = &tpm
	}
	return &clone
}

func Normalize(spec *models.LibvirtDomainSpec) (*models.LibvirtDomainSpec, error) {
	if spec == nil {
		spec = Windows11()
	} else {
		spec = Clone(spec)
	}
	normalize(spec)
	if err := Validate(spec); err != nil {
		return nil, err
	}
	return spec, nil
}

func normalize(spec *models.LibvirtDomainSpec) {
	spec.OSInfo = strings.TrimSpace(spec.OSInfo)
	spec.Firmware = strings.ToLower(strings.TrimSpace(spec.Firmware))
	spec.Disk.Bus = strings.ToLower(strings.TrimSpace(spec.Disk.Bus))
	spec.Disk.Format = strings.ToLower(strings.TrimSpace(spec.Disk.Format))
	spec.Disk.Cache = strings.ToLower(strings.TrimSpace(spec.Disk.Cache))
	spec.Network.Source = strings.TrimSpace(spec.Network.Source)
	spec.Network.Model = strings.TrimSpace(spec.Network.Model)
	spec.Graphics.Type = strings.ToLower(strings.TrimSpace(spec.Graphics.Type))
	spec.Graphics.Listen = strings.TrimSpace(spec.Graphics.Listen)
	if spec.TPM != nil {
		spec.TPM.BackendType = strings.TrimSpace(spec.TPM.BackendType)
		spec.TPM.BackendVersion = strings.TrimSpace(spec.TPM.BackendVersion)
		spec.TPM.Model = strings.TrimSpace(spec.TPM.Model)
	}
}

func Validate(spec *models.LibvirtDomainSpec) error {
	if spec == nil {
		return &apierr.ValidationError{Field: "libvirt", Message: "is required"}
	}
	if spec.OSInfo == "" {
		return &apierr.ValidationError{Field: "libvirt.osinfo", Message: "is required"}
	}
	switch spec.Firmware {
	case "", "bios", "uefi":
	default:
		return &apierr.ValidationError{Field: "libvirt.firmware", Message: `must be "bios" or "uefi"`}
	}
	if spec.SecureBoot && spec.Firmware != "uefi" {
		return &apierr.ValidationError{Field: "libvirt.secure_boot", Message: "requires uefi firmware"}
	}
	switch spec.Disk.Bus {
	case "sata", "virtio", "scsi":
	default:
		return &apierr.ValidationError{Field: "libvirt.disk.bus", Message: `must be "sata", "virtio", or "scsi"`}
	}
	if spec.Disk.Format == "" {
		return &apierr.ValidationError{Field: "libvirt.disk.format", Message: "is required"}
	}
	if spec.Disk.Format != "qcow2" {
		return &apierr.ValidationError{Field: "libvirt.disk.format", Message: `must be "qcow2"`}
	}
	if spec.Network.Model == "" {
		return &apierr.ValidationError{Field: "libvirt.network.model", Message: "is required"}
	}
	if strings.ContainsAny(spec.Network.Source, " \t\r\n,=") {
		return &apierr.ValidationError{Field: "libvirt.network.source", Message: "must not contain whitespace, commas, or equals signs"}
	}
	switch spec.Graphics.Type {
	case "none", "spice", "vnc":
	default:
		return &apierr.ValidationError{Field: "libvirt.graphics.type", Message: `must be "none", "spice", or "vnc"`}
	}
	if spec.Graphics.Type == "none" && spec.Graphics.Listen != "" {
		return &apierr.ValidationError{Field: "libvirt.graphics.listen", Message: `cannot be set when graphics type is "none"`}
	}
	if spec.TPM != nil {
		if spec.TPM.BackendType == "" {
			return &apierr.ValidationError{Field: "libvirt.tpm.backend_type", Message: "is required"}
		}
		if spec.TPM.Model == "" {
			return &apierr.ValidationError{Field: "libvirt.tpm.model", Message: "is required"}
		}
	}
	return nil
}
