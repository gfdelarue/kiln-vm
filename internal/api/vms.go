package api

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/gorilla/websocket"

	"github.com/gfdelarue/kiln-vm/internal/execution"
	"github.com/gfdelarue/kiln-vm/internal/guestagent"
	"github.com/gfdelarue/kiln-vm/internal/models"
	vmservice "github.com/gfdelarue/kiln-vm/internal/vm"
)

var shellUpgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

type VMHandler struct {
	service *vmservice.Service
}

func NewVMHandler(service *vmservice.Service) *VMHandler {
	return &VMHandler{service: service}
}

func (h *VMHandler) Profiles(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, h.service.Profiles())
}

func (h *VMHandler) ListProfileImages(w http.ResponseWriter, r *http.Request) {
	images, err := h.service.ListProfileImages()
	if err != nil {
		writeServiceError(w, err)
		return
	}
	if images == nil {
		images = []models.VMProfileImage{}
	}
	writeJSON(w, http.StatusOK, images)
}

func (h *VMHandler) EnsureProfileImage(w http.ResponseWriter, r *http.Request) {
	var req models.EnsureVMProfileImageRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	image, err := h.service.EnsureProfileImage(r.Context(), &req)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, image)
}

func (h *VMHandler) PrepareProfile(w http.ResponseWriter, r *http.Request) {
	var req models.PrepareVMProfileRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	image, err := h.service.PrepareProfile(r.Context(), chi.URLParam(r, "profile"), &req)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, image)
}

func (h *VMHandler) DeleteProfileImage(w http.ResponseWriter, r *http.Request) {
	if err := h.service.DeleteProfileImage(chi.URLParam(r, "id")); err != nil {
		writeServiceError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *VMHandler) DeleteProfileImagesByProfile(w http.ResponseWriter, r *http.Request) {
	if err := h.service.DeleteProfileImagesByProfile(chi.URLParam(r, "profile")); err != nil {
		writeServiceError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *VMHandler) Create(w http.ResponseWriter, r *http.Request) {
	var req models.CreateVMRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	vm, err := h.service.Create(r.Context(), &req)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, vm)
}

func (h *VMHandler) PlanCreate(w http.ResponseWriter, r *http.Request) {
	var req models.CreateVMRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	plan, err := h.service.PlanCreate(&req)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, plan)
}

func (h *VMHandler) List(w http.ResponseWriter, r *http.Request) {
	vms, err := h.service.List()
	if err != nil {
		writeServiceError(w, err)
		return
	}
	if vms == nil {
		vms = []models.VM{}
	}
	writeJSON(w, http.StatusOK, vms)
}

func (h *VMHandler) Get(w http.ResponseWriter, r *http.Request) {
	vm, err := h.service.Get(chi.URLParam(r, "ref"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, vm)
}

func (h *VMHandler) Start(w http.ResponseWriter, r *http.Request) {
	vm, err := h.service.Start(r.Context(), chi.URLParam(r, "ref"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, vm)
}

func (h *VMHandler) Stop(w http.ResponseWriter, r *http.Request) {
	if err := h.service.Stop(chi.URLParam(r, "ref")); err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "stopping"})
}

func (h *VMHandler) UpdatePower(w http.ResponseWriter, r *http.Request) {
	var req models.UpdateVMPowerRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	vm, err := h.service.UpdatePowerPolicy(r.Context(), chi.URLParam(r, "ref"), req.Policy)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, vm)
}

func (h *VMHandler) VerifyCapabilities(w http.ResponseWriter, r *http.Request) {
	var req models.VerifyVMCapabilitiesRequest
	if err := decodeOptionalJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	conditions, err := h.service.VerifyCapabilities(r.Context(), chi.URLParam(r, "ref"), &req)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, models.VerifyVMCapabilitiesResponse{Conditions: conditions})
}

func (h *VMHandler) Delete(w http.ResponseWriter, r *http.Request) {
	if err := h.service.Delete(chi.URLParam(r, "ref")); err != nil {
		writeServiceError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *VMHandler) Exec(w http.ResponseWriter, r *http.Request) {
	ref := chi.URLParam(r, "ref")
	serveExec(w, r, func(ctx context.Context, request execution.Request) execution.Outcome {
		return h.service.Execute(ctx, ref, request)
	})
}

func (h *VMHandler) UploadFile(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	remotePath := r.URL.Query().Get("path")
	if remotePath == "" {
		writeError(w, http.StatusBadRequest, "path query parameter is required")
		return
	}
	if r.ContentLength < 0 {
		writeError(w, http.StatusLengthRequired, "content-length is required")
		return
	}
	mode, err := parseUploadFileMode(r.URL.Query().Get("mode"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.service.Upload(r.Context(), chi.URLParam(r, "ref"), remotePath, mode, r.ContentLength, r.Body); err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "uploaded",
		"path":   remotePath,
		"bytes":  r.ContentLength,
		"mode":   strconv.FormatUint(uint64(mode), 8),
	})
}

func parseUploadFileMode(raw string) (uint32, error) {
	if raw == "" {
		return 0644, nil
	}
	mode, err := strconv.ParseUint(raw, 8, 32)
	if err != nil || mode > 0777 {
		return 0, errInvalidUploadMode
	}
	return uint32(mode), nil
}

var errInvalidUploadMode = errors.New("mode must be an octal permission between 0000 and 0777")

func (h *VMHandler) Shell(w http.ResponseWriter, r *http.Request) {
	ref := chi.URLParam(r, "ref")

	wsConn, err := shellUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer wsConn.Close()

	agentConn, err := h.service.DialShell(r.Context(), ref)
	if err != nil {
		_ = wsConn.WriteJSON(guestagent.ShellFrame{
			Type:    "error",
			Version: guestagent.ProtocolVersion,
			Error:   err.Error(),
		})
		return
	}
	defer agentConn.Close()

	errCh := make(chan error, 2)

	go func() {
		reader := bufio.NewReader(agentConn)
		for {
			line, err := reader.ReadBytes('\n')
			if err != nil {
				errCh <- err
				return
			}
			line = bytes.TrimSpace(line)
			if len(line) == 0 {
				continue
			}
			if err := wsConn.WriteMessage(websocket.TextMessage, line); err != nil {
				errCh <- err
				return
			}
		}
	}()

	go func() {
		for {
			_, msg, err := wsConn.ReadMessage()
			if err != nil {
				errCh <- err
				return
			}
			msg = bytes.TrimSpace(msg)
			if len(msg) == 0 {
				continue
			}
			msg = append(msg, '\n')
			if _, err := agentConn.Write(msg); err != nil {
				errCh <- err
				return
			}
		}
	}()

	<-errCh
}

const (
	forwardCopyBufferBytes   = 32 * 1024
	websocketCloseReasonMax  = 123
	websocketCloseWriteGrace = time.Second
)

func (h *VMHandler) Forward(w http.ResponseWriter, r *http.Request) {
	ref := chi.URLParam(r, "ref")
	port, err := strconv.Atoi(chi.URLParam(r, "port"))
	if err != nil || !guestagent.ValidForwardTargetPort(port) {
		writeError(w, http.StatusBadRequest, "port must be an integer between 1 and 65535")
		return
	}

	wsConn, err := shellUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer wsConn.Close()

	agentConn, err := h.service.DialForward(r.Context(), ref, port)
	if err != nil {
		writeWebSocketClose(wsConn, websocket.CloseInternalServerErr, err.Error())
		return
	}
	spliceWebSocket(wsConn, agentConn)
}

func spliceWebSocket(wsConn *websocket.Conn, conn io.ReadWriteCloser) {
	done := make(chan struct{}, 2)
	go func() {
		defer func() { done <- struct{}{} }()
		buf := make([]byte, forwardCopyBufferBytes)
		for {
			n, err := conn.Read(buf)
			if n > 0 {
				if writeErr := wsConn.WriteMessage(websocket.BinaryMessage, buf[:n]); writeErr != nil {
					return
				}
			}
			if errors.Is(err, io.EOF) {
				writeWebSocketClose(wsConn, websocket.CloseNormalClosure, "")
				return
			}
			if err != nil {
				return
			}
		}
	}()
	go func() {
		defer func() { done <- struct{}{} }()
		for {
			_, msg, err := wsConn.ReadMessage()
			if err != nil {
				return
			}
			if _, err := conn.Write(msg); err != nil {
				return
			}
		}
	}()
	<-done
	_ = conn.Close()
	_ = wsConn.Close()
	<-done
}

func writeWebSocketClose(wsConn *websocket.Conn, code int, reason string) {
	message := websocket.FormatCloseMessage(code, websocketCloseReason(reason))
	_ = wsConn.WriteControl(websocket.CloseMessage, message, time.Now().Add(websocketCloseWriteGrace))
}

func websocketCloseReason(reason string) string {
	if len(reason) <= websocketCloseReasonMax {
		return reason
	}
	const ellipsis = "..."
	tail := reason[len(reason)-(websocketCloseReasonMax-len(ellipsis)):]
	for len(tail) > 0 && !utf8.RuneStart(tail[0]) {
		tail = tail[1:]
	}
	return ellipsis + tail
}
