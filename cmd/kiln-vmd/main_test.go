package main

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/gfdelarue/kiln-vm/internal/config"
)

func TestRunReturnsConfigurationExitCodeBeforeCreatingState(t *testing.T) {
	isolateDaemonTransportEnv(t)
	t.Setenv("KILN_VM_LISTEN_ADDR", ":8080")
	dataDir := filepath.Join(t.TempDir(), "must-not-be-created")
	t.Setenv("KILN_VM_DATA_DIR", dataDir)
	setDaemonTestArgs(t)

	if got := run(); got != config.ExitCodeConfig {
		t.Fatalf("run exit code = %d, want %d", got, config.ExitCodeConfig)
	}
	if _, err := os.Stat(dataDir); !os.IsNotExist(err) {
		t.Fatalf("invalid transport created durable state at %s, stat error = %v", dataDir, err)
	}
}

func TestRunBindsManagementListenerBeforeCreatingState(t *testing.T) {
	isolateDaemonTransportEnv(t)
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("occupy management listener: %v", err)
	}
	defer occupied.Close()
	t.Setenv("KILN_VM_LISTEN_ADDR", occupied.Addr().String())
	dataDir := filepath.Join(t.TempDir(), "must-not-be-created")
	t.Setenv("KILN_VM_DATA_DIR", dataDir)
	setDaemonTestArgs(t)

	if got := run(); got != 1 {
		t.Fatalf("run exit code = %d, want listener failure code 1", got)
	}
	if _, err := os.Stat(dataDir); !os.IsNotExist(err) {
		t.Fatalf("listener failure created durable state at %s, stat error = %v", dataDir, err)
	}
}

func TestRunCheckConfigHasNoDurableOrListenerSideEffects(t *testing.T) {
	isolateDaemonTransportEnv(t)
	dataDir := filepath.Join(t.TempDir(), "must-not-be-created")
	t.Setenv("KILN_VM_DATA_DIR", dataDir)
	setDaemonTestArgs(t, "--check-config")

	if got := run(); got != 0 {
		t.Fatalf("valid config check exit code = %d, want 0", got)
	}
	if _, err := os.Stat(dataDir); !os.IsNotExist(err) {
		t.Fatalf("config check created durable state at %s, stat error = %v", dataDir, err)
	}

	t.Setenv("KILN_VM_LISTEN_ADDR", ":8080")
	if got := run(); got != config.ExitCodeConfig {
		t.Fatalf("unsafe config check exit code = %d, want %d", got, config.ExitCodeConfig)
	}
	if _, err := os.Stat(dataDir); !os.IsNotExist(err) {
		t.Fatalf("failed config check created durable state at %s, stat error = %v", dataDir, err)
	}
}

func TestRunACMEBindFailureReleasesTLSListenerBeforeCreatingState(t *testing.T) {
	isolateDaemonTransportEnv(t)
	occupiedACME, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("occupy ACME listener: %v", err)
	}
	defer occupiedACME.Close()

	tlsProbe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve TLS listener: %v", err)
	}
	tlsAddr := tlsProbe.Addr().String()
	if err := tlsProbe.Close(); err != nil {
		t.Fatalf("release TLS listener: %v", err)
	}

	t.Setenv("KILN_VM_TLS_ENABLED", "true")
	t.Setenv("KILN_VM_TLS_DOMAIN", "api.kiln-vm.test")
	t.Setenv("KILN_VM_TLS_CERT_DIR", t.TempDir())
	t.Setenv("KILN_VM_TLS_LISTEN_ADDR", tlsAddr)
	t.Setenv("KILN_VM_ACME_LISTEN_ADDR", occupiedACME.Addr().String())
	dataDir := filepath.Join(t.TempDir(), "must-not-be-created")
	t.Setenv("KILN_VM_DATA_DIR", dataDir)
	setDaemonTestArgs(t)

	if got := run(); got != 1 {
		t.Fatalf("ACME bind failure exit code = %d, want 1", got)
	}
	if _, err := os.Stat(dataDir); !os.IsNotExist(err) {
		t.Fatalf("ACME bind failure created durable state at %s, stat error = %v", dataDir, err)
	}
	rebound, err := net.Listen("tcp", tlsAddr)
	if err != nil {
		t.Fatalf("TLS listener remained bound after ACME failure: %v", err)
	}
	rebound.Close()
}

func isolateDaemonTransportEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"KILN_VM_LISTEN_ADDR",
		"KILN_VM_TLS_DOMAIN",
		"KILN_VM_TLS_CERT_DIR",
		"KILN_VM_TLS_LISTEN_ADDR",
		"KILN_VM_ACME_LISTEN_ADDR",
	} {
		t.Setenv(key, "")
	}
	t.Setenv("KILN_VM_ALLOW_INSECURE_REMOTE_HTTP", "false")
	t.Setenv("KILN_VM_TLS_ENABLED", "false")
}

func setDaemonTestArgs(t *testing.T, args ...string) {
	t.Helper()
	previous := os.Args
	os.Args = append([]string{"kiln-vmd"}, args...)
	t.Cleanup(func() { os.Args = previous })
}
