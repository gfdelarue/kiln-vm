package vm

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/gfdelarue/kiln-vm/internal/apierr"
	"github.com/gfdelarue/kiln-vm/internal/models"
)

type resolvedVMCreatePlan struct {
	plan    *models.VMCreatePlan
	profile Profile
}

func (s *Service) PlanCreate(req *models.CreateVMRequest) (*models.VMCreatePlan, error) {
	resolved, err := s.resolveCreatePlan(req)
	if err != nil {
		return nil, err
	}
	return resolved.plan, nil
}

func (s *Service) resolveCreatePlan(req *models.CreateVMRequest) (*resolvedVMCreatePlan, error) {
	if req == nil {
		return nil, &apierr.ValidationError{Field: "request", Message: "is required"}
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return nil, &apierr.ValidationError{Field: "name", Message: "is required"}
	}
	if _, err := s.db.GetVM(name); err == nil {
		return nil, fmt.Errorf("VM %q already exists: %w", name, apierr.ErrConflict)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("check vm name: %w", err)
	}

	profile, err := getProfile(strings.TrimSpace(req.Profile))
	if err != nil {
		return nil, &apierr.ValidationError{Field: "profile", Message: err.Error()}
	}

	diskSizeMB := profile.DefaultDiskSizeMB
	if req.DiskSizeMB != 0 {
		diskSizeMB = req.DiskSizeMB
	}
	if err := s.validateDiskSizeMB("disk_size_mb", diskSizeMB); err != nil {
		return nil, err
	}

	cpu := models.CPUConfig{Kind: "shared", VCPUs: profile.DefaultVCPUs}
	if req.VCPUs != 0 {
		cpu.VCPUs = req.VCPUs
	}
	if req.CPU != nil {
		cpu = *req.CPU
	}
	if cpu.Kind == "" {
		cpu.Kind = "shared"
	}
	if cpu.VCPUs == 0 {
		if cpu.Kind == "pooled" && cpu.Millis > 0 {
			cpu.VCPUs = (cpu.Millis + 999) / 1000
		} else {
			cpu.VCPUs = profile.DefaultVCPUs
		}
	}

	memoryMB := profile.DefaultMemoryMB
	if req.MemoryMB != 0 {
		memoryMB = req.MemoryMB
	}

	powerPolicy, err := NormalizePowerPolicy(req.Power)
	if err != nil {
		return nil, err
	}
	if err := models.ValidateNetworkPolicy(req.NetworkPolicy, profile.Backend); err != nil {
		return nil, &apierr.ValidationError{Field: "network_policy", Message: err.Error(), Cause: err}
	}

	plan := &models.VMCreatePlan{
		Name:                name,
		Profile:             profile.Name,
		NetworkPolicy:       req.NetworkPolicy,
		CPU:                 cpu,
		MemoryMB:            memoryMB,
		DiskSizeMB:          diskSizeMB,
		Power:               powerPolicy,
		ProfileCapabilities: append([]string(nil), profile.Capabilities...),
	}
	return &resolvedVMCreatePlan{plan: plan, profile: profile}, nil
}
