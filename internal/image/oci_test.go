package image

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFetchOCIRootfsDownloadsVerifiesAndWritesSparse(t *testing.T) {
	rootfs := make([]byte, 3*1024*1024)
	copy(rootfs[1024*1024:], []byte("kiln-vm-rootfs"))
	rootfsDigest := "sha256:" + sha256Hex(rootfs)
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	if _, err := zw.Write(rootfs); err != nil {
		t.Fatalf("gzip rootfs: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	layer := gz.Bytes()
	layerDigest := "sha256:" + sha256Hex(layer)
	manifest := map[string]any{
		"schemaVersion": 2,
		"config": map[string]any{
			"mediaType": "application/vnd.kiln-vm.vm-profile.config.v1+json",
			"digest":    "sha256:" + strings.Repeat("0", 64),
			"size":      2,
		},
		"layers": []map[string]any{{
			"mediaType": OCIRootfsGzipMediaType,
			"digest":    layerDigest,
			"size":      len(layer),
			"annotations": map[string]string{
				"io.kiln-vm.rootfs.sha256": rootfsDigest,
			},
		}},
	}
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	manifestDigest := "sha256:" + sha256Hex(manifestBytes)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/example/profile/manifests/" + manifestDigest:
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			_, _ = w.Write(manifestBytes)
		case "/v2/example/profile/blobs/" + layerDigest:
			w.Header().Set("Content-Type", OCIRootfsGzipMediaType)
			_, _ = w.Write(layer)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	target := filepath.Join(t.TempDir(), "rootfs.ext4")
	ref := strings.TrimPrefix(server.URL, "https://") + "/example/profile@" + manifestDigest
	if err := FetchOCIRootfs(context.Background(), OCIRootfsRequest{
		Reference:    ref,
		LayerDigest:  strings.TrimPrefix(layerDigest, "sha256:"),
		RootfsSHA256: strings.TrimPrefix(rootfsDigest, "sha256:"),
		TargetPath:   target,
		HTTPClient:   server.Client(),
	}); err != nil {
		t.Fatalf("FetchOCIRootfs: %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile(target): %v", err)
	}
	if !bytes.Equal(got, rootfs) {
		t.Fatal("downloaded rootfs content mismatch")
	}
	allocated, err := allocatedBytes(target)
	if err != nil {
		t.Fatalf("allocatedBytes: %v", err)
	}
	if allocated >= uint64(len(rootfs)) {
		t.Fatalf("downloaded rootfs allocated %d bytes, want sparse allocation below %d", allocated, len(rootfs))
	}
}

func TestParseOCIReferenceStripsTagBeforeDigest(t *testing.T) {
	ref, err := parseOCIReference("ghcr.io/example/profile:v1-amd64@sha256:" + strings.Repeat("a", 64))
	if err != nil {
		t.Fatalf("parseOCIReference: %v", err)
	}
	if ref.Registry != "ghcr.io" {
		t.Fatalf("Registry = %q", ref.Registry)
	}
	if ref.Repo != "example/profile" {
		t.Fatalf("Repo = %q, want tag stripped repository", ref.Repo)
	}
	if ref.Ref != "sha256:"+strings.Repeat("a", 64) {
		t.Fatalf("Ref = %q", ref.Ref)
	}
}

func TestFetchHTTPRootfsDownloadsVerifiesAndWritesSparse(t *testing.T) {
	rootfs := make([]byte, 3*1024*1024)
	copy(rootfs[1024*1024:], []byte("kiln-vm-http-rootfs"))
	rootfsDigest := "sha256:" + sha256Hex(rootfs)
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	if _, err := zw.Write(rootfs); err != nil {
		t.Fatalf("gzip rootfs: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	layer := gz.Bytes()
	layerDigest := "sha256:" + sha256Hex(layer)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/profiles/alpine/rootfs.ext4.gz" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", OCIRootfsGzipMediaType)
		_, _ = w.Write(layer)
	}))
	defer server.Close()

	target := filepath.Join(t.TempDir(), "rootfs.ext4")
	if err := FetchHTTPRootfs(context.Background(), HTTPRootfsRequest{
		URL:              server.URL + "/profiles/alpine/rootfs.ext4.gz",
		CompressedSHA256: strings.TrimPrefix(layerDigest, "sha256:"),
		RootfsSHA256:     strings.TrimPrefix(rootfsDigest, "sha256:"),
		TargetPath:       target,
		HTTPClient:       server.Client(),
	}); err != nil {
		t.Fatalf("FetchHTTPRootfs: %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile(target): %v", err)
	}
	if !bytes.Equal(got, rootfs) {
		t.Fatal("downloaded rootfs content mismatch")
	}
	allocated, err := allocatedBytes(target)
	if err != nil {
		t.Fatalf("allocatedBytes: %v", err)
	}
	if allocated >= uint64(len(rootfs)) {
		t.Fatalf("downloaded rootfs allocated %d bytes, want sparse allocation below %d", allocated, len(rootfs))
	}
}

func TestFetchHTTPRootfsFailsWhenDownloadStallsAndRemovesPartial(t *testing.T) {
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write([]byte("rootfs payload that will never fully arrive"))
	_ = zw.Close()
	layer := gz.Bytes()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/profiles/stalled.ext4.gz" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", OCIRootfsGzipMediaType)
		_, _ = w.Write(layer[:10])
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		<-r.Context().Done()
	}))
	defer server.Close()

	target := filepath.Join(t.TempDir(), "rootfs.ext4")
	started := time.Now()
	err := FetchHTTPRootfs(context.Background(), HTTPRootfsRequest{
		URL:          server.URL + "/profiles/stalled.ext4.gz",
		TargetPath:   target,
		HTTPClient:   server.Client(),
		StallTimeout: 50 * time.Millisecond,
	})
	if err == nil || !strings.Contains(err.Error(), "stalled") {
		t.Fatalf("FetchHTTPRootfs error = %v, want stalled download", err)
	}
	var stalled *RootfsDownloadStalledError
	if !errors.As(err, &stalled) {
		t.Fatalf("FetchHTTPRootfs error = %T %v, want RootfsDownloadStalledError", err, err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("FetchHTTPRootfs took %s, want stall timeout to fail promptly", elapsed)
	}
	if _, statErr := os.Stat(target); !os.IsNotExist(statErr) {
		t.Fatalf("target stat error = %v, want target removed", statErr)
	}
	if _, statErr := os.Stat(target + ".partial"); !os.IsNotExist(statErr) {
		t.Fatalf("partial stat error = %v, want partial removed", statErr)
	}
}

func TestFetchOCIRootfsRejectsDigestMismatch(t *testing.T) {
	rootfs := []byte("not sparse")
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write(rootfs)
	_ = zw.Close()
	layer := gz.Bytes()
	layerDigest := "sha256:" + sha256Hex(layer)
	manifest := fmt.Sprintf(`{"schemaVersion":2,"config":{"mediaType":"x","digest":"sha256:%s","size":2},"layers":[{"mediaType":%q,"digest":%q,"size":%d}]}`,
		strings.Repeat("0", 64),
		OCIRootfsGzipMediaType,
		layerDigest,
		len(layer),
	)
	manifestDigest := "sha256:" + sha256Hex([]byte(manifest))
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/example/profile/manifests/" + manifestDigest:
			_, _ = w.Write([]byte(manifest))
		case "/v2/example/profile/blobs/" + layerDigest:
			_, _ = w.Write(layer)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	ref := strings.TrimPrefix(server.URL, "https://") + "/example/profile@" + manifestDigest
	err := FetchOCIRootfs(context.Background(), OCIRootfsRequest{
		Reference:    ref,
		LayerDigest:  layerDigest,
		RootfsSHA256: "sha256:" + strings.Repeat("1", 64),
		TargetPath:   filepath.Join(t.TempDir(), "rootfs.ext4"),
		HTTPClient:   server.Client(),
	})
	if err == nil || !strings.Contains(err.Error(), "rootfs digest mismatch") {
		t.Fatalf("FetchOCIRootfs error = %v, want rootfs digest mismatch", err)
	}
}

func TestFetchOCIRootfsAcceptsOCIArtifactManifest(t *testing.T) {
	rootfs := []byte("artifact-rootfs")
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write(rootfs)
	_ = zw.Close()
	layer := gz.Bytes()
	layerDigest := "sha256:" + sha256Hex(layer)
	rootfsDigest := "sha256:" + sha256Hex(rootfs)
	manifest := map[string]any{
		"schemaVersion": 2,
		"artifactType":  "application/vnd.kiln-vm.vm-profile.v1",
		"blobs": []map[string]any{{
			"mediaType": OCIRootfsGzipMediaType,
			"digest":    layerDigest,
			"size":      len(layer),
		}},
	}
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	manifestDigest := "sha256:" + sha256Hex(manifestBytes)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/example/profile/manifests/" + manifestDigest:
			w.Header().Set("Content-Type", "application/vnd.oci.artifact.manifest.v1+json")
			_, _ = w.Write(manifestBytes)
		case "/v2/example/profile/blobs/" + layerDigest:
			w.Header().Set("Content-Type", OCIRootfsGzipMediaType)
			_, _ = w.Write(layer)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	target := filepath.Join(t.TempDir(), "rootfs.ext4")
	ref := strings.TrimPrefix(server.URL, "https://") + "/example/profile@" + manifestDigest
	if err := FetchOCIRootfs(context.Background(), OCIRootfsRequest{
		Reference:    ref,
		LayerDigest:  layerDigest,
		RootfsSHA256: rootfsDigest,
		TargetPath:   target,
		HTTPClient:   server.Client(),
	}); err != nil {
		t.Fatalf("FetchOCIRootfs: %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile(target): %v", err)
	}
	if !bytes.Equal(got, rootfs) {
		t.Fatalf("downloaded rootfs = %q, want %q", got, rootfs)
	}
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
