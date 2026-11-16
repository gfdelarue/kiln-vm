package vm

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/gfdelarue/kiln-vm/internal/image"
	"github.com/gfdelarue/kiln-vm/internal/libvirtdomain"
	"github.com/gfdelarue/kiln-vm/internal/models"
)

type Profile struct {
	models.VMProfile
	Dockerfile string
	Version    int
}

func dockerRootfsPreparation() models.VMProfilePreparation {
	return models.VMProfilePreparation{
		Kind:    models.VMProfilePreparationDockerRootfs,
		Version: 1,
	}
}

func guestAgentAccess() models.VMProfileAccess {
	return models.VMProfileAccess{Kind: models.VMProfileAccessGuestAgent}
}

func sshAccess(user string) models.VMProfileAccess {
	return models.VMProfileAccess{Kind: models.VMProfileAccessSSH, User: user}
}

func guestAgentReadiness() models.VMProfileReadiness {
	return models.VMProfileReadiness{
		Kind:                    models.VMProfileReadinessGuestAgentExec,
		Command:                 "echo kiln-vm-ready",
		ExpectedStdoutSubstring: "kiln-vm-ready",
	}
}

func sshExecReadiness() models.VMProfileReadiness {
	return models.VMProfileReadiness{
		Kind:                    models.VMProfileReadinessSSHExec,
		Command:                 "echo kiln-vm-ready",
		ExpectedStdoutSubstring: "kiln-vm-ready",
	}
}

func firecrackerNetworking() models.VMProfileNetworking {
	return models.VMProfileNetworking{
		Public: models.VMProfilePublicNetwork{
			Kind: models.VMProfilePublicNetworkFirecrackerNAT,
		},
	}
}

func libvirtNetworking(source string) models.VMProfileNetworking {
	return models.VMProfileNetworking{
		Public: models.VMProfilePublicNetwork{
			Kind:   models.VMProfilePublicNetworkLibvirtNAT,
			Source: strings.TrimSpace(source),
		},
	}
}

func profileNetworking(profile Profile, libvirtNetwork string) models.VMProfileNetworking {
	networking := profile.Networking
	if networking.Public.Kind == "" {
		if profile.Backend == "libvirt-qemu" {
			networking.Public.Kind = models.VMProfilePublicNetworkLibvirtNAT
		} else {
			networking.Public.Kind = models.VMProfilePublicNetworkFirecrackerNAT
		}
	}
	if networking.Public.Kind == models.VMProfilePublicNetworkLibvirtNAT && networking.Public.Source == "" {
		networking.Public.Source = strings.TrimSpace(libvirtNetwork)
	}
	return networking
}

func windowsUnattendedPreparation() models.VMProfilePreparation {
	return models.VMProfilePreparation{
		Kind:           models.VMProfilePreparationWindowsUnattended,
		Version:        1,
		RequiredInputs: []string{"media_path"},
		OptionalInputs: []string{"iso_path", "product_key", "product_key_file", "activation_mode", "wait_timeout_seconds"},
		ImageBootMode:  image.BootModeBootableQCOW2,
	}
}

var profiles = map[string]Profile{
	"alpine": {
		VMProfile: models.VMProfile{
			Name:              "alpine",
			Description:       "Small persistent Linux VM for smoke tests and lightweight tools.",
			Backend:           "firecracker",
			GuestOS:           "linux",
			Networking:        firecrackerNetworking(),
			Access:            guestAgentAccess(),
			Readiness:         guestAgentReadiness(),
			Preparation:       dockerRootfsPreparation(),
			DefaultDiskSizeMB: 1024,
			DefaultVCPUs:      1,
			DefaultMemoryMB:   512,
		},
		Dockerfile: "FROM alpine:3.20\nRUN apk add --no-cache ca-certificates iproute2 openssh-server\nENTRYPOINT []\nCMD [\"sleep\", \"infinity\"]\n",
		Version:    1,
	},
	"debian": {
		VMProfile: models.VMProfile{
			Name:              "debian",
			Description:       "Debian stable persistent developer VM.",
			Backend:           "firecracker",
			GuestOS:           "linux",
			Networking:        firecrackerNetworking(),
			Access:            guestAgentAccess(),
			Readiness:         guestAgentReadiness(),
			Preparation:       dockerRootfsPreparation(),
			DefaultDiskSizeMB: 8192,
			DefaultVCPUs:      2,
			DefaultMemoryMB:   2048,
		},
		Dockerfile: "FROM debian:stable-slim\nRUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y ca-certificates curl iproute2 openssh-server tmux vim && apt-get clean && rm -rf /var/lib/apt/lists/*\nENTRYPOINT []\nCMD [\"sleep\", \"infinity\"]\n",
		Version:    1,
	},
	"ubuntu": {
		VMProfile: models.VMProfile{
			Name:              "ubuntu",
			Description:       "Ubuntu LTS persistent developer VM.",
			Backend:           "firecracker",
			GuestOS:           "linux",
			Networking:        firecrackerNetworking(),
			Access:            guestAgentAccess(),
			Readiness:         guestAgentReadiness(),
			Preparation:       dockerRootfsPreparation(),
			DefaultDiskSizeMB: 8192,
			DefaultVCPUs:      2,
			DefaultMemoryMB:   2048,
		},
		Dockerfile: "FROM ubuntu:24.04\nRUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y ca-certificates curl iproute2 openssh-server tmux vim && apt-get clean && rm -rf /var/lib/apt/lists/*\nENTRYPOINT []\nCMD [\"sleep\", \"infinity\"]\n",
		Version:    1,
	},
	"kali": {
		VMProfile: models.VMProfile{
			Name:              "kali",
			Description:       "Kali rolling persistent headless security VM.",
			Backend:           "firecracker",
			GuestOS:           "linux",
			Networking:        firecrackerNetworking(),
			Access:            guestAgentAccess(),
			Readiness:         guestAgentReadiness(),
			Preparation:       dockerRootfsPreparation(),
			DefaultDiskSizeMB: 32768,
			DefaultVCPUs:      2,
			DefaultMemoryMB:   4096,
		},
		Dockerfile: "FROM kalilinux/kali-rolling\nRUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y kali-linux-headless iproute2 openssh-server ca-certificates curl tmux vim && apt-get clean && rm -rf /var/lib/apt/lists/*\nENTRYPOINT []\nCMD [\"sleep\", \"infinity\"]\n",
		Version:    1,
	},
	"ubuntu-docker": {
		VMProfile: models.VMProfile{
			Name:              "ubuntu-docker",
			Description:       "Persistent Ubuntu VM for running local application stacks with Docker isolated from the host.",
			Backend:           "firecracker",
			GuestOS:           "linux",
			Networking:        firecrackerNetworking(),
			Access:            guestAgentAccess(),
			Readiness:         guestAgentReadiness(),
			Preparation:       dockerRootfsPreparation(),
			Capabilities:      []string{"docker"},
			DefaultDiskSizeMB: 65536,
			DefaultVCPUs:      4,
			DefaultMemoryMB:   8192,
		},
		Dockerfile: strings.TrimSpace(`
FROM ubuntu:24.04
RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y \
    busybox-static ca-certificates curl docker.io g++ git iproute2 iptables jq libcairo2-dev libgif-dev \
    libjpeg-dev libpango1.0-dev librsvg2-dev make openssh-server openssl \
    pkg-config postgresql-client procps python3 rsync tar tmux vim xz-utils \
  && apt-get clean && rm -rf /var/lib/apt/lists/*
RUN mkdir -p /usr/share/kiln-vm/docker-capability-rootfs/bin \
  && cp /bin/busybox /usr/share/kiln-vm/docker-capability-rootfs/bin/busybox \
  && for applet in sh wget nc; do ln -s busybox "/usr/share/kiln-vm/docker-capability-rootfs/bin/$applet"; done \
  && tar -C /usr/share/kiln-vm/docker-capability-rootfs -cf /usr/share/kiln-vm/docker-capability-rootfs.tar . \
  && rm -rf /usr/share/kiln-vm/docker-capability-rootfs
RUN mkdir -p /etc/kiln-vm && printf '%s\n' \
  '#!/bin/sh' \
  'set -u' \
  'mkdir -p /run /var/run /var/lib/docker /var/log' \
  'mkdir -p /run/sshd' \
  'if [ ! -f /etc/ssh/ssh_host_ed25519_key ] && [ ! -f /etc/ssh/ssh_host_rsa_key ]; then' \
  '  timeout 10 ssh-keygen -A >/dev/null 2>&1 || echo "kiln-vm boot: ssh host key generation skipped"' \
  'fi' \
  'if ! pgrep -x sshd >/dev/null 2>&1; then' \
  '  timeout 5 /usr/sbin/sshd || echo "kiln-vm boot: sshd failed"' \
  'fi' \
  'if command -v update-alternatives >/dev/null 2>&1 && [ -x /usr/sbin/iptables-legacy ]; then' \
  '  update-alternatives --set iptables /usr/sbin/iptables-legacy >/dev/null 2>&1 || true' \
  '  update-alternatives --set ip6tables /usr/sbin/ip6tables-legacy >/dev/null 2>&1 || true' \
  'fi' \
  'if ! pgrep dockerd >/dev/null 2>&1; then' \
  '  rm -f /var/run/docker.pid /var/run/docker.sock /var/run/docker/containerd/containerd.pid' \
  '  echo "kiln-vm boot: starting dockerd"' \
  '  dockerd --host=unix:///var/run/docker.sock >/var/log/dockerd.log 2>&1 &' \
  'fi' \
  > /etc/kiln-vm/boot.sh
RUN chmod +x /etc/kiln-vm/boot.sh
ENTRYPOINT []
CMD ["sleep", "infinity"]
`) + "\n",
		Version: 10,
	},
	"windows11": {
		VMProfile: models.VMProfile{
			Name:                "windows11",
			Description:         "Windows 11 Pro VM prepared from user-provided installation media.",
			Backend:             "libvirt-qemu",
			GuestOS:             "windows",
			Libvirt:             libvirtdomain.Windows11(),
			Networking:          libvirtNetworking(""),
			Access:              sshAccess("kiln-vm"),
			Readiness:           sshExecReadiness(),
			Preparation:         windowsUnattendedPreparation(),
			RequiresPreparation: true,
			DefaultDiskSizeMB:   65536,
			DefaultVCPUs:        2,
			DefaultMemoryMB:     4096,
		},
		Version: 1,
	},
}

func ListProfiles() []models.VMProfile {
	result := make([]models.VMProfile, 0, len(profiles))
	for _, profile := range profiles {
		result = append(result, profile.VMProfile)
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].Name < result[j].Name
	})
	return result
}

func StandardPublishedProfileNames() []string {
	return []string{"alpine", "ubuntu", "debian", "ubuntu-docker"}
}

func GetProfile(name string) (Profile, error) {
	return getProfile(name)
}

func getProfile(name string) (Profile, error) {
	if name == "" {
		name = "alpine"
	}
	profile, ok := profiles[name]
	if !ok {
		return Profile{}, fmt.Errorf("unknown VM profile %q", name)
	}
	return profile, nil
}

func profileFingerprint(profile Profile, runtimeFingerprint string) string {
	prep := profilePreparation(profile)
	bootMode := profilePreparationBootMode(profile)
	requiredInputs := append([]string(nil), prep.RequiredInputs...)
	optionalInputs := append([]string(nil), prep.OptionalInputs...)
	sort.Strings(requiredInputs)
	sort.Strings(optionalInputs)
	runtimePart := runtimeFingerprint
	switch prep.Kind {
	case models.VMProfilePreparationWindowsUnattended:
		runtimePart = "windows-unattended"
	}
	h := sha256.New()
	for _, part := range []string{
		"profile-fingerprint-v5",
		profile.Name,
		fmt.Sprintf("%d", profile.Version),
		profile.Backend,
		profile.GuestOS,
		string(prep.Kind),
		fmt.Sprintf("%d", prep.Version),
		strings.Join(requiredInputs, ","),
		strings.Join(optionalInputs, ","),
		bootMode,
		runtimePart,
		strings.TrimSpace(profile.Dockerfile),
	} {
		_, _ = h.Write([]byte(part))
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func profileIsWindows(profile Profile) bool {
	return profile.GuestOS == "windows"
}
