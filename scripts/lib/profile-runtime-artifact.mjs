import crypto from "crypto";
import fs from "fs";
import path from "path";
import { spawnSync } from "child_process";
import {
  resolveDebArchitecture,
  standardProfileImages,
} from "./architecture.mjs";
import {
  profileImageArtifactSchemaVersion,
  profileImageBuildRecipeVersion,
} from "./profile-image-artifact.mjs";
import {
  stableJSONString,
} from "./guest-kernel-artifact.mjs";

export const profileRuntimeArtifactSchemaVersion = 2;
export const profileRuntimeBuildRecipeVersion = 2;
export const profileRuntimeBinaryNames = [
  "kiln-vm-agent",
  "kiln-vm-profile-artifact",
];
export const profileRuntimePackages = [
  "./cmd/kiln-vm-agent",
  "./cmd/kiln-vm-profile-artifact",
];
export const profileRuntimeNotices = [
  {
    name: "miekg-dns",
    module: "github.com/miekg/dns@v1.1.72",
    source_path: "third_party/github.com/miekg/dns/LICENSE",
    install_path: "third-party-notices/miekg-dns.LICENSE",
  },
  {
    name: "golang-x-net",
    module: "golang.org/x/net@v0.55.0",
    source_path: "third_party/golang.org/x/net/LICENSE",
    install_path: "third-party-notices/golang-x-net.LICENSE",
  },
];
export const profileRuntimeBuildFlags = {
  cgo_enabled: "0",
  goos: "linux",
  trimpath: true,
  ldflags: "-s -w",
};

export function profileRuntimeArtifactPlan(debArch, options = {}) {
  const arch = resolveDebArchitecture(debArch);
  const repoRoot = path.resolve(options.repoRoot || process.cwd());
  const goVersion = options.goVersion || detectGoVersion();
  const sourceFingerprint = options.sourceFingerprint || profileRuntimeSourceFingerprint({
    repoRoot,
    debArch: arch.debArch,
  });
  const spec = profileRuntimeBuildSpec({
    debArch: arch.debArch,
    goVersion,
    sourceFingerprint,
  });
  const specFingerprint = profileRuntimeSpecFingerprint(spec);
  return {
    ...spec,
    release_tag: profileRuntimeReleaseTag(spec),
    spec_fingerprint: specFingerprint,
    metadata_name: profileRuntimeMetadataName(arch.debArch, specFingerprint),
    binaries: profileRuntimeBinaryNames.map((name) => ({
      name,
      artifact_name: profileRuntimeArtifactName(arch.debArch, specFingerprint, name),
    })),
    notices: profileRuntimeNotices.map((notice) => ({
      ...notice,
      artifact_name: profileRuntimeNoticeArtifactName(arch.debArch, specFingerprint, notice.name),
    })),
  };
}

export function profileRuntimeBuildSpec({ debArch, goVersion, sourceFingerprint }) {
  const arch = resolveDebArchitecture(debArch);
  return {
    schema_version: profileRuntimeArtifactSchemaVersion,
    artifact_kind: "kiln_vm_profile_runtime",
    build_recipe_version: profileRuntimeBuildRecipeVersion,
    deb_arch: arch.debArch,
    go_arch: arch.goArch,
    goos: profileRuntimeBuildFlags.goos,
    cgo_enabled: profileRuntimeBuildFlags.cgo_enabled,
    trimpath: profileRuntimeBuildFlags.trimpath,
    ldflags: profileRuntimeBuildFlags.ldflags,
    go_version: requiredString(goVersion, "goVersion"),
    packages: [...profileRuntimePackages],
    notices: profileRuntimeNotices.map(({ name, module, source_path, install_path }) => ({
      name,
      module,
      source_path,
      install_path,
    })),
    source_fingerprint: requiredSHA256(sourceFingerprint, "sourceFingerprint"),
    profile_image_artifact_schema_version: profileImageArtifactSchemaVersion,
    profile_image_build_recipe_version: profileImageBuildRecipeVersion,
    standard_profile_images: standardProfileImages.map((entry) => ({
      profile: entry.profile,
      rootfs_size_mb: entry.rootfsSizeMB,
    })),
  };
}

export function profileRuntimeSpecFingerprint(spec) {
  return sha256Text(stableJSONString(spec)).slice(0, 16);
}

export function profileRuntimeReleaseTag(spec) {
  return `profile-runtime-${spec.deb_arch}-${profileRuntimeSpecFingerprint(spec)}`;
}

export function profileRuntimeArtifactName(debArch, specFingerprint, binaryName) {
  const arch = resolveDebArchitecture(debArch);
  assertBinaryName(binaryName);
  return `kiln-vm-profile-runtime-${arch.debArch}-${requiredFingerprint(specFingerprint, "specFingerprint")}-${binaryName}`;
}

export function profileRuntimeMetadataName(debArch, specFingerprint) {
  const arch = resolveDebArchitecture(debArch);
  return `kiln-vm-profile-runtime-${arch.debArch}-${requiredFingerprint(specFingerprint, "specFingerprint")}.json`;
}

export function profileRuntimeNoticeArtifactName(debArch, specFingerprint, noticeName) {
  const arch = resolveDebArchitecture(debArch);
  assertNoticeName(noticeName);
  return `kiln-vm-profile-runtime-${arch.debArch}-${requiredFingerprint(specFingerprint, "specFingerprint")}-${noticeName}.LICENSE`;
}

export function writeProfileRuntimeArtifact({ debArch, runtimeDir, artifactDir, repoRoot }) {
  const plan = profileRuntimeArtifactPlan(debArch, { repoRoot });
  const sourceRoot = path.resolve(repoRoot || process.cwd());
  const runtimeRoot = path.resolve(runtimeDir);
  const artifactRoot = path.resolve(artifactDir);
  fs.mkdirSync(artifactRoot, { recursive: true });
  const binaries = [];
  for (const binary of plan.binaries) {
    const source = path.join(runtimeRoot, binary.name);
    requireFile(source, `${binary.name} runtime binary`);
    const target = path.join(artifactRoot, binary.artifact_name);
    fs.copyFileSync(source, target);
    fs.chmodSync(target, 0o755);
    const bytes = fs.readFileSync(target);
    binaries.push({
      ...binary,
      sha256: sha256Buffer(bytes),
      size_bytes: bytes.length,
    });
  }
  const notices = [];
  for (const notice of plan.notices) {
    const source = path.join(sourceRoot, notice.source_path);
    requireFile(source, `${notice.name} notice source`);
    const target = path.join(artifactRoot, notice.artifact_name);
    fs.copyFileSync(source, target);
    fs.chmodSync(target, 0o644);
    const bytes = fs.readFileSync(target);
    notices.push({
      ...notice,
      sha256: sha256Buffer(bytes),
      size_bytes: bytes.length,
    });
  }
  const metadata = {
    ...plan,
    binaries,
    notices,
  };
  fs.writeFileSync(path.join(artifactRoot, plan.metadata_name), `${JSON.stringify(metadata, null, 2)}\n`);
  return metadata;
}

export function installProfileRuntimeArtifact({ debArch, artifactDir, runtimeDir, repoRoot }) {
  const metadata = verifyProfileRuntimeArtifact({ debArch, artifactDir, repoRoot });
  const artifactRoot = path.resolve(artifactDir);
  const runtimeRoot = path.resolve(runtimeDir);
  fs.mkdirSync(runtimeRoot, { recursive: true });
  for (const binary of metadata.binaries) {
    const source = path.join(artifactRoot, binary.artifact_name);
    const target = path.join(runtimeRoot, binary.name);
    fs.copyFileSync(source, target);
    fs.chmodSync(target, 0o755);
  }
  for (const notice of metadata.notices) {
    const source = path.join(artifactRoot, notice.artifact_name);
    const target = path.join(runtimeRoot, notice.install_path);
    fs.mkdirSync(path.dirname(target), { recursive: true });
    fs.copyFileSync(source, target);
    fs.chmodSync(target, 0o644);
  }
  return metadata;
}

export function verifyProfileRuntimeArtifact({ debArch, artifactDir, repoRoot }) {
  const plan = profileRuntimeArtifactPlan(debArch, { repoRoot });
  const sourceRoot = path.resolve(repoRoot || process.cwd());
  const artifactRoot = path.resolve(artifactDir);
  const metadataPath = path.join(artifactRoot, plan.metadata_name);
  requireFile(metadataPath, "profile runtime metadata");
  const metadata = JSON.parse(fs.readFileSync(metadataPath, "utf8"));
  for (const [key, value] of Object.entries(plan)) {
    if (key === "binaries" || key === "notices") continue;
    assertDeepEqual(metadata[key], value, `${metadataPath} ${key}`);
  }
  if (!Array.isArray(metadata.binaries) || metadata.binaries.length !== plan.binaries.length) {
    throw new Error(`${metadataPath} binaries mismatch`);
  }
  const expectedByName = new Map(plan.binaries.map((binary) => [binary.name, binary]));
  const seenBinaries = new Set();
  for (const binary of metadata.binaries) {
    const expected = expectedByName.get(binary.name);
    if (!expected) throw new Error(`${metadataPath} unexpected binary ${binary.name}`);
    if (seenBinaries.has(binary.name)) throw new Error(`${metadataPath} duplicate binary ${binary.name}`);
    seenBinaries.add(binary.name);
    assertDeepEqual(binary.artifact_name, expected.artifact_name, `${metadataPath} ${binary.name} artifact_name`);
    if (!/^[a-f0-9]{64}$/u.test(String(binary.sha256 || ""))) {
      throw new Error(`${metadataPath} ${binary.name} sha256 is invalid`);
    }
    const artifactPath = path.join(artifactRoot, binary.artifact_name);
    requireFile(artifactPath, `${binary.name} runtime artifact`);
    const bytes = fs.readFileSync(artifactPath);
    assertDeepEqual(binary.sha256, sha256Buffer(bytes), `${metadataPath} ${binary.name} sha256`);
    assertDeepEqual(binary.size_bytes, bytes.length, `${metadataPath} ${binary.name} size_bytes`);
  }
  if (!Array.isArray(metadata.notices) || metadata.notices.length !== plan.notices.length) {
    throw new Error(`${metadataPath} notices mismatch`);
  }
  const expectedNoticesByName = new Map(plan.notices.map((notice) => [notice.name, notice]));
  const seenNotices = new Set();
  for (const notice of metadata.notices) {
    const expected = expectedNoticesByName.get(notice.name);
    if (!expected) throw new Error(`${metadataPath} unexpected notice ${notice.name}`);
    if (seenNotices.has(notice.name)) throw new Error(`${metadataPath} duplicate notice ${notice.name}`);
    seenNotices.add(notice.name);
    for (const key of ["artifact_name", "module", "source_path", "install_path"]) {
      assertDeepEqual(notice[key], expected[key], `${metadataPath} ${notice.name} ${key}`);
    }
    if (!/^[a-f0-9]{64}$/u.test(String(notice.sha256 || ""))) {
      throw new Error(`${metadataPath} ${notice.name} sha256 is invalid`);
    }
    const artifactPath = path.join(artifactRoot, notice.artifact_name);
    requireFile(artifactPath, `${notice.name} notice artifact`);
    const bytes = fs.readFileSync(artifactPath);
    assertDeepEqual(notice.sha256, sha256Buffer(bytes), `${metadataPath} ${notice.name} sha256`);
    assertDeepEqual(notice.size_bytes, bytes.length, `${metadataPath} ${notice.name} size_bytes`);
    const sourcePath = path.join(sourceRoot, notice.source_path);
    requireFile(sourcePath, `${notice.name} notice source`);
    assertDeepEqual(bytes.toString("hex"), fs.readFileSync(sourcePath).toString("hex"), `${metadataPath} ${notice.name} source`);
  }
  return metadata;
}

export function profileRuntimeSourceFingerprint({ repoRoot = process.cwd(), debArch }) {
  const root = path.resolve(repoRoot);
  const arch = resolveDebArchitecture(debArch);
  const manifest = [];
  for (const file of ["go.mod", "go.sum"]) {
    manifest.push(fileDigest(root, file));
  }
  for (const notice of profileRuntimeNotices) {
    manifest.push(fileDigest(root, notice.source_path));
  }
  const template = [
    "{{if not .Standard}}",
    "{{.Dir}}",
    "{{range .GoFiles}}\t{{.}}{{end}}",
    "{{range .CgoFiles}}\t{{.}}{{end}}",
    "{{range .SFiles}}\t{{.}}{{end}}",
    "{{range .HFiles}}\t{{.}}{{end}}",
    "{{range .EmbedFiles}}\t{{.}}{{end}}",
    "{{end}}",
  ].join("");
  const result = spawnSync("go", ["list", "-deps", "-f", template, ...profileRuntimePackages], {
    cwd: root,
    encoding: "utf8",
    env: {
      ...process.env,
      CGO_ENABLED: profileRuntimeBuildFlags.cgo_enabled,
      GOOS: profileRuntimeBuildFlags.goos,
      GOARCH: arch.goArch,
    },
  });
  if (result.status !== 0) {
    throw new Error(`go list profile runtime imports failed:\n${result.stdout}${result.stderr}`);
  }
  for (const line of result.stdout.split(/\r?\n/u)) {
    const trimmed = line.trim();
    if (!trimmed) continue;
    const [dir, ...files] = trimmed.split("\t");
    const absoluteDir = path.resolve(dir);
    const relativeDir = path.relative(root, absoluteDir);
    if (relativeDir.startsWith("..") || path.isAbsolute(relativeDir)) continue;
    for (const file of files) {
      if (!file) continue;
      manifest.push(fileDigest(root, path.join(relativeDir, file)));
    }
  }
  manifest.sort((left, right) => left.path.localeCompare(right.path));
  return sha256Text(stableJSONString({
    deb_arch: arch.debArch,
    go_arch: arch.goArch,
    files: manifest,
  }));
}

export function detectGoVersion() {
  const result = spawnSync("go", ["env", "GOVERSION"], { encoding: "utf8" });
  if (result.status !== 0) {
    throw new Error(`go env GOVERSION failed:\n${result.stdout}${result.stderr}`);
  }
  return result.stdout.trim();
}

function fileDigest(root, relativePath) {
  const normalized = relativePath.split(path.sep).join("/");
  const absolute = path.join(root, normalized);
  requireFile(absolute, normalized);
  return {
    path: normalized,
    sha256: sha256Buffer(fs.readFileSync(absolute)),
  };
}

function requireFile(filePath, label) {
  const stat = fs.statSync(filePath, { throwIfNoEntry: false });
  if (!stat || !stat.isFile()) {
    throw new Error(`${label} does not exist: ${filePath}`);
  }
}

function assertBinaryName(name) {
  if (!profileRuntimeBinaryNames.includes(name)) {
    throw new Error(`unsupported profile runtime binary ${name}`);
  }
}

function assertNoticeName(name) {
  if (!profileRuntimeNotices.some((notice) => notice.name === name)) {
    throw new Error(`unsupported profile runtime notice ${name}`);
  }
}

function requiredString(value, label) {
  const normalized = String(value || "").trim();
  if (!normalized) throw new Error(`${label} is required`);
  return normalized;
}

function requiredSHA256(value, label) {
  const normalized = requiredString(value, label);
  if (!/^[a-f0-9]{64}$/u.test(normalized)) throw new Error(`${label} must be a sha256 hex string`);
  return normalized;
}

function requiredFingerprint(value, label) {
  const normalized = requiredString(value, label);
  if (!/^[a-f0-9]{16}$/u.test(normalized)) throw new Error(`${label} must be a 16-character hex fingerprint`);
  return normalized;
}

function sha256Text(value) {
  return crypto.createHash("sha256").update(value).digest("hex");
}

function sha256Buffer(value) {
  return crypto.createHash("sha256").update(value).digest("hex");
}

function assertDeepEqual(actual, expected, label) {
  if (JSON.stringify(actual) !== JSON.stringify(expected)) {
    throw new Error(`${label} mismatch: got ${JSON.stringify(actual)}, expected ${JSON.stringify(expected)}`);
  }
}
