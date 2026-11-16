package vm

import (
	"fmt"
	"strings"

	"github.com/gfdelarue/kiln-vm/internal/apierr"
	"github.com/gfdelarue/kiln-vm/internal/image"
	"github.com/gfdelarue/kiln-vm/internal/models"
)

func profilePreparation(profile Profile) models.VMProfilePreparation {
	prep := profile.Preparation
	if prep.Kind == "" {
		if profileIsWindows(profile) {
			prep = windowsUnattendedPreparation()
		} else {
			prep = dockerRootfsPreparation()
		}
	}
	if prep.Version == 0 {
		prep.Version = 1
	}
	if prep.Kind == models.VMProfilePreparationWindowsUnattended && prep.ImageBootMode == "" {
		prep.ImageBootMode = image.BootModeBootableQCOW2
	}
	return prep
}

func profileRequiresPreparedImage(profile Profile) bool {
	switch profilePreparation(profile).Kind {
	case models.VMProfilePreparationDockerRootfs:
		return false
	default:
		return true
	}
}

func profilePreparationBootMode(profile Profile) string {
	prep := profilePreparation(profile)
	if prep.ImageBootMode != "" {
		return prep.ImageBootMode
	}
	if prep.Kind == models.VMProfilePreparationWindowsUnattended {
		return image.BootModeBootableQCOW2
	}
	return image.BootModePersistent
}

func resolvePrepareStrategy(profile Profile, req *models.PrepareVMProfileRequest) (models.VMProfilePreparation, error) {
	prep := profilePreparation(profile)
	requested := strings.TrimSpace(req.Strategy)
	if requested == "" {
		return prep, nil
	}
	if requested != string(prep.Kind) {
		return prep, &apierr.ValidationError{
			Field:   "strategy",
			Message: fmt.Sprintf("profile %q uses preparation strategy %q", profile.Name, prep.Kind),
		}
	}
	return prep, nil
}

func validateNoPrepareInputs(req *models.PrepareVMProfileRequest, strategy models.VMProfilePreparationKind) error {
	if strings.TrimSpace(req.MediaPath) != "" ||
		strings.TrimSpace(req.VirtIOMediaPath) != "" ||
		strings.TrimSpace(req.ProductKey) != "" ||
		strings.TrimSpace(req.ProductKeyFile) != "" ||
		strings.TrimSpace(req.ActivationMode) != "" {
		return &apierr.ValidationError{
			Field:   "strategy",
			Message: fmt.Sprintf("%q profiles do not accept preparation media or licensing inputs", strategy),
		}
	}
	return nil
}

func prepareCommandHint(profile Profile) string {
	switch profilePreparation(profile).Kind {
	case models.VMProfilePreparationWindowsUnattended:
		return fmt.Sprintf("kiln-vm profiles prepare %s --media /path/to/windows.iso", profile.Name)
	default:
		return fmt.Sprintf("kiln-vm profiles prepare %s", profile.Name)
	}
}
