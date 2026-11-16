package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gfdelarue/kiln-vm/internal/execution"
	"github.com/gfdelarue/kiln-vm/internal/execwire"
)

func TestClientAPIErrorIncludesStructuredDetail(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{
			"error":"internal error",
			"detail":"prepare windows VM profile windows11: start windows profile install VM: permission denied",
			"code":"windows_profile_prepare_failed",
			"status_ref":"/vms/win",
			"phase":"profile_prepare",
			"resource":"windows11",
			"action":"prepare",
			"retryable":true
		}`))
	}))
	defer server.Close()

	client := mustNewClient(t, server.URL, "")
	client.HTTPClient = server.Client()
	err := client.Get("/vm-profiles/windows11/prepare", nil)
	if err == nil {
		t.Fatal("expected API error")
	}
	text := err.Error()
	for _, want := range []string{
		"API error (500): internal error",
		"code=windows_profile_prepare_failed",
		"phase=profile_prepare",
		"resource=windows11",
		"action=prepare",
		"retryable=true",
		"status=/vms/win",
		"permission denied",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("error %q missing %q", text, want)
		}
	}
}

func TestClientExecDuplexStreamsExactInputAndOutput(t *testing.T) {
	payload := strings.Repeat("stdin-payload\n", 5000)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Content-Type"); got != execwire.MediaType {
			t.Fatalf("content type = %q, want %q", got, execwire.MediaType)
		}
		if got := r.Header.Get("Accept"); got != execwire.MediaType {
			t.Fatalf("accept = %q, want %q", got, execwire.MediaType)
		}
		if err := http.NewResponseController(w).EnableFullDuplex(); err != nil {
			t.Fatalf("enable full duplex: %v", err)
		}
		codec := execwire.NewCodec(r.Body, w)
		start, err := codec.ReadRequest()
		if err != nil || start.Start == nil || start.Start.Command != "cat" {
			t.Fatalf("start = %#v, %v", start, err)
		}
		var body bytes.Buffer
		for {
			frame, err := codec.ReadRequest()
			if err != nil {
				t.Fatalf("read request frame: %v", err)
			}
			if frame.Type == execwire.FrameStdinEOF {
				break
			}
			_, _ = body.Write(frame.Data)
		}
		if body.String() != payload {
			t.Fatalf("stdin length = %d, want %d", body.Len(), len(payload))
		}
		w.Header().Set("Content-Type", execwire.MediaType)
		if err := codec.WriteResponse(execwire.ResponseFrame{Version: execwire.Version, Type: execwire.FrameStarted}); err != nil {
			t.Fatalf("write started: %v", err)
		}
		if err := codec.WriteResponse(execwire.ResponseFrame{Version: execwire.Version, Type: execwire.FrameStdout, Data: []byte("ok")}); err != nil {
			t.Fatalf("write stdout: %v", err)
		}
		if err := codec.WriteResponse(execwire.ResponseFrame{
			Version:  execwire.Version,
			Type:     execwire.FrameTerminal,
			Terminal: &execwire.Terminal{},
		}); err != nil {
			t.Fatalf("write terminal: %v", err)
		}
	}))
	defer server.Close()

	client := mustNewClient(t, server.URL, "token")
	client.HTTPClient = server.Client()
	var stdout bytes.Buffer
	outcome, err := client.ExecDuplex(context.Background(), "/exec", execution.Request{
		Command: "cat",
		Stdin:   strings.NewReader(payload),
		Stdout:  &stdout,
	})
	if err != nil {
		t.Fatalf("ExecDuplex: %v", err)
	}
	if stdout.String() != "ok" || !outcome.Succeeded() {
		t.Fatalf("stdout = %q outcome=%#v", stdout.String(), outcome)
	}
}

func TestClientExecDuplexRefusesRedirectWithoutReplay(t *testing.T) {
	for _, status := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var targetRequests atomic.Int32
			target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				targetRequests.Add(1)
			}))
			defer target.Close()
			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := http.NewResponseController(w).EnableFullDuplex(); err != nil {
					t.Fatalf("enable full duplex: %v", err)
				}
				http.Redirect(w, r, target.URL+"/replayed", status)
			}))
			defer origin.Close()

			client := mustNewClient(t, origin.URL, "token")
			client.HTTPClient = origin.Client()
			_, err := client.ExecDuplex(context.Background(), "/exec", execution.Request{Command: "true"})
			if err == nil || !strings.Contains(err.Error(), "redirect") {
				t.Fatalf("ExecDuplex error = %v", err)
			}
			if targetRequests.Load() != 0 {
				t.Fatalf("redirect target requests = %d, want zero", targetRequests.Load())
			}
		})
	}
}

func TestClientRejectsRemotePlaintextBeforeSendingBearerToken(t *testing.T) {
	var requests atomic.Int32
	client := &Client{
		BaseURL:    "http://192.0.2.10:8080",
		Token:      "top-secret-token",
		HTTPClient: &http.Client{},
	}
	client.HTTPClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		requests.Add(1)
		return &http.Response{
			StatusCode: http.StatusNoContent,
			Status:     http.StatusText(http.StatusNoContent),
			Body:       io.NopCloser(strings.NewReader("")),
		}, nil
	})}

	err := client.Get("/health", nil)
	if err == nil || !strings.Contains(err.Error(), "refusing plaintext HTTP") {
		t.Fatalf("Get error = %v, want plaintext transport rejection", err)
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("transport received %d requests after policy rejection, want 0", got)
	}
}

func TestClientRejectsIPv4WithZoneBeforeSendingBearerToken(t *testing.T) {
	var requests atomic.Int32
	client := &Client{
		BaseURL: "http://127.0.0.1%25evil:8080",
		Token:   "top-secret-token",
		HTTPClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			requests.Add(1)
			return nil, errors.New("transport must not be called")
		})},
	}

	err := client.Get("/health", nil)
	if err == nil || !strings.Contains(err.Error(), "refusing plaintext HTTP") {
		t.Fatalf("Get error = %v, want plaintext transport rejection", err)
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("transport received %d requests after IPv4-zone rejection, want 0", got)
	}
}

func TestNewClientRejectsRemotePlaintextAtConstruction(t *testing.T) {
	client, err := newClient("http://192.0.2.10:8080", "top-secret-token", false)
	if err == nil || !strings.Contains(err.Error(), "refusing plaintext HTTP") {
		t.Fatalf("newClient error = %v, want plaintext transport rejection", err)
	}
	if client != nil {
		t.Fatal("newClient returned a client for a rejected remote plaintext URL")
	}
}

func TestNewClientRejectsMalformedConfiguredPortsAndEmptyDelimiters(t *testing.T) {
	for _, apiURL := range []string{
		"https://controller.example:0",
		"https://controller.example:65536",
		"https://controller.example:99999",
		"https://controller.example?",
		"https://controller.example#",
	} {
		client, err := newClient(apiURL, "token", false)
		if err == nil {
			t.Fatalf("newClient(%q) succeeded", apiURL)
		}
		if client != nil {
			t.Fatalf("newClient(%q) returned a client", apiURL)
		}
	}
	if client, err := newClient("https://controller.example:65535", "token", false); err != nil || client == nil {
		t.Fatalf("newClient with maximum valid port = %v, %v", client, err)
	}
}

func TestClientExplicitRemotePlaintextOptInSendsRequest(t *testing.T) {
	var authorization string
	client, err := newClient("http://192.0.2.10:8080", "explicit-token", true)
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}
	client.HTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		authorization = req.Header.Get("Authorization")
		return &http.Response{
			StatusCode: http.StatusNoContent,
			Status:     "204 No Content",
			Body:       io.NopCloser(strings.NewReader("")),
			Header:     make(http.Header),
			Request:    req,
		}, nil
	})}

	if err := client.Get("/health", nil); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if authorization != "Bearer explicit-token" {
		t.Fatalf("Authorization = %q, want bearer token", authorization)
	}
}

func TestClientRejectsCrossHostCredentialRedirect(t *testing.T) {
	var targetRequests atomic.Int32
	var targetAuthorization atomic.Value
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetRequests.Add(1)
		targetAuthorization.Store(r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer target.Close()
	crossHostTarget, err := url.Parse(strings.Replace(target.URL, "127.0.0.1", "localhost", 1))
	if err != nil {
		t.Fatalf("parse cross-host target: %v", err)
	}
	crossHostTarget.RawQuery = "token=sentinel-redirect-query"

	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer redirect-token" {
			t.Errorf("source Authorization = %q, want bearer token", got)
		}
		redirectTarget := *crossHostTarget
		redirectTarget.Path = "/credential-target"
		http.Redirect(w, r, redirectTarget.String(), http.StatusFound)
	}))
	defer source.Close()

	client := mustNewClient(t, source.URL, "redirect-token")
	client.HTTPClient = source.Client()
	err = client.Get("/json", nil)
	if err == nil || !strings.Contains(err.Error(), "credential-bearing redirect") {
		t.Fatalf("request error = %v, want credential redirect rejection", err)
	}
	if strings.Contains(err.Error(), "sentinel-redirect") {
		t.Fatalf("redirect error leaked raw Location secrets: %v", err)
	}
	if got := targetRequests.Load(); got != 0 {
		authorization, _ := targetAuthorization.Load().(string)
		t.Fatalf("redirect target received %d requests with Authorization %q, want no requests", got, authorization)
	}
}

func TestClientRedactsMalformedRedirectLocation(t *testing.T) {
	const malformedLocation = "http://%zz.invalid/path?token=sentinel-redirect-bearer"
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer redirect-token" {
			t.Errorf("source Authorization = %q, want bearer token", got)
		}
		w.Header().Set("Location", malformedLocation)
		w.WriteHeader(http.StatusFound)
	}))
	defer source.Close()

	client := mustNewClient(t, source.URL, "redirect-token")
	client.HTTPClient = source.Client()
	err := client.Get("/json", nil)
	if !errors.Is(err, errInvalidRedirectLocation) {
		t.Fatalf("request error = %v, want invalid redirect Location", err)
	}
	if strings.Contains(err.Error(), "sentinel-redirect-bearer") || strings.Contains(err.Error(), "%zz.invalid") {
		t.Fatalf("malformed redirect error leaked raw Location: %v", err)
	}
}

func TestClientRevalidatesRedirectAfterComposedCallbackMutation(t *testing.T) {
	var targetRequests atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetRequests.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer target.Close()
	targetURL, err := url.Parse(strings.Replace(target.URL, "127.0.0.1", "localhost", 1))
	if err != nil {
		t.Fatalf("parse target URL: %v", err)
	}

	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/initially-safe", http.StatusFound)
	}))
	defer source.Close()
	client := mustNewClient(t, source.URL, "redirect-token")
	client.HTTPClient = source.Client()
	client.HTTPClient.CheckRedirect = func(req *http.Request, _ []*http.Request) error {
		req.URL = targetURL
		return nil
	}

	err = client.Get("/start", nil)
	if err == nil || !strings.Contains(err.Error(), "credential-bearing redirect") {
		t.Fatalf("request error = %v, want post-callback redirect rejection", err)
	}
	if got := targetRequests.Load(); got != 0 {
		t.Fatalf("mutated redirect target received %d requests, want 0", got)
	}
}

func TestClientAllowsSameAuthorityRedirect(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/json", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/json-target", http.StatusFound)
	})
	mux.HandleFunc("/json-target", func(w http.ResponseWriter, r *http.Request) {
		assertBearerToken(t, r, "same-origin-token")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	client := mustNewClient(t, server.URL, "same-origin-token")
	client.HTTPClient = server.Client()
	var result struct {
		OK bool `json:"ok"`
	}
	if err := client.Get("/json", &result); err != nil {
		t.Fatalf("JSON redirect: %v", err)
	}
	if !result.OK {
		t.Fatal("JSON redirect did not decode target response")
	}
}

func TestClientLoopbackBypassesHTTPProxyForInitialAndRedirectedRequests(t *testing.T) {
	const helperKey = "KILN_VM_TEST_HTTP_PROXY_HELPER"
	if os.Getenv(helperKey) == "1" {
		client, err := newClient(os.Getenv("KILN_VM_TEST_HTTP_PROXY_INITIAL_TARGET"), "proxy-bypass-token", false)
		if err != nil {
			t.Fatalf("newClient: %v", err)
		}
		if err := client.Get("/initial", nil); err != nil {
			t.Fatalf("initial Get: %v", err)
		}
		client, err = newClient(os.Getenv("KILN_VM_TEST_HTTP_PROXY_REDIRECT_TARGET"), "proxy-bypass-token", false)
		if err != nil {
			t.Fatalf("newClient for redirect: %v", err)
		}
		if err := client.Get("/redirect", nil); err != nil {
			t.Fatalf("redirect Get: %v", err)
		}
		return
	}

	var targetRequests atomic.Int32
	var authorizedTargetRequests atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetRequests.Add(1)
		if r.Header.Get("Authorization") == "Bearer proxy-bypass-token" {
			authorizedTargetRequests.Add(1)
		}
		if r.URL.Path == "/redirect" {
			_, port, err := net.SplitHostPort(r.Host)
			if err != nil {
				t.Errorf("split redirect host %q: %v", r.Host, err)
				http.Error(w, "invalid host", http.StatusInternalServerError)
				return
			}
			http.Redirect(w, r, "http://LOCALHOST:"+port+"/target", http.StatusFound)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer target.Close()
	targetURL, err := url.Parse(target.URL)
	if err != nil {
		t.Fatalf("parse target URL: %v", err)
	}
	targetPort := targetURL.Port()

	var proxyRequests atomic.Int32
	var proxyAuthorization atomic.Value
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyRequests.Add(1)
		proxyAuthorization.Store(r.Header.Get("Authorization"))
		http.Error(w, "loopback request reached proxy", http.StatusBadGateway)
	}))
	defer proxy.Close()

	cmd := exec.Command(os.Args[0], "-test.run=^TestClientLoopbackBypassesHTTPProxyForInitialAndRedirectedRequests$")
	cmd.Env = subprocessEnvironment(map[string]string{
		helperKey:                                 "1",
		"KILN_VM_TEST_HTTP_PROXY_INITIAL_TARGET":  "http://LOCALHOST.:" + targetPort,
		"KILN_VM_TEST_HTTP_PROXY_REDIRECT_TARGET": "http://localhost:" + targetPort,
		"HTTP_PROXY":                              proxy.URL,
		"http_proxy":                              proxy.URL,
		"HTTPS_PROXY":                             "",
		"https_proxy":                             "",
		"ALL_PROXY":                               "",
		"all_proxy":                               "",
		"NO_PROXY":                                "",
		"no_proxy":                                "",
	}, "REQUEST_METHOD")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("proxy-bypass helper failed: %v\n%s", err, output)
	}
	if got := proxyRequests.Load(); got != 0 {
		authorization, _ := proxyAuthorization.Load().(string)
		t.Fatalf("proxy received %d loopback requests with Authorization %q", got, authorization)
	}
	if got := targetRequests.Load(); got != 3 {
		t.Fatalf("loopback target received %d requests, want initial plus redirect pair", got)
	}
	if got := authorizedTargetRequests.Load(); got != 3 {
		t.Fatalf("loopback target received bearer on %d of 3 requests", got)
	}
}

func TestNewClientReusesPolicyTransportAcrossRequests(t *testing.T) {
	client := mustNewClient(t, "http://localhost:8080", "token")
	first := client.requestHTTPClient(false).Transport
	second := client.requestHTTPClient(false).Transport
	streaming := client.requestHTTPClient(true).Transport
	if first == nil || first != second || first != streaming || first != client.HTTPClient.Transport {
		t.Fatal("validated client did not reuse its policy-wrapped transport")
	}
}

func TestClientAllowsSameHostHTTPToHTTPSUpgrade(t *testing.T) {
	var targetAuthorization string
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetAuthorization = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer target.Close()

	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/secure", http.StatusFound)
	}))
	defer source.Close()

	client := mustNewClient(t, source.URL, "upgrade-token")
	client.HTTPClient = target.Client()
	if err := client.Get("/start", nil); err != nil {
		t.Fatalf("HTTP to HTTPS upgrade: %v", err)
	}
	if targetAuthorization != "Bearer upgrade-token" {
		t.Fatalf("upgraded Authorization = %q, want bearer token", targetAuthorization)
	}
}

func TestClientRejectsHTTPSDowngradeBeforeSendingRedirectedRequest(t *testing.T) {
	var source *httptest.Server
	source = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertBearerToken(t, r, "tls-token")
		downgradeTarget := "http://" + r.Host + "/plaintext-target"
		http.Redirect(w, r, downgradeTarget, http.StatusFound)
	}))
	defer source.Close()

	client := mustNewClient(t, source.URL, "tls-token")
	client.HTTPClient = source.Client()
	err := client.Get("/events", nil)
	if err == nil || !strings.Contains(err.Error(), "HTTPS to HTTP redirect") {
		t.Fatalf("Get error = %v, want HTTPS downgrade rejection", err)
	}
}

func assertBearerToken(t *testing.T, req *http.Request, token string) {
	t.Helper()
	if got := req.Header.Get("Authorization"); got != "Bearer "+token {
		t.Errorf("Authorization = %q, want bearer token", got)
	}
}

func mustNewClient(t *testing.T, baseURL, token string) *Client {
	t.Helper()
	client, err := newClient(baseURL, token, false)
	if err != nil {
		t.Fatalf("newClient(%q): %v", baseURL, err)
	}
	return client
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func subprocessEnvironment(overrides map[string]string, unset ...string) []string {
	removed := make(map[string]struct{}, len(overrides)+len(unset))
	for key := range overrides {
		removed[key] = struct{}{}
	}
	for _, key := range unset {
		removed[key] = struct{}{}
	}
	environment := make([]string, 0, len(os.Environ())+len(overrides))
	for _, assignment := range os.Environ() {
		key, _, _ := strings.Cut(assignment, "=")
		if _, remove := removed[key]; !remove {
			environment = append(environment, assignment)
		}
	}
	for key, value := range overrides {
		environment = append(environment, key+"="+value)
	}
	return environment
}
