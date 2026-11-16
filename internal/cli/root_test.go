package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/gfdelarue/kiln-vm/internal/version"
)

func TestRootVersionFlagPrintsTheVersion(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	rootCmd.SetArgs([]string{"--version"})
	rootCmd.SetOut(&stdout)
	rootCmd.SetErr(&stderr)
	t.Cleanup(func() {
		rootCmd.SetArgs(nil)
		rootCmd.SetOut(nil)
		rootCmd.SetErr(nil)
	})

	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("Execute --version: %v", err)
	}

	output := stdout.String()
	if stderr.String() != "" {
		t.Fatalf("unexpected stderr: %q", stderr.String())
	}
	if !strings.Contains(output, "kiln-vm version "+version.Version) {
		t.Fatalf("version output = %q, want kiln-vm version %s", output, version.Version)
	}
}
