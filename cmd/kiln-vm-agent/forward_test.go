package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/gfdelarue/kiln-vm/internal/guestagent"
)

type forwardSession struct {
	conn   net.Conn
	reader *bufio.Reader
	done   chan struct{}
}

func startForwardSession(t *testing.T, request string) *forwardSession {
	t.Helper()
	client, agent := net.Pipe()
	done := make(chan struct{})
	go func() {
		handleForwardConn(agent)
		close(done)
	}()
	t.Cleanup(func() {
		client.Close()
		waitForwardDone(t, done)
	})
	if err := client.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}
	go func() { _, _ = io.WriteString(client, request) }()
	return &forwardSession{conn: client, reader: bufio.NewReader(client), done: done}
}

func (s *forwardSession) response(t *testing.T) guestagent.ForwardResponse {
	t.Helper()
	line, err := s.reader.ReadBytes('\n')
	if err != nil {
		t.Fatalf("read forward response: %v", err)
	}
	var resp guestagent.ForwardResponse
	if err := json.Unmarshal(line, &resp); err != nil {
		t.Fatalf("decode forward response %q: %v", line, err)
	}
	if resp.Version != guestagent.ProtocolVersion {
		t.Fatalf("response version = %d, want %d", resp.Version, guestagent.ProtocolVersion)
	}
	return resp
}

func waitForwardDone(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("forward handler did not return")
	}
}

func forwardRequest(port int) string {
	return fmt.Sprintf("{\"version\":1,\"port\":%d}\n", port)
}

func listenTCP(t *testing.T, network, address string) net.Listener {
	t.Helper()
	listener, err := net.Listen(network, address)
	if err != nil {
		t.Skipf("listen %s %s: %v", network, address, err)
	}
	t.Cleanup(func() { listener.Close() })
	return listener
}

func listenerPort(listener net.Listener) int {
	return listener.Addr().(*net.TCPAddr).Port
}

func serveOneEcho(listener net.Listener) <-chan error {
	result := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			result <- err
			return
		}
		defer conn.Close()
		_, err = io.Copy(conn, conn)
		result <- err
	}()
	return result
}

func TestHandleForwardConnSplicesToLoopbackTarget(t *testing.T) {
	target := listenTCP(t, "tcp4", "127.0.0.1:0")
	echoDone := serveOneEcho(target)

	session := startForwardSession(t, forwardRequest(listenerPort(target)))
	if resp := session.response(t); resp.Error != "" {
		t.Fatalf("forward error = %q", resp.Error)
	}
	if _, err := io.WriteString(session.conn, "hello"); err != nil {
		t.Fatalf("write: %v", err)
	}
	echo := make([]byte, len("hello"))
	if _, err := io.ReadFull(session.reader, echo); err != nil || string(echo) != "hello" {
		t.Fatalf("echo = %q, %v; want hello", echo, err)
	}

	session.conn.Close()
	waitForwardDone(t, session.done)
	select {
	case err := <-echoDone:
		if err != nil {
			t.Fatalf("target echo: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("target connection stayed open after client closed")
	}
}

func TestHandleForwardConnClosesClientWhenTargetCloses(t *testing.T) {
	target := listenTCP(t, "tcp4", "127.0.0.1:0")
	go func() {
		conn, err := target.Accept()
		if err != nil {
			return
		}
		_, _ = io.WriteString(conn, "bye")
		conn.Close()
	}()

	session := startForwardSession(t, forwardRequest(listenerPort(target)))
	if resp := session.response(t); resp.Error != "" {
		t.Fatalf("forward error = %q", resp.Error)
	}
	data, err := io.ReadAll(session.reader)
	if err != nil || string(data) != "bye" {
		t.Fatalf("read until close = %q, %v; want bye", data, err)
	}
	waitForwardDone(t, session.done)
}

func TestHandleForwardConnFallsBackToIPv6Loopback(t *testing.T) {
	target := listenTCP(t, "tcp6", "[::1]:0")
	port := listenerPort(target)
	if conn, err := net.DialTimeout("tcp4", fmt.Sprintf("127.0.0.1:%d", port), time.Second); err == nil {
		conn.Close()
		t.Skipf("127.0.0.1:%d is also bound", port)
	}
	serveOneEcho(target)

	session := startForwardSession(t, forwardRequest(port))
	if resp := session.response(t); resp.Error != "" {
		t.Fatalf("forward error = %q", resp.Error)
	}
	if _, err := io.WriteString(session.conn, "six"); err != nil {
		t.Fatalf("write: %v", err)
	}
	echo := make([]byte, len("six"))
	if _, err := io.ReadFull(session.reader, echo); err != nil || string(echo) != "six" {
		t.Fatalf("echo = %q, %v; want six", echo, err)
	}
}

func TestHandleForwardConnReportsRefusedPort(t *testing.T) {
	closed := listenTCP(t, "tcp4", "127.0.0.1:0")
	port := listenerPort(closed)
	closed.Close()

	session := startForwardSession(t, forwardRequest(port))
	resp := session.response(t)
	if !strings.HasPrefix(resp.Error, "connection refused on 127.0.0.1") {
		t.Fatalf("forward error = %q, want connection refused on 127.0.0.1", resp.Error)
	}
	waitForwardDone(t, session.done)
}

func TestHandleForwardConnRejectsInvalidRequests(t *testing.T) {
	tests := []struct {
		name    string
		request string
		want    string
	}{
		{name: "port zero", request: forwardRequest(0), want: "port must be between 1 and 65535"},
		{name: "port too large", request: forwardRequest(65536), want: "port must be between 1 and 65535"},
		{name: "version", request: "{\"version\":2,\"port\":80}\n", want: "protocol mismatch"},
		{name: "malformed", request: "not json\n", want: "decode forward request"},
		{name: "oversized", request: strings.Repeat("x", forwardRequestMaxBytes+1) + "\n", want: "read forward request"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			session := startForwardSession(t, test.request)
			resp := session.response(t)
			if !strings.Contains(resp.Error, test.want) {
				t.Fatalf("forward error = %q, want %q", resp.Error, test.want)
			}
			waitForwardDone(t, session.done)
		})
	}
}
