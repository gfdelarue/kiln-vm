import assert from "node:assert/strict";
import crypto from "node:crypto";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { test } from "node:test";
import {
  profileRuntimeArtifactPlan,
  profileRuntimeArtifactSchemaVersion,
  profileRuntimeBinaryNames,
  profileRuntimeBuildRecipeVersion,
  profileRuntimeBuildSpec,
  profileRuntimeNoticeArtifactName,
  profileRuntimeNotices,
  profileRuntimeReleaseTag,
  profileRuntimeSourceFingerprint,
  profileRuntimeSpecFingerprint,
  verifyProfileRuntimeArtifact,
  installProfileRuntimeArtifact,
  writeProfileRuntimeArtifact,
} from "../lib/profile-runtime-artifact.mjs";

test("build spec pins the runtime build recipe and fingerprints", () => {
  const syntheticSourceFingerprint = sha256("synthetic profile runtime source");
  const spec = profileRuntimeBuildSpec({
    debArch: "amd64",
    goVersion: "go1.24.0",
    sourceFingerprint: syntheticSourceFingerprint,
  });
  assert.equal(spec.schema_version, profileRuntimeArtifactSchemaVersion);
  assert.equal(spec.artifact_kind, "kiln_vm_profile_runtime");
  assert.equal(spec.build_recipe_version, profileRuntimeBuildRecipeVersion);
  assert.equal(spec.deb_arch, "amd64");
  assert.equal(spec.goos, "linux");
  assert.equal(spec.cgo_enabled, "0");
  assert.equal(spec.trimpath, true);
  assert.equal(spec.ldflags, "-s -w");
  assert.deepEqual(
    spec.notices,
    profileRuntimeNotices.map(({ name, module, source_path, install_path }) => ({
      name,
      module,
      source_path,
      install_path,
    })),
  );
  assert.equal(spec.source_fingerprint, syntheticSourceFingerprint);
  assert.match(profileRuntimeSpecFingerprint(spec), /^[a-f0-9]{16}$/u);
  assert.match(profileRuntimeReleaseTag(spec), /^profile-runtime-amd64-[a-f0-9]{16}$/u);
});

test("live plan names every binary and notice from the checkout", () => {
  const livePlan = profileRuntimeArtifactPlan("amd64");
  assert.equal(livePlan.binaries.length, profileRuntimeBinaryNames.length);
  assert.equal(livePlan.notices.length, profileRuntimeNotices.length);
  assert.match(livePlan.release_tag, /^profile-runtime-amd64-[a-f0-9]{16}$/u);
  assert.match(livePlan.metadata_name, /^kiln-vm-profile-runtime-amd64-[a-f0-9]{16}\.json$/u);
  assert.equal(profileRuntimeSourceFingerprint({ repoRoot: process.cwd(), debArch: "amd64" }).length, 64);
  for (const notice of livePlan.notices) {
    assert.equal(
      notice.artifact_name,
      profileRuntimeNoticeArtifactName("amd64", livePlan.spec_fingerprint, notice.name),
    );
  }
});

test("written artifact matches the live plan and verifies", (t) => {
  const livePlan = profileRuntimeArtifactPlan("amd64");
  const { metadata, artifactDir } = writtenArtifact(t);
  assert.equal(metadata.release_tag, livePlan.release_tag);
  assert.equal(metadata.metadata_name, livePlan.metadata_name);
  assert.equal(metadata.binaries.length, profileRuntimeBinaryNames.length);
  assert.equal(metadata.notices.length, profileRuntimeNotices.length);
  verifyProfileRuntimeArtifact({ debArch: "amd64", artifactDir });
});

test("installed artifact reproduces every binary and notice byte for byte", (t) => {
  const livePlan = profileRuntimeArtifactPlan("amd64");
  const { tempDir, runtimeDir, artifactDir } = writtenArtifact(t);
  const installDir = path.join(tempDir, "install");
  const installed = installProfileRuntimeArtifact({
    debArch: "amd64",
    artifactDir,
    runtimeDir: installDir,
  });
  assert.equal(installed.release_tag, livePlan.release_tag);
  for (const name of profileRuntimeBinaryNames) {
    assert.equal(
      fs.readFileSync(path.join(installDir, name), "utf8"),
      fs.readFileSync(path.join(runtimeDir, name), "utf8"),
    );
  }
  for (const notice of profileRuntimeNotices) {
    assert.equal(
      fs.readFileSync(path.join(installDir, notice.install_path), "utf8"),
      fs.readFileSync(path.join(process.cwd(), notice.source_path), "utf8"),
    );
  }
});

test("a tampered notice fails verification", (t) => {
  const { metadata, artifactDir } = writtenArtifact(t);
  fs.appendFileSync(path.join(artifactDir, metadata.notices[0].artifact_name), "tamper\n");
  assert.throws(
    () => verifyProfileRuntimeArtifact({ debArch: "amd64", artifactDir }),
    /sha256 mismatch/,
  );
});

test("a tampered binary fails verification", (t) => {
  const { metadata, artifactDir } = writtenArtifact(t);
  fs.appendFileSync(path.join(artifactDir, metadata.binaries[0].artifact_name), "tamper\n");
  assert.throws(
    () => verifyProfileRuntimeArtifact({ debArch: "amd64", artifactDir }),
    /sha256 mismatch/,
  );
});

function writtenArtifact(t) {
  const tempDir = fs.mkdtempSync(path.join(os.tmpdir(), "kiln-vm-profile-runtime-contract-"));
  t.after(() => fs.rmSync(tempDir, { recursive: true, force: true }));
  const runtimeDir = path.join(tempDir, "runtime");
  const artifactDir = path.join(tempDir, "artifacts");
  fs.mkdirSync(runtimeDir, { recursive: true });
  for (const name of profileRuntimeBinaryNames) {
    fs.writeFileSync(path.join(runtimeDir, name), `synthetic binary ${name}\n`, { mode: 0o755 });
  }
  const metadata = writeProfileRuntimeArtifact({
    debArch: "amd64",
    runtimeDir,
    artifactDir,
  });
  return { tempDir, runtimeDir, artifactDir, metadata };
}

function sha256(value) {
  return crypto.createHash("sha256").update(value).digest("hex");
}
