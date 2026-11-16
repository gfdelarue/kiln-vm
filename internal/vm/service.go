package vm

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"

	"github.com/gfdelarue/kiln-vm/internal/apierr"
	"github.com/gfdelarue/kiln-vm/internal/execution"
	"github.com/gfdelarue/kiln-vm/internal/guestagent"
	"github.com/gfdelarue/kiln-vm/internal/image"
	"github.com/gfdelarue/kiln-vm/internal/libvirtdomain"
	"github.com/gfdelarue/kiln-vm/internal/limits"
	"github.com/gfdelarue/kiln-vm/internal/machine"
	"github.com/gfdelarue/kiln-vm/internal/models"
	"github.com/gfdelarue/kiln-vm/internal/refid"
	"github.com/gfdelarue/kiln-vm/internal/windowsprofile"
)

type Store interface {
	CreateVM(vm *models.VMRecord) error
	GetVM(ref string) (*models.VMRecord, error)
	GetVMByMachineID(machineID string) (*models.VMRecord, error)
	ListVMs() ([]models.VMRecord, error)
	ListPowerStartVMs() ([]models.VMRecord, error)
	UpdateVMPowerPolicy(id string, policy models.VMPowerPolicy) error
	RecordVMPowerActivity(id string, activityAt time.Time) error
	RecordVMPowerStarted(id string, startedAt time.Time) error
	RecordVMPowerStopped(id string, stoppedAt time.Time, reason string) error
	DeleteVM(id string) error
	ListVMCapabilityConditions(vmID string) ([]models.VMCapabilityCondition, error)
	UpsertVMCapabilityCondition(vmID string, condition models.VMCapabilityCondition) error
	CountVMsByImageID(imageID string) (int, error)
	CreateVMProfileImage(img *models.VMProfileImage) error
	GetVMProfileImage(id string) (*models.VMProfileImage, error)
	GetVMProfileImageByImageID(imageID string) (*models.VMProfileImage, error)
	FindVMProfileImage(profile, fingerprint string, rootfsSizeMB int64) (*models.VMProfileImage, error)
	ListVMProfileImages() ([]models.VMProfileImage, error)
	DeleteVMProfileImage(id string) error
	DeleteVMProfileImagesByProfile(profile string) ([]models.VMProfileImage, error)
	CreateVMImage(img *models.VMImage) error
	GetVMImage(ref string) (*models.VMImage, error)
	GetVMImageByImageID(imageID string) (*models.VMImage, error)
	ListVMImages() ([]models.VMImage, error)
	DeleteVMImage(id string) error
}

type ImageService interface {
	Build(ctx context.Context, req *models.BuildImageRequest) (*models.Image, error)
	ImportRootfs(ctx context.Context, req *models.ImportImageRequest) (*models.Image, error)
	Get(id string) (*models.Image, error)
	Delete(id string) error
	RuntimeFingerprint() string
}

type Service struct {
	db                      Store
	images                  ImageService
	machines                machine.MachineService
	maxDiskSizeMB           int64
	profileImageMu          sync.Mutex
	powerMu                 sync.Mutex
	powerLeases             map[string]int
	powerStopping           map[string]bool
	powerStarting           map[string]int
	savingImage             map[string]bool
	profileImages           ProfileImageSource
	catalog                 *ProfileImageCatalog
	ociHTTPClient           *http.Client
	gatewayDNSReadyTimeout  time.Duration
	gatewayDNSReadyInterval time.Duration
	pullTimeout             time.Duration
	pullStallTimeout        time.Duration
	pullHeaderTimeout       time.Duration
	windowsProfileDir       string
	libvirtConnectURI       string
	libvirtNetwork          string
	libvirtNetworkCIDR      string
}

var (
	ErrVMStoppedBeforeReady = errors.New("vm stopped before ready")
	ErrVMErrorBeforeReady   = errors.New("vm entered error state before ready")
	ErrVMActiveUse          = errors.New("vm has active use leases")
)

const defaultProfileImagePullHeaderTimeout = 30 * time.Second
const DefaultMaxDiskSizeMB int64 = limits.DefaultMaxVMRootDiskMB

type WaitReadyStateError struct {
	VMName     string
	MachineID  string
	State      models.MachineState
	MachineErr string
	LastErr    error
	Probe      string
}

func (e *WaitReadyStateError) Error() string {
	probe := e.Probe
	if probe == "" {
		probe = "vm guest agent"
	}
	base := fmt.Sprintf("wait for %s: VM %s entered %s before ready", probe, e.VMName, e.State)
	if e.MachineErr != "" {
		base += ": " + e.MachineErr
	}
	if e.LastErr != nil {
		base += fmt.Sprintf("; last error: %v", e.LastErr)
	}
	return base
}

func (e *WaitReadyStateError) Unwrap() []error {
	if e.State == models.MachineStateStopped {
		return []error{ErrVMStoppedBeforeReady, apierr.ErrConflict}
	}
	if e.State == models.MachineStateError {
		return []error{ErrVMErrorBeforeReady, apierr.ErrConflict}
	}
	return []error{apierr.ErrConflict}
}

type ActiveUseError struct {
	VMName string
}

func (e *ActiveUseError) Error() string {
	return fmt.Sprintf("VM %q has active use leases and cannot be stopped", e.VMName)
}

func (e *ActiveUseError) Unwrap() []error {
	return []error{ErrVMActiveUse, apierr.ErrConflict}
}

func NewService(db Store, images ImageService, machines machine.MachineService) *Service {
	return &Service{
		db:                      db,
		images:                  images,
		machines:                machines,
		maxDiskSizeMB:           DefaultMaxDiskSizeMB,
		powerLeases:             map[string]int{},
		powerStopping:           map[string]bool{},
		powerStarting:           map[string]int{},
		savingImage:             map[string]bool{},
		profileImages:           ProfileImageSource{Mode: ProfileImageModeLocal},
		pullTimeout:             image.DefaultOCIPullTimeout,
		pullStallTimeout:        image.DefaultOCIPullStallTimeout,
		pullHeaderTimeout:       defaultProfileImagePullHeaderTimeout,
		gatewayDNSReadyTimeout:  defaultGatewayDNSReadinessTimeout,
		gatewayDNSReadyInterval: defaultGatewayDNSReadinessInterval,
	}
}

func (s *Service) SetMaxDiskSizeMB(maxDiskSizeMB int64) {
	if maxDiskSizeMB <= 0 {
		maxDiskSizeMB = DefaultMaxDiskSizeMB
	}
	if maxDiskSizeMB < image.MinRootfsSizeMB {
		maxDiskSizeMB = image.MinRootfsSizeMB
	}
	if maxDiskSizeMB > image.MaxRootfsSizeMB {
		maxDiskSizeMB = image.MaxRootfsSizeMB
	}
	s.maxDiskSizeMB = maxDiskSizeMB
}

func (s *Service) SetProfileInfrastructureConfig(profileDir, connectURI, network, networkCIDR string) {
	s.windowsProfileDir = strings.TrimSpace(profileDir)
	s.libvirtConnectURI = strings.TrimSpace(connectURI)
	s.libvirtNetwork = strings.TrimSpace(network)
	s.libvirtNetworkCIDR = strings.TrimSpace(networkCIDR)
}

func (s *Service) effectiveMaxDiskSizeMB() int64 {
	if s.maxDiskSizeMB > 0 {
		return s.maxDiskSizeMB
	}
	return DefaultMaxDiskSizeMB
}

func (s *Service) validateDiskSizeMB(field string, diskSizeMB int64) error {
	maxDiskSizeMB := s.effectiveMaxDiskSizeMB()
	if diskSizeMB < image.MinRootfsSizeMB || diskSizeMB > maxDiskSizeMB {
		return &apierr.ValidationError{
			Field:   field,
			Message: fmt.Sprintf("must be between %d and %d", image.MinRootfsSizeMB, maxDiskSizeMB),
		}
	}
	return nil
}

func (s *Service) SetProfileImageSource(source ProfileImageSource, catalog *ProfileImageCatalog) error {
	mode, err := NormalizeProfileImageMode(source.Mode)
	if err != nil {
		return err
	}
	source.Mode = mode
	s.profileImages = source
	s.catalog = catalog
	return nil
}

func (s *Service) SetProfileImagePullTimeout(timeout time.Duration) {
	if timeout > 0 {
		s.pullTimeout = timeout
	}
}

func (s *Service) SetProfileImagePullStallTimeout(timeout time.Duration) {
	if timeout > 0 {
		s.pullStallTimeout = timeout
	}
}

func (s *Service) SetProfileImagePullHeaderTimeout(timeout time.Duration) {
	if timeout > 0 {
		s.pullHeaderTimeout = timeout
	}
}

func (s *Service) SetProfileImageHTTPClient(client *http.Client) {
	s.ociHTTPClient = client
}

func NormalizePowerPolicy(policy *models.VMPowerPolicy) (models.VMPowerPolicy, error) {
	if policy == nil {
		return defaultPowerPolicy(), nil
	}
	normalized := *policy
	if normalized.Mode == "" {
		normalized.Mode = models.VMPowerModeManual
	}
	switch normalized.Mode {
	case models.VMPowerModeManual, models.VMPowerModeAlways:
		normalized.IdleTimeoutSeconds = 0
	case models.VMPowerModeOnDemand, models.VMPowerModeWarm:
		if normalized.IdleTimeoutSeconds < 0 {
			return normalized, &apierr.ValidationError{Field: "power.idle_timeout_seconds", Message: "must be positive"}
		}
		if normalized.IdleTimeoutSeconds == 0 {
			normalized.IdleTimeoutSeconds = 10 * 60
		}
	default:
		return normalized, &apierr.ValidationError{Field: "power.mode", Message: "must be one of: manual, always, on_demand, warm"}
	}
	return normalized, nil
}

func defaultPowerPolicy() models.VMPowerPolicy {
	return models.VMPowerPolicy{Mode: models.VMPowerModeManual}
}

func powerPolicyCanIdleStop(policy models.VMPowerPolicy) bool {
	return (policy.Mode == models.VMPowerModeOnDemand || policy.Mode == models.VMPowerModeWarm) && policy.IdleTimeoutSeconds > 0
}

func (s *Service) Profiles() []models.VMProfile {
	profiles := ListProfiles()
	for i := range profiles {
		profiles[i] = s.applyProfileInfrastructureConfig(Profile{VMProfile: profiles[i]}).VMProfile
	}
	records, err := s.db.ListVMProfileImages()
	if err != nil {
		slog.Warn("list VM profile images while composing profile list failed", "error", err)
		return profiles
	}
	prepared := map[string]bool{}
	for _, record := range records {
		cached := record
		if err := s.validateCachedImage(&cached); err != nil {
			continue
		}
		prepared[record.Profile] = true
	}
	for i := range profiles {
		if prepared[profiles[i].Name] {
			profiles[i].Prepared = true
		}
	}
	return profiles
}

func (s *Service) ListProfileImages() ([]models.VMProfileImage, error) {
	images, err := s.db.ListVMProfileImages()
	if err != nil {
		return nil, fmt.Errorf("list vm profile images: %w", err)
	}
	return images, nil
}

func (s *Service) EnsureProfileImage(ctx context.Context, req *models.EnsureVMProfileImageRequest) (*models.VMProfileImage, error) {
	if req == nil {
		req = &models.EnsureVMProfileImageRequest{}
	}
	profile, diskSizeMB, err := s.normalizeProfileImageRequest(req)
	if err != nil {
		return nil, err
	}

	fingerprint := profileFingerprint(profile, s.images.RuntimeFingerprint())
	s.profileImageMu.Lock()
	defer s.profileImageMu.Unlock()

	var replace *models.VMProfileImage
	if !req.Refresh {
		cached, err := s.db.FindVMProfileImage(profile.Name, fingerprint, diskSizeMB)
		if err == nil {
			if err := s.validateCachedImage(cached); err == nil {
				return cached, nil
			}
			slog.Warn("ignoring stale VM profile image cache entry", "profile", cached.Profile, "image", cached.ImageID, "cache", cached.ID)
			replace = cached
		} else if !errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("find vm profile image: %w", err)
		}
	}
	if profileRequiresPreparedImage(profile) {
		if !req.Refresh {
			cached, err := s.findReusablePreparedProfileImage(profile, diskSizeMB)
			if err == nil {
				return s.migratePreparedProfileImageFingerprint(cached, fingerprint)
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return nil, fmt.Errorf("find reusable prepared profile image: %w", err)
			}
		}
		return nil, &apierr.ValidationError{
			Field:   "profile",
			Message: fmt.Sprintf("profile %q requires preparation with `%s`", profile.Name, prepareCommandHint(profile)),
		}
	}
	if req.Refresh {
		cached, err := s.db.FindVMProfileImage(profile.Name, fingerprint, diskSizeMB)
		if err == nil {
			replace = cached
		} else if !errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("find existing vm profile image for refresh: %w", err)
		}
	}

	img, err := s.installProfileImage(ctx, profile, fingerprint, diskSizeMB)
	if err != nil {
		return nil, err
	}

	record := &models.VMProfileImage{
		ID:           uuid.New().String(),
		Profile:      profile.Name,
		ImageID:      img.ID,
		Fingerprint:  fingerprint,
		RootfsSizeMB: diskSizeMB,
		CreatedAt:    time.Now().UTC(),
	}
	if replace != nil {
		if err := s.db.DeleteVMProfileImage(replace.ID); err != nil && !errors.Is(err, sql.ErrNoRows) {
			_ = s.images.Delete(img.ID)
			return nil, fmt.Errorf("delete old vm profile image record before replacement: %w", err)
		}
	}
	if err := s.db.CreateVMProfileImage(record); err != nil {
		_ = s.images.Delete(img.ID)
		if replace != nil {
			_ = s.db.CreateVMProfileImage(replace)
		}
		if isConstraintError(err) && !req.Refresh {
			cached, findErr := s.db.FindVMProfileImage(profile.Name, fingerprint, diskSizeMB)
			if findErr == nil {
				return cached, nil
			}
		}
		return nil, fmt.Errorf("save vm profile image: %w", err)
	}

	if replace != nil {
		if err := s.deleteImageIfUnused(replace.ImageID); err != nil {
			slog.Warn("failed to delete replaced vm profile image backing image", "image", replace.ImageID, "error", err)
		}
	}
	if req.Refresh {
		s.deleteSupersededProfileImages(record)
	}
	return record, nil
}

func (s *Service) PrepareProfile(ctx context.Context, profileName string, req *models.PrepareVMProfileRequest) (*models.VMProfileImage, error) {
	if req == nil {
		req = &models.PrepareVMProfileRequest{}
	}
	profile, err := getProfile(strings.TrimSpace(profileName))
	if err != nil {
		return nil, &apierr.ValidationError{Field: "profile", Message: err.Error()}
	}
	prep, err := resolvePrepareStrategy(profile, req)
	if err != nil {
		return nil, err
	}
	switch prep.Kind {
	case models.VMProfilePreparationDockerRootfs:
		if err := validateNoPrepareInputs(req, prep.Kind); err != nil {
			return nil, err
		}
		return s.EnsureProfileImage(ctx, &models.EnsureVMProfileImageRequest{
			Profile: profile.Name,
			Refresh: req.Refresh,
		})
	case models.VMProfilePreparationWindowsUnattended:
		return s.prepareWindowsProfile(ctx, profile, req)
	default:
		return nil, &apierr.ValidationError{
			Field:   "strategy",
			Message: fmt.Sprintf("preparation strategy %q is not implemented", prep.Kind),
		}
	}
}

func (s *Service) prepareWindowsProfile(ctx context.Context, profile Profile, req *models.PrepareVMProfileRequest) (*models.VMProfileImage, error) {
	mediaPath := strings.TrimSpace(req.MediaPath)
	if mediaPath == "" {
		return nil, &apierr.ValidationError{Field: "media_path", Message: "is required for windows-unattended profile preparation"}
	}
	diskSizeMB := profile.DefaultDiskSizeMB
	if err := s.validateDiskSizeMB("disk_size_mb", diskSizeMB); err != nil {
		return nil, err
	}
	fingerprint := profileFingerprint(profile, s.images.RuntimeFingerprint())
	s.profileImageMu.Lock()
	defer s.profileImageMu.Unlock()

	var replace *models.VMProfileImage
	if !req.Refresh {
		cached, err := s.db.FindVMProfileImage(profile.Name, fingerprint, diskSizeMB)
		if err == nil {
			if err := s.validateCachedImage(cached); err == nil {
				return cached, nil
			}
			replace = cached
		} else if !errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("find prepared windows profile image: %w", err)
		}
	} else {
		cached, err := s.db.FindVMProfileImage(profile.Name, fingerprint, diskSizeMB)
		if err == nil {
			replace = cached
		} else if !errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("find existing windows profile image for refresh: %w", err)
		}
	}

	timeout := time.Duration(req.WaitTimeoutSeconds) * time.Second
	workDir := s.windowsProfileDir
	if workDir == "" {
		workDir = filepath.Join(os.TempDir(), "kiln-vm-windows-profiles")
	}
	prepared, err := windowsprofile.Prepare(ctx, windowsprofile.PrepareRequest{
		ProfileName:     profile.Name,
		MediaPath:       mediaPath,
		VirtIOMediaPath: strings.TrimSpace(req.VirtIOMediaPath),
		ProductKey:      req.ProductKey,
		ProductKeyFile:  req.ProductKeyFile,
		ActivationMode:  req.ActivationMode,
		Refresh:         req.Refresh,
		DiskSizeMB:      diskSizeMB,
		MemoryMB:        profile.DefaultMemoryMB,
		VCPUs:           profile.DefaultVCPUs,
		WorkDir:         workDir,
		ConnectURI:      s.libvirtConnectURI,
		Network:         s.libvirtNetwork,
		NetworkCIDR:     s.libvirtNetworkCIDR,
		WaitTimeout:     timeout,
	})
	if err != nil {
		return nil, &apierr.OperationError{
			Code:      "windows_profile_prepare_failed",
			Phase:     "profile_prepare",
			Resource:  profile.Name,
			Action:    "prepare",
			Retryable: true,
			Err:       fmt.Errorf("prepare windows VM profile %s: %w", profile.Name, err),
		}
	}
	img, err := s.images.ImportRootfs(ctx, &models.ImportImageRequest{
		Name:         "vm-profile-" + profile.Name,
		Tag:          fmt.Sprintf("v%d-%dmb", profile.Version, diskSizeMB),
		RootfsPath:   prepared.DiskPath,
		RootfsSizeMB: diskSizeMB,
		BootMode:     image.BootModeBootableQCOW2,
	})
	if err != nil {
		return nil, fmt.Errorf("import prepared windows profile disk: %w", err)
	}
	if err := copyProfileCredential(prepared.PrivateKeyPath, filepath.Join(filepath.Dir(img.RootfsPath), "id_ed25519")); err != nil {
		_ = s.images.Delete(img.ID)
		return nil, fmt.Errorf("install prepared windows profile credentials: %w", err)
	}
	record := &models.VMProfileImage{
		ID:           uuid.New().String(),
		Profile:      profile.Name,
		ImageID:      img.ID,
		Fingerprint:  fingerprint,
		RootfsSizeMB: diskSizeMB,
		CreatedAt:    time.Now().UTC(),
	}
	if replace != nil {
		if err := s.db.DeleteVMProfileImage(replace.ID); err != nil && !errors.Is(err, sql.ErrNoRows) {
			_ = s.images.Delete(img.ID)
			return nil, fmt.Errorf("delete old windows vm profile image record before replacement: %w", err)
		}
	}
	if err := s.db.CreateVMProfileImage(record); err != nil {
		_ = s.images.Delete(img.ID)
		if replace != nil {
			_ = s.db.CreateVMProfileImage(replace)
		}
		return nil, fmt.Errorf("save prepared windows vm profile image: %w", err)
	}
	if replace != nil {
		if err := s.deleteImageIfUnused(replace.ImageID); err != nil {
			slog.Warn("failed to delete replaced windows VM profile backing image", "image", replace.ImageID, "error", err)
		}
	}
	return record, nil
}

func (s *Service) installProfileImage(ctx context.Context, profile Profile, fingerprint string, diskSizeMB int64) (*models.Image, error) {
	if s.profileImages.Mode != ProfileImageModeLocal {
		img, pulled, err := s.pullProfileImage(ctx, profile, fingerprint, diskSizeMB)
		if err == nil && pulled {
			return img, nil
		}
		if err != nil && s.profileImages.Mode == ProfileImageModePull {
			return nil, err
		}
		if err != nil && ctx.Err() != nil {
			return nil, err
		}
		if err != nil {
			slog.Warn("trusted VM profile image pull failed; falling back to local build", "profile", profile.Name, "error", err)
		}
	}
	img, err := s.images.Build(ctx, &models.BuildImageRequest{
		Name:         "vm-profile-" + profile.Name,
		Tag:          fmt.Sprintf("v%d-%dmb", profile.Version, diskSizeMB),
		Dockerfile:   profile.Dockerfile,
		RootfsSizeMB: diskSizeMB,
		BootMode:     image.BootModePersistent,
	})
	if err != nil {
		return nil, fmt.Errorf("build vm profile image: %w", err)
	}
	return img, nil
}

func (s *Service) pullProfileImage(ctx context.Context, profile Profile, fingerprint string, diskSizeMB int64) (*models.Image, bool, error) {
	entry := s.catalog.Match(profile, fingerprint, diskSizeMB)
	if entry == nil {
		if s.profileImages.Mode == ProfileImageModePull {
			return nil, false, fmt.Errorf("no trusted profile image for profile=%s version=%d fingerprint=%s size_mb>=%d", profile.Name, profile.Version, fingerprint, diskSizeMB)
		}
		return nil, false, nil
	}
	if s.pullTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.pullTimeout)
		defer cancel()
	}
	tmp, err := os.CreateTemp("", "kiln-vm-profile-rootfs-*.ext4")
	if err != nil {
		return nil, false, fmt.Errorf("create profile rootfs temp file: %w", err)
	}
	tmpPath := tmp.Name()
	_ = tmp.Close()
	defer os.Remove(tmpPath)

	slog.Info("pulling trusted VM profile image", "profile", profile.Name, "reference", entry.Reference, "base_size_mb", entry.RootfsSizeMB, "target_size_mb", diskSizeMB)
	if err := s.fetchProfileRootfs(ctx, entry, tmpPath); err != nil {
		return nil, false, fmt.Errorf("pull trusted profile image %s: %w", entry.Reference, err)
	}
	if err := image.EnsureRootfsSize(ctx, tmpPath, diskSizeMB); err != nil {
		return nil, false, fmt.Errorf("prepare trusted profile rootfs: %w", err)
	}
	img, err := s.images.ImportRootfs(ctx, &models.ImportImageRequest{
		Name:         "vm-profile-" + profile.Name,
		Tag:          fmt.Sprintf("v%d-%dmb", profile.Version, diskSizeMB),
		RootfsPath:   tmpPath,
		RootfsSizeMB: diskSizeMB,
		BootMode:     image.BootModePersistent,
	})
	if err != nil {
		return nil, false, fmt.Errorf("install trusted profile rootfs: %w", err)
	}
	return img, true, nil
}

func (s *Service) fetchProfileRootfs(ctx context.Context, entry *ProfileImageCatalogEntry, targetPath string) error {
	if strings.HasPrefix(entry.Reference, "https://") || strings.HasPrefix(entry.Reference, "http://") {
		fetchHTTP := func(client *http.Client) error {
			return image.FetchHTTPRootfs(ctx, image.HTTPRootfsRequest{
				URL:              entry.Reference,
				CompressedSHA256: entry.LayerDigest,
				RootfsSHA256:     entry.RootfsSHA256,
				TargetPath:       targetPath,
				Token:            s.profileImages.Token,
				HTTPClient:       client,
				StallTimeout:     s.pullStallTimeout,
			})
		}
		defaultClient, _ := profileImageHTTPClient(s.ociHTTPClient, "default", s.pullHeaderTimeout)
		err := fetchHTTP(defaultClient)
		if err == nil || !shouldRetryProfileHTTPPullOverIPv4(ctx, err) {
			return err
		}
		ipv4Client, ok := profileImageHTTPClient(s.ociHTTPClient, "ipv4", s.pullHeaderTimeout)
		if !ok {
			return err
		}
		slog.Warn("trusted VM profile image pull failed transiently; retrying over IPv4", "profile", entry.Profile, "reference", entry.Reference, "error", err)
		if retryErr := fetchHTTP(ipv4Client); retryErr != nil {
			return fmt.Errorf("trusted HTTP profile image pull failed on default network (%v) and IPv4 retry failed: %w", err, retryErr)
		}
		return nil
	}
	ociClient, _ := profileImageHTTPClient(s.ociHTTPClient, "default", s.pullHeaderTimeout)
	if err := image.FetchOCIRootfs(ctx, image.OCIRootfsRequest{
		Reference:      entry.Reference,
		LayerDigest:    entry.LayerDigest,
		RootfsSHA256:   entry.RootfsSHA256,
		LayerMediaType: entry.LayerMediaType,
		TargetPath:     targetPath,
		Token:          s.profileImages.Token,
		HTTPClient:     ociClient,
		StallTimeout:   s.pullStallTimeout,
	}); err != nil {
		return fmt.Errorf("fetch OCI profile image: %w", err)
	}
	return nil
}

func shouldRetryProfileHTTPPullOverIPv4(ctx context.Context, err error) bool {
	if err == nil || ctx.Err() != nil {
		return false
	}
	var stalled *image.RootfsDownloadStalledError
	if errors.As(err, &stalled) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	return errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.ECONNABORTED) ||
		errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF)
}

func profileImageHTTPClient(base *http.Client, network string, responseHeaderTimeout time.Duration) (*http.Client, bool) {
	source := base
	if source == nil {
		source = http.DefaultClient
	}
	transport := source.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	httpTransport, ok := transport.(*http.Transport)
	if !ok {
		if network == "ipv4" || responseHeaderTimeout > 0 {
			return source, false
		}
		return source, true
	}
	cloned := httpTransport.Clone()
	if responseHeaderTimeout > 0 {
		cloned.ResponseHeaderTimeout = responseHeaderTimeout
	}
	if network == "ipv4" {
		dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
		cloned.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			if strings.HasPrefix(network, "tcp") {
				network = "tcp4"
			}
			return dialer.DialContext(ctx, network, addr)
		}
		cloned.DialTLSContext = nil
	}
	return &http.Client{
		Transport:     cloned,
		CheckRedirect: source.CheckRedirect,
		Jar:           source.Jar,
		Timeout:       source.Timeout,
	}, true
}

func (s *Service) DeleteProfileImage(id string) error {
	record, err := s.resolveProfileImage(id)
	if err != nil {
		return err
	}
	if err := s.db.DeleteVMProfileImage(record.ID); err != nil {
		return fmt.Errorf("delete vm profile image record: %w", err)
	}
	if err := s.deleteImageIfUnused(record.ImageID); err != nil {
		return fmt.Errorf("delete vm profile image backing image: %w", err)
	}
	return nil
}

func (s *Service) DeleteProfileImagesByProfile(profileName string) error {
	profileName = strings.TrimSpace(profileName)
	if profileName == "" {
		return &apierr.ValidationError{Field: "profile", Message: "is required"}
	}
	if _, err := getProfile(profileName); err != nil {
		return &apierr.ValidationError{Field: "profile", Message: err.Error()}
	}
	records, err := s.db.DeleteVMProfileImagesByProfile(profileName)
	if err != nil {
		return fmt.Errorf("delete vm profile image records by profile: %w", err)
	}
	for _, record := range records {
		if err := s.deleteImageIfUnused(record.ImageID); err != nil {
			return fmt.Errorf("delete vm profile image backing image %s: %w", record.ImageID, err)
		}
	}
	return nil
}

func (s *Service) normalizeProfileImageRequest(req *models.EnsureVMProfileImageRequest) (Profile, int64, error) {
	if req == nil {
		req = &models.EnsureVMProfileImageRequest{}
	}
	profile, err := getProfile(strings.TrimSpace(req.Profile))
	if err != nil {
		return Profile{}, 0, &apierr.ValidationError{Field: "profile", Message: err.Error()}
	}
	diskSizeMB := req.RootfsSizeMB
	if diskSizeMB == 0 {
		diskSizeMB = profile.DefaultDiskSizeMB
	}
	if err := s.validateDiskSizeMB("rootfs_size_mb", diskSizeMB); err != nil {
		return Profile{}, 0, err
	}
	return profile, diskSizeMB, nil
}

func (s *Service) validateCachedImage(cached *models.VMProfileImage) error {
	img, err := s.images.Get(cached.ImageID)
	if err != nil {
		return fmt.Errorf("get cached backing image: %w", err)
	}
	if _, err := os.Stat(img.RootfsPath); err != nil {
		return fmt.Errorf("stat cached backing image rootfs: %w", err)
	}
	return nil
}

func (s *Service) findReusablePreparedProfileImage(profile Profile, diskSizeMB int64) (*models.VMProfileImage, error) {
	records, err := s.db.ListVMProfileImages()
	if err != nil {
		return nil, fmt.Errorf("list vm profile images: %w", err)
	}
	prep := profilePreparation(profile)
	for _, record := range records {
		if record.Profile != profile.Name || record.RootfsSizeMB != diskSizeMB {
			continue
		}
		img, err := s.images.Get(record.ImageID)
		if err != nil {
			continue
		}
		if !preparedImageBootModeMatches(prep, img.BootMode) {
			continue
		}
		if err := s.validateCachedImage(&record); err != nil {
			continue
		}
		if prep.Kind == models.VMProfilePreparationWindowsUnattended {
			keyPath := filepath.Join(filepath.Dir(img.RootfsPath), "id_ed25519")
			if _, err := os.Stat(keyPath); err != nil {
				continue
			}
		}
		reusable := record
		return &reusable, nil
	}
	return nil, sql.ErrNoRows
}

func preparedImageBootModeMatches(prep models.VMProfilePreparation, bootMode string) bool {
	switch prep.Kind {
	case models.VMProfilePreparationWindowsUnattended:
		if prep.ImageBootMode != "" {
			if image.IsBootableQCOW2BootMode(prep.ImageBootMode) {
				return image.IsBootableQCOW2BootMode(bootMode)
			}
			return bootMode == prep.ImageBootMode
		}
		return image.IsBootableQCOW2BootMode(bootMode)
	default:
		return true
	}
}

func (s *Service) migratePreparedProfileImageFingerprint(cached *models.VMProfileImage, fingerprint string) (*models.VMProfileImage, error) {
	if cached.Fingerprint == fingerprint {
		return cached, nil
	}
	migrated := *cached
	migrated.ID = uuid.New().String()
	migrated.Fingerprint = fingerprint
	migrated.CreatedAt = time.Now().UTC()
	if err := s.db.DeleteVMProfileImage(cached.ID); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("delete old prepared windows profile image record: %w", err)
	}
	if err := s.db.CreateVMProfileImage(&migrated); err != nil {
		_ = s.db.CreateVMProfileImage(cached)
		return nil, fmt.Errorf("save migrated prepared windows profile image record: %w", err)
	}
	return &migrated, nil
}

func (s *Service) deleteSupersededProfileImages(current *models.VMProfileImage) {
	records, err := s.db.ListVMProfileImages()
	if err != nil {
		slog.Warn("list vm profile images for refresh cleanup failed", "error", err)
		return
	}
	for _, record := range records {
		if record.ID == current.ID ||
			record.Profile != current.Profile ||
			record.RootfsSizeMB != current.RootfsSizeMB {
			continue
		}
		if err := s.db.DeleteVMProfileImage(record.ID); err != nil && !errors.Is(err, sql.ErrNoRows) {
			slog.Warn("delete superseded vm profile image record failed", "cache", record.ID, "error", err)
			continue
		}
		if err := s.deleteImageIfUnused(record.ImageID); err != nil {
			slog.Warn("delete superseded vm profile backing image failed", "image", record.ImageID, "error", err)
		}
	}
}

func (s *Service) deleteImageIfUnused(imageID string) error {
	if _, err := s.db.GetVMImageByImageID(imageID); err == nil {
		return nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("check vm image ownership: %w", err)
	}
	count, err := s.db.CountVMsByImageID(imageID)
	if err != nil {
		return fmt.Errorf("count vms using image: %w", err)
	}
	if count > 0 {
		return nil
	}
	if err := s.images.Delete(imageID); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("delete image: %w", err)
	}
	return nil
}

func copyProfileCredential(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("read profile credential: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0750); err != nil {
		return fmt.Errorf("create profile credential directory: %w", err)
	}
	if err := os.WriteFile(dst, data, 0600); err != nil {
		return fmt.Errorf("write profile credential: %w", err)
	}
	return nil
}

func (s *Service) Create(ctx context.Context, req *models.CreateVMRequest) (*models.VM, error) {
	resolved, err := s.resolveCreatePlan(req)
	if err != nil {
		return nil, err
	}
	plan := resolved.plan
	profile := s.applyProfileInfrastructureConfig(resolved.profile)

	imageID, rootfsSizeMB, err := s.resolveCreateImage(ctx, resolved, profile)
	if err != nil {
		return nil, err
	}

	machineCreated := false
	m, err := s.machines.Create(ctx, &models.CreateMachineRequest{
		Name:          plan.Name,
		Backend:       profile.Backend,
		GuestOS:       profile.GuestOS,
		Libvirt:       libvirtdomain.Clone(profile.Libvirt),
		ImageID:       imageID,
		NetworkPolicy: plan.NetworkPolicy,
		CPU:           &plan.CPU,
		MemoryMB:      plan.MemoryMB,
		RootfsSizeMB:  rootfsSizeMB,
	})
	if err != nil {
		return nil, fmt.Errorf("create vm machine: %w", err)
	}
	machineCreated = true

	now := time.Now().UTC()
	record := &models.VMRecord{
		ID:         uuid.New().String(),
		Name:       plan.Name,
		Profile:    profile.Name,
		MachineID:  m.ID,
		ImageID:    imageID,
		VMImage:    plan.Image,
		Labels:     plan.Labels,
		DiskSizeMB: plan.DiskSizeMB,
		Power: models.VMPower{
			Policy:         plan.Power,
			LastActivityAt: &now,
			LastStartedAt:  &now,
		},
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.db.CreateVM(record); err != nil {
		if machineCreated {
			_ = s.machines.Delete(m.ID)
		}
		if isConstraintError(err) {
			return nil, fmt.Errorf("VM %q already exists: %w", plan.Name, apierr.ErrConflict)
		}
		return nil, fmt.Errorf("save vm: %w", err)
	}
	if err := s.initializeVMCapabilityConditions(record.ID, profile.Capabilities); err != nil {
		_ = s.db.DeleteVM(record.ID)
		if machineCreated {
			_ = s.machines.Delete(m.ID)
		}
		return nil, fmt.Errorf("initialize vm capability conditions: %w", err)
	}

	return s.composeWithMachine(record, m)
}

func (s *Service) resolveCreateImage(ctx context.Context, resolved *resolvedVMCreatePlan, profile Profile) (string, int64, error) {
	if resolved.image != nil {
		return resolved.image.ImageID, resolved.plan.DiskSizeMB, nil
	}
	baseImage, err := s.EnsureProfileImage(ctx, &models.EnsureVMProfileImageRequest{
		Profile:      profile.Name,
		RootfsSizeMB: resolved.plan.DiskSizeMB,
	})
	if err != nil {
		return "", 0, fmt.Errorf("ensure vm profile image: %w", err)
	}
	return baseImage.ImageID, 0, nil
}

func (s *Service) Get(ref string) (*models.VM, error) {
	record, err := s.getRecord(ref)
	if err != nil {
		return nil, err
	}
	return s.compose(record)
}

func (s *Service) List(selectors ...LabelSelector) ([]models.VM, error) {
	records, err := s.db.ListVMs()
	if err != nil {
		return nil, fmt.Errorf("list vms: %w", err)
	}
	vms := make([]models.VM, 0, len(records))
	for _, record := range records {
		if !MatchLabels(record.Labels, selectors) {
			continue
		}
		composed, err := s.compose(&record)
		if err != nil {
			if errors.Is(err, apierr.ErrNotFound) {
				continue
			}
			return nil, err
		}
		vms = append(vms, *composed)
	}
	return vms, nil
}

func (s *Service) Start(ctx context.Context, ref string) (*models.VM, error) {
	record, err := s.getRecord(ref)
	if err != nil {
		return nil, err
	}
	releaseStart, err := s.reserveStart(record)
	if err != nil {
		return nil, err
	}
	defer releaseStart()
	m, err := s.machines.Get(record.MachineID)
	if err != nil {
		return nil, fmt.Errorf("get vm machine: %w", err)
	}
	switch m.State {
	case models.MachineStateRunning, models.MachineStateCreating:
		now := time.Now().UTC()
		_ = s.db.RecordVMPowerActivity(record.ID, now)
		record.Power.LastActivityAt = &now
		return s.composeWithMachine(record, m)
	case models.MachineStateStopped, models.MachineStateError:
		m, err = s.machines.Start(ctx, m.ID)
		if err != nil {
			return nil, fmt.Errorf("start vm machine: %w", err)
		}
		now := time.Now().UTC()
		_ = s.db.RecordVMPowerStarted(record.ID, now)
		_ = s.db.RecordVMPowerActivity(record.ID, now)
		record.Power.LastStartedAt = &now
		record.Power.LastActivityAt = &now
		record.Power.StopReason = ""
		return s.composeWithMachine(record, m)
	default:
		return nil, fmt.Errorf("VM %q is %s: %w", record.Name, m.State, apierr.ErrConflict)
	}
}

func (s *Service) Stop(ref string) error {
	return s.stopWithReason(ref, "manual", true)
}

func (s *Service) stopWithReason(ref, reason string, guardActiveUses bool) error {
	record, err := s.getRecord(ref)
	if err != nil {
		return err
	}
	var releaseStop func()
	if guardActiveUses {
		var reserveErr error
		releaseStop, reserveErr = s.reserveStop(record.ID, record.Name)
		if reserveErr != nil {
			return reserveErr
		}
		defer releaseStop()
	}
	m, err := s.machines.Get(record.MachineID)
	if err != nil {
		return fmt.Errorf("get vm machine: %w", err)
	}
	if m.State == models.MachineStateStopped {
		_ = s.db.RecordVMPowerStopped(record.ID, time.Now().UTC(), reason)
		return nil
	}
	if err := s.machines.Stop(record.MachineID); err != nil {
		return fmt.Errorf("stop vm machine: %w", err)
	}
	_ = s.db.RecordVMPowerStopped(record.ID, time.Now().UTC(), reason)
	return nil
}

func (s *Service) applyProfileInfrastructureConfig(profile Profile) Profile {
	profile.Networking = profileNetworking(profile, s.libvirtNetwork)
	if profile.Libvirt != nil && profile.Libvirt.Network.Source == "" {
		profile.Libvirt = libvirtdomain.Clone(profile.Libvirt)
		profile.Libvirt.Network.Source = strings.TrimSpace(s.libvirtNetwork)
	}
	return profile
}

func (s *Service) UpdatePowerPolicy(ctx context.Context, ref string, policy models.VMPowerPolicy) (*models.VM, error) {
	record, err := s.getRecord(ref)
	if err != nil {
		return nil, err
	}
	normalized, err := NormalizePowerPolicy(&policy)
	if err != nil {
		return nil, err
	}
	if err := s.db.UpdateVMPowerPolicy(record.ID, normalized); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, &apierr.NotFoundError{Resource: "vm", ID: ref}
		}
		return nil, fmt.Errorf("update vm power policy: %w", err)
	}
	if normalized.StartsWithDaemon() {
		if _, err := s.Start(ctx, record.ID); err != nil {
			return nil, fmt.Errorf("start vm for power policy %s: %w", normalized.Mode, err)
		}
	}
	return s.Get(record.ID)
}

type UseLease struct {
	service *Service
	vmID    string
	once    sync.Once
}

func (l *UseLease) Release() {
	if l == nil || l.service == nil || l.vmID == "" {
		return
	}
	l.once.Do(func() {
		l.service.releaseUse(l.vmID)
		_ = l.service.db.RecordVMPowerActivity(l.vmID, time.Now().UTC())
	})
}

func (s *Service) beginUse(ctx context.Context, ref, reason string) (*models.VMRecord, *UseLease, error) {
	record, err := s.getRecord(ref)
	if err != nil {
		return nil, nil, err
	}
	s.powerMu.Lock()
	if s.powerStopping[record.ID] {
		s.powerMu.Unlock()
		return nil, nil, fmt.Errorf("VM %q is stopping for power policy: %w", record.Name, apierr.ErrConflict)
	}
	s.powerLeases[record.ID]++
	s.powerMu.Unlock()
	lease := &UseLease{service: s, vmID: record.ID}
	if err := s.db.RecordVMPowerActivity(record.ID, time.Now().UTC()); err != nil {
		lease.Release()
		return nil, nil, fmt.Errorf("record vm activity for %s: %w", reason, err)
	}
	if err := s.ensureStartedForUse(ctx, record); err != nil {
		lease.Release()
		return nil, nil, err
	}
	return record, lease, nil
}

func (s *Service) ensureStartedForUse(ctx context.Context, record *models.VMRecord) error {
	m, err := s.machines.Get(record.MachineID)
	if err != nil {
		return fmt.Errorf("get vm machine for use: %w", err)
	}
	switch m.State {
	case models.MachineStateRunning, models.MachineStateCreating:
		return nil
	case models.MachineStateStopped, models.MachineStateError:
		if !record.Power.Policy.WakesOnUse() {
			return fmt.Errorf("VM %q power policy %s does not wake on use: %w", record.Name, record.Power.Policy.Mode, apierr.ErrConflict)
		}
		if _, err := s.Start(ctx, record.ID); err != nil {
			return err
		}
		return nil
	default:
		return fmt.Errorf("VM %q is %s: %w", record.Name, m.State, apierr.ErrConflict)
	}
}

func (s *Service) releaseUse(vmID string) {
	s.powerMu.Lock()
	defer s.powerMu.Unlock()
	if s.powerLeases[vmID] <= 1 {
		delete(s.powerLeases, vmID)
		return
	}
	s.powerLeases[vmID]--
}

func (s *Service) reserveStop(vmID, vmName string) (func(), error) {
	s.powerMu.Lock()
	defer s.powerMu.Unlock()
	if s.powerStopping[vmID] {
		return nil, fmt.Errorf("VM %q is already stopping: %w", vmName, apierr.ErrConflict)
	}
	if s.powerLeases[vmID] > 0 {
		return nil, &ActiveUseError{VMName: vmName}
	}
	s.powerStopping[vmID] = true
	return func() {
		s.releaseIdleStop(vmID)
	}, nil
}

func (s *Service) reserveIdleStop(vmID string) bool {
	s.powerMu.Lock()
	defer s.powerMu.Unlock()
	if s.powerStopping[vmID] || s.powerLeases[vmID] > 0 {
		return false
	}
	s.powerStopping[vmID] = true
	return true
}

func (s *Service) releaseIdleStop(vmID string) {
	s.powerMu.Lock()
	defer s.powerMu.Unlock()
	delete(s.powerStopping, vmID)
}

type leasedConnection struct {
	io.ReadWriteCloser
	lease *UseLease
}

func (c *leasedConnection) Close() error {
	err := c.ReadWriteCloser.Close()
	c.lease.Release()
	if err != nil {
		return fmt.Errorf("close leased connection: %w", err)
	}
	return nil
}

func (s *Service) Delete(ref string) error {
	record, err := s.getRecord(ref)
	if err != nil {
		return err
	}
	if s.isSavingImage(record.ID) {
		return fmt.Errorf("VM %q is being saved as an image: %w", record.Name, apierr.ErrConflict)
	}

	if err := s.machines.Delete(record.MachineID); err != nil {
		if errors.Is(err, apierr.ErrNotFound) {
			if deleteErr := s.db.DeleteVM(record.ID); deleteErr != nil && !errors.Is(deleteErr, sql.ErrNoRows) {
				return fmt.Errorf("delete stale vm record: %w", deleteErr)
			}
		} else {
			return fmt.Errorf("delete vm machine: %w", err)
		}
	} else if err := s.db.DeleteVM(record.ID); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("delete vm record: %w", err)
	}
	if _, err := s.db.GetVMProfileImageByImageID(record.ImageID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			if err := s.deleteImageIfUnused(record.ImageID); err != nil {
				return fmt.Errorf("delete vm image: %w", err)
			}
		} else {
			return fmt.Errorf("check vm image ownership: %w", err)
		}
	}
	return nil
}

func (s *Service) Exec(ctx context.Context, ref, command string) (*execution.BufferedResult, error) {
	result, err := execution.RunBuffered(ctx, execution.RunnerFunc(func(ctx context.Context, request execution.Request) execution.Outcome {
		return s.Execute(ctx, ref, request)
	}), command, nil)
	if err != nil {
		return nil, fmt.Errorf("exec vm command: %w", err)
	}
	return result, nil
}

func (s *Service) Execute(ctx context.Context, ref string, request execution.Request) execution.Outcome {
	record, err := s.getRecord(ref)
	if err != nil {
		return execution.OutcomeFromError(err)
	}
	if err := s.ensureProfileAccessOperation(record, vmAccessOperationExec); err != nil {
		return execution.OutcomeFromError(err)
	}
	record, lease, err := s.beginUse(ctx, record.ID, "exec")
	if err != nil {
		return execution.OutcomeFromError(err)
	}
	defer lease.Release()
	if err := s.WaitReady(ctx, record.ID); err != nil {
		return execution.OutcomeFromError(fmt.Errorf("wait for vm before exec: %w", err))
	}
	return s.machines.Execute(ctx, record.MachineID, request)
}

func (s *Service) Upload(ctx context.Context, ref, remotePath string, mode uint32, size int64, reader io.Reader) error {
	record, err := s.getRecord(ref)
	if err != nil {
		return err
	}
	if err := s.ensureProfileAccessOperation(record, vmAccessOperationUpload); err != nil {
		return err
	}
	record, lease, err := s.beginUse(ctx, record.ID, "upload")
	if err != nil {
		return err
	}
	defer lease.Release()
	if err := s.WaitReady(ctx, record.ID); err != nil {
		return fmt.Errorf("wait for vm before upload: %w", err)
	}
	if err := s.machines.Upload(ctx, record.MachineID, remotePath, mode, size, reader); err != nil {
		return fmt.Errorf("upload vm file: %w", err)
	}
	return nil
}

func (s *Service) DialShell(ctx context.Context, ref string) (io.ReadWriteCloser, error) {
	record, err := s.getRecord(ref)
	if err != nil {
		return nil, err
	}
	if err := s.ensureProfileAccessOperation(record, vmAccessOperationShell); err != nil {
		return nil, err
	}
	record, lease, err := s.beginUse(ctx, record.ID, "shell")
	if err != nil {
		return nil, err
	}
	if err := s.WaitReady(ctx, record.ID); err != nil {
		lease.Release()
		return nil, fmt.Errorf("wait for vm before shell: %w", err)
	}
	conn, err := s.machines.DialShell(ctx, record.MachineID)
	if err != nil {
		lease.Release()
		return nil, fmt.Errorf("dial vm shell: %w", err)
	}
	return &leasedConnection{ReadWriteCloser: conn, lease: lease}, nil
}

func (s *Service) DialForward(ctx context.Context, ref string, port int) (io.ReadWriteCloser, error) {
	if !guestagent.ValidForwardTargetPort(port) {
		return nil, &apierr.ValidationError{
			Field:   "port",
			Message: fmt.Sprintf("port must be between %d and %d", guestagent.MinForwardTargetPort, guestagent.MaxForwardTargetPort),
		}
	}
	record, err := s.getRecord(ref)
	if err != nil {
		return nil, err
	}
	if err := s.ensureProfileAccessOperation(record, vmAccessOperationForward); err != nil {
		return nil, err
	}
	record, lease, err := s.beginUse(ctx, record.ID, "forward")
	if err != nil {
		return nil, err
	}
	if err := s.WaitReady(ctx, record.ID); err != nil {
		lease.Release()
		return nil, fmt.Errorf("wait for vm before forward: %w", err)
	}
	conn, err := s.machines.DialForward(ctx, record.MachineID, port)
	if err != nil {
		lease.Release()
		return nil, fmt.Errorf("dial vm forward: %w", err)
	}
	return &leasedConnection{ReadWriteCloser: conn, lease: lease}, nil
}

func (s *Service) WaitReady(ctx context.Context, ref string) error {
	record, err := s.getRecord(ref)
	if err != nil {
		return err
	}
	if err := s.waitProfileReady(ctx, record); err != nil {
		return err
	}
	if err := s.waitGatewayDNSReady(ctx, record); err != nil {
		return err
	}
	verifyCtx, cancel := contextWithDefaultTimeout(ctx, 5*time.Minute)
	defer cancel()
	if err := s.verifyDeclaredCapabilities(verifyCtx, record); err != nil {
		return err
	}
	return nil
}

func (s *Service) waitReadyTerminalState(record *models.VMRecord, lastErr error, probe string) error {
	current, err := s.compose(record)
	if err != nil {
		if errors.Is(err, apierr.ErrNotFound) {
			if probe == "" {
				probe = "vm guest agent"
			}
			return fmt.Errorf("wait for %s: VM %s disappeared before ready: %w", probe, record.Name, err)
		}
		return nil
	}
	if current.State != models.MachineStateStopped && current.State != models.MachineStateError {
		return nil
	}
	stateErr := &WaitReadyStateError{
		VMName:     current.Name,
		MachineID:  current.MachineID,
		State:      current.State,
		MachineErr: current.Error,
		LastErr:    lastErr,
		Probe:      probe,
	}
	if len(current.Diagnostics) > 0 {
		return machineDiagnosticsOperationError(current, stateErr)
	}
	return stateErr
}

func (s *Service) ReconcilePowerOnDaemonStart(ctx context.Context) {
	records, err := s.db.ListPowerStartVMs()
	if err != nil {
		slog.Error("list daemon-start VMs", "error", err)
		return
	}
	for _, record := range records {
		record := record
		go func() {
			if _, err := s.Start(ctx, record.ID); err != nil {
				slog.Error("power daemon-start VM failed", "vm", record.Name, "mode", record.Power.Policy.Mode, "error", err)
			}
		}()
	}
}

func (s *Service) StartPowerManager(ctx context.Context) {
	s.ReconcilePowerOnDaemonStart(ctx)
	go s.runIdleStopLoop(ctx, 30*time.Second)
}

func (s *Service) runIdleStopLoop(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := s.StopIdleVMs(ctx, time.Now().UTC()); err != nil {
				slog.Error("idle-stop VM sweep failed", "error", err)
			}
		}
	}
}

func (s *Service) StopIdleVMs(ctx context.Context, now time.Time) ([]models.VM, error) {
	records, err := s.db.ListVMs()
	if err != nil {
		return nil, fmt.Errorf("list VMs for idle stop: %w", err)
	}
	stopped := []models.VM{}
	var cleanupErrs []error
	for _, record := range records {
		if !powerPolicyCanIdleStop(record.Power.Policy) {
			continue
		}
		m, err := s.machines.Get(record.MachineID)
		if err != nil {
			cleanupErrs = append(cleanupErrs, fmt.Errorf("get VM %s machine: %w", record.Name, err))
			continue
		}
		if m.State != models.MachineStateRunning {
			continue
		}
		lastActivity := record.UpdatedAt
		if record.Power.LastActivityAt != nil {
			lastActivity = *record.Power.LastActivityAt
		}
		if now.Sub(lastActivity) < time.Duration(record.Power.Policy.IdleTimeoutSeconds)*time.Second {
			continue
		}
		if !s.reserveIdleStop(record.ID) {
			continue
		}
		fresh, err := s.db.GetVM(record.ID)
		if err != nil {
			s.releaseIdleStop(record.ID)
			cleanupErrs = append(cleanupErrs, fmt.Errorf("refresh VM %s before idle stop: %w", record.Name, err))
			continue
		}
		if !powerPolicyCanIdleStop(fresh.Power.Policy) {
			s.releaseIdleStop(record.ID)
			continue
		}
		freshLastActivity := fresh.UpdatedAt
		if fresh.Power.LastActivityAt != nil {
			freshLastActivity = *fresh.Power.LastActivityAt
		}
		if now.Sub(freshLastActivity) < time.Duration(fresh.Power.Policy.IdleTimeoutSeconds)*time.Second {
			s.releaseIdleStop(record.ID)
			continue
		}
		err = s.stopWithReason(record.ID, "idle_timeout", false)
		s.releaseIdleStop(record.ID)
		if err != nil {
			cleanupErrs = append(cleanupErrs, fmt.Errorf("idle-stop VM %s: %w", record.Name, err))
			continue
		}
		stoppedRecord, getErr := s.Get(record.ID)
		if getErr != nil {
			cleanupErrs = append(cleanupErrs, fmt.Errorf("refresh idle-stopped VM %s: %w", record.Name, getErr))
			continue
		}
		stopped = append(stopped, *stoppedRecord)
	}
	if len(cleanupErrs) > 0 {
		return stopped, errors.Join(cleanupErrs...)
	}
	return stopped, nil
}

func (s *Service) getRecord(ref string) (*models.VMRecord, error) {
	record, err := s.db.GetVM(ref)
	if err == nil {
		return record, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("get vm: %w", err)
	}
	records, err := s.db.ListVMs()
	if err != nil {
		return nil, fmt.Errorf("list vms for ID resolution: %w", err)
	}
	candidates := make([]refid.Candidate, 0, len(records))
	for _, record := range records {
		candidates = append(candidates, refid.Candidate{ID: record.ID, Label: record.Name})
	}
	candidate, err := refid.Resolve("vm", ref, candidates)
	if err != nil {
		return nil, fmt.Errorf("resolve vm ID: %w", err)
	}
	record, err = s.db.GetVM(candidate.ID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, &apierr.NotFoundError{Resource: "vm", ID: ref}
		}
		return nil, fmt.Errorf("get vm: %w", err)
	}
	return record, nil
}

func (s *Service) resolveProfileImage(ref string) (*models.VMProfileImage, error) {
	record, err := s.db.GetVMProfileImage(ref)
	if err == nil {
		return record, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("get vm profile image: %w", err)
	}
	records, err := s.db.ListVMProfileImages()
	if err != nil {
		return nil, fmt.Errorf("list vm profile images for ID resolution: %w", err)
	}
	candidates := make([]refid.Candidate, 0, len(records))
	for _, record := range records {
		candidates = append(candidates, refid.Candidate{ID: record.ID, Label: record.Profile})
	}
	candidate, err := refid.Resolve("vm_profile_image", ref, candidates)
	if err != nil {
		return nil, fmt.Errorf("resolve vm profile image ID: %w", err)
	}
	record, err = s.db.GetVMProfileImage(candidate.ID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, &apierr.NotFoundError{Resource: "vm_profile_image", ID: ref}
		}
		return nil, fmt.Errorf("get vm profile image: %w", err)
	}
	return record, nil
}

func (s *Service) compose(record *models.VMRecord) (*models.VM, error) {
	m, err := s.machines.Get(record.MachineID)
	if err != nil {
		if errors.Is(err, apierr.ErrNotFound) {
			return nil, &apierr.NotFoundError{Resource: "vm", ID: record.ID}
		}
		return nil, fmt.Errorf("get vm machine: %w", err)
	}
	return s.composeWithMachine(record, m)
}

func (s *Service) activeLeaseCount(vmID string) int {
	s.powerMu.Lock()
	defer s.powerMu.Unlock()
	return s.powerLeases[vmID]
}

func composeVM(record *models.VMRecord, m *models.Machine) *models.VM {
	return &models.VM{
		ID:            record.ID,
		Name:          record.Name,
		Profile:       record.Profile,
		Backend:       m.Backend,
		GuestOS:       m.GuestOS,
		MachineID:     record.MachineID,
		ImageID:       record.ImageID,
		Image:         record.VMImage,
		Labels:        record.Labels,
		NetworkPolicy: m.NetworkPolicy,
		State:         m.State,
		IP:            m.IP,
		GatewayIP:     m.GatewayIP,
		TAPDevice:     m.TAPDevice,
		VSockCID:      m.VSockCID,
		VCPUs:         m.VCPUs,
		CPU:           m.CPU,
		MemoryMB:      m.MemoryMB,
		DiskSizeMB:    record.DiskSizeMB,
		Power:         record.Power,
		Error:         m.Error,
		Diagnostics:   m.Diagnostics,
		CreatedAt:     record.CreatedAt,
		UpdatedAt:     record.UpdatedAt,
	}
}

func (s *Service) composeWithMachine(record *models.VMRecord, m *models.Machine) (*models.VM, error) {
	vm := composeVM(record, m)
	capabilities, err := s.declaredCapabilityConditions(record)
	if err != nil {
		return nil, err
	}
	vm.Capabilities = capabilities
	vm.Power.ActiveLeases = s.activeLeaseCount(record.ID)
	return vm, nil
}

func contextWithDefaultTimeout(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, timeout)
}

func isConstraintError(err error) bool {
	return strings.Contains(strings.ToLower(err.Error()), "constraint")
}
