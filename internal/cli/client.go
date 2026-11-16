package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Client wraps HTTP communication with the kiln-vmd API.
type Client struct {
	BaseURL           string
	Token             string
	AllowInsecureHTTP bool
	HTTPClient        *http.Client
}

func newClient(baseURL, token string, allowInsecureHTTP bool) (*Client, error) {
	if err := validateAPIURL(baseURL, allowInsecureHTTP); err != nil {
		return nil, err
	}
	baseURL = canonicalizeLocalhostURLString(baseURL)
	return &Client{
		BaseURL:           strings.TrimRight(baseURL, "/"),
		Token:             token,
		AllowInsecureHTTP: allowInsecureHTTP,
		HTTPClient: &http.Client{
			Timeout:   clientTimeout(),
			Transport: transportWithManagementProxyPolicy(nil),
		},
	}, nil
}

func clientTimeout() time.Duration {
	if value := os.Getenv("KILN_VM_HTTP_TIMEOUT"); value != "" {
		if d, err := time.ParseDuration(value); err == nil {
			return d
		}
	}
	return 10 * time.Minute
}

// NewClientFromConfig creates a client from saved configuration.
func NewClientFromConfig() (*Client, error) {
	cfg, err := LoadConfig()
	if err != nil {
		return nil, err
	}
	if cfg.APIURL == "" {
		return nil, fmt.Errorf("API URL not configured; run 'kiln-vm login' or set KILN_VM_API_URL")
	}
	if cfg.Token == "" {
		return nil, fmt.Errorf("token not configured; run 'kiln-vm login' or set KILN_VM_TOKEN")
	}
	return newClient(cfg.APIURL, cfg.Token, cfg.AllowInsecureHTTP)
}

func validateAPIURL(rawURL string, allowInsecureHTTP bool) error {
	return validateURL(rawURL, allowInsecureHTTP, false)
}

func validateRequestURL(rawURL string, allowInsecureHTTP bool) error {
	return validateURL(rawURL, allowInsecureHTTP, true)
}

func validateURL(rawURL string, allowInsecureHTTP, allowQuery bool) error {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid API URL: the configured value could not be parsed")
	}
	description := safeAPIURLDescription(parsed)
	if parsed.Scheme == "" || parsed.Host == "" || parsed.Hostname() == "" {
		return fmt.Errorf("invalid API URL %s: an absolute http or https URL is required", description)
	}
	if parsed.User != nil {
		return fmt.Errorf("invalid API URL %s: user information is not allowed", description)
	}
	if strings.Contains(rawURL, "#") {
		return fmt.Errorf("invalid API URL %s: fragments are not allowed", description)
	}
	if !allowQuery && (parsed.RawQuery != "" || parsed.ForceQuery) {
		return fmt.Errorf("invalid API URL %s: query strings are not allowed in the configured API URL", description)
	}
	if port := parsed.Port(); port != "" {
		portNumber, err := strconv.Atoi(port)
		if err != nil || portNumber < 1 || portNumber > 65535 {
			return fmt.Errorf("invalid API URL %s: port must be between 1 and 65535", description)
		}
	}

	switch strings.ToLower(parsed.Scheme) {
	case "https":
		return nil
	case "http":
		if isLoopbackAPIHost(parsed.Hostname()) || allowInsecureHTTP {
			return nil
		}
		return fmt.Errorf("refusing plaintext HTTP for non-loopback API URL %s; use HTTPS, run 'kiln-vm login --allow-insecure-http' for saved consent, or set KILN_VM_ALLOW_INSECURE_HTTP=true for this process", description)
	default:
		return fmt.Errorf("invalid API URL %s: scheme must be http or https", description)
	}
}

func safeAPIURLDescription(parsed *url.URL) string {
	if parsed == nil || parsed.Scheme == "" || parsed.Host == "" {
		return "(configured value redacted)"
	}
	safe := &url.URL{Scheme: strings.ToLower(parsed.Scheme), Host: parsed.Host}
	return fmt.Sprintf("%q", safe.String())
}

// sanitizeHTTPClientError preserves the underlying error chain while removing
// credentials, query strings, fragments, and paths from net/http's URL error.
// Redirect policy errors otherwise reintroduce the raw Location value after
// validateURL has already produced a secret-safe diagnostic.
func sanitizeHTTPClientError(err error) error {
	var urlErr *url.Error
	if !errors.As(err, &urlErr) {
		return err
	}
	safe := *urlErr
	if urlErr.Err != nil && strings.Contains(urlErr.Err.Error(), "failed to parse Location header") {
		safe.Err = errInvalidRedirectLocation
	}
	parsed, parseErr := url.Parse(urlErr.URL)
	if parseErr != nil || parsed.Scheme == "" || parsed.Host == "" {
		safe.URL = "(request URL redacted)"
	} else {
		safe.URL = (&url.URL{Scheme: strings.ToLower(parsed.Scheme), Host: parsed.Host}).String()
	}
	return &safe
}

func isLoopbackAPIHost(host string) bool {
	if strings.EqualFold(strings.TrimSuffix(host, "."), "localhost") {
		return true
	}
	addr, err := netip.ParseAddr(host)
	return err == nil && addr.IsLoopback()
}

func canonicalizeLocalhostURLString(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	canonicalizeLocalhostURL(parsed)
	return parsed.String()
}

func canonicalizeLocalhostURL(parsed *url.URL) {
	if parsed == nil || !strings.EqualFold(strings.TrimSuffix(parsed.Hostname(), "."), "localhost") {
		return
	}
	port := parsed.Port()
	parsed.Host = "localhost"
	if port != "" {
		parsed.Host += ":" + port
	}
}

func (c *Client) newRequest(method, path string, body io.Reader) (*http.Request, error) {
	if err := validateAPIURL(c.BaseURL, c.AllowInsecureHTTP); err != nil {
		return nil, err
	}
	target := strings.TrimRight(canonicalizeLocalhostURLString(c.BaseURL), "/") + path
	if err := validateRequestURL(target, c.AllowInsecureHTTP); err != nil {
		return nil, err
	}
	req, err := http.NewRequest(method, target, body)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	return req, nil
}

func (c *Client) requestHTTPClient(stream bool) *http.Client {
	base := c.HTTPClient
	if base == nil {
		base = &http.Client{Timeout: clientTimeout()}
	}
	client := *base
	client.Transport = transportWithManagementProxyPolicy(base.Transport)
	previousCheckRedirect := base.CheckRedirect
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		canonicalizeLocalhostURL(req.URL)
		if err := c.checkRedirect(req, via); err != nil {
			return err
		}
		if previousCheckRedirect != nil {
			if err := previousCheckRedirect(req, via); err != nil {
				return err
			}
		}
		// A composed callback is allowed to inspect or rewrite the request,
		// but kiln-vm's credential policy remains the final authority.
		canonicalizeLocalhostURL(req.URL)
		if err := c.checkRedirect(req, via); err != nil {
			return err
		}
		// net/http's built-in header copier does not treat every localhost
		// spelling kiln-vm accepts as the same authority. Restore the bearer only
		// after kiln-vm and any caller redirect policies have approved the target.
		if c.Token != "" {
			req.Header.Set("Authorization", "Bearer "+c.Token)
		}
		return nil
	}
	if stream {
		client.Timeout = 0
	}
	return &client
}

type managementProxyTransport struct {
	*http.Transport
}

var errInvalidRedirectLocation = errors.New("invalid redirect Location header")

func (t *managementProxyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.Transport.RoundTrip(req)
	if err != nil {
		return resp, fmt.Errorf("perform management HTTP request: %w", err)
	}
	if resp == nil || !isRedirectStatus(resp.StatusCode) {
		return resp, nil
	}
	location := resp.Header.Get("Location")
	if location == "" {
		return resp, nil
	}
	if _, err := req.URL.Parse(location); err != nil {
		if resp.Body != nil {
			_ = resp.Body.Close()
		}
		return nil, errInvalidRedirectLocation
	}
	return resp, nil
}

func isRedirectStatus(status int) bool {
	switch status {
	case http.StatusMovedPermanently,
		http.StatusFound,
		http.StatusSeeOther,
		http.StatusTemporaryRedirect,
		http.StatusPermanentRedirect:
		return true
	default:
		return false
	}
}

func transportWithManagementProxyPolicy(roundTripper http.RoundTripper) http.RoundTripper {
	if _, ok := roundTripper.(*managementProxyTransport); ok {
		return roundTripper
	}
	if roundTripper == nil {
		roundTripper = http.DefaultTransport
	}
	transport, ok := roundTripper.(*http.Transport)
	if !ok {
		// A deliberately injected custom RoundTripper owns its own routing.
		return roundTripper
	}
	clone := transport.Clone()
	clone.Proxy = proxyWithManagementLoopbackBypass(clone.Proxy)
	return &managementProxyTransport{Transport: clone}
}

func proxyWithManagementLoopbackBypass(proxy func(*http.Request) (*url.URL, error)) func(*http.Request) (*url.URL, error) {
	return func(req *http.Request) (*url.URL, error) {
		if req != nil && req.URL != nil && isLoopbackAPIHost(req.URL.Hostname()) {
			return nil, nil
		}
		if proxy == nil {
			return nil, nil
		}
		return proxy(req)
	}
}

func (c *Client) checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return fmt.Errorf("stopped after 10 redirects")
	}
	if err := validateRequestURL(req.URL.String(), c.AllowInsecureHTTP); err != nil {
		return fmt.Errorf("refusing redirect: %w", err)
	}
	if len(via) == 0 {
		return nil
	}
	previous := via[len(via)-1].URL
	if strings.EqualFold(previous.Scheme, "https") && strings.EqualFold(req.URL.Scheme, "http") {
		return fmt.Errorf("refusing HTTPS to HTTP redirect")
	}
	if c.Token != "" && !sameURLAuthority(previous, req.URL) && !isSameHostHTTPSUpgrade(previous, req.URL) {
		return fmt.Errorf("refusing credential-bearing redirect from %s to %s", previous.Host, req.URL.Host)
	}
	return nil
}

func isSameHostHTTPSUpgrade(previous, next *url.URL) bool {
	return strings.EqualFold(previous.Scheme, "http") &&
		strings.EqualFold(next.Scheme, "https") &&
		strings.EqualFold(previous.Hostname(), next.Hostname())
}

func sameURLAuthority(left, right *url.URL) bool {
	return strings.EqualFold(left.Hostname(), right.Hostname()) && effectiveURLPort(left) == effectiveURLPort(right)
}

func effectiveURLPort(parsed *url.URL) string {
	if port := parsed.Port(); port != "" {
		return port
	}
	if strings.EqualFold(parsed.Scheme, "https") {
		return "443"
	}
	if strings.EqualFold(parsed.Scheme, "http") {
		return "80"
	}
	return ""
}

func (c *Client) do(method, path string, body, result any) error {
	var bodyReader io.Reader
	contentType := ""
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal request: %w", err)
		}
		bodyReader = bytes.NewReader(data)
		contentType = "application/json"
	}
	return c.doReader(method, path, bodyReader, contentType, result)
}

func (c *Client) doReader(method, path string, body io.Reader, contentType string, result any) error {
	req, err := c.newRequest(method, path, body)
	if err != nil {
		return err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	return c.send(req, result)
}

func (c *Client) send(req *http.Request, result any) error {
	resp, err := c.requestHTTPClient(false).Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", sanitizeHTTPClientError(err))
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		var apiErr apiErrorResponse
		respBody, _ := io.ReadAll(resp.Body)
		if json.Unmarshal(respBody, &apiErr) == nil && apiErr.Error != "" {
			return fmt.Errorf("API error (%d): %s", resp.StatusCode, formatAPIError(apiErr))
		}
		return fmt.Errorf("API error: %s", resp.Status)
	}

	if result != nil && resp.StatusCode != http.StatusNoContent {
		if err := json.NewDecoder(resp.Body).Decode(result); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}

type apiErrorResponse struct {
	Error     string `json:"error"`
	Detail    string `json:"detail"`
	Code      string `json:"code"`
	StatusRef string `json:"status_ref"`
	Phase     string `json:"phase"`
	Resource  string `json:"resource"`
	Action    string `json:"action"`
	Retryable *bool  `json:"retryable"`
}

func formatAPIError(apiErr apiErrorResponse) string {
	var parts []string
	parts = append(parts, apiErr.Error)
	var fields []string
	if apiErr.Code != "" {
		fields = append(fields, "code="+apiErr.Code)
	}
	if apiErr.Phase != "" {
		fields = append(fields, "phase="+apiErr.Phase)
	}
	if apiErr.Resource != "" {
		fields = append(fields, "resource="+apiErr.Resource)
	}
	if apiErr.Action != "" {
		fields = append(fields, "action="+apiErr.Action)
	}
	if apiErr.Retryable != nil {
		fields = append(fields, fmt.Sprintf("retryable=%t", *apiErr.Retryable))
	}
	if apiErr.StatusRef != "" {
		fields = append(fields, "status="+apiErr.StatusRef)
	}
	if len(fields) > 0 {
		parts = append(parts, "("+strings.Join(fields, " ")+")")
	}
	if apiErr.Detail != "" && apiErr.Detail != apiErr.Error {
		parts = append(parts, apiErr.Detail)
	}
	return strings.Join(parts, ": ")
}

// Get performs an HTTP GET request.
func (c *Client) Get(path string, result any) error {
	return c.do("GET", path, nil, result)
}

// Post performs an HTTP POST request.
func (c *Client) Post(path string, body, result any) error {
	return c.do("POST", path, body, result)
}

// Patch performs an HTTP PATCH request.
func (c *Client) Patch(path string, body, result any) error {
	return c.do("PATCH", path, body, result)
}

// Delete performs an HTTP DELETE request.
func (c *Client) Delete(path string) error {
	return c.do("DELETE", path, nil, nil)
}
