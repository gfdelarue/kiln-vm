package image

import (
	"bufio"
	"context"
	"encoding/json"
	"os/exec"
	"strings"
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

func (s *Service) ResolveBaseImages(ctx context.Context, dockerfile string) map[string]string {
	resolved := map[string]string{}
	for _, ref := range DockerfileBaseImages(dockerfile) {
		if digest := localRepoDigest(ctx, ref); digest != "" {
			resolved[ref] = digest
		}
	}
	if len(resolved) == 0 {
		return nil
	}
	return resolved
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
	return digests[0]
}
