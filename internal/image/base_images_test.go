package image

import (
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
