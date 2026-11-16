package cli

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gorilla/websocket"
)

func TestOpenShellRejectsRemotePlaintextBeforeBearerDial(t *testing.T) {
	dialer := &recordingShellDialer{err: errors.New("dial should not be reached")}
	client := &Client{
		BaseURL: "http://192.0.2.10:8080",
		Token:   "top-secret-token",
	}

	err := openShellWithDialer(client, "/machines/example/shell", dialer)
	if err == nil || !strings.Contains(err.Error(), "refusing plaintext HTTP") {
		t.Fatalf("openShell error = %v, want plaintext transport rejection", err)
	}
	if dialer.calls != 0 {
		t.Fatalf("WebSocket dialer received %d calls after policy rejection, want 0", dialer.calls)
	}
}

func TestOpenShellExplicitRemotePlaintextOptInCarriesBearer(t *testing.T) {
	dialErr := errors.New("synthetic dial stop")
	dialer := &recordingShellDialer{err: dialErr}
	client, err := newClient("http://192.0.2.10:8080", "explicit-token", true)
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}

	err = openShellWithDialer(client, "/machines/example/shell", dialer)
	if !errors.Is(err, dialErr) {
		t.Fatalf("openShell error = %v, want synthetic dial error", err)
	}
	if dialer.calls != 1 {
		t.Fatalf("WebSocket dialer calls = %d, want 1", dialer.calls)
	}
	if dialer.url != "ws://192.0.2.10:8080/machines/example/shell" {
		t.Fatalf("WebSocket URL = %q", dialer.url)
	}
	if got := dialer.header.Get("Authorization"); got != "Bearer explicit-token" {
		t.Fatalf("Authorization = %q, want explicit bearer token", got)
	}
}

func TestManagementWebSocketDialerBypassesHTTPProxyForLoopback(t *testing.T) {
	const helperKey = "KILN_VM_TEST_WEBSOCKET_PROXY_HELPER"
	if os.Getenv(helperKey) == "1" {
		header := http.Header{"Authorization": []string{"Bearer websocket-proxy-bypass-token"}}
		connection, _, err := managementWebSocketDialer().Dial(os.Getenv("KILN_VM_TEST_WEBSOCKET_TARGET"), header)
		if err != nil {
			t.Fatalf("dial loopback WebSocket: %v", err)
		}
		if err := connection.Close(); err != nil {
			t.Fatalf("close loopback WebSocket: %v", err)
		}
		return
	}

	var targetRequests atomic.Int32
	var targetAuthorization atomic.Value
	upgrader := websocket.Upgrader{}
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetRequests.Add(1)
		targetAuthorization.Store(r.Header.Get("Authorization"))
		connection, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade WebSocket: %v", err)
			return
		}
		_ = connection.Close()
	}))
	defer target.Close()
	targetURL, err := url.Parse(target.URL)
	if err != nil {
		t.Fatalf("parse WebSocket target URL: %v", err)
	}

	var proxyRequests atomic.Int32
	var proxyAuthorization atomic.Value
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyRequests.Add(1)
		proxyAuthorization.Store(r.Header.Get("Authorization"))
		http.Error(w, "loopback WebSocket reached proxy", http.StatusBadGateway)
	}))
	defer proxy.Close()

	cmd := exec.Command(os.Args[0], "-test.run=^TestManagementWebSocketDialerBypassesHTTPProxyForLoopback$")
	cmd.Env = subprocessEnvironment(map[string]string{
		helperKey:                       "1",
		"KILN_VM_TEST_WEBSOCKET_TARGET": "ws://LOCALHOST.:" + targetURL.Port() + "/shell",
		"HTTP_PROXY":                    proxy.URL,
		"http_proxy":                    proxy.URL,
		"HTTPS_PROXY":                   "",
		"https_proxy":                   "",
		"ALL_PROXY":                     "",
		"all_proxy":                     "",
		"NO_PROXY":                      "",
		"no_proxy":                      "",
	}, "REQUEST_METHOD")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("WebSocket proxy-bypass helper failed: %v\n%s", err, output)
	}
	if got := proxyRequests.Load(); got != 0 {
		authorization, _ := proxyAuthorization.Load().(string)
		t.Fatalf("proxy received %d loopback WebSocket requests with Authorization %q", got, authorization)
	}
	if got := targetRequests.Load(); got != 1 {
		t.Fatalf("loopback WebSocket target received %d requests, want 1", got)
	}
	if authorization, _ := targetAuthorization.Load().(string); authorization != "Bearer websocket-proxy-bypass-token" {
		t.Fatalf("WebSocket target Authorization = %q, want bearer token", authorization)
	}
}

type recordingShellDialer struct {
	calls  int
	url    string
	header http.Header
	err    error
}

func (d *recordingShellDialer) Dial(urlStr string, header http.Header) (*websocket.Conn, *http.Response, error) {
	d.calls++
	d.url = urlStr
	d.header = header.Clone()
	return nil, nil, d.err
}
