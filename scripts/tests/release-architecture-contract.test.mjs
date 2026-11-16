import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { test } from "node:test";
import { fileURLToPath } from "node:url";
import {
  firecrackerReleaseBinaryPath,
  firecrackerReleaseURL,
  guestKernelConfigDeltas,
  guestKernelConfigURL,
  guestKernelSourceURL,
  guestKernelVersion,
  resolveDebArchitecture,
  standardProfileImages,
  supportedDebArchitectures,
} from "../lib/architecture.mjs";
import {
  curlRetryArgs,
} from "../lib/downloads.mjs";
import {
  guestKernelArtifactName,
  guestKernelArtifactPlan,
  guestKernelMetadataName,
  guestKernelReleaseTag,
  guestKernelSpecFingerprint,
} from "../lib/guest-kernel-artifact.mjs";
import {
  profileImageArtifactEntry,
  profileImageLayerMediaType,
  profileImageReleaseTag,
} from "../lib/profile-image-artifact.mjs";
import {
  profileRuntimeArtifactName,
  profileRuntimeBuildSpec,
  profileRuntimeMetadataName,
  profileRuntimeNoticeArtifactName,
  profileRuntimeNotices,
  profileRuntimeReleaseTag,
  profileRuntimeSpecFingerprint,
} from "../lib/profile-runtime-artifact.mjs";

const testsDir = path.dirname(fileURLToPath(import.meta.url));
const repoRoot = path.resolve(testsDir, "..", "..");
const readRepoFile = (...segments) => fs.readFileSync(path.join(repoRoot, ...segments), "utf8");
const readReleaseWorkflow = () => readRepoFile(".github", "workflows", "release.yml");
const readReleaseArchitectureDoc = () => readRepoFile("docs", "release-architecture.md");
const readFlake = () => readRepoFile("flake.nix");

test("amd64 resolves to its Go, Firecracker, kernel, Docker and QEMU names", () => {
  const amd64 = resolveDebArchitecture("amd64");
  assert.equal(amd64.debArch, "amd64");
  assert.equal(amd64.goArch, "amd64");
  assert.equal(amd64.catalogArch, "amd64");
  assert.equal(amd64.firecrackerArch, "x86_64");
  assert.equal(amd64.kernelArch, "x86_64");
  assert.equal(amd64.linuxKernelArch, "x86");
  assert.equal(amd64.dockerPlatform, "linux/amd64");
  assert.equal(amd64.qemuSystemPackage, "qemu-system-x86");
});

test("arm64 resolves to its Go, Firecracker, kernel, Docker and QEMU names", () => {
  const arm64 = resolveDebArchitecture("arm64");
  assert.equal(arm64.debArch, "arm64");
  assert.equal(arm64.goArch, "arm64");
  assert.equal(arm64.catalogArch, "arm64");
  assert.equal(arm64.firecrackerArch, "aarch64");
  assert.equal(arm64.kernelArch, "aarch64");
  assert.equal(arm64.linuxKernelArch, "arm64");
  assert.equal(arm64.dockerPlatform, "linux/arm64");
  assert.equal(arm64.qemuSystemPackage, "qemu-system-arm");
});

test("only amd64 and arm64 are supported Debian architectures", () => {
  assert.deepEqual(supportedDebArchitectures, ["amd64", "arm64"]);
  assert.throws(() => resolveDebArchitecture("aarch64"), /unsupported Debian architecture/);
});

test("Firecracker release URL and binary name follow the target architecture", () => {
  assert.equal(
    firecrackerReleaseURL("v1.12.0", "arm64"),
    "https://github.com/firecracker-microvm/firecracker/releases/download/v1.12.0/firecracker-v1.12.0-aarch64.tgz"
  );
  assert.equal(
    firecrackerReleaseBinaryPath("v1.12.0", "arm64").binaryName,
    "firecracker-v1.12.0-aarch64"
  );
});

test("guest kernel version, config deltas, source and config URLs are pinned", () => {
  assert.equal(
    guestKernelVersion,
    "6.1.155"
  );
  assert.deepEqual(
    guestKernelConfigDeltas,
    ["CONFIG_IP_NF_RAW=y", "CONFIG_IP6_NF_RAW=y"]
  );
  assert.equal(
    guestKernelSourceURL(),
    "https://cdn.kernel.org/pub/linux/kernel/v6.x/linux-6.1.155.tar.xz"
  );
  assert.equal(
    guestKernelConfigURL("arm64"),
    "https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/v1.15/aarch64/vmlinux-6.1.155.config"
  );
});

test("guest kernel release tag, fingerprint and artifact names are stable", () => {
  assert.equal(
    guestKernelReleaseTag(),
    "guest-kernel-v6.1.155-5db0ae88e8981161"
  );
  assert.equal(
    guestKernelSpecFingerprint("amd64"),
    "cdfbb02087a31a18"
  );
  assert.equal(
    guestKernelArtifactName("amd64"),
    "kiln-vm-guest-kernel-v6.1.155-amd64-cdfbb02087a31a18.vmlinux"
  );
  assert.equal(
    guestKernelMetadataName("arm64"),
    "kiln-vm-guest-kernel-v6.1.155-arm64-44c3dc22d9ddf605.json"
  );
});

test("guest kernel artifact plan has exactly its documented fields", () => {
  assert.deepEqual(
    Object.keys(guestKernelArtifactPlan("arm64")).sort(),
    [
      "artifact_kind",
      "artifact_name",
      "build_output",
      "build_recipe_version",
      "build_target",
      "config_deltas",
      "config_url",
      "deb_arch",
      "firecracker_arch",
      "go_arch",
      "kernel_version",
      "linux_kernel_arch",
      "metadata_name",
      "release_tag",
      "schema_version",
      "source_url",
      "spec_fingerprint",
    ]
  );
});

test("standard profile images keep their rootfs sizes", () => {
  assert.deepEqual(
    standardProfileImages.map((entry) => `${entry.profile}:${entry.rootfsSizeMB}`),
    ["alpine:128", "ubuntu:2048", "debian:2048", "ubuntu-docker:4096"]
  );
});

test("profile image artifact names, release tag and download URL are stable", () => {
  const profileReleaseBaseURL = "https://github.com/gfdelarue/kiln-vm/releases/download";
  const sampleProfileFingerprint = {
    profile: "ubuntu-docker",
    profile_version: 10,
    fingerprint: "a".repeat(64),
    arch: "arm64",
  };
  const sampleProfileEntry = profileImageArtifactEntry({ fingerprint: sampleProfileFingerprint, rootfsSizeMB: 4096 });
  assert.equal(sampleProfileEntry.artifact_kind, "kiln_vm_profile_image");
  assert.equal(sampleProfileEntry.layer_media_type, profileImageLayerMediaType);
  assert.equal(sampleProfileEntry.artifact_name, "kiln-vm-profile-ubuntu-docker-pv10-arm64-4096mb-8517b9757ad2ac80-rootfs.ext4.gz");
  assert.equal(sampleProfileEntry.metadata_name, "kiln-vm-profile-ubuntu-docker-pv10-arm64-4096mb-8517b9757ad2ac80.json");
  assert.equal(
    profileImageReleaseTag([sampleProfileEntry]),
    "profile-images-arm64-b41b862786f98d0e"
  );
  assert.equal(
    `${profileReleaseBaseURL}/${profileImageReleaseTag([sampleProfileEntry])}/${sampleProfileEntry.artifact_name}`,
    "https://github.com/gfdelarue/kiln-vm/releases/download/profile-images-arm64-b41b862786f98d0e/kiln-vm-profile-ubuntu-docker-pv10-arm64-4096mb-8517b9757ad2ac80-rootfs.ext4.gz"
  );
});

test("profile runtime release tag and artifact names derive from the spec fingerprint", () => {
  const sampleRuntimeSpec = profileRuntimeBuildSpec({
    debArch: "arm64",
    goVersion: "go1.24.0",
    sourceFingerprint: "b".repeat(64),
  });
  const sampleRuntimeFingerprint = profileRuntimeSpecFingerprint(sampleRuntimeSpec);
  assert.match(sampleRuntimeFingerprint, /^[a-f0-9]{16}$/u);
  assert.equal(
    profileRuntimeReleaseTag(sampleRuntimeSpec),
    `profile-runtime-arm64-${sampleRuntimeFingerprint}`
  );
  assert.equal(
    profileRuntimeArtifactName("arm64", sampleRuntimeFingerprint, "kiln-vm-agent"),
    `kiln-vm-profile-runtime-arm64-${sampleRuntimeFingerprint}-kiln-vm-agent`
  );
  assert.equal(
    profileRuntimeMetadataName("arm64", sampleRuntimeFingerprint),
    `kiln-vm-profile-runtime-arm64-${sampleRuntimeFingerprint}.json`
  );
  for (const notice of profileRuntimeNotices) {
    assert.equal(
      profileRuntimeNoticeArtifactName("arm64", sampleRuntimeFingerprint, notice.name),
      `kiln-vm-profile-runtime-arm64-${sampleRuntimeFingerprint}-${notice.name}.LICENSE`,
    );
  }
});

test("downloads retry with the pinned curl arguments", () => {
  assert.deepEqual(
    curlRetryArgs,
    ["--connect-timeout", "20", "--retry", "6", "--retry-delay", "10", "--retry-max-time", "300", "--retry-all-errors"]
  );
});

test("release workflow gates on the default Testkit run, which holds the profile runtime contract", () => {
  assert.match(readReleaseWorkflow(), /- name: Default Testkit release gate\n\s+id: testkit\n\s+run: npx testkit run\n/);
  assert.ok(fs.existsSync(path.join(testsDir, "profile-runtime-artifact-contract.test.mjs")));
});

test("release architecture doc keeps every job-boundary marker", async (t) => {
  const releaseArchitectureDoc = readReleaseArchitectureDoc();
  for (const marker of [
    "kiln-vm release jobs must treat target architecture as explicit workflow data.",
    "| `validate` | Architecture-neutral |",
    "| `build` | Cross-build-safe Go binaries |",
    "| `release-notices` | Architecture-neutral |",
    "| `guest-kernel` | Native-routed target artifact |",
    "| `profile-images` | Native-routed target artifact |",
    "| `profile-catalog` | Architecture-neutral aggregation |",
    "| `deb` | Native-routed package assembly |",
    "| `release` | Architecture-neutral publishing |",
    "Its manifest and cache include the pinned third-party notices required by the bundled runtime tooling",
    "Raw cross-platform `kiln-vm` and `kiln-vmd` release binaries must be accompanied by the pinned",
    "Every job runs on a GitHub-hosted runner.",
    "A job must not build an `amd64` artifact on an `arm64` runner, or the reverse, unless a cross-build path is deliberately implemented",
  ]) {
    await t.test(marker, () => {
      assert.match(releaseArchitectureDoc, textMarkerRegExp(marker), `release architecture doc is missing marker: ${marker}`);
    });
  }
});

test("cached profile runtime download verifies and installs the artifact and its notices", () => {
  const profileRuntimeDownloadStep = workflowStepBlock(readReleaseWorkflow(), "Download cached profile runtime artifact", "Build profile tooling");
  assert.match(profileRuntimeDownloadStep, /gh release download/);
  assert.match(profileRuntimeDownloadStep, /steps\.runtime_plan\.outputs\.release_tag/);
  assert.match(profileRuntimeDownloadStep, /verify-profile-runtime-artifact\.mjs/);
  assert.match(profileRuntimeDownloadStep, /--install-dir dist\/profile-runtime/);
  assert.match(profileRuntimeDownloadStep, /artifact_notice_miekg_dns/);
  assert.match(profileRuntimeDownloadStep, /artifact_notice_golang_x_net/);
});

test("profile runtime build, cache write and publish run only on a cache miss", () => {
  const releaseWorkflow = readReleaseWorkflow();
  const profileRuntimeBuildStep = workflowStepBlock(releaseWorkflow, "Build profile tooling", "Write profile runtime artifact cache");
  assert.match(profileRuntimeBuildStep, /if: steps\.runtime_cache\.outputs\.hit != 'true'/);
  assert.match(profileRuntimeBuildStep, /go build -trimpath -ldflags "-s -w"/);

  const profileRuntimeWriteStep = workflowStepBlock(releaseWorkflow, "Write profile runtime artifact cache", "Publish profile runtime artifact cache");
  assert.match(profileRuntimeWriteStep, /if: steps\.runtime_cache\.outputs\.hit != 'true'/);
  assert.match(profileRuntimeWriteStep, /write-profile-runtime-artifact\.mjs/);
  assert.match(profileRuntimeWriteStep, /verify-profile-runtime-artifact\.mjs/);
  assert.match(profileRuntimeWriteStep, /--install-dir dist\/profile-runtime/);

  const profileRuntimePublishStep = workflowStepBlock(releaseWorkflow, "Publish profile runtime artifact cache", "Prepare profile runtime");
  assert.match(profileRuntimePublishStep, /if: steps\.runtime_cache\.outputs\.hit != 'true'/);
  assert.match(profileRuntimePublishStep, /gh release upload/);
  assert.match(profileRuntimePublishStep, /dist\/profile-runtime\/cache\/kiln-vm-profile-runtime-\*/);
});

test("release workflow ships the profile runtime's third-party notices", () => {
  const releaseWorkflow = readReleaseWorkflow();
  assert.match(releaseWorkflow, /dist\/profile-runtime\/third-party-notices/);
  assert.match(releaseWorkflow, /third_party\/github\.com\/miekg\/dns\/LICENSE/);
  assert.match(releaseWorkflow, /third_party\/golang\.org\/x\/net\/LICENSE/);
});

test("flake packages carry exactly the notices their binaries need", () => {
  const flake = readFlake();
  const daemonPackage = sourceBlock(flake, "kiln-vmd = buildGoModule", "kiln-vm = buildGoModule");
  assert.match(daemonPackage, /third_party\/github\.com\/miekg\/dns\/LICENSE/);
  assert.match(daemonPackage, /third_party\/golang\.org\/x\/net\/LICENSE/);
  const cliPackage = sourceBlock(flake, "kiln-vm = buildGoModule", "kiln-vm-agent = buildGoModule");
  assert.match(cliPackage, /third_party\/github\.com\/miekg\/dns\/LICENSE/);
  assert.match(cliPackage, /third_party\/golang\.org\/x\/net\/LICENSE/);
  const agentPackage = sourceBlock(flake, "kiln-vm-agent = buildGoModule", "default = self.packages");
  assert.doesNotMatch(agentPackage, /third_party\/github\.com\/miekg\/dns\/LICENSE/);
  assert.match(agentPackage, /third_party\/golang\.org\/x\/net\/LICENSE/);
});

test("release-notices job builds the notices and the release job publishes them", () => {
  const releaseWorkflow = readReleaseWorkflow();
  const releaseNoticesJob = sourceBlock(releaseWorkflow, "release-notices:", "deb:");
  assert.match(releaseNoticesJob, /needs:\s*\n\s*- validate/);
  assert.match(releaseNoticesJob, /third_party\/github\.com\/miekg\/dns\/LICENSE dist\/kiln-vm-miekg-dns\.LICENSE/);
  assert.match(releaseNoticesJob, /third_party\/golang\.org\/x\/net\/LICENSE dist\/kiln-vm-golang-x-net\.LICENSE/);
  assert.match(releaseNoticesJob, /name: release-notices/);
  const releaseJobStart = releaseWorkflow.indexOf("\n  release:\n");
  assert.notEqual(releaseJobStart, -1, "release job not found");
  const releaseJob = releaseWorkflow.slice(releaseJobStart);
  assert.match(releaseJob, /needs:[\s\S]*- release-notices/);
  assert.match(releaseJob, /name: Download third-party notices[\s\S]*name: release-notices[\s\S]*path: dist/);
  assert.match(releaseJob, /files: dist\/\*/);
});

test("cached profile image download fetches metadata only", () => {
  const profileCacheDownloadStep = workflowStepBlock(readReleaseWorkflow(), "Download cached VM profile images", "Install rootfs build prerequisites");
  assert.match(profileCacheDownloadStep, /patterns=\(\)/);
  assert.match(profileCacheDownloadStep, /--metadata-only/);
  assert.doesNotMatch(profileCacheDownloadStep, /--pattern\s+"kiln-vm-profile-\*"/);
});

test("profile image verification checks metadata only on a cache hit", () => {
  const profileVerifyStep = workflowStepBlock(readReleaseWorkflow(), "Verify profile image artifacts", "Upload profile image catalog");
  assert.match(profileVerifyStep, /--metadata-only/);
  assert.match(profileVerifyStep, /steps\.cache\.outputs\.hit/);
});

function workflowStepBlock(workflow, startName, nextName) {
  const start = workflow.indexOf(`- name: ${startName}`);
  assert.notEqual(start, -1, `workflow step not found: ${startName}`);
  const end = workflow.indexOf(`- name: ${nextName}`, start + 1);
  assert.notEqual(end, -1, `workflow step after ${startName} not found: ${nextName}`);
  return workflow.slice(start, end);
}

function sourceBlock(source, startMarker, nextMarker) {
  const start = source.indexOf(startMarker);
  assert.notEqual(start, -1, `source marker not found: ${startMarker}`);
  const end = source.indexOf(nextMarker, start + startMarker.length);
  assert.notEqual(end, -1, `source marker after ${startMarker} not found: ${nextMarker}`);
  return source.slice(start, end);
}

function textMarkerRegExp(value) {
  return new RegExp(value.trim().split(/\s+/u).map((part) => part.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")).join("\\s+"), "u");
}
