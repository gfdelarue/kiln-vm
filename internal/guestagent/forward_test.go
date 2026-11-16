package guestagent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeForwardBridge struct {
	udsPath  string
	requests chan ForwardRequest
}

func newFakeForwardBridge(t *testing.T, serve func(conn net.Conn, reader *bufio.Reader)) *fakeForwardBridge {
	t.Helper()
	udsPath := filepath.Join(t.TempDir(), "vsock.sock")
	listener, err := net.Listen("unix", udsPath)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	t.Cleanup(func() { listener.Close() })
	bridge := &fakeForwardBridge{udsPath: udsPath, requests: make(chan ForwardRequest, 1)}
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		reader := bufio.NewReader(conn)
		connect, err := reader.ReadString('\n')
		if err != nil || connect != "CONNECT 10707\n" {
			return
		}
		serve(conn, reader)
	}()
	return bridge
}

func (b *fakeForwardBridge) acceptRequest(conn net.Conn, reader *bufio.Reader) bool {
	if _, err := conn.Write([]byte("OK 52000\n")); err != nil {
		return false
	}
	line, err := reader.ReadBytes('\n')
	if err != nil {
		return false
	}
	var request ForwardRequest
	if err := json.Unmarshal(line, &request); err != nil {
		return false
	}
	b.requests <- request
	return true
}

func TestDialForwardSplicesBytesAfterReply(t *testing.T) {
	var bridge *fakeForwardBridge
	bridge = newFakeForwardBridge(t, func(conn net.Conn, reader *bufio.Reader) {
		if !bridge.acceptRequest(conn, reader) {
			return
		}
		if _, err := conn.Write([]byte("{\"version\":1}\nearly")); err != nil {
			return
		}
		buf := make([]byte, 4)
		if _, err := io.ReadFull(reader, buf); err != nil {
			return
		}
		_, _ = conn.Write(buf)
	})

	conn, err := DialForward(context.Background(), bridge.udsPath, 3000)
	if err != nil {
		t.Fatalf("DialForward: %v", err)
	}
	defer conn.Close()

	request := <-bridge.requests
	if request.Version != ProtocolVersion || request.Port != 3000 {
		t.Fatalf("forward request = %+v, want version %d port 3000", request, ProtocolVersion)
	}
	early := make([]byte, len("early"))
	if _, err := io.ReadFull(conn, early); err != nil || string(early) != "early" {
		t.Fatalf("read bytes buffered after reply = %q, %v; want early", early, err)
	}
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatalf("write: %v", err)
	}
	echo := make([]byte, 4)
	if _, err := io.ReadFull(conn, echo); err != nil || string(echo) != "ping" {
		t.Fatalf("echo = %q, %v; want ping", echo, err)
	}
	if n, err := conn.Read(make([]byte, 1)); n != 0 || err != io.EOF {
		t.Fatalf("read after guest close = %d, %v; want 0, io.EOF", n, err)
	}
}

func TestDialForwardReturnsGuestError(t *testing.T) {
	var bridge *fakeForwardBridge
	bridge = newFakeForwardBridge(t, func(conn net.Conn, reader *bufio.Reader) {
		if !bridge.acceptRequest(conn, reader) {
			return
		}
		_ = json.NewEncoder(conn).Encode(ForwardResponse{Version: ProtocolVersion, Error: "connection refused on 127.0.0.1 and [::1]"})
	})

	_, err := DialForward(context.Background(), bridge.udsPath, 3000)
	var targetErr *ForwardTargetError
	if !errors.As(err, &targetErr) || targetErr.Message != "connection refused on 127.0.0.1 and [::1]" {
		t.Fatalf("DialForward error = %v, want guest target error", err)
	}
}

func TestDialForwardFailsWhenGuestHasNoForwardListener(t *testing.T) {
	bridge := newFakeForwardBridge(t, func(net.Conn, *bufio.Reader) {})

	_, err := DialForward(context.Background(), bridge.udsPath, 3000)
	if err == nil || !strings.Contains(err.Error(), "read guest vsock ack") {
		t.Fatalf("DialForward error = %v, want vsock ack failure", err)
	}
}

func TestDialForwardRejectsInvalidPortBeforeDialing(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.sock")
	for _, port := range []int{-1, 0, 65536} {
		_, err := DialForward(context.Background(), missing, port)
		if err == nil || !strings.Contains(err.Error(), "outside 1-65535") {
			t.Fatalf("DialForward(port %d) error = %v, want range error", port, err)
		}
	}
}

func TestDialForwardHonorsCancellationDuringHandshake(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	bridge := newFakeForwardBridge(t, func(net.Conn, *bufio.Reader) {
		<-release
	})

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	started := time.Now()
	_, err := DialForward(ctx, bridge.udsPath, 3000)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("DialForward error = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("DialForward took %s after cancellation", elapsed)
	}
}
