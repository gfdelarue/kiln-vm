package sdnotify

import (
	"fmt"
	"net"
	"os"
	"strings"
)

type Notifier struct {
	socket string
}

// FromEnvironment captures and removes systemd's notification socket from the
// process environment. Capture it before provider recovery so any child
// processes kiln-vm starts cannot notify systemd on the daemon's behalf.
func FromEnvironment() Notifier {
	socket, ok := os.LookupEnv("NOTIFY_SOCKET")
	if !ok || socket == "" {
		return Notifier{}
	}
	_ = os.Unsetenv("NOTIFY_SOCKET")
	return Notifier{socket: socket}
}

// Ready notifies systemd that kiln-vm completed startup. It is a no-op outside a
// notify-enabled unit.
func (n Notifier) Ready(status string) error {
	socket := n.socket
	if socket == "" {
		return nil
	}
	if strings.HasPrefix(socket, "@") {
		socket = "\x00" + strings.TrimPrefix(socket, "@")
	}
	address := &net.UnixAddr{Name: socket, Net: "unixgram"}
	conn, err := net.DialUnix("unixgram", nil, address)
	if err != nil {
		return fmt.Errorf("connect systemd notification socket: %w", err)
	}
	defer conn.Close()

	message := "READY=1"
	if status = strings.TrimSpace(strings.ReplaceAll(status, "\n", " ")); status != "" {
		message += "\nSTATUS=" + status
	}
	if _, err := conn.Write([]byte(message)); err != nil {
		return fmt.Errorf("send systemd readiness notification: %w", err)
	}
	return nil
}
