package api

import (
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/gorilla/websocket"

	"github.com/gfdelarue/kiln-vm/internal/apierr"
)

type forwardCall struct {
	id   string
	port int
}

func startVMForwardServer(t *testing.T, forward func(id string, port int) (io.ReadWriteCloser, error)) (string, <-chan forwardCall) {
	t.Helper()
	svc, machines := setupVMUploadTestService(t)
	calls := make(chan forwardCall, 1)
	machines.forward = func(id string, port int) (io.ReadWriteCloser, error) {
		calls <- forwardCall{id: id, port: port}
		return forward(id, port)
	}
	server := httptest.NewServer(setupVMRouter(svc))
	t.Cleanup(server.Close)
	return "ws" + strings.TrimPrefix(server.URL, "http"), calls
}

func dialVMForward(t *testing.T, url string) *websocket.Conn {
	t.Helper()
	conn, resp, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		t.Fatalf("dial %s: %v (status %d)", url, err, status)
	}
	t.Cleanup(func() { conn.Close() })
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	return conn
}

func TestVMForwardRejectsInvalidPortBeforeUpgrade(t *testing.T) {
	svc, machines := setupVMUploadTestService(t)
	machines.forward = func(string, int) (io.ReadWriteCloser, error) {
		t.Fatal("DialForward called for invalid port")
		return nil, nil
	}
	router := setupVMRouter(svc)
	for _, port := range []string{"0", "65536", "-1", "http"} {
		req := httptest.NewRequest(http.MethodGet, "/vms/ready/forward/"+port, nil)
		req.Header.Set("Connection", "Upgrade")
		req.Header.Set("Upgrade", "websocket")
		req.Header.Set("Sec-WebSocket-Version", "13")
		req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
		w := httptest.NewRecorder()

		router.ServeHTTP(w, req)

		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "port must be an integer between 1 and 65535") {
			t.Fatalf("port %q: status = %d body = %s, want 400 port validation", port, w.Code, w.Body.String())
		}
	}
}

func TestVMForwardSplicesWebSocketToGuestConnection(t *testing.T) {
	guestClosed := make(chan struct{})
	url, calls := startVMForwardServer(t, func(string, int) (io.ReadWriteCloser, error) {
		host, guest := net.Pipe()
		go func() {
			defer close(guestClosed)
			buf := make([]byte, 64)
			for {
				n, err := guest.Read(buf)
				if err != nil {
					return
				}
				if _, err := guest.Write(append([]byte("echo:"), buf[:n]...)); err != nil {
					return
				}
			}
		}()
		return host, nil
	})

	conn := dialVMForward(t, url+"/vms/ready/forward/3000")
	if err := conn.WriteMessage(websocket.BinaryMessage, []byte("hello")); err != nil {
		t.Fatalf("write: %v", err)
	}
	messageType, data, err := conn.ReadMessage()
	if err != nil || messageType != websocket.BinaryMessage || string(data) != "echo:hello" {
		t.Fatalf("read = %d %q %v, want binary echo:hello", messageType, data, err)
	}
	if call := <-calls; call.id != "machine-vm-ready" || call.port != 3000 {
		t.Fatalf("DialForward call = %+v, want machine-vm-ready port 3000", call)
	}

	if err := conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "")); err != nil {
		t.Fatalf("write close: %v", err)
	}
	select {
	case <-guestClosed:
	case <-time.After(5 * time.Second):
		t.Fatal("guest connection stayed open after the WebSocket closed")
	}
}

func TestVMForwardClosesWebSocketWhenGuestEnds(t *testing.T) {
	url, _ := startVMForwardServer(t, func(string, int) (io.ReadWriteCloser, error) {
		host, guest := net.Pipe()
		go func() {
			_, _ = guest.Write([]byte("bye"))
			guest.Close()
		}()
		return host, nil
	})

	conn := dialVMForward(t, url+"/vms/ready/forward/8080")
	if _, data, err := conn.ReadMessage(); err != nil || string(data) != "bye" {
		t.Fatalf("read = %q %v, want bye", data, err)
	}
	_, _, err := conn.ReadMessage()
	var closeErr *websocket.CloseError
	if !errors.As(err, &closeErr) || closeErr.Code != websocket.CloseNormalClosure {
		t.Fatalf("read after guest end = %v, want normal closure", err)
	}
}

func TestVMForwardClosesWebSocketWithPublicErrorWhenDialFails(t *testing.T) {
	url, _ := startVMForwardServer(t, func(string, int) (io.ReadWriteCloser, error) {
		return nil, &apierr.ValidationError{Field: "port", Message: "guest port 3000: connection refused on 127.0.0.1 and [::1]"}
	})

	conn := dialVMForward(t, url+"/vms/ready/forward/3000")
	_, _, err := conn.ReadMessage()
	var closeErr *websocket.CloseError
	if !errors.As(err, &closeErr) || closeErr.Code != websocket.CloseInternalServerErr || !strings.Contains(closeErr.Text, "guest port 3000: connection refused on 127.0.0.1 and [::1]") {
		t.Fatalf("read after failed dial = %v, want internal-error close naming the refusal", err)
	}
}

func TestWebSocketCloseReasonKeepsValidUTF8Tail(t *testing.T) {
	reason := strings.Repeat("é", 100) + " root cause"
	got := websocketCloseReason(reason)
	if len(got) > websocketCloseReasonMax || !utf8.ValidString(got) || !strings.HasPrefix(got, "...") || !strings.HasSuffix(got, " root cause") {
		t.Fatalf("websocketCloseReason = %q (%d bytes)", got, len(got))
	}
	if short := "vm not found"; websocketCloseReason(short) != short {
		t.Fatalf("short reason changed to %q", websocketCloseReason(short))
	}
}
