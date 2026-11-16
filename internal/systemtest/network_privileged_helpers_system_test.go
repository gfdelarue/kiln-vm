//go:build system && privileged

package systemtest

import "errors"

type commandExitErrorForTest interface {
	error
	ExitCode() int
}

func commandExitedWithCodeForTest(err error, code int) bool {
	var exitErr commandExitErrorForTest
	return errors.As(err, &exitErr) && exitErr.ExitCode() == code
}
