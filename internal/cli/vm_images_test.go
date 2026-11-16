package cli

import (
	"archive/tar"
	"bytes"
	"encoding/base64"
	"io"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestParseVMCreateCommandCarriesImageAndLabels(t *testing.T) {
	cmd := newVMCreateParseTestCommand()
	for _, set := range [][2]string{{"image", "saved-env"}, {"label", "owner=kiln-run"}, {"label", "env=a=b"}, {"label", "empty="}} {
		if err := cmd.Flags().Set(set[0], set[1]); err != nil {
			t.Fatalf("set %s: %v", set[0], err)
		}
	}

	req, _, _, err := parseVMCreateCommand(cmd, "from-image")
	if err != nil {
		t.Fatalf("parseVMCreateCommand: %v", err)
	}
	want := map[string]string{"owner": "kiln-run", "env": "a=b", "empty": ""}
	if req.Image != "saved-env" || req.Profile != "" || !maps.Equal(req.Labels, want) {
		t.Fatalf("request = %+v, want image saved-env and labels %v", req, want)
	}
}

func TestLabelFlagsRequireKeyValue(t *testing.T) {
	for _, value := range []string{"owner", "=v"} {
		cmd := &cobra.Command{}
		cmd.Flags().StringArray("label", nil, "")
		if err := cmd.Flags().Set("label", value); err != nil {
			t.Fatalf("set label: %v", err)
		}
		if _, err := labelFlags(cmd); err == nil || !strings.Contains(err.Error(), "key=value") {
			t.Errorf("labelFlags(%q) error = %v, want key=value error", value, err)
		}
	}

	cmd := &cobra.Command{}
	cmd.Flags().StringArray("label", nil, "")
	if labels, err := labelFlags(cmd); err != nil || labels != nil {
		t.Fatalf("labelFlags without flags = %v, %v; want nil", labels, err)
	}
}

func TestWithLabelQueryRepeatsLabelParameter(t *testing.T) {
	if got := withLabelQuery("/vms", nil); got != "/vms" {
		t.Fatalf("withLabelQuery(nil) = %q", got)
	}
	got := withLabelQuery("/vm-images", []string{"owner=kiln-run", "env"})
	path, rawQuery, _ := strings.Cut(got, "?")
	query, err := url.ParseQuery(rawQuery)
	if err != nil {
		t.Fatalf("parse query %q: %v", rawQuery, err)
	}
	if path != "/vm-images" || strings.Join(query["label"], ",") != "owner=kiln-run,env" {
		t.Fatalf("withLabelQuery = %q", got)
	}
}

func TestFormatLabelsSortsKeys(t *testing.T) {
	if got := formatLabels(map[string]string{"b": "2", "a": "1"}); got != "a=1,b=2" {
		t.Fatalf("formatLabels = %q", got)
	}
	if got := formatLabels(nil); got != "" {
		t.Fatalf("formatLabels(nil) = %q", got)
	}
}

func newVMImageBuildParseTestCommand() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Flags().StringP("file", "f", "", "")
	cmd.Flags().String("context", "", "")
	cmd.Flags().String("profile", "", "")
	cmd.Flags().String("disk", "", "")
	cmd.Flags().StringArray("label", nil, "")
	return cmd
}

func TestParseVMImageBuildCommandArchivesContext(t *testing.T) {
	dir := t.TempDir()
	dockerfile := "FROM alpine:3.20\nCOPY app/hello.txt /hello.txt\n"
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(dockerfile), 0o644); err != nil {
		t.Fatalf("write Dockerfile: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "app"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "app", "hello.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatalf("write context file: %v", err)
	}
	cmd := newVMImageBuildParseTestCommand()
	for _, set := range [][2]string{{"context", dir}, {"profile", "alpine"}, {"disk", "2G"}, {"label", "team=ci"}} {
		if err := cmd.Flags().Set(set[0], set[1]); err != nil {
			t.Fatalf("set %s: %v", set[0], err)
		}
	}

	req, err := parseVMImageBuildCommand(cmd, "built")
	if err != nil {
		t.Fatalf("parseVMImageBuildCommand: %v", err)
	}
	if req.Name != "built" || req.Dockerfile != dockerfile || req.Profile != "alpine" || req.DiskSizeMB != 2048 || req.Labels["team"] != "ci" || req.FromVM != "" {
		t.Fatalf("request = %+v", req)
	}
	archive, err := base64.StdEncoding.DecodeString(req.Context)
	if err != nil {
		t.Fatalf("decode context: %v", err)
	}
	files := map[string]string{}
	tr := tar.NewReader(bytes.NewReader(archive))
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read context archive: %v", err)
		}
		body, _ := io.ReadAll(tr)
		files[header.Name] = string(body)
	}
	if files["app/hello.txt"] != "hello" || files["Dockerfile"] != dockerfile {
		t.Fatalf("context archive files = %v", files)
	}
	if _, ok := files["app"]; !ok {
		t.Fatalf("context archive lacks the app directory: %v", files)
	}
}

func TestParseVMImageBuildCommandWithoutContext(t *testing.T) {
	cmd := newVMImageBuildParseTestCommand()
	if _, err := parseVMImageBuildCommand(cmd, "built"); err == nil || !strings.Contains(err.Error(), "--file") {
		t.Fatalf("missing Dockerfile error = %v, want --file error", err)
	}

	path := filepath.Join(t.TempDir(), "Dockerfile.ci")
	if err := os.WriteFile(path, []byte("FROM alpine:3.20\n"), 0o644); err != nil {
		t.Fatalf("write Dockerfile: %v", err)
	}
	if err := cmd.Flags().Set("file", path); err != nil {
		t.Fatalf("set file: %v", err)
	}
	req, err := parseVMImageBuildCommand(cmd, "built")
	if err != nil {
		t.Fatalf("parseVMImageBuildCommand: %v", err)
	}
	if req.Dockerfile != "FROM alpine:3.20\n" || req.Context != "" || req.DiskSizeMB != 0 || req.Profile != "" {
		t.Fatalf("request = %+v", req)
	}
}

func TestVMImageCommandDoesNotShadowProfileImages(t *testing.T) {
	names := map[string]bool{}
	for _, cmd := range rootCmd.Commands() {
		if names[cmd.Name()] {
			t.Fatalf("duplicate root command %q", cmd.Name())
		}
		names[cmd.Name()] = true
	}
	if !names["vm-images"] {
		t.Fatalf("root commands %v lack vm-images", names)
	}
}
