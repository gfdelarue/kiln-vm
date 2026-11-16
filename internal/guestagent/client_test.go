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
)

func TestUpload(t *testing.T) {
	dir := t.TempDir()
	udsPath := filepath.Join(dir, "vsock.sock")

	listener, err := net.Listen("unix", udsPath)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	defer listener.Close()

	bodyCh := make(chan string, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()

		buf := make([]byte, len("CONNECT 10702\n"))
		if _, err := conn.Read(buf); err != nil {
			return
		}
		if _, err := conn.Write([]byte("OK 12345\n")); err != nil {
			return
		}

		reader := bufio.NewReader(conn)
		header, err := reader.ReadBytes('\n')
		if err != nil {
			return
		}
		var req UploadRequest
		if err := json.Unmarshal(header, &req); err != nil {
			return
		}
		data := make([]byte, req.Size)
		if _, err := io.ReadFull(reader, data); err != nil {
			return
		}
		bodyCh <- string(data)
		_ = json.NewEncoder(conn).Encode(&UploadResponse{
			Version: ProtocolVersion,
		})
	}()

	err = Upload(context.Background(), udsPath, "/tmp/example.txt", 0644, int64(len("hello")), strings.NewReader("hello"))
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if got := <-bodyCh; got != "hello" {
		t.Fatalf("uploaded body = %q, want hello", got)
	}
}

func TestUploadResponseReadFailureIsTransientTransport(t *testing.T) {
	dir := t.TempDir()
	udsPath := filepath.Join(dir, "vsock.sock")

	listener, err := net.Listen("unix", udsPath)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	defer listener.Close()

	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()

		buf := make([]byte, len("CONNECT 10702\n"))
		if _, err := conn.Read(buf); err != nil {
			return
		}
		if _, err := conn.Write([]byte("OK 12345\n")); err != nil {
			return
		}

		reader := bufio.NewReader(conn)
		header, err := reader.ReadBytes('\n')
		if err != nil {
			return
		}
		var req UploadRequest
		if err := json.Unmarshal(header, &req); err != nil {
			return
		}
		_, _ = io.CopyN(io.Discard, reader, req.Size)
	}()

	err = Upload(context.Background(), udsPath, "/tmp/example.txt", 0644, int64(len("hello")), strings.NewReader("hello"))
	if !errors.Is(err, ErrTransientTransport) {
		t.Fatalf("Upload error = %v, want ErrTransientTransport", err)
	}
}

func TestUploadGuestErrorIsNotTransientTransport(t *testing.T) {
	dir := t.TempDir()
	udsPath := filepath.Join(dir, "vsock.sock")

	listener, err := net.Listen("unix", udsPath)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	defer listener.Close()

	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()

		buf := make([]byte, len("CONNECT 10702\n"))
		if _, err := conn.Read(buf); err != nil {
			return
		}
		if _, err := conn.Write([]byte("OK 12345\n")); err != nil {
			return
		}

		reader := bufio.NewReader(conn)
		header, err := reader.ReadBytes('\n')
		if err != nil {
			return
		}
		var req UploadRequest
		if err := json.Unmarshal(header, &req); err != nil {
			return
		}
		_, _ = io.CopyN(io.Discard, reader, req.Size)
		_ = json.NewEncoder(conn).Encode(&UploadResponse{
			Version: ProtocolVersion,
			Error:   "no space left on device",
		})
	}()

	err = Upload(context.Background(), udsPath, "/tmp/example.txt", 0644, int64(len("hello")), strings.NewReader("hello"))
	if err == nil {
		t.Fatal("Upload error = nil, want guest error")
	}
	if errors.Is(err, ErrTransientTransport) {
		t.Fatalf("Upload error = %v, must not be ErrTransientTransport", err)
	}
	if !strings.Contains(err.Error(), "no space left on device") {
		t.Fatalf("Upload error = %v, want guest error detail", err)
	}
}
