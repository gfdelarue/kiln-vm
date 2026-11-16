import assert from "node:assert/strict";
import crypto from "node:crypto";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { test } from "node:test";
import {
  createProfileImagePlan,
  stampProfileImageMetadata,
  verifyProfileImagePlan,
  verifyProfileImagePlanArtifacts,
} from "../lib/profile-image-artifact.mjs";
import { standardProfileImages } from "../lib/architecture.mjs";

test("plan covers every standard profile under a content-addressed release tag", () => {
  const plan = syntheticPlan();
  verifyProfileImagePlan(plan);
  assert.match(plan.release_tag, /^profile-images-amd64-[a-f0-9]{16}$/u);
  assert.equal(plan.entries.length, standardProfileImages.length);
});

test("each entry has a version-independent artifact name and stamps its metadata", (t) => {
  const plan = syntheticPlan();
  const tempDir = tempDirFor(t);
  for (const entry of plan.entries) {
    assert.equal(entry.artifact_kind, "kiln_vm_profile_image");
    assert.equal(entry.release_tag, plan.release_tag);
    assert.match(entry.artifact_name, /^kiln-vm-profile-[a-z0-9-]+-pv[0-9]+-amd64-[0-9]+mb-[a-f0-9]{16}-rootfs\.ext4\.gz$/u);
    assert(!entry.artifact_name.includes("0.1."), `artifact name is release-version coupled: ${entry.artifact_name}`);
    const stamped = writeStampedEntry(tempDir, entry);
    assert.equal(stamped.artifact_kind, "kiln_vm_profile_image");
    assert.equal(stamped.release_tag, plan.release_tag);
    assert.equal(stamped.spec_fingerprint, entry.spec_fingerprint);
  }
});

test("stamped artifacts verify with and without their content", (t) => {
  const { planPath, tempDir } = stampedArtifacts(t);
  verifyProfileImagePlanArtifacts({ planPath, artifactDir: tempDir, metadataOnly: true });
  verifyProfileImagePlanArtifacts({ planPath, artifactDir: tempDir });
});

test("a tampered artifact fails only the content verification", (t) => {
  const { plan, planPath, tempDir } = stampedArtifacts(t);
  fs.appendFileSync(path.join(tempDir, plan.entries[0].artifact_name), "tamper\n");
  verifyProfileImagePlanArtifacts({ planPath, artifactDir: tempDir, metadataOnly: true });
  assert.throws(() => verifyProfileImagePlanArtifacts({ planPath, artifactDir: tempDir }), /compressed_sha256 mismatch/);
});

test("missing artifacts fail only the content verification", (t) => {
  const { plan, planPath, tempDir } = stampedArtifacts(t);
  for (const entry of plan.entries) {
    fs.rmSync(path.join(tempDir, entry.artifact_name));
  }
  verifyProfileImagePlanArtifacts({ planPath, artifactDir: tempDir, metadataOnly: true });
  assert.throws(() => verifyProfileImagePlanArtifacts({ planPath, artifactDir: tempDir }), /profile artifact does not exist/);
});

function syntheticPlan() {
  const fingerprints = standardProfileImages.map(({ profile }) => ({
    profile,
    profile_version: profileVersion(profile),
    fingerprint: crypto.createHash("sha256").update(`profile:${profile}`).digest("hex"),
    arch: "amd64",
  }));
  return createProfileImagePlan({ arch: "amd64", fingerprints });
}

function stampedArtifacts(t) {
  const plan = syntheticPlan();
  const tempDir = tempDirFor(t);
  for (const entry of plan.entries) {
    writeStampedEntry(tempDir, entry);
  }
  const planPath = path.join(tempDir, "profile-plan.json");
  fs.writeFileSync(planPath, JSON.stringify(plan, null, 2) + "\n");
  return { plan, planPath, tempDir };
}

function writeStampedEntry(tempDir, entry) {
  const artifactPath = path.join(tempDir, entry.artifact_name);
  const metadataPath = path.join(tempDir, entry.metadata_name);
  const artifact = Buffer.from(`synthetic ${entry.profile} ${entry.arch} ${entry.rootfs_size_mb}\n`);
  fs.writeFileSync(artifactPath, artifact);
  fs.writeFileSync(metadataPath, JSON.stringify({
    schema_version: 1,
    profile: entry.profile,
    profile_version: entry.profile_version,
    fingerprint: entry.fingerprint,
    arch: entry.arch,
    rootfs_size_mb: entry.rootfs_size_mb,
    artifact: entry.artifact_name,
    compressed_sha256: `sha256:${sha256(artifact)}`,
    rootfs_sha256: `sha256:${sha256(Buffer.from(`rootfs ${entry.profile}`))}`,
    layer_media_type: entry.layer_media_type,
    created_at: "2026-01-01T00:00:00Z",
  }, null, 2) + "\n");
  return stampProfileImageMetadata({ entry, artifactPath, metadataPath });
}

function tempDirFor(t) {
  const tempDir = fs.mkdtempSync(path.join(os.tmpdir(), "kiln-vm-profile-cache-contract-"));
  t.after(() => fs.rmSync(tempDir, { recursive: true, force: true }));
  return tempDir;
}

function sha256(value) {
  return crypto.createHash("sha256").update(value).digest("hex");
}

function profileVersion(profile) {
  if (profile === "ubuntu-docker") return 10;
  return 1;
}
