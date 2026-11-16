package image

import (
	"context"
	"maps"
	"slices"
	"strings"
	"testing"
)

func TestDockerfileBaseImages(t *testing.T) {
	dockerfile := strings.Join([]string{
		"# syntax=docker/dockerfile:1",
		"ARG BASE=alpine:3.19",
		"FROM --platform=linux/amd64 golang:1.24 AS build",
		"RUN go build ./...",
		"from build AS test",
		"FROM ${BASE}",
		"FROM scratch",
		"FROM \\",
		"  alpine:3.20",
		"FROM alpine:3.20",
		"COPY --from=build /out /out",
	}, "\r\n")

	got := DockerfileBaseImages(dockerfile)
	want := []string{"golang:1.24", "alpine:3.20"}
	if !slices.Equal(got, want) {
		t.Fatalf("DockerfileBaseImages = %v, want %v", got, want)
	}
	if got := DockerfileBaseImages("RUN echo FROM nothing\n"); len(got) != 0 {
		t.Fatalf("DockerfileBaseImages without FROM = %v", got)
	}
}

func TestTailWriterKeepsLastBytes(t *testing.T) {
	w := &tailWriter{limit: 8}
	for _, chunk := range []string{"step 1\n", "step 2\n", "error!"} {
		if n, err := w.Write([]byte(chunk)); err != nil || n != len(chunk) {
			t.Fatalf("Write(%q) = %d, %v", chunk, n, err)
		}
	}
	if got := w.String(); got != "2\nerror!" {
		t.Fatalf("tail = %q, want %q", got, "2\nerror!")
	}
}

func TestCanonicalImageRef(t *testing.T) {
	cases := []struct {
		ref, name, tag, digest string
	}{
		{"alpine", "docker.io/library/alpine", "latest", ""},
		{"alpine:3.20", "docker.io/library/alpine", "3.20", ""},
		{"library/alpine:3.20", "docker.io/library/alpine", "3.20", ""},
		{"docker.io/library/alpine:3.20", "docker.io/library/alpine", "3.20", ""},
		{"index.docker.io/library/alpine:3.20", "docker.io/library/alpine", "3.20", ""},
		{"ghcr.io/acme/tool:1", "ghcr.io/acme/tool", "1", ""},
		{"localhost:5000/tool", "localhost:5000/tool", "latest", ""},
		{"localhost/tool:dev", "localhost/tool", "dev", ""},
		{"acme/tool", "docker.io/acme/tool", "latest", ""},
		{"alpine@sha256:abc", "docker.io/library/alpine", "latest", "sha256:abc"},
	}
	for _, tc := range cases {
		name, tag, digest := canonicalImageRef(tc.ref)
		if name != tc.name || tag != tc.tag || digest != tc.digest {
			t.Errorf("canonicalImageRef(%q) = %q, %q, %q; want %q, %q, %q", tc.ref, name, tag, digest, tc.name, tc.tag, tc.digest)
		}
	}
}

func TestPinnedFromWriterReadsBuildKitOutput(t *testing.T) {
	alpine := "sha256:" + strings.Repeat("a", 64)
	golang := "sha256:" + strings.Repeat("b", 64)
	output := strings.Join([]string{
		"#2 [internal] load metadata for docker.io/library/alpine:3.20",
		"#4 [build 1/3] FROM docker.io/library/golang:1.23@" + golang,
		"#4 resolve docker.io/library/golang:1.23@" + golang + " done",
		"#5 [stage-1 1/2] FROM docker.io/library/alpine:3.20@" + alpine,
		"#5 CACHED",
		"#6 [build 2/3] RUN echo '#7 [x 1/1] FROM evil/image:1@sha256:" + strings.Repeat("c", 64) + "'",
		"#9 [stage-1 2/2] FROM docker.io/library/alpine:3.20@sha256:short",
	}, "\r\n")
	w := &pinnedFromWriter{}
	for chunk := range slices.Chunk([]byte(output), 7) {
		if n, err := w.Write(chunk); err != nil || n != len(chunk) {
			t.Fatalf("Write = %d, %v", n, err)
		}
	}
	want := map[string]string{
		"docker.io/library/alpine:3.20": alpine,
		"docker.io/library/golang:1.23": golang,
	}
	if got := w.Pinned(); !maps.Equal(got, want) {
		t.Fatalf("Pinned() = %v, want %v", got, want)
	}
}

func TestResolveBaseImagesUsesBuildOutputAndPinnedRefs(t *testing.T) {
	alpine := "sha256:" + strings.Repeat("a", 64)
	tool := "sha256:" + strings.Repeat("d", 64)
	dockerfile := strings.Join([]string{
		"FROM alpine:3.20 AS base",
		"FROM ghcr.io/acme/tool@" + tool + " AS tool",
		"FROM base",
	}, "\n")
	pinned := map[string]string{"docker.io/library/alpine:3.20": alpine}
	want := map[string]string{
		"alpine:3.20":               "docker.io/library/alpine@" + alpine,
		"ghcr.io/acme/tool@" + tool: "ghcr.io/acme/tool@" + tool,
	}
	if got := resolveBaseImages(context.Background(), dockerfile, pinned); !maps.Equal(got, want) {
		t.Fatalf("resolveBaseImages() = %v, want %v", got, want)
	}
}
