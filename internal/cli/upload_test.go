package cli

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUploadSendsTheFileWithItsPermissions(t *testing.T) {
	local := filepath.Join(t.TempDir(), "tool.sh")
	if err := os.WriteFile(local, []byte("#!/bin/sh\necho hi\n"), 0o750); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.Method != http.MethodPut || r.URL.Path != "/vms/dev/files" {
			t.Errorf("request = %s %s, want PUT /vms/dev/files", r.Method, r.URL.Path)
		}
		if got := r.URL.Query().Get("path"); got != `C:\tools\tool.sh` {
			t.Errorf("path = %q", got)
		}
		if got := r.URL.Query().Get("mode"); got != "750" {
			t.Errorf("mode = %q, want 750", got)
		}
		if r.ContentLength != int64(len(body)) || string(body) != "#!/bin/sh\necho hi\n" {
			t.Errorf("content length %d, body %q", r.ContentLength, body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"uploaded","path":"C:\\tools\\tool.sh","bytes":18,"mode":"750"}`))
	}))
	defer server.Close()
	client := mustNewClient(t, server.URL, "")
	client.HTTPClient = server.Client()

	result, err := client.Upload("dev", local, `C:\tools\tool.sh`, "")
	if err != nil {
		t.Fatal(err)
	}
	if result.Bytes != 18 || result.Path != `C:\tools\tool.sh` {
		t.Fatalf("result = %+v", result)
	}
}

func TestUploadRejectsDirectoriesBeforeCallingTheAPI(t *testing.T) {
	client := mustNewClient(t, "http://127.0.0.1:1", "")

	_, err := client.Upload("dev", t.TempDir(), "/root/x", "")
	if err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("err = %v, want not a regular file", err)
	}
}
