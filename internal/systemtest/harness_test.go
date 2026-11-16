//go:build system

package systemtest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gfdelarue/kiln-vm/internal/models"
	"github.com/gfdelarue/kiln-vm/internal/version"

	"github.com/gorilla/websocket"
)

const resourcePrefix = "kiln-vm-systemtest-"

type apiClient struct {
	baseURL        string
	token          string
	http           *http.Client
	restartOwned   map[string]struct{}
	restartOwnedMu sync.RWMutex
}

type apiStatusError struct {
	StatusCode int
	Body       string
}

type healthResponse struct {
	Status  string `json:"status"`
	Version string `json:"version"`
}

func (e *apiStatusError) Error() string {
	return fmt.Sprintf("API status %d: %s", e.StatusCode, strings.TrimSpace(e.Body))
}

func requireClient(t *testing.T) *apiClient {
	t.Helper()

	baseURL := strings.TrimRight(os.Getenv("KILN_VM_API_URL"), "/")
	token := os.Getenv("KILN_VM_TOKEN")
	if baseURL == "" || token == "" {
		t.Fatalf("KILN_VM_API_URL and KILN_VM_TOKEN are required; create .env.systemtest from .env.systemtest.example and run task test:system")
	}

	client := &apiClient{
		baseURL:      baseURL,
		token:        token,
		http:         &http.Client{Timeout: 90 * time.Minute},
		restartOwned: map[string]struct{}{},
	}
	client.requireCompatibleHealth(t)
	return client
}

func (c *apiClient) requireCompatibleHealth(t *testing.T) {
	t.Helper()
	health, err := c.healthInfo()
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	if os.Getenv("KILN_VM_SYSTEMTEST_ALLOW_DAEMON_VERSION_MISMATCH") == "1" {
		return
	}
	if err := compatibleHealthError(*health); err != nil {
		t.Fatal(err)
	}
}

func compatibleHealthError(health healthResponse) error {
	if health.Version == version.Version {
		return nil
	}
	return fmt.Errorf(
		"system tests are connected to kiln-vmd version %s, but the source under test is version %s; rebuild/restart kiln-vmd or set KILN_VM_SYSTEMTEST_ALLOW_DAEMON_VERSION_MISMATCH=1 for explicit external-daemon debugging",
		health.Version,
		version.Version,
	)
}

func (c *apiClient) healthErr() error {
	_, err := c.healthInfo()
	return err
}

func (c *apiClient) healthInfo() (*healthResponse, error) {
	req, err := http.NewRequest(http.MethodGet, c.baseURL+"/health", nil)
	if err != nil {
		return nil, fmt.Errorf("create health request: %w", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("health request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("health status %d: %s", resp.StatusCode, body)
	}
	var health healthResponse
	if err := json.NewDecoder(resp.Body).Decode(&health); err != nil {
		return nil, fmt.Errorf("decode health: %w", err)
	}
	return &health, nil
}

func (c *apiClient) do(ctx context.Context, method, path string, body, out any) error {
	if body == nil {
		return c.doRaw(ctx, method, path, "", nil, out)
	}
	data, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}
	return c.doRaw(ctx, method, path, "application/json", bytes.NewReader(data), out)
}

func (c *apiClient) doRaw(ctx context.Context, method, path, contentType string, body io.Reader, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("request %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(resp.Body)
		return &apiStatusError{StatusCode: resp.StatusCode, Body: string(respBody)}
	}
	if out != nil && resp.StatusCode != http.StatusNoContent {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}

func (c *apiClient) metricsSummary(ctx context.Context) (*models.MetricsSummary, error) {
	var summary models.MetricsSummary
	if err := c.do(ctx, http.MethodGet, "/metrics/summary", nil, &summary); err != nil {
		return nil, err
	}
	return &summary, nil
}

func (c *apiClient) diskUsage(ctx context.Context) (*models.DiskUsageReport, error) {
	var report models.DiskUsageReport
	if err := c.do(ctx, http.MethodGet, "/disk/usage", nil, &report); err != nil {
		return nil, err
	}
	return &report, nil
}

func (c *apiClient) vmMetrics(ctx context.Context, ref string) (*models.VMMetrics, error) {
	var metrics models.VMMetrics
	if err := c.do(ctx, http.MethodGet, "/vms/"+ref+"/metrics", nil, &metrics); err != nil {
		return nil, err
	}
	return &metrics, nil
}

func (c *apiClient) vmMachineMetrics(ctx context.Context, ref string) (*models.MachineMetrics, error) {
	metrics, err := c.vmMetrics(ctx, ref)
	if err != nil {
		return nil, err
	}
	return &metrics.Machine, nil
}

func assertExplicitMachineMemoryObservation(t *testing.T, metrics models.MachineMetrics) {
	t.Helper()
	switch metrics.MemoryStatus {
	case "measured":
		if !metrics.MemoryObserved || metrics.MemoryError != "" {
			t.Fatalf("machine memory observation = observed:%t status:%q error:%q, want measured without error", metrics.MemoryObserved, metrics.MemoryStatus, metrics.MemoryError)
		}
	case "unavailable":
		if metrics.MemoryObserved || metrics.MemoryError == "" {
			t.Fatalf("machine memory observation = observed:%t status:%q error:%q, want explicit unavailable error", metrics.MemoryObserved, metrics.MemoryStatus, metrics.MemoryError)
		}
	default:
		t.Fatalf("machine memory status = %q observed:%t error:%q, want measured or unavailable", metrics.MemoryStatus, metrics.MemoryObserved, metrics.MemoryError)
	}
}

func (c *apiClient) createVM(ctx context.Context, req models.CreateVMRequest) (*models.VM, error) {
	var vm models.VM
	if err := c.do(ctx, http.MethodPost, "/vms", &req, &vm); err != nil {
		return nil, err
	}
	c.rememberRestartOwned(vm.ID)
	return &vm, nil
}

func (c *apiClient) prepareVMProfile(ctx context.Context, profile string, req models.PrepareVMProfileRequest) (*models.VMProfileImage, error) {
	var image models.VMProfileImage
	if err := c.do(ctx, http.MethodPost, "/vm-profiles/"+profile+"/prepare", &req, &image); err != nil {
		return nil, err
	}
	return &image, nil
}

func (c *apiClient) listVMProfileImages(ctx context.Context) ([]models.VMProfileImage, error) {
	var images []models.VMProfileImage
	if err := c.do(ctx, http.MethodGet, "/vm-profile-images", nil, &images); err != nil {
		return nil, err
	}
	return images, nil
}

func (c *apiClient) deleteVMProfileImage(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/vm-profile-images/"+id, nil, nil)
}

func (c *apiClient) getVM(ctx context.Context, ref string) (*models.VM, error) {
	var vm models.VM
	if err := c.do(ctx, http.MethodGet, "/vms/"+ref, nil, &vm); err != nil {
		return nil, err
	}
	return &vm, nil
}

func (c *apiClient) verifyVMCapabilities(ctx context.Context, ref string, req models.VerifyVMCapabilitiesRequest) (*models.VerifyVMCapabilitiesResponse, error) {
	var response models.VerifyVMCapabilitiesResponse
	if err := c.do(ctx, http.MethodPost, "/vms/"+ref+"/verify", &req, &response); err != nil {
		return nil, err
	}
	return &response, nil
}

func (c *apiClient) listVMs(ctx context.Context) ([]models.VM, error) {
	var vms []models.VM
	if err := c.do(ctx, http.MethodGet, "/vms", nil, &vms); err != nil {
		return nil, err
	}
	return vms, nil
}

func (c *apiClient) startVM(ctx context.Context, ref string) (*models.VM, error) {
	var vm models.VM
	if err := c.do(ctx, http.MethodPost, "/vms/"+ref+"/start", nil, &vm); err != nil {
		return nil, err
	}
	return &vm, nil
}

func (c *apiClient) stopVM(ctx context.Context, ref string) error {
	return c.do(ctx, http.MethodPost, "/vms/"+ref+"/stop", nil, nil)
}

func (c *apiClient) uploadVM(ctx context.Context, ref, guestPath string, content []byte) error {
	query := url.Values{"path": {guestPath}}
	return c.doRaw(ctx, http.MethodPut, "/vms/"+ref+"/files?"+query.Encode(), "application/octet-stream", bytes.NewReader(content), nil)
}

func (c *apiClient) dialVMShell(ctx context.Context, ref string) (*websocket.Conn, *http.Response, error) {
	return c.dialVMWebSocket(ctx, "/vms/"+ref+"/shell")
}

func (c *apiClient) dialVMForward(ctx context.Context, ref string, port int) (*websocket.Conn, *http.Response, error) {
	return c.dialVMWebSocket(ctx, fmt.Sprintf("/vms/%s/forward/%d", ref, port))
}

func (c *apiClient) dialVMWebSocket(ctx context.Context, path string) (*websocket.Conn, *http.Response, error) {
	wsURL, err := url.Parse(c.baseURL + path)
	if err != nil {
		return nil, nil, fmt.Errorf("parse VM websocket URL: %w", err)
	}
	switch wsURL.Scheme {
	case "http":
		wsURL.Scheme = "ws"
	case "https":
		wsURL.Scheme = "wss"
	default:
		return nil, nil, fmt.Errorf("unsupported VM websocket scheme %q", wsURL.Scheme)
	}
	header := http.Header{}
	header.Set("Authorization", "Bearer "+c.token)
	conn, response, err := websocket.DefaultDialer.DialContext(ctx, wsURL.String(), header)
	if err != nil {
		return conn, response, fmt.Errorf("dial VM websocket %s: %w", path, err)
	}
	return conn, response, nil
}

func (c *apiClient) deleteVM(ctx context.Context, ref string) error {
	return c.do(ctx, http.MethodDelete, "/vms/"+ref, nil, nil)
}

func (c *apiClient) rememberRestartOwned(vmID string) {
	vmID = strings.TrimSpace(vmID)
	if vmID == "" {
		return
	}
	c.restartOwnedMu.Lock()
	defer c.restartOwnedMu.Unlock()
	if c.restartOwned == nil {
		c.restartOwned = map[string]struct{}{}
	}
	c.restartOwned[vmID] = struct{}{}
}

func (c *apiClient) restartOwnedSnapshot() map[string]struct{} {
	c.restartOwnedMu.RLock()
	defer c.restartOwnedMu.RUnlock()
	result := make(map[string]struct{}, len(c.restartOwned))
	for id := range c.restartOwned {
		result[id] = struct{}{}
	}
	return result
}

type execResult struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code"`
}

func (c *apiClient) execVM(ctx context.Context, ref, command string) (*execResult, error) {
	var result execResult
	body := struct {
		Command string `json:"command"`
	}{Command: command}
	if err := c.do(ctx, http.MethodPost, "/vms/"+ref+"/exec", &body, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *apiClient) waitForVMState(ctx context.Context, ref string, states ...models.MachineState) (*models.VM, error) {
	want := make(map[models.MachineState]struct{}, len(states))
	for _, state := range states {
		want[state] = struct{}{}
	}

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		vm, err := c.getVM(ctx, ref)
		if err != nil {
			return nil, err
		}
		if _, ok := want[vm.State]; ok {
			return vm, nil
		}
		if vm.State == models.MachineStateError {
			return vm, fmt.Errorf("VM entered error state: %s", vm.Error)
		}

		select {
		case <-ctx.Done():
			return vm, fmt.Errorf("wait for VM state %v: %w", states, ctx.Err())
		case <-ticker.C:
		}
	}
}

func waitForExecResult(ctx context.Context, client *apiClient, ref, command string, accept func(*execResult) bool) (*execResult, error) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	var lastResult *execResult
	var lastErr error
	for {
		attemptCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
		result, err := client.execVM(attemptCtx, ref, command)
		cancel()
		if err == nil {
			lastResult = result
			if accept(result) {
				return result, nil
			}
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			if lastErr != nil {
				return lastResult, fmt.Errorf("wait for exec result: %w; last error: %v", ctx.Err(), lastErr)
			}
			if lastResult != nil {
				return lastResult, fmt.Errorf("wait for exec result: %w; last result exit=%d stdout=%q stderr=%q", ctx.Err(), lastResult.ExitCode, lastResult.Stdout, lastResult.Stderr)
			}
			return nil, fmt.Errorf("wait for exec result: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func (c *apiClient) waitForExecReady(ctx context.Context, ref string) error {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		result, err := c.execVM(ctx, ref, "echo kiln-vm-exec-ready")
		if err == nil {
			if result.ExitCode == 0 && strings.Contains(result.Stdout, "kiln-vm-exec-ready") {
				return nil
			}
			return fmt.Errorf("exec readiness command failed: exit=%d stdout=%q stderr=%q", result.ExitCode, result.Stdout, result.Stderr)
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for exec readiness: %w; last error: %v", ctx.Err(), err)
		case <-ticker.C:
		}
	}
}

func cleanupResourcesByPrefix(t *testing.T, client *apiClient) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	vms, err := client.listVMs(ctx)
	if err != nil {
		t.Logf("list VMs for cleanup: %v", err)
	} else {
		for _, vm := range vms {
			if !strings.HasPrefix(vm.Name, resourcePrefix) {
				continue
			}
			if vm.State == models.MachineStateRunning || vm.State == models.MachineStateCreating || vm.State == models.MachineStateStopping {
				if err := client.stopVM(ctx, vm.Name); err != nil {
					t.Logf("stop stale VM %s: %v", vm.Name, err)
				}
				if _, err := client.waitForVMState(ctx, vm.Name, models.MachineStateStopped); err != nil {
					t.Logf("wait stale VM %s stopped: %v", vm.Name, err)
				}
			}
			if err := client.deleteVM(ctx, vm.Name); err != nil {
				t.Logf("delete stale VM %s: %v", vm.Name, err)
			}
		}
	}

	profileImages, err := client.listVMProfileImages(ctx)
	if err != nil {
		t.Logf("list VM profile images for cleanup: %v", err)
		return
	}
	for _, image := range profileImages {
		if strings.HasPrefix(image.Profile, resourcePrefix) {
			if err := client.deleteVMProfileImage(ctx, image.ID); err != nil {
				t.Logf("delete stale VM profile image %s: %v", image.ID, err)
			}
		}
	}
}

func uniqueName(label string) string {
	return fmt.Sprintf("%s%s-%d", resourcePrefix, label, time.Now().UnixNano())
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}

func snapshotVMProfileImages(t *testing.T, client *apiClient, ctx context.Context) map[string]struct{} {
	t.Helper()
	images, err := client.listVMProfileImages(ctx)
	if err != nil {
		t.Fatalf("list VM profile images: %v", err)
	}
	ids := make(map[string]struct{}, len(images))
	for _, image := range images {
		ids[image.ID] = struct{}{}
	}
	return ids
}

func cleanupNewVMProfileImages(t *testing.T, client *apiClient, ctx context.Context, before map[string]struct{}) {
	t.Helper()
	images, err := client.listVMProfileImages(ctx)
	if err != nil {
		t.Logf("list VM profile images for cleanup: %v", err)
		return
	}
	for _, image := range images {
		if _, existed := before[image.ID]; existed {
			continue
		}
		if err := client.deleteVMProfileImage(ctx, image.ID); err != nil {
			t.Logf("delete VM profile image %s: %v", image.ID, err)
		}
	}
}

func withSystemProfileImageCleanup(t *testing.T, client *apiClient, ctx context.Context) {
	t.Helper()
	profileImagesBefore := snapshotVMProfileImages(t, client, ctx)
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		cleanupNewVMProfileImages(t, client, cleanupCtx, profileImagesBefore)
	})
}

func waitForNoTap(ctx context.Context, tap string) error {
	if tap == "" {
		return nil
	}
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		exists, err := tapExists(tap)
		if err != nil {
			return fmt.Errorf("inspect tap device %s: %w", tap, err)
		}
		if !exists {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("tap device %s still exists: %w", tap, ctx.Err())
		case <-ticker.C:
		}
	}
}

func tapExists(tap string) (bool, error) {
	return linkExistsForTest(tap)
}

func assertNoStopErrorLog(t *testing.T, since time.Time, machineID string) {
	t.Helper()
	if _, err := exec.LookPath("journalctl"); err != nil {
		t.Log("journalctl unavailable; skipping stop-error log assertion")
		return
	}
	cmd := exec.Command("journalctl", "-u", "kiln-vmd", "-b", "--since", since.Format("2006-01-02 15:04:05"), "--no-pager")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Logf("journalctl unavailable for kiln-vmd; skipping stop-error log assertion: %v", err)
		return
	}
	logs := string(out)
	if strings.Contains(logs, machineID) && strings.Contains(logs, "VM exited with error") {
		t.Fatalf("intentional stop logged VM exited with error for machine %s:\n%s", machineID, logs)
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve caller")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

var (
	cliBuildMu sync.Mutex
	cliBinary  string
)

func cliBinaryPath(t *testing.T) string {
	t.Helper()
	cliBuildMu.Lock()
	defer cliBuildMu.Unlock()

	if cliBinary != "" {
		return cliBinary
	}

	binDir, err := os.MkdirTemp("", "kiln-vm-systemtest-cli-*")
	if err != nil {
		t.Fatalf("create CLI build dir: %v", err)
	}
	bin := filepath.Join(binDir, "kiln-vm")
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/kiln-vm")
	cmd.Dir = repoRoot(t)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build kiln-vm CLI: %v\n%s", err, out)
	}
	cliBinary = bin
	return cliBinary
}

func runCLI(t *testing.T, xdgConfigHome string, args ...string) (string, string, error) {
	return runCLIWithStdin(t, xdgConfigHome, nil, args...)
}

func runCLIWithStdin(t *testing.T, xdgConfigHome string, stdin io.Reader, args ...string) (string, string, error) {
	t.Helper()

	cmd := newCLICommand(t, xdgConfigHome, args...)
	cmd.Stdin = stdin

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

func newCLICommand(t *testing.T, xdgConfigHome string, args ...string) *exec.Cmd {
	t.Helper()

	cmd := exec.Command(cliBinaryPath(t), args...)
	cmd.Dir = repoRoot(t)
	cmd.Env = filteredEnv(os.Environ(), map[string]struct{}{
		"KILN_VM_API_URL": {},
		"KILN_VM_TOKEN":   {},
	})
	cmd.Env = append(cmd.Env, "XDG_CONFIG_HOME="+xdgConfigHome)
	return cmd
}

func filteredEnv(env []string, remove map[string]struct{}) []string {
	filtered := make([]string, 0, len(env))
	for _, entry := range env {
		key, _, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		if _, shouldRemove := remove[key]; shouldRemove {
			continue
		}
		filtered = append(filtered, entry)
	}
	return filtered
}

func writeMalformedCLIConfig(t *testing.T, xdgConfigHome string) {
	t.Helper()
	path := filepath.Join(xdgConfigHome, "kiln-vm", "config.json")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatalf("create config dir: %v", err)
	}
	if err := os.WriteFile(path, []byte(`{"api_url":`), 0600); err != nil {
		t.Fatalf("write malformed config: %v", err)
	}
}

func expectAPIStatus(t *testing.T, err error, status int) {
	t.Helper()
	var statusErr *apiStatusError
	if !errors.As(err, &statusErr) {
		t.Fatalf("expected API status %d, got %v", status, err)
	}
	if statusErr.StatusCode != status {
		t.Fatalf("expected API status %d, got %d (%s)", status, statusErr.StatusCode, statusErr.Body)
	}
}

func isAPIStatus(err error, status int) bool {
	var statusErr *apiStatusError
	return errors.As(err, &statusErr) && statusErr.StatusCode == status
}
