import crypto from "crypto";
import fs from "fs";
import path from "path";
import {
  guestKernelConfigDeltas,
  guestKernelConfigURL,
  guestKernelSourceURL,
  guestKernelVersion,
  resolveDebArchitecture,
  supportedDebArchitectures,
} from "./architecture.mjs";

export const guestKernelArtifactSchemaVersion = 1;
export const guestKernelBuildRecipeVersion = 1;

export function guestKernelBuildSpec(debArch) {
  const arch = resolveDebArchitecture(debArch);
  const buildTarget = arch.linuxKernelArch === "arm64" ? "Image" : "vmlinux";
  const buildOutput = arch.linuxKernelArch === "arm64"
    ? "arch/arm64/boot/Image"
    : "vmlinux";
  return {
    schema_version: guestKernelArtifactSchemaVersion,
    artifact_kind: "kiln_vm_guest_kernel",
    build_recipe_version: guestKernelBuildRecipeVersion,
    deb_arch: arch.debArch,
    go_arch: arch.goArch,
    linux_kernel_arch: arch.linuxKernelArch,
    firecracker_arch: arch.firecrackerArch,
    kernel_version: guestKernelVersion,
    source_url: guestKernelSourceURL(),
    config_url: guestKernelConfigURL(arch.debArch),
    config_deltas: guestKernelConfigDeltas,
    build_target: buildTarget,
    build_output: buildOutput,
  };
}

export function guestKernelSpecFingerprint(debArch) {
  return sha256Text(stableJSONString(guestKernelBuildSpec(debArch))).slice(0, 16);
}

export function guestKernelReleaseTag() {
  const specs = supportedDebArchitectures.map((arch) => guestKernelBuildSpec(arch));
  const fingerprint = sha256Text(stableJSONString(specs)).slice(0, 16);
  return `guest-kernel-v${guestKernelVersion}-${fingerprint}`;
}

export function guestKernelArtifactName(debArch) {
  const arch = resolveDebArchitecture(debArch);
  return `kiln-vm-guest-kernel-v${guestKernelVersion}-${arch.debArch}-${guestKernelSpecFingerprint(arch.debArch)}.vmlinux`;
}

export function guestKernelMetadataName(debArch) {
  const arch = resolveDebArchitecture(debArch);
  return `kiln-vm-guest-kernel-v${guestKernelVersion}-${arch.debArch}-${guestKernelSpecFingerprint(arch.debArch)}.json`;
}

export function guestKernelArtifactPlan(debArch) {
  const arch = resolveDebArchitecture(debArch);
  return {
    release_tag: guestKernelReleaseTag(),
    artifact_name: guestKernelArtifactName(arch.debArch),
    metadata_name: guestKernelMetadataName(arch.debArch),
    spec_fingerprint: guestKernelSpecFingerprint(arch.debArch),
    ...guestKernelBuildSpec(arch.debArch),
  };
}

export function createGuestKernelMetadata({ debArch, kernelPath }) {
  const arch = resolveDebArchitecture(debArch);
  const artifactPath = path.resolve(kernelPath);
  const bytes = fs.readFileSync(artifactPath);
  return {
    ...guestKernelArtifactPlan(arch.debArch),
    sha256: crypto.createHash("sha256").update(bytes).digest("hex"),
    size_bytes: bytes.length,
  };
}

export function writeGuestKernelMetadata({ debArch, kernelPath, metadataPath }) {
  const metadata = createGuestKernelMetadata({ debArch, kernelPath });
  fs.mkdirSync(path.dirname(metadataPath), { recursive: true });
  fs.writeFileSync(metadataPath, `${JSON.stringify(metadata, null, 2)}\n`);
  return metadata;
}

export function verifyGuestKernelArtifact({ debArch, kernelPath, metadataPath }) {
  const arch = resolveDebArchitecture(debArch);
  const metadata = JSON.parse(fs.readFileSync(metadataPath, "utf8"));
  const expected = guestKernelArtifactPlan(arch.debArch);
  for (const [key, value] of Object.entries(expected)) {
    assertDeepEqual(metadata[key], value, `${metadataPath} ${key}`);
  }
  const bytes = fs.readFileSync(kernelPath);
  const sha256 = crypto.createHash("sha256").update(bytes).digest("hex");
  assertDeepEqual(metadata.sha256, sha256, `${metadataPath} sha256`);
  assertDeepEqual(metadata.size_bytes, bytes.length, `${metadataPath} size_bytes`);
  return metadata;
}

export function stableJSONString(value) {
  return JSON.stringify(sortJSON(value));
}

function sortJSON(value) {
  if (Array.isArray(value)) return value.map(sortJSON);
  if (!value || typeof value !== "object") return value;
  return Object.fromEntries(Object.keys(value).sort().map((key) => [key, sortJSON(value[key])]));
}

function sha256Text(value) {
  return crypto.createHash("sha256").update(value).digest("hex");
}

function assertDeepEqual(actual, expected, label) {
  if (JSON.stringify(actual) !== JSON.stringify(expected)) {
    throw new Error(`${label} mismatch: got ${JSON.stringify(actual)}, expected ${JSON.stringify(expected)}`);
  }
}
