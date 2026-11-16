import crypto from "crypto";
import fs from "fs";
import path from "path";
import {
  resolveDebArchitecture,
  standardProfileImages,
} from "./architecture.mjs";
import {
  stableJSONString,
} from "./guest-kernel-artifact.mjs";

export const profileImageArtifactSchemaVersion = 1;
export const profileImageBuildRecipeVersion = 1;
export const profileImageLayerMediaType = "application/vnd.kiln-vm.rootfs.ext4.gzip";

export function profileImageBuildSpec({ fingerprint, rootfsSizeMB }) {
  const normalized = normalizeProfileFingerprint(fingerprint);
  return {
    schema_version: profileImageArtifactSchemaVersion,
    artifact_kind: "kiln_vm_profile_image",
    build_recipe_version: profileImageBuildRecipeVersion,
    profile: normalized.profile,
    profile_version: normalized.profile_version,
    fingerprint: normalized.fingerprint,
    arch: normalized.arch,
    rootfs_size_mb: normalizeRootfsSizeMB(rootfsSizeMB),
    layer_media_type: profileImageLayerMediaType,
  };
}

export function profileImageSpecFingerprint({ fingerprint, rootfsSizeMB }) {
  return sha256Text(stableJSONString(profileImageBuildSpec({ fingerprint, rootfsSizeMB }))).slice(0, 16);
}

export function profileImageArtifactName({ fingerprint, rootfsSizeMB }) {
  const spec = profileImageBuildSpec({ fingerprint, rootfsSizeMB });
  const specFingerprint = profileImageSpecFingerprint({ fingerprint, rootfsSizeMB });
  return `kiln-vm-profile-${spec.profile}-pv${spec.profile_version}-${spec.arch}-${spec.rootfs_size_mb}mb-${specFingerprint}-rootfs.ext4.gz`;
}

export function profileImageMetadataName({ fingerprint, rootfsSizeMB }) {
  const artifact = profileImageArtifactName({ fingerprint, rootfsSizeMB });
  return artifact.replace(/-rootfs\.ext4\.gz$/u, ".json");
}

export function profileImageArtifactEntry({ fingerprint, rootfsSizeMB }) {
  const spec = profileImageBuildSpec({ fingerprint, rootfsSizeMB });
  return {
    ...spec,
    spec_fingerprint: profileImageSpecFingerprint({ fingerprint, rootfsSizeMB }),
    artifact_name: profileImageArtifactName({ fingerprint, rootfsSizeMB }),
    metadata_name: profileImageMetadataName({ fingerprint, rootfsSizeMB }),
  };
}

export function profileImageReleaseTag(entries) {
  const normalized = normalizeEntries(entries);
  if (normalized.length === 0) {
    throw new Error("profile image release tag requires at least one entry");
  }
  const arches = new Set(normalized.map((entry) => entry.arch));
  if (arches.size !== 1) {
    throw new Error(`profile image release tag requires one arch, got: ${[...arches].join(", ")}`);
  }
  const arch = normalized[0].arch;
  const specs = normalized.map((entry) => profileImageReleaseSpec(entry));
  const fingerprint = sha256Text(stableJSONString(specs)).slice(0, 16);
  return `profile-images-${arch}-${fingerprint}`;
}

export function createProfileImagePlan({ arch, fingerprints }) {
  const resolved = resolveDebArchitecture(arch).debArch;
  const byProfile = new Map();
  for (const fingerprint of fingerprints) {
    const normalized = normalizeProfileFingerprint(fingerprint);
    if (normalized.arch !== resolved) {
      throw new Error(`${normalized.profile}: fingerprint arch ${normalized.arch} does not match ${resolved}`);
    }
    byProfile.set(normalized.profile, normalized);
  }

  const entries = standardProfileImages.map(({ profile, rootfsSizeMB }) => {
    const fingerprint = byProfile.get(profile);
    if (!fingerprint) throw new Error(`missing profile fingerprint for ${profile}`);
    return profileImageArtifactEntry({ fingerprint, rootfsSizeMB });
  });
  const releaseTag = profileImageReleaseTag(entries);
  return {
    schema_version: profileImageArtifactSchemaVersion,
    artifact_kind: "kiln_vm_profile_image_set",
    release_tag: releaseTag,
    arch: resolved,
    entries: entries.map((entry) => ({
      release_tag: releaseTag,
      ...entry,
    })),
  };
}

export function stampProfileImageMetadata({ entry, artifactPath, metadataPath }) {
  const metadataFile = path.resolve(metadataPath);
  const artifactFile = path.resolve(artifactPath);
  const metadata = JSON.parse(fs.readFileSync(metadataFile, "utf8"));
  assertProfileMetadataMatchesEntry(metadata, entry, metadataFile);
  const compressedSHA256 = `sha256:${sha256File(artifactFile)}`;
  assertDeepEqual(normalizeSHA256(metadata.compressed_sha256), compressedSHA256, `${metadataFile} compressed_sha256`);
  const stamped = {
    ...metadata,
    schema_version: entry.schema_version,
    artifact_kind: entry.artifact_kind,
    build_recipe_version: entry.build_recipe_version,
    spec_fingerprint: entry.spec_fingerprint,
    release_tag: entry.release_tag,
    artifact: entry.artifact_name,
    compressed_sha256: compressedSHA256,
  };
  fs.writeFileSync(metadataFile, `${JSON.stringify(stamped, null, 2)}\n`);
  return stamped;
}

export function verifyProfileImagePlanArtifacts({ planPath, artifactDir, metadataOnly = false }) {
  const plan = JSON.parse(fs.readFileSync(path.resolve(planPath), "utf8"));
  verifyProfileImagePlan(plan);
  const dir = path.resolve(artifactDir);
  for (const entry of plan.entries) {
    const metadataPath = path.join(dir, entry.metadata_name);
    if (metadataOnly) {
      verifyProfileImageMetadata({ entry, metadataPath });
    } else {
      verifyProfileImageArtifact({
        entry,
        artifactPath: path.join(dir, entry.artifact_name),
        metadataPath,
      });
    }
  }
  return plan;
}

export function verifyProfileImageArtifact({ entry, artifactPath, metadataPath }) {
  const artifactFile = path.resolve(artifactPath);
  requireFile(artifactFile, `${entry.profile} profile artifact`);
  const metadata = verifyProfileImageMetadata({ entry, metadataPath });
  const metadataFile = path.resolve(metadataPath);
  const compressedSHA256 = `sha256:${sha256File(artifactFile)}`;
  assertDeepEqual(normalizeSHA256(metadata.compressed_sha256), compressedSHA256, `${metadataFile} compressed_sha256`);
  return metadata;
}

export function verifyProfileImageMetadata({ entry, metadataPath }) {
  const metadataFile = path.resolve(metadataPath);
  requireFile(metadataFile, `${entry.profile} profile metadata`);
  const metadata = JSON.parse(fs.readFileSync(metadataFile, "utf8"));
  for (const key of [
    "schema_version",
    "artifact_kind",
    "build_recipe_version",
    "spec_fingerprint",
    "release_tag",
    "profile",
    "profile_version",
    "fingerprint",
    "arch",
    "rootfs_size_mb",
    "artifact",
    "layer_media_type",
  ]) {
    const expectedKey = key === "artifact" ? "artifact_name" : key;
    assertDeepEqual(metadata[key], entry[expectedKey], `${metadataFile} ${key}`);
  }
  if (!/^sha256:[a-f0-9]{64}$/u.test(normalizeSHA256(metadata.compressed_sha256))) {
    throw new Error(`${metadataFile} compressed_sha256 is invalid: ${metadata.compressed_sha256}`);
  }
  if (!/^sha256:[a-f0-9]{64}$/u.test(normalizeSHA256(metadata.rootfs_sha256))) {
    throw new Error(`${metadataFile} rootfs_sha256 is invalid: ${metadata.rootfs_sha256}`);
  }
  return metadata;
}

export function verifyProfileImagePlan(plan) {
  if (plan.schema_version !== profileImageArtifactSchemaVersion) {
    throw new Error(`unsupported profile image plan schema_version ${plan.schema_version}`);
  }
  if (plan.artifact_kind !== "kiln_vm_profile_image_set") {
    throw new Error(`unsupported profile image plan artifact_kind ${plan.artifact_kind}`);
  }
  const entries = normalizeEntries(plan.entries);
  const releaseTag = profileImageReleaseTag(entries);
  assertDeepEqual(plan.release_tag, releaseTag, "profile image plan release_tag");
  for (const entry of entries) {
    assertDeepEqual(entry.release_tag, releaseTag, `${entry.profile} release_tag`);
    const expected = profileImageArtifactEntry({ fingerprint: entry, rootfsSizeMB: entry.rootfs_size_mb });
    for (const [key, value] of Object.entries(expected)) {
      assertDeepEqual(entry[key], value, `${entry.profile} ${key}`);
    }
  }
  return true;
}

function normalizeEntries(entries) {
  if (!Array.isArray(entries)) throw new Error("profile image entries must be an array");
  return [...entries].sort((left, right) =>
    String(left.profile).localeCompare(String(right.profile)) ||
    String(left.arch).localeCompare(String(right.arch)) ||
    Number(left.rootfs_size_mb) - Number(right.rootfs_size_mb)
  );
}

function profileImageReleaseSpec(entry) {
  return {
    schema_version: entry.schema_version,
    artifact_kind: entry.artifact_kind,
    build_recipe_version: entry.build_recipe_version,
    profile: entry.profile,
    profile_version: entry.profile_version,
    fingerprint: entry.fingerprint,
    arch: entry.arch,
    rootfs_size_mb: entry.rootfs_size_mb,
    layer_media_type: entry.layer_media_type,
    spec_fingerprint: entry.spec_fingerprint,
    artifact_name: entry.artifact_name,
    metadata_name: entry.metadata_name,
  };
}

function normalizeProfileFingerprint(value) {
  if (!value || typeof value !== "object") throw new Error(`profile fingerprint must be an object: ${JSON.stringify(value)}`);
  const profile = requiredString(value.profile, "profile");
  const profileVersion = Number(value.profile_version);
  if (!Number.isInteger(profileVersion) || profileVersion <= 0) throw new Error(`${profile}: profile_version must be a positive integer`);
  const fingerprint = requiredString(value.fingerprint, `${profile} fingerprint`);
  if (!/^[a-f0-9]{64}$/u.test(fingerprint)) throw new Error(`${profile}: fingerprint must be a sha256 hex string`);
  const arch = resolveDebArchitecture(requiredString(value.arch, `${profile} arch`)).debArch;
  return {
    profile,
    profile_version: profileVersion,
    fingerprint,
    arch,
  };
}

function normalizeRootfsSizeMB(value) {
  const normalized = Number(value);
  if (!Number.isInteger(normalized) || normalized <= 0) {
    throw new Error(`rootfsSizeMB must be a positive integer, got ${JSON.stringify(value)}`);
  }
  return normalized;
}

function assertProfileMetadataMatchesEntry(metadata, entry, label) {
  for (const key of ["profile", "profile_version", "fingerprint", "arch", "rootfs_size_mb", "layer_media_type"]) {
    assertDeepEqual(metadata[key], entry[key], `${label} ${key}`);
  }
  assertDeepEqual(metadata.artifact, entry.artifact_name, `${label} artifact`);
}

function sha256File(filePath) {
  return crypto.createHash("sha256").update(fs.readFileSync(filePath)).digest("hex");
}

function normalizeSHA256(value) {
  const normalized = String(value || "").trim();
  return normalized.startsWith("sha256:") ? normalized : `sha256:${normalized}`;
}

function requireFile(filePath, label) {
  if (!fs.existsSync(filePath) || !fs.statSync(filePath).isFile()) {
    throw new Error(`${label} does not exist: ${filePath}`);
  }
}

function requiredString(value, label) {
  const normalized = String(value || "").trim();
  if (!normalized) throw new Error(`${label} is required`);
  return normalized;
}

function assertDeepEqual(actual, expected, label) {
  if (JSON.stringify(actual) !== JSON.stringify(expected)) {
    throw new Error(`${label} mismatch: got ${JSON.stringify(actual)}, expected ${JSON.stringify(expected)}`);
  }
}

function sha256Text(value) {
  return crypto.createHash("sha256").update(value).digest("hex");
}
