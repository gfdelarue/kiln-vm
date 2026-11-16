#!/usr/bin/env node
import fs from "fs";
import os from "os";
import path from "path";
import { spawnSync } from "child_process";
import { fileURLToPath } from "url";
import {
  firecrackerReleaseBinaryPath,
  firecrackerReleaseURL,
  resolveDebArchitecture,
} from "./lib/architecture.mjs";
import {
  curlDownloadArgs,
} from "./lib/downloads.mjs";
import {
  createGuestKernelMetadata,
  verifyGuestKernelArtifact,
} from "./lib/guest-kernel-artifact.mjs";

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const pkg = {
  name: "kiln-vm",
  version: process.env.KILN_VM_DEB_VERSION || readPackageVersion(),
  arch: process.env.DEB_ARCH || "amd64",
};
const targetArch = resolveDebArchitecture(pkg.arch);
const firecrackerVersion = process.env.FIRECRACKER_VERSION || "v1.12.0";
const debDataPaths = {
  daemonEnvDefaults: "/usr/share/kiln-vm/kiln-vmd.env.defaults",
  copyright: "/usr/share/doc/kiln-vm/copyright",
  miekgDNSLicense: "/usr/share/doc/kiln-vm/third-party/miekg-dns.LICENSE",
  golangXNetLicense: "/usr/share/doc/kiln-vm/third-party/golang-x-net.LICENSE",
};
if (process.argv.includes("--print-deb-contract")) {
  console.log(JSON.stringify({
    data_files: [
      debDataPaths.daemonEnvDefaults,
      debDataPaths.copyright,
      debDataPaths.miekgDNSLicense,
      debDataPaths.golangXNetLicense,
    ],
    omitted_data_files: ["/etc/kiln-vm/kiln-vmd.env"],
    dependencies: debianDependencies(),
    recommends: debianRecommends(),
    copyright: copyrightFile(),
    daemon_env_defaults: daemonEnvFile(),
    daemon_service_unit: daemonServiceUnit(),
    postinst: maintainerScript(postinstScript()),
    postinst_service_activation: maintainerScript(postinstServiceActivationScript()),
    prerm: maintainerScript(prermScript()),
  }, null, 2));
  process.exit(0);
}
const outDir = path.join(repoRoot, "dist", "deb");
const workDir = fs.mkdtempSync(path.join(os.tmpdir(), "kiln-vm-deb-"));

try {
  const root = path.join(workDir, "root");
  const buildDir = path.join(workDir, "build");
  fs.mkdirSync(path.join(root, "usr", "bin"), { recursive: true });
  fs.mkdirSync(path.join(root, "usr", "lib", "kiln-vm"), { recursive: true });
  fs.mkdirSync(path.join(root, "usr", "share", "kiln-vm"), { recursive: true });
  fs.mkdirSync(path.join(root, "usr", "share", "doc", "kiln-vm", "third-party"), { recursive: true });
  fs.mkdirSync(path.join(root, "lib", "systemd", "system"), { recursive: true });
  fs.mkdirSync(path.join(root, "etc", "kiln-vm"), { recursive: true });
  fs.mkdirSync(buildDir, { recursive: true });

  for (const [name, target, envName] of [
    ["kiln-vm", "./cmd/kiln-vm", ""],
    ["kiln-vmd", "./cmd/kiln-vmd", ""],
    ["kiln-vm-agent", "./cmd/kiln-vm-agent", "KILN_VM_AGENT_BIN"],
  ]) {
    if (!envName || !process.env[envName]) {
      run("go", ["build", "-trimpath", "-ldflags", "-s -w", "-o", path.join(buildDir, name), target], {
        cwd: repoRoot,
        env: { ...process.env, CGO_ENABLED: "0", GOOS: "linux", GOARCH: targetArch.goArch },
      });
    }
  }

  installFile(path.join(buildDir, "kiln-vm"), path.join(root, "usr", "bin", "kiln-vm"), 0o755);
  installFile(path.join(buildDir, "kiln-vmd"), path.join(root, "usr", "bin", "kiln-vmd"), 0o755);
  installFile(resolvePackagedBinary("KILN_VM_AGENT_BIN", path.join(buildDir, "kiln-vm-agent")), path.join(root, "usr", "lib", "kiln-vm", "kiln-vm-agent"), 0o755);
  installFile(resolveFirecracker(), path.join(root, "usr", "bin", "firecracker"), 0o755);
  const kernelSource = resolveKernel();
  const packagedKernelPath = path.join(root, "usr", "lib", "kiln-vm", "vmlinux");
  installFile(kernelSource, packagedKernelPath, 0o644);
  writePackagedGuestKernelMetadata(path.join(root, "usr", "share", "kiln-vm", "guest-kernel.json"), packagedKernelPath);
  installProfileImageCatalog(path.join(root, "usr", "share", "kiln-vm", "profile-images.json"));
  writeFile(path.join(root, "lib", "systemd", "system", "kiln-vmd.service"), daemonServiceUnit(), 0o644);
  writeFile(path.join(root, debDataPaths.daemonEnvDefaults.slice(1)), daemonEnvFile(), 0o640);
  writeFile(path.join(root, debDataPaths.copyright.slice(1)), copyrightFile(), 0o644);
  installFile(
    path.join(repoRoot, "third_party", "github.com", "miekg", "dns", "LICENSE"),
    path.join(root, debDataPaths.miekgDNSLicense.slice(1)),
    0o644,
  );
  installFile(
    path.join(repoRoot, "third_party", "golang.org", "x", "net", "LICENSE"),
    path.join(root, debDataPaths.golangXNetLicense.slice(1)),
    0o644,
  );

  const controlDir = path.join(workDir, "control");
  fs.mkdirSync(controlDir, { recursive: true });
  writeFile(path.join(controlDir, "control"), controlFile(), 0o644);
  writeFile(path.join(controlDir, "postinst"), maintainerScript(postinstScript()), 0o755);
  writeFile(path.join(controlDir, "prerm"), maintainerScript(prermScript()), 0o755);

  fs.mkdirSync(outDir, { recursive: true });
  const debPath = path.join(outDir, `${pkg.name}_${pkg.version}_${pkg.arch}.deb`);
  buildDeb({ root, controlDir, debPath });
  console.log(debPath);
} finally {
  fs.rmSync(workDir, { recursive: true, force: true });
}

function daemonServiceUnit() {
  return `[Unit]
Description=kiln-vm microVM manager
After=network-online.target libvirtd.service
Wants=network-online.target libvirtd.service

[Service]
Type=notify
NotifyAccess=main
EnvironmentFile=-/etc/kiln-vm/kiln-vmd.env
ExecStart=/usr/bin/kiln-vmd
Restart=always
RestartPreventExitStatus=78
RestartSec=5
TimeoutStopSec=30
KillMode=control-group
Delegate=yes
StateDirectory=kiln-vm
StateDirectoryMode=0750

[Install]
WantedBy=multi-user.target
`;
}

function daemonEnvFile() {
  return `KILN_VM_LISTEN_ADDR=127.0.0.1:5456
KILN_VM_ALLOW_INSECURE_REMOTE_HTTP=false
KILN_VM_TLS_ENABLED=false
KILN_VM_TLS_DOMAIN=
KILN_VM_TLS_CERT_DIR=/var/lib/kiln-vm/certs
KILN_VM_TLS_LISTEN_ADDR=:443
KILN_VM_ACME_LISTEN_ADDR=:80
KILN_VM_ACME_EMAIL=
KILN_VM_DATA_DIR=/var/lib/kiln-vm
KILN_VM_KERNEL_PATH=/usr/lib/kiln-vm/vmlinux
KILN_VM_FIRECRACKER_BIN=/usr/bin/firecracker
KILN_VM_AGENT_BIN=/usr/lib/kiln-vm/kiln-vm-agent
KILN_VM_BRIDGE_CIDR=10.231.0.0/16
KILN_VM_GUEST_DNS_PROBE_NAME=example.com.
KILN_VM_MAX_RUNNING_VMS=8
KILN_VM_MAX_TOTAL_VCPUS=8
KILN_VM_MAX_TOTAL_MEMORY_MB=8192
KILN_VM_MAX_VM_DISK_MB=524288
KILN_VM_PROFILE_IMAGE_MODE=auto
KILN_VM_PROFILE_IMAGE_CATALOG=/usr/share/kiln-vm/profile-images.json
KILN_VM_PROFILE_IMAGE_CACHE_MAX_MB=8192
KILN_VM_PROFILE_IMAGE_TOKEN_FILE=
KILN_VM_PROFILE_IMAGE_PULL_TIMEOUT=30m
KILN_VM_PROFILE_IMAGE_PULL_HEADER_TIMEOUT=30s
KILN_VM_LIBVIRT_CONNECT_URI=qemu:///system
KILN_VM_LIBVIRT_NETWORK=kiln-vm
KILN_VM_LIBVIRT_CIDR=10.232.0.0/24
KILN_VM_WINDOWS_PROFILE_DIR=/var/lib/kiln-vm/windows-profiles
KILN_VM_LOG_LEVEL=info
KILN_VM_BOOTSTRAP_TOKEN_FILE=/var/lib/kiln-vm/bootstrap-token
`;
}

function controlFile() {
  return `Package: ${pkg.name}
Version: ${pkg.version}
Section: devel
Priority: optional
Architecture: ${pkg.arch}
Maintainer: George de la Rue <229141708+gfdelarue@users.noreply.github.com>
Homepage: https://kiln-vm.com
Depends: ${debianDependencies().join(", ")}
Recommends: ${debianRecommends().join(", ")}
Suggests: docker.io
Description: kiln-vm microVM manager
 kiln-vm runs persistent VMs on one Linux host: Linux guests on Firecracker
 and Windows guests on libvirt and QEMU. kiln-vmd serves an HTTP API and
 kiln-vm is its CLI.
`;
}

function debianDependencies() {
  return [
    "ca-certificates",
    "curl",
    "e2fsprogs",
    "iproute2",
    "iptables",
    "init-system-helpers",
    "tar",
    "util-linux",
    "xz-utils",
  ];
}

function debianRecommends() {
  return [
    "libvirt-clients",
    "libvirt-daemon-system",
    "mtools",
    "openssh-client",
    "ovmf",
    targetArch.qemuSystemPackage,
    "qemu-utils",
    "swtpm",
    "swtpm-tools",
    "virtinst",
  ];
}

function installProfileImageCatalog(target) {
  const source = process.env.KILN_VM_PROFILE_IMAGE_CATALOG_SOURCE;
  if (source) {
    installFile(source, target, 0o644);
    return;
  }
  writeFile(target, JSON.stringify({ schema_version: 1, images: [] }, null, 2) + "\n", 0o644);
}

function writePackagedGuestKernelMetadata(target, packagedKernelPath) {
  const metadataSource = process.env.KILN_VM_GUEST_KERNEL_METADATA_SOURCE;
  const artifactMetadata = metadataSource
    ? verifyGuestKernelArtifact({ debArch: targetArch.debArch, kernelPath: packagedKernelPath, metadataPath: metadataSource })
    : createGuestKernelMetadata({ debArch: targetArch.debArch, kernelPath: packagedKernelPath });
  const metadata = {
    ...artifactMetadata,
    kiln_vm_version: pkg.version,
    packaged_path: "/usr/lib/kiln-vm/vmlinux",
  };
  writeFile(target, JSON.stringify(metadata, null, 2) + "\n", 0o644);
}

function resolvePackagedBinary(envName, fallback) {
  const override = process.env[envName];
  if (!override) return fallback;
  const resolved = path.resolve(override);
  if (!fs.existsSync(resolved)) throw new Error(`${envName} does not exist: ${resolved}`);
  return resolved;
}

function postinstScript() {
  return `KILN_VM_ROOT="\${KILN_VM_DEB_ROOT:-}"
KILN_VM_ETC_DIR="\${KILN_VM_ROOT}/etc/kiln-vm"
KILN_VM_ENV_FILE="\${KILN_VM_ETC_DIR}/kiln-vmd.env"
KILN_VM_DEFAULT_ENV="\${KILN_VM_ROOT}${debDataPaths.daemonEnvDefaults}"
KILN_VM_PROFILE_TOKEN_FILE="\${KILN_VM_ETC_DIR}/profile-image-token"
KILN_VM_ENV_CONTENT=""
KILN_VM_ENV_REPLACEMENT=""
KILN_VM_ENV_NEXT=""

cleanup_env_temps() {
  [ -z "\${KILN_VM_ENV_CONTENT}" ] || rm -f -- "\${KILN_VM_ENV_CONTENT}"
  [ -z "\${KILN_VM_ENV_REPLACEMENT}" ] || rm -f -- "\${KILN_VM_ENV_REPLACEMENT}"
  [ -z "\${KILN_VM_ENV_NEXT}" ] || rm -f -- "\${KILN_VM_ENV_NEXT}"
}

cleanup_env_signal() {
  cleanup_env_temps
  trap - 0 1 2 15
  exit 1
}

append_env_line() {
  if [ -s "\${KILN_VM_ENV_CONTENT}" ] && [ "$(tail -c 1 "\${KILN_VM_ENV_CONTENT}" | wc -l)" -eq 0 ]; then
    printf '\\n' >> "\${KILN_VM_ENV_CONTENT}"
  fi
  printf '%s\\n' "\${1}" >> "\${KILN_VM_ENV_CONTENT}"
}

mkdir -p "\${KILN_VM_ETC_DIR}"

if [ -f "\${KILN_VM_DEFAULT_ENV}" ]; then
  if [ ! -e "\${KILN_VM_ENV_FILE}" ] && [ ! -L "\${KILN_VM_ENV_FILE}" ]; then
    install -m 0640 "\${KILN_VM_DEFAULT_ENV}" "\${KILN_VM_ENV_FILE}"
  fi

  if [ -f "\${KILN_VM_ENV_FILE}" ] && [ ! -L "\${KILN_VM_ENV_FILE}" ]; then
    trap cleanup_env_temps 0
    trap cleanup_env_signal 1 2 15
    KILN_VM_ENV_CONTENT="$(mktemp "\${KILN_VM_ENV_FILE}.content.XXXXXX")"
    KILN_VM_ENV_REPLACEMENT="$(mktemp "\${KILN_VM_ENV_FILE}.replacement.XXXXXX")"
    KILN_VM_ENV_NEXT="$(mktemp "\${KILN_VM_ENV_FILE}.next.XXXXXX")"

    cp -- "\${KILN_VM_ENV_FILE}" "\${KILN_VM_ENV_CONTENT}"
    cp --preserve=all -- "\${KILN_VM_ENV_FILE}" "\${KILN_VM_ENV_REPLACEMENT}"

    while IFS= read -r line || [ -n "\${line}" ]; do
      case "\${line}" in
        ""|\\#*) continue ;;
        *=*) key="\${line%%=*}" ;;
        *) continue ;;
      esac
      case "\${key}" in
        ""|*[!A-Za-z0-9_]*) continue ;;
      esac
      if ! grep -Eq "^[[:space:]]*\${key}[[:space:]]*=" "\${KILN_VM_ENV_CONTENT}"; then
        append_env_line "\${line}"
      fi
    done < "\${KILN_VM_DEFAULT_ENV}"

    if [ -s "\${KILN_VM_PROFILE_TOKEN_FILE}" ]; then
      if grep -Eq '^[[:space:]]*KILN_VM_PROFILE_IMAGE_TOKEN_FILE[[:space:]]*=' "\${KILN_VM_ENV_CONTENT}"; then
        current_token_file="$(sed -n 's/^[[:space:]]*KILN_VM_PROFILE_IMAGE_TOKEN_FILE[[:space:]]*=[[:space:]]*//p' "\${KILN_VM_ENV_CONTENT}" | tail -n 1)"
        if [ -z "\${current_token_file}" ]; then
          awk '{
            if ($0 ~ /^[[:space:]]*KILN_VM_PROFILE_IMAGE_TOKEN_FILE[[:space:]]*=/) {
              print "KILN_VM_PROFILE_IMAGE_TOKEN_FILE=/etc/kiln-vm/profile-image-token"
            } else {
              print
            }
          }' "\${KILN_VM_ENV_CONTENT}" > "\${KILN_VM_ENV_NEXT}"
          mv -f -- "\${KILN_VM_ENV_NEXT}" "\${KILN_VM_ENV_CONTENT}"
        fi
      else
        append_env_line 'KILN_VM_PROFILE_IMAGE_TOKEN_FILE=/etc/kiln-vm/profile-image-token'
      fi
    fi

    if ! cmp -s -- "\${KILN_VM_ENV_FILE}" "\${KILN_VM_ENV_CONTENT}"; then
      cat "\${KILN_VM_ENV_CONTENT}" > "\${KILN_VM_ENV_REPLACEMENT}"
      mv -f -- "\${KILN_VM_ENV_REPLACEMENT}" "\${KILN_VM_ENV_FILE}"
      KILN_VM_ENV_REPLACEMENT=""
    fi

    cleanup_env_temps
    KILN_VM_ENV_CONTENT=""
    KILN_VM_ENV_REPLACEMENT=""
    KILN_VM_ENV_NEXT=""
    trap - 0 1 2 15
  else
    printf '%s\\n' "kiln-vm: preserving non-regular operator environment at \${KILN_VM_ENV_FILE}" >&2
  fi
fi

if [ -z "\${KILN_VM_DEB_ROOT:-}" ]; then
${postinstServiceActivationScript()}
fi
`;
}

function postinstServiceActivationScript() {
  return `if command -v deb-systemd-helper >/dev/null 2>&1; then
  if deb-systemd-helper debian-installed kiln-vmd.service >/dev/null 2>&1; then
    deb-systemd-helper unmask kiln-vmd.service >/dev/null || true
    if deb-systemd-helper --quiet was-enabled kiln-vmd.service; then
      deb-systemd-helper enable kiln-vmd.service >/dev/null
    fi
  else
    deb-systemd-helper enable kiln-vmd.service >/dev/null
  fi
  deb-systemd-helper update-state kiln-vmd.service >/dev/null
fi

KILN_VM_SYSTEMD_RUN_DIR="\${KILN_VM_DEB_SYSTEMD_RUN_DIR:-/run/systemd/system}"
if command -v ischroot >/dev/null 2>&1 && ischroot >/dev/null 2>&1; then
  exit 0
fi
if [ ! -d "\${KILN_VM_SYSTEMD_RUN_DIR}" ]; then
  exit 0
fi
if ! command -v systemctl >/dev/null 2>&1; then
  printf '%s\\n' 'kiln-vm: running systemd detected but systemctl is unavailable' >&2
  exit 1
fi
if ! command -v deb-systemd-invoke >/dev/null 2>&1; then
  printf '%s\\n' 'kiln-vm: running systemd detected but deb-systemd-invoke is unavailable' >&2
  exit 1
fi
systemctl --system daemon-reload
if [ -n "\${2:-}" ]; then
  deb-systemd-invoke restart kiln-vmd.service
else
  deb-systemd-invoke start kiln-vmd.service
fi`;
}

function prermScript() {
  return `if [ -n "\${KILN_VM_DEB_ROOT:-}" ]; then
  exit 0
fi

case "\${1:-}" in
  remove|deconfigure)
    if [ -d /run/systemd/system ] && command -v deb-systemd-invoke >/dev/null 2>&1; then
      deb-systemd-invoke stop kiln-vmd.service || true
    fi
    ;;
esac
`;
}

function buildDeb({ root, controlDir, debPath }) {
  fs.writeFileSync(path.join(workDir, "debian-binary"), "2.0\n");
  run("tar", ["--sort=name", "--owner=0", "--group=0", "--numeric-owner", "-czf", path.join(workDir, "control.tar.gz"), "-C", controlDir, "."]);
  run("tar", ["--sort=name", "--owner=0", "--group=0", "--numeric-owner", "-czf", path.join(workDir, "data.tar.gz"), "-C", root, "."]);
  fs.rmSync(debPath, { force: true });
  run("ar", ["r", debPath, path.join(workDir, "debian-binary"), path.join(workDir, "control.tar.gz"), path.join(workDir, "data.tar.gz")]);
}

function maintainerScript(body) {
  return `#!/bin/sh
set -e
${body}
exit 0
`;
}

function installFile(source, target, mode) {
  fs.copyFileSync(source, target);
  fs.chmodSync(target, mode);
}

function resolveFirecracker() {
  if (process.env.FIRECRACKER_BIN) return process.env.FIRECRACKER_BIN;

  const { releaseName, releaseDir, binaryName } = firecrackerReleaseBinaryPath(firecrackerVersion, pkg.arch);
  const releaseUrl = firecrackerReleaseURL(firecrackerVersion, pkg.arch);
  const archivePath = path.join(workDir, releaseName);
  const extractDir = path.join(workDir, "firecracker");
  const binaryPath = path.join(extractDir, releaseDir, binaryName);

  run("curl", curlDownloadArgs(archivePath, releaseUrl));
  fs.mkdirSync(extractDir, { recursive: true });
  run("tar", ["-xzf", archivePath, "-C", extractDir]);
  if (!fs.existsSync(binaryPath)) throw new Error(`Firecracker binary not found in release archive: ${binaryName}`);
  return binaryPath;
}

function resolveKernel() {
  if (process.env.KILN_VM_KERNEL_PATH) return process.env.KILN_VM_KERNEL_PATH;

  const kernelPath = path.join(workDir, "vmlinux");
  run("node", [
    path.join(repoRoot, "scripts", "build-guest-kernel.mjs"),
    "--arch",
    pkg.arch,
    "--out",
    kernelPath,
    "--work-dir",
    path.join(workDir, "guest-kernel"),
  ], { cwd: repoRoot });
  return kernelPath;
}

function readPackageVersion() {
  const source = fs.readFileSync(path.join(repoRoot, "internal", "version", "version.go"), "utf8");
  const match = source.match(/const\s+Version\s*=\s*"([^"]+)"/);
  if (!match) throw new Error("Unable to read kiln-vm version from internal/version/version.go");
  return match[1];
}

function copyrightFile() {
  return `Format: https://www.debian.org/doc/packaging-manuals/copyright-format/1.0/
Upstream-Name: kiln-vm
Source: https://github.com/gfdelarue/kiln-vm

Files: *
Copyright: George de la Rue
License: Apache-2.0
 On Debian systems, the full text of the Apache License, Version 2.0 is in
 /usr/share/common-licenses/Apache-2.0.

Files: third_party/github.com/miekg/dns/*
Copyright: 2009 The Go Authors, 2011 Miek Gieben
License: BSD-3-Clause
 See /usr/share/doc/kiln-vm/third-party/miekg-dns.LICENSE.

Files: third_party/golang.org/x/net/*
Copyright: 2009 The Go Authors
License: BSD-3-Clause
 See /usr/share/doc/kiln-vm/third-party/golang-x-net.LICENSE.
`;
}

function writeFile(filePath, content, mode) {
  fs.mkdirSync(path.dirname(filePath), { recursive: true });
  fs.writeFileSync(filePath, content);
  fs.chmodSync(filePath, mode);
}

function run(command, args, options = {}) {
  const result = spawnSync(command, args, { stdio: "inherit", ...options });
  if (result.status !== 0) throw new Error(`Command failed: ${command} ${args.join(" ")}`);
}
