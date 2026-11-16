package guestagent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"
)

const (
	ShellPort                     = 10701
	UploadPort                    = 10702
	ProtocolVersion               = 1
	defaultGuestOperationDeadline = 15 * time.Minute
)

type ShellFrame struct {
	Type     string `json:"type"`
	Version  int    `json:"version,omitempty"`
	Data     []byte `json:"data,omitempty"`
	Rows     uint16 `json:"rows,omitempty"`
	Cols     uint16 `json:"cols,omitempty"`
	ExitCode int    `json:"exit_code,omitempty"`
	Error    string `json:"error,omitempty"`
}

type UploadRequest struct {
	Version int    `json:"version"`
	Path    string `json:"path"`
	Mode    uint32 `json:"mode"`
	Size    int64  `json:"size"`
}

type UploadResponse struct {
	Version int    `json:"version"`
	Error   string `json:"error,omitempty"`
}

var ErrTransientTransport = errors.New("guest agent transient transport")

type TransientTransportError struct {
	Operation string
	Err       error
}

func (e *TransientTransportError) Error() string {
	if e == nil || e.Err == nil {
		return "guest agent transient transport"
	}
	if e.Operation == "" {
		return fmt.Sprintf("guest agent transient transport: %v", e.Err)
	}
	return fmt.Sprintf("guest agent transient transport during %s: %v", e.Operation, e.Err)
}

func (e *TransientTransportError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func (e *TransientTransportError) Is(target error) bool {
	return target == ErrTransientTransport
}

func closeOnContextCancel(ctx context.Context, conn net.Conn) func() {
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-done:
		}
	}()
	return func() {
		close(done)
	}
}

func DialShell(ctx context.Context, udsPath string) (net.Conn, error) {
	return dialGuest(ctx, udsPath, ShellPort)
}

func Upload(ctx context.Context, udsPath, remotePath string, mode uint32, size int64, reader io.Reader) error {
	conn, err := dialGuest(ctx, udsPath, UploadPort)
	if err != nil {
		return transientUploadTransportError(ctx, "dial", err)
	}
	defer conn.Close()
	done := closeOnContextCancel(ctx, conn)
	defer done()

	if deadline, ok := ctx.Deadline(); ok {
		conn.SetDeadline(deadline)
	} else {
		conn.SetDeadline(time.Now().Add(defaultGuestOperationDeadline))
	}

	req := UploadRequest{
		Version: ProtocolVersion,
		Path:    remotePath,
		Mode:    mode,
		Size:    size,
	}
	header, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("encode guest upload header: %w", err)
	}
	if _, err := fmt.Fprintf(conn, "%s\n", header); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("write guest upload header cancelled: %w", ctx.Err())
		}
		return transientUploadTransportError(ctx, "write header", err)
	}
	if _, err := io.CopyN(conn, reader, size); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("stream guest upload body cancelled: %w", ctx.Err())
		}
		return transientUploadTransportError(ctx, "stream body", err)
	}

	var resp UploadResponse
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("decode guest upload response cancelled: %w", ctx.Err())
		}
		return transientUploadTransportError(ctx, "read response", err)
	}
	if resp.Version != ProtocolVersion {
		return fmt.Errorf("guest agent protocol mismatch: got %d want %d", resp.Version, ProtocolVersion)
	}
	if resp.Error != "" {
		return fmt.Errorf("guest agent upload: %s", resp.Error)
	}
	return nil
}

func transientUploadTransportError(ctx context.Context, operation string, err error) error {
	if ctx.Err() != nil {
		return fmt.Errorf("%s cancelled: %w", operation, ctx.Err())
	}
	return &TransientTransportError{Operation: operation, Err: err}
}

func dialGuest(ctx context.Context, udsPath string, port int) (net.Conn, error) {
	dialer := net.Dialer{}
	conn, err := dialer.DialContext(ctx, "unix", udsPath)
	if err != nil {
		return nil, fmt.Errorf("dial firecracker vsock bridge: %w", err)
	}

	br, err := connectGuestPort(conn, port)
	if err != nil {
		conn.Close()
		return nil, err
	}

	return &bufferedConn{Conn: conn, reader: br}, nil
}

func connectGuestPort(conn net.Conn, port int) (*bufio.Reader, error) {
	if _, err := fmt.Fprintf(conn, "CONNECT %d\n", port); err != nil {
		return nil, fmt.Errorf("request guest vsock connection: %w", err)
	}

	br := bufio.NewReader(conn)
	ack, err := br.ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("read guest vsock ack: %w", err)
	}
	if !strings.HasPrefix(ack, "OK ") {
		return nil, fmt.Errorf("unexpected guest vsock ack: %q", strings.TrimSpace(ack))
	}
	return br, nil
}

type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) {
	n, err := c.reader.Read(p)
	if err != nil {
		return n, fmt.Errorf("read guest agent stream: %w", err)
	}
	return n, nil
}
