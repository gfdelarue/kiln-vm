package cli

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/gfdelarue/kiln-vm/internal/version"
)

var rootCmd = &cobra.Command{
	Use:     "kiln-vm",
	Short:   "Run persistent microVMs on your own Linux host",
	Version: version.Version,

	SilenceErrors: true,
	SilenceUsage:  true,
}

func Execute() int {
	err := rootCmd.Execute()
	if err == nil {
		return 0
	}
	var exitErr *ExitError
	if errors.As(err, &exitErr) {
		return exitErr.Code
	}
	fmt.Fprintln(os.Stderr, "Error:", err)
	return 1
}

func init() {
	rootCmd.AddCommand(loginCmd, tokensCmd, diskCmd, cleanupCmd, doctorCmd)
}
