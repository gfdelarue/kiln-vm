package vm

import (
	"fmt"
	"runtime"
	"strings"

	"github.com/gfdelarue/kiln-vm/internal/apierr"
	"github.com/gfdelarue/kiln-vm/internal/models"
)

type ProfileImageShapeStatus struct {
	Mode                       string
	Profile                    string
	ProfileVersion             int
	Arch                       string
	Fingerprint                string
	RequestedRootfsSizeMB      int64
	MatchedRootfsSizeMB        int64
	MinimumTrustedRootfsSizeMB int64
}

type ProfileImageShapeError struct {
	Status ProfileImageShapeStatus
}

func (e *ProfileImageShapeError) Error() string {
	status := e.Status
	if status.MinimumTrustedRootfsSizeMB > 0 {
		return fmt.Sprintf(
			"trusted VM profile image is not available for profile=%s version=%d arch=%s requested_size_mb=%d; minimum trusted rootfs size is %dMiB",
			status.Profile,
			status.ProfileVersion,
			status.Arch,
			status.RequestedRootfsSizeMB,
			status.MinimumTrustedRootfsSizeMB,
		)
	}
	return fmt.Sprintf(
		"trusted VM profile image is not available for profile=%s version=%d arch=%s requested_size_mb=%d",
		status.Profile,
		status.ProfileVersion,
		status.Arch,
		status.RequestedRootfsSizeMB,
	)
}

func (e *ProfileImageShapeError) Unwrap() error {
	return apierr.ErrValidation
}

func (s *Service) PreflightProfileImageShape(profileName string, rootfsSizeMB int64) (*ProfileImageShapeStatus, error) {
	profile, diskSizeMB, err := s.normalizeProfileImageRequest(&models.EnsureVMProfileImageRequest{
		Profile:      profileName,
		RootfsSizeMB: rootfsSizeMB,
	})
	if err != nil {
		return nil, err
	}
	fingerprint := profileFingerprint(profile, s.images.RuntimeFingerprint())
	status := ProfileImageShapeStatus{
		Mode:                  s.profileImages.Mode,
		Profile:               profile.Name,
		ProfileVersion:        profile.Version,
		Arch:                  runtime.GOARCH,
		Fingerprint:           fingerprint,
		RequestedRootfsSizeMB: diskSizeMB,
	}
	if s.profileImages.Mode == ProfileImageModeLocal || profileRequiresPreparedImage(profile) {
		return &status, nil
	}
	if entry := s.catalog.Match(profile, fingerprint, diskSizeMB); entry != nil {
		status.MatchedRootfsSizeMB = entry.RootfsSizeMB
		return &status, nil
	}
	status.MinimumTrustedRootfsSizeMB = s.catalog.MinimumRootfsSizeMB(profile, fingerprint, runtime.GOARCH)
	if s.profileImages.Mode == ProfileImageModePull {
		return &status, &ProfileImageShapeError{Status: status}
	}
	return &status, nil
}

func (c *ProfileImageCatalog) MinimumRootfsSizeMB(profile Profile, fingerprint string, arch string) int64 {
	if c == nil {
		return 0
	}
	arch = strings.TrimSpace(arch)
	var minimum int64
	for _, entry := range c.Images {
		if entry.Profile != profile.Name ||
			entry.ProfileVersion != profile.Version ||
			entry.Arch != arch ||
			entry.RootfsSizeMB <= 0 ||
			entry.Reference == "" ||
			entry.LayerDigest == "" ||
			entry.RootfsSHA256 == "" {
			continue
		}
		if entry.Fingerprint != "" && entry.Fingerprint != fingerprint {
			continue
		}
		if minimum == 0 || entry.RootfsSizeMB < minimum {
			minimum = entry.RootfsSizeMB
		}
	}
	return minimum
}
