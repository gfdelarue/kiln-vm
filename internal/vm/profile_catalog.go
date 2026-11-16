package vm

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"sort"
	"strings"
	"time"
)

const (
	ProfileImageModeLocal = "local"
	ProfileImageModeAuto  = "auto"
	ProfileImageModePull  = "pull"
)

type ProfileImageSource struct {
	Mode        string
	CatalogPath string
	Token       string
}

type ProfileImageCatalog struct {
	SchemaVersion int                        `json:"schema_version"`
	Images        []ProfileImageCatalogEntry `json:"images"`
}

type ProfileImageCatalogEntry struct {
	Profile        string `json:"profile"`
	ProfileVersion int    `json:"profile_version"`
	Fingerprint    string `json:"fingerprint"`
	Arch           string `json:"arch"`
	RootfsSizeMB   int64  `json:"rootfs_size_mb"`
	Reference      string `json:"reference"`
	LayerDigest    string `json:"layer_digest"`
	RootfsSHA256   string `json:"rootfs_sha256"`
	LayerMediaType string `json:"layer_media_type,omitempty"`
	Source         string `json:"source,omitempty"`
	CreatedAt      string `json:"created_at,omitempty"`
}

const maxProfileImageCatalogBytes = 4 << 20

func LoadProfileImageCatalogWithToken(location string, token string) (*ProfileImageCatalog, error) {
	location = strings.TrimSpace(location)
	if location == "" {
		return &ProfileImageCatalog{SchemaVersion: 1}, nil
	}
	var data []byte
	var err error
	if isHTTPProfileCatalog(location) {
		data, err = readProfileImageCatalogURL(location, token)
	} else {
		data, err = os.ReadFile(location)
	}
	if err != nil {
		if os.IsNotExist(err) {
			return &ProfileImageCatalog{SchemaVersion: 1}, nil
		}
		return nil, fmt.Errorf("read profile image catalog %s: %w", location, err)
	}
	var catalog ProfileImageCatalog
	if err := json.Unmarshal(data, &catalog); err != nil {
		return nil, fmt.Errorf("decode profile image catalog: %w", err)
	}
	if catalog.SchemaVersion != 1 {
		return nil, fmt.Errorf("unsupported profile image catalog schema_version %d", catalog.SchemaVersion)
	}
	return &catalog, nil
}

func isHTTPProfileCatalog(location string) bool {
	u, err := url.Parse(location)
	if err != nil {
		return false
	}
	return u.Scheme == "http" || u.Scheme == "https"
}

func readProfileImageCatalogURL(location string, token string) ([]byte, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	req, err := http.NewRequest(http.MethodGet, location, nil)
	if err != nil {
		return nil, fmt.Errorf("create profile image catalog request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if strings.TrimSpace(token) != "" {
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(token))
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch profile image catalog: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	limited := io.LimitReader(resp.Body, maxProfileImageCatalogBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("read profile image catalog: %w", err)
	}
	if len(data) > maxProfileImageCatalogBytes {
		return nil, fmt.Errorf("catalog exceeds %d bytes", maxProfileImageCatalogBytes)
	}
	return data, nil
}

func (c *ProfileImageCatalog) Match(profile Profile, fingerprint string, rootfsSizeMB int64) *ProfileImageCatalogEntry {
	if c == nil {
		return nil
	}
	arch := runtime.GOARCH
	candidates := make([]ProfileImageCatalogEntry, 0, len(c.Images))
	for _, entry := range c.Images {
		if entry.Profile != profile.Name ||
			entry.ProfileVersion != profile.Version ||
			entry.Arch != arch ||
			entry.RootfsSizeMB <= 0 ||
			entry.RootfsSizeMB > rootfsSizeMB ||
			entry.Reference == "" ||
			entry.LayerDigest == "" ||
			entry.RootfsSHA256 == "" {
			continue
		}
		if entry.Fingerprint != "" && entry.Fingerprint != fingerprint {
			continue
		}
		candidates = append(candidates, entry)
	}
	if len(candidates) == 0 {
		return nil
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].RootfsSizeMB > candidates[j].RootfsSizeMB
	})
	return &candidates[0]
}

func NormalizeProfileImageMode(mode string) (string, error) {
	mode = strings.TrimSpace(mode)
	if mode == "" {
		return ProfileImageModeLocal, nil
	}
	switch mode {
	case ProfileImageModeLocal, ProfileImageModeAuto, ProfileImageModePull:
		return mode, nil
	default:
		return "", fmt.Errorf("profile image mode must be one of: local, auto, pull")
	}
}

func CatalogFingerprint(profile Profile, runtimeFingerprint string) string {
	return profileFingerprint(profile, runtimeFingerprint)
}
