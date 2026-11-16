package image

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	OCIRootfsGzipMediaType     = "application/vnd.kiln-vm.rootfs.ext4.gzip"
	DefaultOCIPullTimeout      = 30 * time.Minute
	DefaultOCIPullStallTimeout = 90 * time.Second
)

type OCIRootfsRequest struct {
	Reference      string
	LayerDigest    string
	RootfsSHA256   string
	LayerMediaType string
	TargetPath     string
	Token          string
	HTTPClient     *http.Client
	StallTimeout   time.Duration
}

type HTTPRootfsRequest struct {
	URL              string
	CompressedSHA256 string
	RootfsSHA256     string
	TargetPath       string
	Token            string
	HTTPClient       *http.Client
	StallTimeout     time.Duration
}

type RootfsDownloadStalledError struct {
	Timeout time.Duration
}

func (e *RootfsDownloadStalledError) Error() string {
	return fmt.Sprintf("profile rootfs download stalled for %s", e.Timeout)
}

type ociReference struct {
	Registry string
	Repo     string
	Ref      string
}

type ociManifest struct {
	SchemaVersion int    `json:"schemaVersion"`
	ArtifactType  string `json:"artifactType,omitempty"`
	Config        struct {
		MediaType string `json:"mediaType"`
		Digest    string `json:"digest"`
		Size      int64  `json:"size"`
	} `json:"config,omitempty"`
	Layers []ociDescriptor `json:"layers"`
	Blobs  []ociDescriptor `json:"blobs,omitempty"`
}

type ociDescriptor struct {
	MediaType   string            `json:"mediaType"`
	Digest      string            `json:"digest"`
	Size        int64             `json:"size"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

func FetchOCIRootfs(ctx context.Context, req OCIRootfsRequest) error {
	if strings.TrimSpace(req.Reference) == "" {
		return fmt.Errorf("OCI reference is required")
	}
	if strings.TrimSpace(req.TargetPath) == "" {
		return fmt.Errorf("target path is required")
	}
	req.LayerDigest = normalizeSHA256Digest(req.LayerDigest)
	req.RootfsSHA256 = normalizeSHA256Digest(req.RootfsSHA256)
	ref, err := parseOCIReference(req.Reference)
	if err != nil {
		return err
	}
	client := req.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	transport := &ociTransport{client: client, registry: ref.Registry, repo: ref.Repo, token: req.Token}
	manifest, err := fetchOCIManifest(ctx, transport, ref)
	if err != nil {
		return err
	}
	layer, err := selectOCIRootfsLayer(manifest, req.LayerMediaType, req.LayerDigest)
	if err != nil {
		return err
	}
	if req.LayerDigest != "" && layer.Digest != req.LayerDigest {
		return fmt.Errorf("OCI layer digest mismatch: manifest has %s, catalog requires %s", layer.Digest, req.LayerDigest)
	}

	tmpPath := req.TargetPath + ".partial"
	_ = os.Remove(tmpPath)
	if err := downloadOCIRootfsLayer(ctx, transport, ref, layer, tmpPath, rootfsWriteOptions{
		ExpectedRootfsSHA256: req.RootfsSHA256,
		StallTimeout:         req.StallTimeout,
	}); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	_ = os.Remove(req.TargetPath)
	if err := os.Rename(tmpPath, req.TargetPath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("promote downloaded rootfs: %w", err)
	}
	return nil
}

func FetchHTTPRootfs(ctx context.Context, req HTTPRootfsRequest) error {
	if strings.TrimSpace(req.URL) == "" {
		return fmt.Errorf("HTTP rootfs URL is required")
	}
	if strings.TrimSpace(req.TargetPath) == "" {
		return fmt.Errorf("target path is required")
	}
	req.CompressedSHA256 = normalizeSHA256Digest(req.CompressedSHA256)
	req.RootfsSHA256 = normalizeSHA256Digest(req.RootfsSHA256)
	u, err := url.Parse(req.URL)
	if err != nil {
		return fmt.Errorf("parse HTTP rootfs URL: %w", err)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return fmt.Errorf("HTTP rootfs URL must use http or https: %s", req.URL)
	}
	client := req.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return fmt.Errorf("create HTTP rootfs request: %w", err)
	}
	if req.Token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+req.Token)
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("fetch HTTP rootfs: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return responseError("fetch HTTP rootfs", resp)
	}

	tmpPath := req.TargetPath + ".partial"
	_ = os.Remove(tmpPath)
	if err := writeVerifiedGzipRootfs(ctx, resp.Body, tmpPath, rootfsWriteOptions{
		ExpectedCompressedSHA256: req.CompressedSHA256,
		ExpectedRootfsSHA256:     req.RootfsSHA256,
		StallTimeout:             req.StallTimeout,
	}); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	_ = os.Remove(req.TargetPath)
	if err := os.Rename(tmpPath, req.TargetPath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("promote downloaded rootfs: %w", err)
	}
	return nil
}

func parseOCIReference(raw string) (ociReference, error) {
	raw = strings.TrimSpace(raw)
	var tagOrDigest string
	if at := strings.LastIndex(raw, "@"); at >= 0 {
		tagOrDigest = raw[at+1:]
		raw = raw[:at]
		slash := strings.LastIndex(raw, "/")
		colon := strings.LastIndex(raw, ":")
		if colon > slash {
			raw = raw[:colon]
		}
	} else {
		slash := strings.LastIndex(raw, "/")
		colon := strings.LastIndex(raw, ":")
		if colon <= slash {
			return ociReference{}, fmt.Errorf("OCI reference must include tag or digest: %s", raw)
		}
		tagOrDigest = raw[colon+1:]
		raw = raw[:colon]
	}
	firstSlash := strings.Index(raw, "/")
	if firstSlash <= 0 || firstSlash == len(raw)-1 {
		return ociReference{}, fmt.Errorf("OCI reference must include registry and repository: %s", raw)
	}
	return ociReference{
		Registry: raw[:firstSlash],
		Repo:     raw[firstSlash+1:],
		Ref:      tagOrDigest,
	}, nil
}

func fetchOCIManifest(ctx context.Context, transport *ociTransport, ref ociReference) (*ociManifest, error) {
	path := fmt.Sprintf("/v2/%s/manifests/%s", ref.Repo, ref.Ref)
	req, err := transport.newRequest(ctx, http.MethodGet, path)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", strings.Join([]string{
		"application/vnd.oci.artifact.manifest.v1+json",
		"application/vnd.oci.image.manifest.v1+json",
		"application/vnd.docker.distribution.manifest.v2+json",
	}, ", "))
	resp, err := transport.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, responseError("fetch OCI manifest", resp)
	}
	var manifest ociManifest
	if err := json.NewDecoder(resp.Body).Decode(&manifest); err != nil {
		return nil, fmt.Errorf("decode OCI manifest: %w", err)
	}
	if manifest.SchemaVersion != 2 {
		return nil, fmt.Errorf("unsupported OCI manifest schemaVersion %d", manifest.SchemaVersion)
	}
	return &manifest, nil
}

func selectOCIRootfsLayer(manifest *ociManifest, mediaType, digest string) (ociDescriptor, error) {
	if mediaType == "" {
		mediaType = OCIRootfsGzipMediaType
	}
	descriptors := append([]ociDescriptor{}, manifest.Layers...)
	descriptors = append(descriptors, manifest.Blobs...)
	for _, layer := range descriptors {
		if digest != "" && layer.Digest == digest {
			return layer, nil
		}
		if layer.MediaType == mediaType {
			return layer, nil
		}
	}
	if digest != "" {
		return ociDescriptor{}, fmt.Errorf("OCI manifest does not contain required rootfs layer %s", digest)
	}
	return ociDescriptor{}, fmt.Errorf("OCI manifest does not contain rootfs layer mediaType %s", mediaType)
}

func downloadOCIRootfsLayer(ctx context.Context, transport *ociTransport, ref ociReference, layer ociDescriptor, targetPath string, opts rootfsWriteOptions) error {
	path := fmt.Sprintf("/v2/%s/blobs/%s", ref.Repo, layer.Digest)
	req, err := transport.newRequest(ctx, http.MethodGet, path)
	if err != nil {
		return err
	}
	resp, err := transport.do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return responseError("fetch OCI blob", resp)
	}
	if opts.ExpectedRootfsSHA256 == "" {
		opts.ExpectedRootfsSHA256 = layer.Annotations["io.kiln-vm.rootfs.sha256"]
	}
	opts.ExpectedCompressedSHA256 = layer.Digest
	return writeVerifiedGzipRootfs(ctx, resp.Body, targetPath, opts)
}

type rootfsWriteOptions struct {
	ExpectedCompressedSHA256 string
	ExpectedRootfsSHA256     string
	StallTimeout             time.Duration
}

func writeVerifiedGzipRootfs(ctx context.Context, in io.ReadCloser, targetPath string, opts rootfsWriteOptions) error {
	out, err := os.OpenFile(targetPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0640)
	if err != nil {
		return fmt.Errorf("create rootfs target: %w", err)
	}
	defer out.Close()

	stallTimeout := opts.StallTimeout
	if stallTimeout <= 0 {
		stallTimeout = DefaultOCIPullStallTimeout
	}
	var reader io.Reader = &stallReadCloser{
		ctx:     ctx,
		reader:  in,
		closer:  in,
		timeout: stallTimeout,
	}
	compressedHash := sha256.New()
	gr, err := gzip.NewReader(io.TeeReader(reader, compressedHash))
	if err != nil {
		return fmt.Errorf("open gzip rootfs layer: %w", err)
	}
	defer gr.Close()
	rootfsHash := sha256.New()
	size, err := copySparseAndHash(out, gr, rootfsHash)
	if err != nil {
		return err
	}
	if err := out.Truncate(size); err != nil {
		return fmt.Errorf("truncate sparse rootfs: %w", err)
	}
	if err := out.Sync(); err != nil {
		return fmt.Errorf("sync rootfs: %w", err)
	}

	if err := verifyDigest("sha256:"+hex.EncodeToString(compressedHash.Sum(nil)), opts.ExpectedCompressedSHA256, "compressed rootfs"); err != nil {
		return err
	}
	if opts.ExpectedRootfsSHA256 != "" {
		if err := verifyDigest("sha256:"+hex.EncodeToString(rootfsHash.Sum(nil)), normalizeSHA256Digest(opts.ExpectedRootfsSHA256), "rootfs"); err != nil {
			return err
		}
	}
	return nil
}

type stallReadCloser struct {
	ctx     context.Context
	reader  io.Reader
	closer  io.Closer
	timeout time.Duration
}

type asyncReadResult struct {
	n   int
	buf []byte
	err error
}

func (r *stallReadCloser) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		_ = r.closer.Close()
		return 0, fmt.Errorf("rootfs read canceled: %w", err)
	}
	result := make(chan asyncReadResult, 1)
	buf := make([]byte, len(p))
	go func() {
		n, err := r.reader.Read(buf)
		result <- asyncReadResult{n: n, buf: buf, err: err}
	}()
	timer := time.NewTimer(r.timeout)
	defer timer.Stop()
	select {
	case res := <-result:
		if res.n > 0 {
			copy(p, res.buf[:res.n])
		}
		return res.n, res.err
	case <-r.ctx.Done():
		_ = r.closer.Close()
		return 0, fmt.Errorf("rootfs read canceled: %w", r.ctx.Err())
	case <-timer.C:
		_ = r.closer.Close()
		return 0, &RootfsDownloadStalledError{Timeout: r.timeout}
	}
}

func copySparseAndHash(out *os.File, in io.Reader, h hash.Hash) (int64, error) {
	buf := make([]byte, 1024*1024)
	var offset int64
	for {
		n, readErr := in.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			if _, err := h.Write(chunk); err != nil {
				return 0, fmt.Errorf("hash rootfs chunk: %w", err)
			}
			if allZero(chunk) {
				next := offset + int64(n)
				if _, err := out.Seek(next, io.SeekStart); err != nil {
					return 0, fmt.Errorf("seek sparse rootfs: %w", err)
				}
			} else if _, err := out.Write(chunk); err != nil {
				return 0, fmt.Errorf("write rootfs chunk: %w", err)
			}
			offset += int64(n)
		}
		if readErr == io.EOF {
			return offset, nil
		}
		if readErr != nil {
			return 0, fmt.Errorf("read gzip rootfs layer: %w", readErr)
		}
	}
}

func allZero(buf []byte) bool {
	for _, b := range buf {
		if b != 0 {
			return false
		}
	}
	return true
}

func verifyDigest(got, want, label string) error {
	if want == "" {
		return nil
	}
	if got != want {
		return fmt.Errorf("%s digest mismatch: got %s, want %s", label, got, want)
	}
	return nil
}

func normalizeSHA256Digest(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || strings.HasPrefix(value, "sha256:") {
		return value
	}
	return "sha256:" + value
}

type ociTransport struct {
	client   *http.Client
	registry string
	repo     string
	token    string
}

func (t *ociTransport) newRequest(ctx context.Context, method, path string) (*http.Request, error) {
	u := url.URL{Scheme: "https", Host: t.registry, Path: path}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("create OCI request: %w", err)
	}
	if t.token != "" {
		req.Header.Set("Authorization", "Bearer "+t.token)
	}
	return req, nil
}

func (t *ociTransport) do(req *http.Request) (*http.Response, error) {
	resp, err := t.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("send OCI request: %w", err)
	}
	if resp.StatusCode != http.StatusUnauthorized || t.token != "" {
		return resp, nil
	}
	challenge := resp.Header.Get("WWW-Authenticate")
	_ = resp.Body.Close()
	token, err := t.fetchBearerToken(req.Context(), challenge)
	if err != nil {
		return nil, fmt.Errorf("fetch OCI bearer token: %w", err)
	}
	t.token = token
	retry := req.Clone(req.Context())
	retry.Header = req.Header.Clone()
	retry.Header.Set("Authorization", "Bearer "+token)
	resp, err = t.client.Do(retry)
	if err != nil {
		return nil, fmt.Errorf("retry OCI request with bearer token: %w", err)
	}
	return resp, nil
}

func (t *ociTransport) fetchBearerToken(ctx context.Context, challenge string) (string, error) {
	realm, params := parseBearerChallenge(challenge)
	if realm == "" {
		return "", fmt.Errorf("OCI registry requires authentication but did not provide a bearer realm")
	}
	u, err := url.Parse(realm)
	if err != nil {
		return "", fmt.Errorf("parse OCI auth realm: %w", err)
	}
	query := u.Query()
	if service := params["service"]; service != "" {
		query.Set("service", service)
	}
	scope := params["scope"]
	if scope == "" {
		scope = "repository:" + t.repo + ":pull"
	}
	query.Set("scope", scope)
	u.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", fmt.Errorf("create OCI bearer token request: %w", err)
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("request OCI bearer token: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", responseError("fetch OCI bearer token", resp)
	}
	var body struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", fmt.Errorf("decode OCI bearer token: %w", err)
	}
	if body.Token != "" {
		return body.Token, nil
	}
	if body.AccessToken != "" {
		return body.AccessToken, nil
	}
	return "", fmt.Errorf("OCI bearer token response did not include a token")
}

var bearerParamRE = regexp.MustCompile(`([a-zA-Z_]+)="([^"]*)"`)

func parseBearerChallenge(challenge string) (string, map[string]string) {
	params := map[string]string{}
	if !strings.HasPrefix(strings.ToLower(challenge), "bearer ") {
		return "", params
	}
	for _, match := range bearerParamRE.FindAllStringSubmatch(challenge, -1) {
		params[strings.ToLower(match[1])] = match[2]
	}
	return params["realm"], params
}

func responseError(action string, resp *http.Response) error {
	limited, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	detail := strings.TrimSpace(string(limited))
	if detail == "" {
		detail = http.StatusText(resp.StatusCode)
	}
	return fmt.Errorf("%s: HTTP %d %s", action, resp.StatusCode, strconv.Quote(detail))
}
