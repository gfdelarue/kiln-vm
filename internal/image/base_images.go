package image

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"os/exec"
	"regexp"
	"strings"
	"sync"
)

func DockerfileBaseImages(dockerfile string) []string {
	var refs []string
	stages := map[string]bool{}
	seen := map[string]bool{}
	scanner := bufio.NewScanner(strings.NewReader(joinDockerfileContinuations(dockerfile)))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 || !strings.EqualFold(fields[0], "FROM") {
			continue
		}
		args := fields[1:]
		for len(args) > 0 && strings.HasPrefix(args[0], "--") {
			args = args[1:]
		}
		if len(args) == 0 {
			continue
		}
		ref := args[0]
		lower := strings.ToLower(ref)
		skip := lower == "scratch" || strings.Contains(ref, "$") || stages[lower] || seen[ref]
		if len(args) >= 3 && strings.EqualFold(args[1], "AS") {
			stages[strings.ToLower(args[2])] = true
		}
		if skip {
			continue
		}
		seen[ref] = true
		refs = append(refs, ref)
	}
	return refs
}

func joinDockerfileContinuations(dockerfile string) string {
	lines := strings.Split(strings.ReplaceAll(dockerfile, "\r\n", "\n"), "\n")
	var out []string
	var current strings.Builder
	for _, line := range lines {
		trimmed := strings.TrimRight(line, " \t")
		if strings.HasPrefix(strings.TrimSpace(trimmed), "#") && current.Len() == 0 {
			continue
		}
		if strings.HasSuffix(trimmed, "\\") {
			current.WriteString(strings.TrimSuffix(trimmed, "\\"))
			current.WriteString(" ")
			continue
		}
		current.WriteString(trimmed)
		out = append(out, current.String())
		current.Reset()
	}
	if current.Len() > 0 {
		out = append(out, current.String())
	}
	return strings.Join(out, "\n")
}

func resolveBaseImages(ctx context.Context, dockerfile string, pinned map[string]string) map[string]string {
	resolved := map[string]string{}
	for _, ref := range DockerfileBaseImages(dockerfile) {
		name, tag, digest := canonicalImageRef(ref)
		switch {
		case digest != "":
			resolved[ref] = name + "@" + digest
		case pinned[name+":"+tag] != "":
			resolved[ref] = name + "@" + pinned[name+":"+tag]
		default:
			if repoDigest := localRepoDigest(ctx, ref); repoDigest != "" {
				resolved[ref] = repoDigest
			}
		}
	}
	if len(resolved) == 0 {
		return nil
	}
	return resolved
}

func canonicalImageRef(ref string) (name, tag, digest string) {
	name = ref
	if i := strings.Index(name, "@"); i >= 0 {
		name, digest = name[:i], name[i+1:]
	}
	if i := strings.LastIndex(name, ":"); i > strings.LastIndex(name, "/") {
		name, tag = name[:i], name[i+1:]
	}
	if tag == "" {
		tag = "latest"
	}
	domain, rest, found := strings.Cut(name, "/")
	if !found || (!strings.ContainsAny(domain, ".:") && domain != "localhost") {
		domain, rest = "docker.io", name
	}
	if domain == "index.docker.io" {
		domain = "docker.io"
	}
	if domain == "docker.io" && !strings.Contains(rest, "/") {
		rest = "library/" + rest
	}
	return domain + "/" + rest, tag, digest
}

func localRepoDigest(ctx context.Context, ref string) string {
	out, err := exec.CommandContext(ctx, "docker", "image", "inspect", "--format", "{{json .RepoDigests}}", ref).Output()
	if err != nil {
		return ""
	}
	var digests []string
	if err := json.Unmarshal(out, &digests); err != nil || len(digests) == 0 {
		return ""
	}
	name, _, digest := canonicalImageRef(digests[0])
	if digest == "" {
		return ""
	}
	return name + "@" + digest
}

var buildKitPinnedFromPattern = regexp.MustCompile(`^#\d+ \[[^\]]*\] FROM (\S+)@(sha256:[0-9a-f]{64})$`)

const pinnedFromLineLimit = 64 * 1024

type pinnedFromWriter struct {
	mu      sync.Mutex
	partial []byte
	pinned  map[string]string
}

func (w *pinnedFromWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.partial = append(w.partial, p...)
	for {
		i := bytes.IndexByte(w.partial, '\n')
		if i < 0 {
			break
		}
		w.scanLine(w.partial[:i])
		w.partial = w.partial[i+1:]
	}
	if len(w.partial) > pinnedFromLineLimit {
		w.partial = nil
	}
	return len(p), nil
}

func (w *pinnedFromWriter) scanLine(line []byte) {
	match := buildKitPinnedFromPattern.FindSubmatch(bytes.TrimSpace(line))
	if match == nil {
		return
	}
	name, tag, _ := canonicalImageRef(string(match[1]))
	if w.pinned == nil {
		w.pinned = map[string]string{}
	}
	w.pinned[name+":"+tag] = string(match[2])
}

func (w *pinnedFromWriter) Pinned() map[string]string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.partial) > 0 {
		w.scanLine(w.partial)
		w.partial = nil
	}
	return w.pinned
}
