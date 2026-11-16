package sdnotify

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReadyIsNoopWithoutNotifySocket(t *testing.T) {
	unsetEnv(t, "NOTIFY_SOCKET")
	if err := FromEnvironment().Ready("ready"); err != nil {
		t.Fatalf("Ready without systemd: %v", err)
	}
}

func TestReadySendsStatusAndRemovesSocketFromEnvironment(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "notify.sock")
	listener, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: socketPath, Net: "unixgram"})
	if err != nil {
		t.Fatalf("listen notification socket: %v", err)
	}
	defer listener.Close()
	t.Setenv("NOTIFY_SOCKET", socketPath)

	notifier := FromEnvironment()
	if _, ok := os.LookupEnv("NOTIFY_SOCKET"); ok {
		t.Fatal("FromEnvironment retained NOTIFY_SOCKET in the daemon environment")
	}
	if err := notifier.Ready("kiln-vm management\ntransport ready"); err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if err := listener.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	buffer := make([]byte, 256)
	n, _, err := listener.ReadFromUnix(buffer)
	if err != nil {
		t.Fatalf("read notification: %v", err)
	}
	message := string(buffer[:n])
	if message != "READY=1\nSTATUS=kiln-vm management transport ready" {
		t.Fatalf("notification = %q", message)
	}
}

func TestReadyRejectsUnavailableNotifySocket(t *testing.T) {
	t.Setenv("NOTIFY_SOCKET", filepath.Join(t.TempDir(), "missing.sock"))
	notifier := FromEnvironment()
	if _, ok := os.LookupEnv("NOTIFY_SOCKET"); ok {
		t.Fatal("FromEnvironment retained NOTIFY_SOCKET in the daemon environment")
	}
	err := notifier.Ready("ready")
	if err == nil || !strings.Contains(err.Error(), "connect systemd notification socket") {
		t.Fatalf("Ready error = %v", err)
	}
}

func unsetEnv(t *testing.T, key string) {
	t.Helper()
	previous, existed := os.LookupEnv(key)
	if err := os.Unsetenv(key); err != nil {
		t.Fatalf("unset %s: %v", key, err)
	}
	t.Cleanup(func() {
		if existed {
			_ = os.Setenv(key, previous)
		} else {
			_ = os.Unsetenv(key)
		}
	})
}
