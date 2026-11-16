package cli

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, _ = b.buf.Write(p)
	return len(p), nil
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type fakeForwardAPI struct {
	server   *httptest.Server
	upgrades atomic.Int32
	requests chan *http.Request
}

func newFakeForwardAPI(t *testing.T, session func(w http.ResponseWriter, r *http.Request, upgrade func() *websocket.Conn)) *fakeForwardAPI {
	t.Helper()
	api := &fakeForwardAPI{requests: make(chan *http.Request, 16)}
	upgrader := websocket.Upgrader{}
	api.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		api.requests <- r
		session(w, r, func() *websocket.Conn {
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				t.Errorf("upgrade: %v", err)
				return nil
			}
			api.upgrades.Add(1)
			return conn
		})
	}))
	t.Cleanup(api.server.Close)
	return api
}

func echoForwardSession(_ http.ResponseWriter, _ *http.Request, upgrade func() *websocket.Conn) {
	conn := upgrade()
	if conn == nil {
		return
	}
	defer conn.Close()
	for {
		messageType, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		if messageType != websocket.BinaryMessage {
			_ = conn.WriteMessage(websocket.BinaryMessage, []byte("unexpected message type"))
			return
		}
		if err := conn.WriteMessage(websocket.BinaryMessage, append([]byte("echo:"), data...)); err != nil {
			return
		}
	}
}

type runningForward struct {
	addr   string
	stderr *lockedBuffer
	cancel context.CancelFunc
	result chan error
}

func startVMForward(t *testing.T, api *fakeForwardAPI, ref string, port int) *runningForward {
	t.Helper()
	client, err := newClient(api.server.URL, "forward-token", false)
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	stdoutReader, stdoutWriter := io.Pipe()
	forward := &runningForward{stderr: &lockedBuffer{}, cancel: cancel, result: make(chan error, 1)}
	go func() {
		forward.result <- runVMForward(ctx, vmForwardOptions{
			client: client,
			ref:    ref,
			port:   port,
			listen: "127.0.0.1:0",
			dialer: websocket.DefaultDialer,
			stdout: stdoutWriter,
			stderr: forward.stderr,
		})
		stdoutWriter.Close()
	}()
	t.Cleanup(func() {
		cancel()
		forward.wait(t)
	})

	lines := bufio.NewReader(stdoutReader)
	line, err := lines.ReadString('\n')
	if err != nil {
		t.Fatalf("read forward banner: %v", err)
	}
	go func() { _, _ = io.Copy(io.Discard, lines) }()
	fields := strings.Fields(line)
	if len(fields) != 7 {
		t.Fatalf("banner = %q", line)
	}
	if want := fmt.Sprintf("forwarding %s to port %d in %s\n", fields[1], port, ref); line != want {
		t.Fatalf("banner = %q, want %q", line, want)
	}
	if host, _, err := net.SplitHostPort(fields[1]); err != nil || host != "127.0.0.1" || strings.HasSuffix(fields[1], ":0") {
		t.Fatalf("banner address = %q, want the bound loopback address", fields[1])
	}
	forward.addr = fields[1]
	return forward
}

func (f *runningForward) wait(t *testing.T) error {
	t.Helper()
	select {
	case err := <-f.result:
		f.result <- err
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("runVMForward did not return after cancellation")
		return nil
	}
}

func (f *runningForward) dial(t *testing.T) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("tcp", f.addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial forward listener: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}
	return conn
}

func roundTrip(t *testing.T, conn net.Conn, message string) {
	t.Helper()
	if _, err := io.WriteString(conn, message); err != nil {
		t.Fatalf("write %q: %v", message, err)
	}
	want := "echo:" + message
	got := make([]byte, len(want))
	if _, err := io.ReadFull(conn, got); err != nil || string(got) != want {
		t.Fatalf("read = %q, %v; want %q", got, err, want)
	}
}

func TestVMForwardTunnelsEachConnectionOverItsOwnWebSocket(t *testing.T) {
	api := newFakeForwardAPI(t, echoForwardSession)
	forward := startVMForward(t, api, "web", 3000)

	first := forward.dial(t)
	second := forward.dial(t)
	roundTrip(t, first, "one")
	roundTrip(t, second, "two")
	roundTrip(t, first, "three")

	if got := api.upgrades.Load(); got != 2 {
		t.Fatalf("WebSocket upgrades = %d, want one per TCP connection", got)
	}
	for range 2 {
		request := <-api.requests
		if request.URL.Path != "/vms/web/forward/3000" {
			t.Fatalf("request path = %q, want /vms/web/forward/3000", request.URL.Path)
		}
		if got := request.Header.Get("Authorization"); got != "Bearer forward-token" {
			t.Fatalf("Authorization = %q, want bearer token", got)
		}
	}

	forward.cancel()
	if err := forward.wait(t); err != nil {
		t.Fatalf("runVMForward after cancellation = %v, want nil", err)
	}
	if conn, err := net.DialTimeout("tcp", forward.addr, time.Second); err == nil {
		conn.Close()
		t.Fatal("listener still accepts connections after cancellation")
	}
	var netErr net.Error
	if _, err := first.Read(make([]byte, 1)); err == nil || errors.As(err, &netErr) && netErr.Timeout() {
		t.Fatalf("read on open TCP connection after cancellation = %v, want it closed", err)
	}
}

func TestVMForwardClosesWebSocketWhenTCPClientCloses(t *testing.T) {
	serverSaw := make(chan error, 1)
	api := newFakeForwardAPI(t, func(_ http.ResponseWriter, _ *http.Request, upgrade func() *websocket.Conn) {
		conn := upgrade()
		if conn == nil {
			return
		}
		defer conn.Close()
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, _, err := conn.ReadMessage()
		serverSaw <- err
	})
	forward := startVMForward(t, api, "web", 3000)

	forward.dial(t).Close()

	select {
	case err := <-serverSaw:
		var closeErr *websocket.CloseError
		if !errors.As(err, &closeErr) || closeErr.Code != websocket.CloseNormalClosure {
			t.Fatalf("server read after client close = %v, want normal closure", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not see the WebSocket close")
	}
}

func TestVMForwardClosesTCPConnectionWhenWebSocketCloses(t *testing.T) {
	api := newFakeForwardAPI(t, func(_ http.ResponseWriter, _ *http.Request, upgrade func() *websocket.Conn) {
		conn := upgrade()
		if conn == nil {
			return
		}
		defer conn.Close()
		_ = conn.WriteMessage(websocket.BinaryMessage, []byte("bye"))
		_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
	})
	forward := startVMForward(t, api, "web", 3000)

	data, err := io.ReadAll(forward.dial(t))
	if err != nil || string(data) != "bye" {
		t.Fatalf("read until close = %q, %v; want bye", data, err)
	}
	if got := forward.stderr.String(); got != "" {
		t.Fatalf("stderr after normal closure = %q, want empty", got)
	}
}

func TestVMForwardKeepsServingAfterFailedDials(t *testing.T) {
	var calls atomic.Int32
	api := newFakeForwardAPI(t, func(w http.ResponseWriter, r *http.Request, upgrade func() *websocket.Conn) {
		switch calls.Add(1) {
		case 1:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":"vm not found"}`)
		case 2:
			conn := upgrade()
			if conn == nil {
				return
			}
			defer conn.Close()
			_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseInternalServerErr, "guest port 3000: connection refused on 127.0.0.1 and [::1]"), time.Now().Add(time.Second))
		default:
			echoForwardSession(w, r, upgrade)
		}
	})
	forward := startVMForward(t, api, "web", 3000)

	for range 2 {
		if data, err := io.ReadAll(forward.dial(t)); err != nil || len(data) != 0 {
			t.Fatalf("failed forward read = %q, %v; want immediate EOF", data, err)
		}
	}
	roundTrip(t, forward.dial(t), "still serving")

	stderr := forward.stderr.String()
	for _, want := range []string{"API error (404): vm not found", "VM forward closed: guest port 3000: connection refused on 127.0.0.1 and [::1]"} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("stderr = %q, want %q", stderr, want)
		}
	}
}

func TestVMForwardValidatesBeforeListening(t *testing.T) {
	dialer := &recordingForwardDialer{}
	tests := []struct {
		name   string
		client *Client
		port   int
		want   string
	}{
		{name: "port zero", client: &Client{BaseURL: "http://127.0.0.1:8080"}, port: 0, want: "port must be an integer between 1 and 65535"},
		{name: "port too large", client: &Client{BaseURL: "http://127.0.0.1:8080"}, port: 65536, want: "port must be an integer between 1 and 65535"},
		{name: "remote plaintext", client: &Client{BaseURL: "http://192.0.2.10:8080", Token: "secret"}, port: 3000, want: "refusing plaintext HTTP"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stdout bytes.Buffer
			err := runVMForward(context.Background(), vmForwardOptions{
				client: test.client,
				ref:    "web",
				port:   test.port,
				listen: "127.0.0.1:0",
				dialer: dialer,
				stdout: &stdout,
				stderr: io.Discard,
			})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("runVMForward error = %v, want %q", err, test.want)
			}
			if stdout.Len() != 0 || dialer.calls.Load() != 0 {
				t.Fatalf("validation failure printed %q and dialed %d times", stdout.String(), dialer.calls.Load())
			}
		})
	}
}

func TestParseVMForwardPort(t *testing.T) {
	if port, err := parseVMForwardPort("3773"); err != nil || port != 3773 {
		t.Fatalf("parseVMForwardPort(3773) = %d, %v", port, err)
	}
	for _, value := range []string{"0", "65536", "-1", "http", ""} {
		if _, err := parseVMForwardPort(value); err == nil {
			t.Fatalf("parseVMForwardPort(%q) succeeded", value)
		}
	}
}

func TestVMForwardReportsListenFailure(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer occupied.Close()

	var stdout bytes.Buffer
	err = runVMForward(context.Background(), vmForwardOptions{
		client: &Client{BaseURL: "http://127.0.0.1:8080"},
		ref:    "web",
		port:   3000,
		listen: occupied.Addr().String(),
		dialer: &recordingForwardDialer{},
		stdout: &stdout,
		stderr: io.Discard,
	})
	if err == nil || !strings.Contains(err.Error(), "listen on "+occupied.Addr().String()) {
		t.Fatalf("runVMForward error = %v, want listen failure", err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("listen failure printed %q", stdout.String())
	}
}

type recordingForwardDialer struct {
	calls atomic.Int32
}

func (d *recordingForwardDialer) DialContext(context.Context, string, http.Header) (*websocket.Conn, *http.Response, error) {
	d.calls.Add(1)
	return nil, nil, errors.New("dial should not be reached")
}
