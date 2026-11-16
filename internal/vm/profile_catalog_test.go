package vm

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestLoadProfileImageCatalogFromHTTPURL(t *testing.T) {
	var authorization string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{
			"schema_version": 1,
			"images": [{
				"profile": "ubuntu-docker",
				"profile_version": 10,
				"fingerprint": "abc",
				"arch": "amd64",
				"rootfs_size_mb": 4096,
				"reference": "https://example.invalid/rootfs.ext4.gz",
				"layer_digest": "sha256:layer",
				"rootfs_sha256": "sha256:rootfs"
			}]
		}`)
	}))
	defer server.Close()

	catalog, err := LoadProfileImageCatalogWithToken(server.URL, "test-token")
	if err != nil {
		t.Fatalf("LoadProfileImageCatalogWithToken() error = %v", err)
	}
	if authorization != "Bearer test-token" {
		t.Fatalf("Authorization = %q", authorization)
	}
	if catalog.SchemaVersion != 1 || len(catalog.Images) != 1 {
		t.Fatalf("unexpected catalog: %+v", catalog)
	}
	if got := catalog.Images[0].Reference; got != "https://example.invalid/rootfs.ext4.gz" {
		t.Fatalf("catalog reference = %q", got)
	}
}

func TestLoadProfileImageCatalogFromHTTPURLRejectsErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no catalog", http.StatusNotFound)
	}))
	defer server.Close()

	if _, err := LoadProfileImageCatalogWithToken(server.URL, ""); err == nil {
		t.Fatal("LoadProfileImageCatalogWithToken() error = nil, want HTTP error")
	}
}
