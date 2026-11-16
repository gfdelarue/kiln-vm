package image

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/gfdelarue/kiln-vm/internal/apierr"
	"github.com/gfdelarue/kiln-vm/internal/guestagent"
	"github.com/gfdelarue/kiln-vm/internal/hostfs"
	"github.com/gfdelarue/kiln-vm/internal/limits"
	"github.com/gfdelarue/kiln-vm/internal/models"
	"github.com/gfdelarue/kiln-vm/internal/refid"
	"github.com/gfdelarue/kiln-vm/internal/store"
	thirdparty "github.com/gfdelarue/kiln-vm/third_party"
)

const (
	MinRootfsSizeMB       int64 = limits.MinVMRootDiskMB
	MaxRootfsSizeMB       int64 = limits.AbsoluteMaxVMRootDiskMB
	DefaultBuildTimeout         = 30 * time.Minute
	BootModePersistent          = "persistent"
	BootModeBootableQCOW2       = "bootable-qcow2"
)

// Service manages image lifecycle.
type Service struct {
	db           *store.DB
	builder      *Builder
	dataDir      string
	buildTimeout time.Duration
}

func NewService(db *store.DB, dataDir, agentBinPath string) *Service {
	return &Service{
		db:           db,
		builder:      NewBuilder(dataDir, agentBinPath),
		dataDir:      dataDir,
		buildTimeout: DefaultBuildTimeout,
	}
}

func (s *Service) SetBuildTimeout(timeout time.Duration) {
	s.buildTimeout = timeout
}

func (s *Service) SetBuildDir(buildDir string) {
	s.builder.SetBuildDir(buildDir)
}

func (s *Service) SetMinBuildFreeMB(minFreeMB int64) {
	s.builder.SetMinFreeMB(minFreeMB)
}

func (s *Service) CleanupStaging() {
	s.builder.CleanupStaging()
}

func (s *Service) RuntimeFingerprint() string {
	return RuntimeFingerprint()
}

// Bump when Linux profile images must be rebuilt although the guest-agent
// wire protocol is unchanged.
const profileRuntimeCompatibilityVersion = 8

func RuntimeFingerprint() string {
	return runtimeCompatibilityFingerprint(
		profileRuntimeCompatibilityVersion,
		guestagent.ProtocolVersion,
		guestagent.ExecDuplexProtocolVersion,
		thirdparty.GoXNetLicense(),
	)
}

func runtimeCompatibilityFingerprint(
	profileRuntimeCompatibilityVersion,
	guestAgentProtocolVersion,
	execDuplexProtocolVersion int,
	goXNetLicense []byte,
) string {
	h := sha256.New()
	for _, part := range []string{
		"runtime-compatibility-v1",
		fmt.Sprintf("profile-runtime-compatibility:%d", profileRuntimeCompatibilityVersion),
		fmt.Sprintf("guest-agent-protocol:%d", guestAgentProtocolVersion),
		fmt.Sprintf("exec-duplex-protocol:%d", execDuplexProtocolVersion),
	} {
		_, _ = h.Write([]byte(part))
		_, _ = h.Write([]byte{0})
	}
	_, _ = h.Write([]byte("notice:golang.org/x/net/LICENSE"))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write(goXNetLicense)
	_, _ = h.Write([]byte{0})
	return hex.EncodeToString(h.Sum(nil))
}

func (s *Service) Build(ctx context.Context, req *models.BuildImageRequest) (*models.Image, error) {
	id := uuid.New().String()
	imageDir := filepath.Join(s.dataDir, "images", id)
	tag := req.Tag
	if tag == "" {
		tag = "latest"
	}
	bootMode, err := normalizeBootMode(req.BootMode)
	if err != nil {
		return nil, err
	}
	if IsBootableQCOW2BootMode(bootMode) {
		return nil, &apierr.ValidationError{
			Field:   "boot_mode",
			Message: fmt.Sprintf("%q images cannot be built from a Dockerfile; import or prepare a bootable qcow2 disk instead", bootMode),
		}
	}
	rootfsSizeMB, err := normalizeRootfsSizeMB(req.RootfsSizeMB)
	if err != nil {
		return nil, err
	}

	if s.buildTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.buildTimeout)
		defer cancel()
	}

	rootfsPath, err := s.builder.Build(ctx, id, req.Name, tag, req.Dockerfile, req.Context, rootfsSizeMB)
	if err != nil {
		os.RemoveAll(imageDir)
		return nil, fmt.Errorf("build image: %w", err)
	}

	// Get rootfs size
	info, err := os.Stat(rootfsPath)
	if err != nil {
		return nil, fmt.Errorf("stat rootfs: %w", err)
	}

	img := &models.Image{
		ID:         id,
		Name:       req.Name,
		Tag:        tag,
		BootMode:   bootMode,
		SizeMB:     info.Size() / (1024 * 1024),
		RootfsPath: rootfsPath,
		CreatedAt:  time.Now().UTC(),
	}

	if err := s.db.CreateImage(img); err != nil {
		os.RemoveAll(imageDir)
		return nil, fmt.Errorf("save image: %w", err)
	}

	return img, nil
}

func (s *Service) ImportRootfs(ctx context.Context, req *models.ImportImageRequest) (*models.Image, error) {
	if req == nil {
		return nil, fmt.Errorf("import image request is required")
	}
	if strings.TrimSpace(req.RootfsPath) == "" {
		return nil, fmt.Errorf("rootfs_path is required")
	}
	id := uuid.New().String()
	imageDir := filepath.Join(s.dataDir, "images", id)
	if err := os.MkdirAll(filepath.Dir(imageDir), 0750); err != nil {
		return nil, fmt.Errorf("create image parent dir: %w", err)
	}
	tag := req.Tag
	if tag == "" {
		tag = "latest"
	}
	bootMode, err := normalizeBootMode(req.BootMode)
	if err != nil {
		return nil, err
	}
	rootfsSizeMB, err := normalizeRootfsSizeMB(req.RootfsSizeMB)
	if err != nil {
		return nil, err
	}

	buildParent := filepath.Join(s.builder.buildDir, "images", "staging")
	if err := os.MkdirAll(buildParent, 0750); err != nil {
		return nil, fmt.Errorf("create image staging parent: %w", err)
	}
	stagingDir, err := os.MkdirTemp(buildParent, id+"-import-")
	if err != nil {
		return nil, fmt.Errorf("create image import staging dir: %w", err)
	}
	defer os.RemoveAll(stagingDir)
	stagedRootfsPath := filepath.Join(stagingDir, imageDiskFilename(bootMode))
	if err := stageImportedRootfs(ctx, req.RootfsPath, stagedRootfsPath, bootMode); err != nil {
		return nil, err
	}
	info, err := os.Stat(stagedRootfsPath)
	if err != nil {
		return nil, fmt.Errorf("stat imported rootfs: %w", err)
	}
	expectedSize := rootfsSizeMB * 1024 * 1024
	if !IsBootableQCOW2BootMode(bootMode) && info.Size() != expectedSize {
		return nil, fmt.Errorf("imported rootfs size = %d, want %d", info.Size(), expectedSize)
	}
	if err := promoteStagingDir(stagingDir, imageDir); err != nil {
		return nil, err
	}
	rootfsPath := filepath.Join(imageDir, imageDiskFilename(bootMode))

	img := &models.Image{
		ID:         id,
		Name:       req.Name,
		Tag:        tag,
		BootMode:   bootMode,
		SizeMB:     rootfsSizeMB,
		RootfsPath: rootfsPath,
		CreatedAt:  time.Now().UTC(),
	}
	if err := s.db.CreateImage(img); err != nil {
		os.RemoveAll(imageDir)
		return nil, fmt.Errorf("save imported image: %w", err)
	}
	return img, nil
}

func stageImportedRootfs(ctx context.Context, sourcePath, targetPath, bootMode string) error {
	if IsBootableQCOW2BootMode(bootMode) {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("stage imported bootable qcow2 canceled: %w", err)
		}
		if err := hostfs.CopyFilePreserveSparse(ctx, sourcePath, targetPath, 0640); err != nil {
			return fmt.Errorf("copy imported bootable qcow2: %w", err)
		}
		return nil
	}
	if err := hostfs.CopyFilePreserveSparse(ctx, sourcePath, targetPath, 0640); err != nil {
		return fmt.Errorf("copy imported rootfs: %w", err)
	}
	return nil
}

func normalizeBootMode(bootMode string) (string, error) {
	switch bootMode {
	case "":
		return BootModePersistent, nil
	case BootModePersistent, BootModeBootableQCOW2:
		return bootMode, nil
	default:
		return "", fmt.Errorf("boot_mode must be %q or %q", BootModePersistent, BootModeBootableQCOW2)
	}
}

func imageDiskFilename(bootMode string) string {
	if IsBootableQCOW2BootMode(bootMode) {
		return "disk.qcow2"
	}
	return "rootfs.ext4"
}

func IsBootableQCOW2BootMode(bootMode string) bool {
	return bootMode == BootModeBootableQCOW2
}

func normalizeRootfsSizeMB(sizeMB int64) (int64, error) {
	if sizeMB == 0 {
		return DefaultRootfsSizeMB, nil
	}
	if sizeMB < MinRootfsSizeMB || sizeMB > MaxRootfsSizeMB {
		return 0, fmt.Errorf("rootfs_size_mb must be between %d and %d", MinRootfsSizeMB, MaxRootfsSizeMB)
	}
	return sizeMB, nil
}

func (s *Service) Get(id string) (*models.Image, error) {
	img, err := s.resolveImage(id)
	if err != nil {
		return nil, err
	}
	return img, nil
}

func (s *Service) List() ([]models.Image, error) {
	images, err := s.db.ListImages()
	if err != nil {
		return nil, fmt.Errorf("list images: %w", err)
	}
	return images, nil
}

func (s *Service) Delete(id string) error {
	img, err := s.resolveImage(id)
	if err != nil {
		return err
	}

	// Remove image files
	imageDir := filepath.Join(s.dataDir, "images", img.ID)
	os.RemoveAll(imageDir)

	if err := s.db.DeleteImage(img.ID); err != nil {
		return fmt.Errorf("delete image: %w", err)
	}
	return nil
}

func (s *Service) resolveImage(ref string) (*models.Image, error) {
	img, err := s.db.GetImage(ref)
	if err == nil {
		return img, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("get image: %w", err)
	}

	images, err := s.db.ListImages()
	if err != nil {
		return nil, fmt.Errorf("list images for ID resolution: %w", err)
	}
	candidates := make([]refid.Candidate, 0, len(images))
	for _, img := range images {
		candidates = append(candidates, refid.Candidate{ID: img.ID, Label: img.Name + ":" + img.Tag})
	}
	candidate, err := refid.Resolve("image", ref, candidates)
	if err != nil {
		return nil, fmt.Errorf("resolve image ID: %w", err)
	}
	img, err = s.db.GetImage(candidate.ID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, &apierr.NotFoundError{Resource: "image", ID: ref}
		}
		return nil, fmt.Errorf("get image: %w", err)
	}
	return img, nil
}
