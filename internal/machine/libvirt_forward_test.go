package machine

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/gfdelarue/kiln-vm/internal/apierr"
	"github.com/gfdelarue/kiln-vm/internal/models"
)

type directTCPIPRequest struct {
	Host       string
	Port       uint32
	OriginHost string
	OriginPort uint32
}

func TestDialSSHForwardTunnelsToTheGuestLoopback(t *testing.T) {
	requests := make(chan directTCPIPRequest, 1)
	client := startForwardSSHServer(t, func(req directTCPIPRequest, channel ssh.Channel) {
		requests <- req
		_, _ = channel.Write([]byte("hello"))
	})

	conn, err := dialSSHForward(context.Background(), client, 3389)
	if err != nil {
		t.Fatalf("dialSSHForward: %v", err)
	}
	got, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "hello" {
		t.Fatalf("read %q, want hello", got)
	}
	if req := <-requests; req.Host != "127.0.0.1" || req.Port != 3389 {
		t.Fatalf("direct-tcpip target = %s:%d, want 127.0.0.1:3389", req.Host, req.Port)
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, _, err := client.SendRequest("keepalive@kiln-vm", true, nil); err == nil {
		t.Fatal("closing the forward left the SSH client open")
	}
}

func TestDialSSHForwardReportsARefusedGuestPort(t *testing.T) {
	client := startForwardSSHServer(t, nil)

	conn, err := dialSSHForward(context.Background(), client, 8080)
	if conn != nil {
		conn.Close()
	}
	if !isValidationError(err) {
		t.Fatalf("dialSSHForward error = %v, want a validation error", err)
	}
	if _, _, err := client.SendRequest("keepalive@kiln-vm", true, nil); err == nil {
		t.Fatal("a failed forward left the SSH client open")
	}
}

func TestDialLibvirtForwardRejectsBadTargets(t *testing.T) {
	m := &Manager{}
	for _, tc := range []struct {
		name    string
		machine *models.Machine
		port    int
		want    func(error) bool
	}{
		{name: "port out of range", machine: &models.Machine{IP: "127.0.0.1"}, port: 70000, want: isValidationError},
		{name: "no IP", machine: &models.Machine{}, port: 80, want: func(err error) bool { return errors.Is(err, apierr.ErrConflict) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn, err := m.dialLibvirtForward(context.Background(), tc.machine, tc.port)
			if conn != nil {
				conn.Close()
			}
			if !tc.want(err) {
				t.Fatalf("dialLibvirtForward error = %v", err)
			}
		})
	}
}

func startForwardSSHServer(t *testing.T, accept func(directTCPIPRequest, ssh.Channel)) *ssh.Client {
	t.Helper()
	_, hostKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(hostKey)
	if err != nil {
		t.Fatal(err)
	}
	config := &ssh.ServerConfig{NoClientAuth: true}
	config.AddHostKey(signer)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		_, chans, reqs, err := ssh.NewServerConn(conn, config)
		if err != nil {
			conn.Close()
			return
		}
		go ssh.DiscardRequests(reqs)
		for newChannel := range chans {
			var req directTCPIPRequest
			if newChannel.ChannelType() != "direct-tcpip" || ssh.Unmarshal(newChannel.ExtraData(), &req) != nil || accept == nil {
				_ = newChannel.Reject(ssh.ConnectionFailed, "connection refused")
				continue
			}
			channel, channelReqs, err := newChannel.Accept()
			if err != nil {
				continue
			}
			go ssh.DiscardRequests(channelReqs)
			accept(req, channel)
			channel.Close()
		}
	}()

	client, err := ssh.Dial("tcp", listener.Addr().String(), &ssh.ClientConfig{
		User:            "kiln-vm",
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	})
	if err != nil {
		t.Fatalf("dial test ssh server: %v", err)
	}
	t.Cleanup(func() { client.Close() })
	return client
}

func isValidationError(err error) bool {
	var validation *apierr.ValidationError
	return errors.As(err, &validation)
}
