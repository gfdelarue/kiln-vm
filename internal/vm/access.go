package vm

import (
	"fmt"

	"github.com/gfdelarue/kiln-vm/internal/apierr"
	"github.com/gfdelarue/kiln-vm/internal/models"
)

const (
	vmAccessOperationExec    = "exec"
	vmAccessOperationUpload  = "upload"
	vmAccessOperationShell   = "shell"
	vmAccessOperationForward = "forward"
)

func (s *Service) ensureProfileAccessOperation(record *models.VMRecord, operation string) error {
	profile, err := getProfile(record.Profile)
	if err != nil {
		return err
	}
	if profileAccessSupported(profileAccess(profile)) {
		return nil
	}
	return unsupportedProfileAccessError(profile, operation)
}

func profileAccessSupported(access models.VMProfileAccess) bool {
	switch access.Kind {
	case models.VMProfileAccessGuestAgent, models.VMProfileAccessSSH:
		return true
	default:
		return false
	}
}

func unsupportedProfileAccessError(profile Profile, operation string) error {
	access := profileAccess(profile)
	return &apierr.ValidationError{
		Field:   "profile.access",
		Message: fmt.Sprintf("profile %q access %q does not support %s", profile.Name, access.Kind, operation),
	}
}
