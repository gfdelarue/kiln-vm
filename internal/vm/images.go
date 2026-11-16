package vm

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/gfdelarue/kiln-vm/internal/apierr"
	"github.com/gfdelarue/kiln-vm/internal/image"
	"github.com/gfdelarue/kiln-vm/internal/models"
	"github.com/gfdelarue/kiln-vm/internal/refid"
)

const DefaultVMImageBuildProfile = "debian"

const vmImageBackingName = "vm-image"

func (s *Service) CreateImage(ctx context.Context, req *models.CreateVMImageRequest) (*models.VMImage, error) {
	if req == nil {
		return nil, &apierr.ValidationError{Field: "request", Message: "is required"}
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return nil, &apierr.ValidationError{Field: "name", Message: "is required"}
	}
	if err := ValidateLabels("labels", req.Labels); err != nil {
		return nil, err
	}
	fromVM := strings.TrimSpace(req.FromVM)
	hasDockerfile := strings.TrimSpace(req.Dockerfile) != ""
	switch {
	case fromVM != "" && hasDockerfile:
		return nil, &apierr.ValidationError{Field: "from_vm", Message: "cannot be combined with dockerfile; set one of from_vm or dockerfile"}
	case fromVM == "" && !hasDockerfile:
		return nil, &apierr.ValidationError{Field: "from_vm", Message: "one of from_vm or dockerfile is required"}
	case fromVM != "" && (req.Context != "" || req.Profile != "" || req.DiskSizeMB != 0):
		return nil, &apierr.ValidationError{Field: "from_vm", Message: "cannot be combined with context, profile or disk_size_mb; the image takes the VM's profile and disk"}
	}
	if err := s.checkVMImageNameAvailable(name); err != nil {
		return nil, err
	}
	if fromVM != "" {
		return s.saveVMImage(ctx, name, fromVM, req.Labels)
	}
	return s.buildVMImage(ctx, name, req)
}

func (s *Service) checkVMImageNameAvailable(name string) error {
	if _, err := s.db.GetVMImage(name); err == nil {
		return fmt.Errorf("VM image %q already exists: %w", name, apierr.ErrConflict)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("check vm image name: %w", err)
	}
	return nil
}

func (s *Service) saveVMImage(ctx context.Context, name, fromVM string, labels map[string]string) (*models.VMImage, error) {
	record, err := s.getRecord(fromVM)
	if err != nil {
		if errors.Is(err, apierr.ErrNotFound) {
			return nil, &apierr.ValidationError{Field: "from_vm", Message: fmt.Sprintf("VM %q not found", fromVM)}
		}
		return nil, err
	}
	releaseSave, err := s.reserveSave(record)
	if err != nil {
		return nil, err
	}
	defer releaseSave()

	m, err := s.machines.Get(record.MachineID)
	if err != nil {
		return nil, fmt.Errorf("get vm machine: %w", err)
	}
	if m.Backend != "firecracker" {
		return nil, &apierr.ValidationError{Field: "from_vm", Message: fmt.Sprintf("VM %q uses the %s backend; only Firecracker VMs can be saved as images", record.Name, m.Backend)}
	}
	if m.State != models.MachineStateStopped {
		return nil, fmt.Errorf("VM %q is %s; stop it before saving it as an image: %w", record.Name, m.State, apierr.ErrConflict)
	}
	rootDisk, err := s.machines.RootDiskPath(m.ID)
	if err != nil {
		return nil, fmt.Errorf("locate vm root disk: %w", err)
	}
	info, err := os.Stat(rootDisk)
	if err != nil {
		return nil, fmt.Errorf("stat vm root disk: %w", err)
	}
	const mib = 1024 * 1024
	if info.Size()%mib != 0 {
		return nil, fmt.Errorf("vm root disk size %d is not a whole number of MiB", info.Size())
	}
	diskSizeMB := info.Size() / mib

	id := uuid.New().String()
	img, err := s.images.ImportRootfs(ctx, &models.ImportImageRequest{
		Name:         vmImageBackingName,
		Tag:          id,
		RootfsPath:   rootDisk,
		RootfsSizeMB: diskSizeMB,
		BootMode:     image.BootModePersistent,
	})
	if err != nil {
		return nil, fmt.Errorf("copy vm root disk: %w", err)
	}
	return s.recordVMImage(&models.VMImage{
		ID:         id,
		Name:       name,
		Profile:    record.Profile,
		Source:     models.VMImageSourceVM,
		ImageID:    img.ID,
		DiskSizeMB: diskSizeMB,
		Labels:     normalizeLabels(labels),
		CreatedAt:  time.Now().UTC(),
	})
}

func (s *Service) buildVMImage(ctx context.Context, name string, req *models.CreateVMImageRequest) (*models.VMImage, error) {
	profileName := strings.TrimSpace(req.Profile)
	if profileName == "" {
		profileName = DefaultVMImageBuildProfile
	}
	profile, err := getProfile(profileName)
	if err != nil {
		return nil, &apierr.ValidationError{Field: "profile", Message: err.Error()}
	}
	if profile.Backend != "firecracker" || profile.GuestOS != "linux" || profile.Preparation.Kind != models.VMProfilePreparationDockerRootfs {
		return nil, &apierr.ValidationError{Field: "profile", Message: fmt.Sprintf("profile %q cannot be built from a Dockerfile; use a Firecracker Linux profile", profile.Name)}
	}
	diskSizeMB := profile.DefaultDiskSizeMB
	if req.DiskSizeMB != 0 {
		diskSizeMB = req.DiskSizeMB
	}
	if err := s.validateDiskSizeMB("disk_size_mb", diskSizeMB); err != nil {
		return nil, err
	}
	if req.Context != "" {
		if _, err := base64.StdEncoding.DecodeString(req.Context); err != nil {
			return nil, &apierr.ValidationError{Field: "context", Message: "must be a base64-encoded tar archive"}
		}
	}

	id := uuid.New().String()
	img, err := s.images.Build(ctx, &models.BuildImageRequest{
		Name:         vmImageBackingName,
		Tag:          id,
		Dockerfile:   req.Dockerfile,
		Context:      req.Context,
		RootfsSizeMB: diskSizeMB,
		BootMode:     image.BootModePersistent,
	})
	if err != nil {
		return nil, fmt.Errorf("build vm image: %w", err)
	}
	return s.recordVMImage(&models.VMImage{
		ID:         id,
		Name:       name,
		Profile:    profile.Name,
		Source:     models.VMImageSourceDockerfile,
		ImageID:    img.ID,
		DiskSizeMB: diskSizeMB,
		Labels:     normalizeLabels(req.Labels),
		Resolved:   img.BaseImages,
		CreatedAt:  time.Now().UTC(),
	})
}

func (s *Service) recordVMImage(vmImage *models.VMImage) (*models.VMImage, error) {
	if err := s.db.CreateVMImage(vmImage); err != nil {
		_ = s.images.Delete(vmImage.ImageID)
		if isConstraintError(err) {
			return nil, fmt.Errorf("VM image %q already exists: %w", vmImage.Name, apierr.ErrConflict)
		}
		return nil, fmt.Errorf("save vm image: %w", err)
	}
	return vmImage, nil
}

func (s *Service) GetImage(ref string) (*models.VMImage, error) {
	return s.resolveVMImage(ref)
}

func (s *Service) ListImages(selectors ...LabelSelector) ([]models.VMImage, error) {
	records, err := s.db.ListVMImages()
	if err != nil {
		return nil, fmt.Errorf("list vm images: %w", err)
	}
	images := make([]models.VMImage, 0, len(records))
	for _, record := range records {
		if MatchLabels(record.Labels, selectors) {
			images = append(images, record)
		}
	}
	return images, nil
}

func (s *Service) DeleteImage(ref string) error {
	vmImage, err := s.resolveVMImage(ref)
	if err != nil {
		return err
	}
	if err := s.db.DeleteVMImage(vmImage.ID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return &apierr.NotFoundError{Resource: "vm_image", ID: ref}
		}
		return fmt.Errorf("delete vm image record: %w", err)
	}
	if err := s.deleteImageIfUnused(vmImage.ImageID); err != nil {
		return fmt.Errorf("delete vm image files: %w", err)
	}
	return nil
}

func (s *Service) resolveVMImage(ref string) (*models.VMImage, error) {
	record, err := s.db.GetVMImage(ref)
	if err == nil {
		return record, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("get vm image: %w", err)
	}
	records, err := s.db.ListVMImages()
	if err != nil {
		return nil, fmt.Errorf("list vm images for ID resolution: %w", err)
	}
	candidates := make([]refid.Candidate, 0, len(records))
	for _, record := range records {
		candidates = append(candidates, refid.Candidate{ID: record.ID, Label: record.Name})
	}
	candidate, err := refid.Resolve("vm_image", ref, candidates)
	if err != nil {
		return nil, fmt.Errorf("resolve vm image ID: %w", err)
	}
	record, err = s.db.GetVMImage(candidate.ID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, &apierr.NotFoundError{Resource: "vm_image", ID: ref}
		}
		return nil, fmt.Errorf("get vm image: %w", err)
	}
	return record, nil
}

func (s *Service) reserveSave(record *models.VMRecord) (func(), error) {
	s.powerMu.Lock()
	defer s.powerMu.Unlock()
	if s.savingImage[record.ID] {
		return nil, fmt.Errorf("VM %q is already being saved as an image: %w", record.Name, apierr.ErrConflict)
	}
	if s.powerStarting[record.ID] > 0 {
		return nil, fmt.Errorf("VM %q is starting: %w", record.Name, apierr.ErrConflict)
	}
	s.savingImage[record.ID] = true
	return func() {
		s.powerMu.Lock()
		defer s.powerMu.Unlock()
		delete(s.savingImage, record.ID)
	}, nil
}

func (s *Service) reserveStart(record *models.VMRecord) (func(), error) {
	s.powerMu.Lock()
	defer s.powerMu.Unlock()
	if s.savingImage[record.ID] {
		return nil, fmt.Errorf("VM %q is being saved as an image: %w", record.Name, apierr.ErrConflict)
	}
	s.powerStarting[record.ID]++
	return func() {
		s.powerMu.Lock()
		defer s.powerMu.Unlock()
		if s.powerStarting[record.ID] <= 1 {
			delete(s.powerStarting, record.ID)
			return
		}
		s.powerStarting[record.ID]--
	}, nil
}

func (s *Service) isSavingImage(vmID string) bool {
	s.powerMu.Lock()
	defer s.powerMu.Unlock()
	return s.savingImage[vmID]
}
