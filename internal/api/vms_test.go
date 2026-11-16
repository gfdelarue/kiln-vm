package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/gfdelarue/kiln-vm/internal/apierr"
	"github.com/gfdelarue/kiln-vm/internal/execution"
	"github.com/gfdelarue/kiln-vm/internal/execwire"
	"github.com/gfdelarue/kiln-vm/internal/models"
	"github.com/gfdelarue/kiln-vm/internal/store"
	vmservice "github.com/gfdelarue/kiln-vm/internal/vm"
)

func TestVMUploadFileStreamsBodyToGuestUpload(t *testing.T) {
	svc, machines := setupVMUploadTestService(t)
	req := httptest.NewRequest(http.MethodPut, "/vms/ready/files?path=%2Fworkspace%2Fapp%2Fruntime-prepare.mjs&mode=0600", bytes.NewReader([]byte("large runtime helper")))
	w := httptest.NewRecorder()

	setupVMRouter(svc).ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	if machines.uploadID != "machine-vm-ready" {
		t.Fatalf("upload machine = %q", machines.uploadID)
	}
	if machines.uploadPath != "/workspace/app/runtime-prepare.mjs" {
		t.Fatalf("upload path = %q", machines.uploadPath)
	}
	if machines.uploadMode != 0600 {
		t.Fatalf("upload mode = %#o", machines.uploadMode)
	}
	if machines.uploadSize != int64(len("large runtime helper")) {
		t.Fatalf("upload size = %d", machines.uploadSize)
	}
	if string(machines.uploadBody) != "large runtime helper" {
		t.Fatalf("upload body = %q", machines.uploadBody)
	}
	var response map[string]any
	if err := json.NewDecoder(w.Body).Decode(&response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response["path"] != "/workspace/app/runtime-prepare.mjs" || response["mode"] != "600" {
		t.Fatalf("response = %#v", response)
	}
}

func TestVMExecDuplexStreamsStdinThroughWakeAndUseLease(t *testing.T) {
	svc, machines := setupVMUploadTestService(t)
	server := httptest.NewServer(setupVMRouter(svc))
	defer server.Close()
	payload := strings.Repeat("vm-stdin\n", 4096)

	bodyReader, bodyWriter := io.Pipe()
	req, err := http.NewRequest(http.MethodPost, server.URL+"/vms/ready/exec", bodyReader)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", execwire.MediaType)
	req.Header.Set("Accept", execwire.MediaType)
	go func() {
		codec := execwire.NewCodec(nil, bodyWriter)
		frames := []execwire.RequestFrame{{Version: execwire.Version, Type: execwire.FrameStart, Start: &execwire.Start{Command: "cat"}}}
		for offset := 0; offset < len(payload); offset += execution.MaxDataBytes {
			end := min(offset+execution.MaxDataBytes, len(payload))
			frames = append(frames, execwire.RequestFrame{Version: execwire.Version, Type: execwire.FrameStdin, Data: []byte(payload[offset:end])})
		}
		frames = append(frames, execwire.RequestFrame{Version: execwire.Version, Type: execwire.FrameStdinEOF})
		for _, frame := range frames {
			if err := codec.WriteRequest(frame); err != nil {
				_ = bodyWriter.CloseWithError(err)
				return
			}
		}
		_ = bodyWriter.Close()
	}()

	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want %d; body: %s", resp.StatusCode, http.StatusOK, body)
	}
	codec := execwire.NewCodec(resp.Body, nil)
	var terminal *execwire.Terminal
	for terminal == nil {
		frame, err := codec.ReadResponse()
		if err != nil {
			t.Fatalf("read response: %v", err)
		}
		terminal = frame.Terminal
	}
	server.Close()

	if terminal.ExitCode != 0 || terminal.Category != execution.CategoryNone {
		t.Fatalf("terminal = %#v", terminal)
	}
	if machines.execID != "machine-vm-ready" || machines.execCommand != "cat" || string(machines.execBody) != payload {
		t.Fatalf("exec machine=%q command=%q stdin length=%d", machines.execID, machines.execCommand, len(machines.execBody))
	}
}

func TestVMUploadFileRejectsMissingContentLength(t *testing.T) {
	svc, _ := setupVMUploadTestService(t)
	req := httptest.NewRequest(http.MethodPut, "/vms/ready/files?path=%2Ftmp%2Fasset", struct{ io.Reader }{bytes.NewReader([]byte("body"))})
	req.ContentLength = -1
	w := httptest.NewRecorder()

	setupVMRouter(svc).ServeHTTP(w, req)

	if w.Code != http.StatusLengthRequired {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusLengthRequired, w.Body.String())
	}
}

func TestVMUploadFileRejectsInvalidMode(t *testing.T) {
	svc, _ := setupVMUploadTestService(t)
	req := httptest.NewRequest(http.MethodPut, "/vms/ready/files?path=%2Ftmp%2Fasset&mode=0999", bytes.NewReader([]byte("body")))
	w := httptest.NewRecorder()

	setupVMRouter(svc).ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusBadRequest, w.Body.String())
	}
}

func TestVMCreateReportsConfiguredDiskCap(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	svc := vmservice.NewService(db, nil, nil)
	svc.SetMaxDiskSizeMB(1024)
	body := []byte(`{"name":"too-large","profile":"alpine","disk_size_mb":1025}`)
	req := httptest.NewRequest(http.MethodPost, "/vms", bytes.NewReader(body))
	w := httptest.NewRecorder()

	setupVMRouter(svc).ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusBadRequest, w.Body.String())
	}
	var response map[string]string
	if err := json.NewDecoder(w.Body).Decode(&response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got := response["error"]; got != "validation: disk_size_mb: must be between 128 and 1024" {
		t.Fatalf("error = %q", got)
	}
}

func TestVMPlanEndpointResolvesProfileAndOverrides(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	svc := vmservice.NewService(db, nil, nil)
	body := []byte(`{"name":"runner","profile":"ubuntu-docker","memory_mb":4096,"disk_size_mb":65536,"power":{"mode":"on_demand","idle_timeout_seconds":60}}`)
	req := httptest.NewRequest(http.MethodPost, "/vms/plan", bytes.NewReader(body))
	w := httptest.NewRecorder()

	setupVMRouter(svc).ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	var plan models.VMCreatePlan
	if err := json.NewDecoder(w.Body).Decode(&plan); err != nil {
		t.Fatalf("decode plan: %v", err)
	}
	if plan.Profile != "ubuntu-docker" || plan.MemoryMB != 4096 || plan.DiskSizeMB != 65536 {
		t.Fatalf("plan = %+v", plan)
	}
	if plan.Power.Mode != models.VMPowerModeOnDemand || plan.Power.IdleTimeoutSeconds != 60 || plan.Power.StartsWithDaemon() {
		t.Fatalf("plan power = %+v", plan.Power)
	}
}

func TestVMPlanEndpointReportsProfileValidation(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	svc := vmservice.NewService(db, nil, nil)
	body := []byte(`{"name":"runner","profile":"github-runner"}`)
	req := httptest.NewRequest(http.MethodPost, "/vms/plan", bytes.NewReader(body))
	w := httptest.NewRecorder()

	setupVMRouter(svc).ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusBadRequest, w.Body.String())
	}
	var response map[string]string
	if err := json.NewDecoder(w.Body).Decode(&response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got := response["error"]; got != `validation: profile: unknown VM profile "github-runner"` {
		t.Fatalf("error = %q", got)
	}
}

func setupVMRouter(service *vmservice.Service) chi.Router {
	h := NewVMHandler(service)
	r := chi.NewRouter()
	r.Post("/vms/plan", h.PlanCreate)
	r.Post("/vms", h.Create)
	r.Post("/vms/{ref}/exec", h.Exec)
	r.Put("/vms/{ref}/files", h.UploadFile)
	r.Get("/vms/{ref}/forward/{port}", h.Forward)
	return r
}

func setupVMUploadTestService(t *testing.T) (*vmservice.Service, *recordingVMMachineService) {
	t.Helper()
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	now := time.Now().UTC()
	if err := db.CreateImage(&models.Image{
		ID:         "image-vm-ready",
		Name:       "vm-ready-image",
		Tag:        "test",
		RootfsPath: "/tmp/rootfs.ext4",
		CreatedAt:  now,
	}); err != nil {
		t.Fatalf("CreateImage: %v", err)
	}
	machineRecord := &models.Machine{
		ID:        "machine-vm-ready",
		Name:      "vm-ready-machine",
		ImageID:   "image-vm-ready",
		State:     models.MachineStateRunning,
		VCPUs:     1,
		CPU:       models.CPUConfig{Kind: "shared", VCPUs: 1},
		MemoryMB:  256,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := db.CreateMachine(machineRecord); err != nil {
		t.Fatalf("CreateMachine: %v", err)
	}
	if err := db.CreateVM(&models.VMRecord{
		ID:         "vm-ready",
		Name:       "ready",
		Profile:    "alpine",
		MachineID:  machineRecord.ID,
		ImageID:    "image-vm-ready",
		DiskSizeMB: 128,
		CreatedAt:  now,
		UpdatedAt:  now,
	}); err != nil {
		t.Fatalf("CreateVM: %v", err)
	}
	machines := &recordingVMMachineService{machine: machineRecord}
	return vmservice.NewService(db, nil, machines), machines
}

type recordingVMMachineService struct {
	machine     *models.Machine
	uploadID    string
	uploadPath  string
	uploadMode  uint32
	uploadSize  int64
	uploadBody  []byte
	execID      string
	execCommand string
	execBody    []byte
	forward     func(id string, port int) (io.ReadWriteCloser, error)
}

func (s *recordingVMMachineService) Create(context.Context, *models.CreateMachineRequest) (*models.Machine, error) {
	return nil, fmt.Errorf("unexpected Create")
}

func (s *recordingVMMachineService) Get(id string) (*models.Machine, error) {
	if id != s.machine.ID {
		return nil, &apierr.NotFoundError{Resource: "machine", ID: id}
	}
	clone := *s.machine
	return &clone, nil
}

func (s *recordingVMMachineService) List() ([]models.Machine, error) {
	return nil, fmt.Errorf("unexpected List")
}

func (s *recordingVMMachineService) Stop(string) error {
	return fmt.Errorf("unexpected Stop")
}

func (s *recordingVMMachineService) Start(context.Context, string) (*models.Machine, error) {
	return nil, fmt.Errorf("unexpected Start")
}

func (s *recordingVMMachineService) Delete(string) error {
	return fmt.Errorf("unexpected Delete")
}

func (s *recordingVMMachineService) Exec(ctx context.Context, id, command string, stdin io.Reader) (*execution.BufferedResult, error) {
	if id != s.machine.ID {
		return nil, &apierr.NotFoundError{Resource: "machine", ID: id}
	}
	if stdin == nil {
		return &execution.BufferedResult{ExitCode: 0, Stdout: "kiln-vm-ready\n"}, nil
	}
	body, err := io.ReadAll(stdin)
	if err != nil {
		return nil, fmt.Errorf("read exec stdin: %w", err)
	}
	s.execID = id
	s.execCommand = command
	s.execBody = body
	return &execution.BufferedResult{ExitCode: 0, Stdout: "ok\n"}, nil
}

func (s *recordingVMMachineService) Execute(ctx context.Context, id string, request execution.Request) execution.Outcome {
	result, err := s.Exec(ctx, id, request.Command, request.Stdin)
	if err != nil {
		return execution.OutcomeFromError(err)
	}
	if request.Started != nil {
		if err := request.Started(); err != nil {
			return execution.OutcomeFromError(err)
		}
	}
	if request.Stdout != nil {
		if _, err := io.WriteString(request.Stdout, result.Stdout); err != nil {
			return execution.OutcomeFromError(err)
		}
	}
	if request.Stderr != nil {
		if _, err := io.WriteString(request.Stderr, result.Stderr); err != nil {
			return execution.OutcomeFromError(err)
		}
	}
	return execution.Outcome{ExitCode: result.ExitCode}
}

func (s *recordingVMMachineService) Upload(_ context.Context, id, remotePath string, mode uint32, size int64, reader io.Reader) error {
	body, err := io.ReadAll(reader)
	if err != nil {
		return fmt.Errorf("read upload body: %w", err)
	}
	s.uploadID = id
	s.uploadPath = remotePath
	s.uploadMode = mode
	s.uploadSize = size
	s.uploadBody = body
	return nil
}

func (s *recordingVMMachineService) DialShell(context.Context, string) (io.ReadWriteCloser, error) {
	return nil, fmt.Errorf("unexpected DialShell")
}

func (s *recordingVMMachineService) DialForward(_ context.Context, id string, port int) (io.ReadWriteCloser, error) {
	if s.forward == nil {
		return nil, fmt.Errorf("unexpected DialForward")
	}
	return s.forward(id, port)
}

func (s *recordingVMMachineService) SSHKey(string) ([]byte, error) {
	return nil, fmt.Errorf("unexpected SSHKey")
}
