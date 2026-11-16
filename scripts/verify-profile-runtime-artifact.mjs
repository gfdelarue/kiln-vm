#!/usr/bin/env node
import {
  installProfileRuntimeArtifact,
  verifyProfileRuntimeArtifact,
} from "./lib/profile-runtime-artifact.mjs";

const options = parseArgs(process.argv.slice(2));
const args = {
  debArch: required(options.arch, "--arch"),
  artifactDir: required(options["artifact-dir"], "--artifact-dir"),
};
const metadata = options["install-dir"]
  ? installProfileRuntimeArtifact({ ...args, runtimeDir: required(options["install-dir"], "--install-dir") })
  : verifyProfileRuntimeArtifact(args);

console.log(JSON.stringify({
  release_tag: metadata.release_tag,
  metadata_name: metadata.metadata_name,
  binaries: metadata.binaries.map((binary) => ({
    name: binary.name,
    artifact_name: binary.artifact_name,
    sha256: binary.sha256,
    size_bytes: binary.size_bytes,
  })),
  notices: metadata.notices.map((notice) => ({
    name: notice.name,
    module: notice.module,
    artifact_name: notice.artifact_name,
    install_path: notice.install_path,
    sha256: notice.sha256,
    size_bytes: notice.size_bytes,
  })),
}, null, 2));

function parseArgs(args) {
  const parsed = {};
  for (let index = 0; index < args.length; index += 1) {
    const arg = args[index];
    if (!arg.startsWith("--")) usage(`unexpected argument: ${arg}`);
    const key = arg.slice(2);
    const value = args[index + 1];
    if (!value || value.startsWith("--")) usage(`missing value for ${arg}`);
    parsed[key] = value;
    index += 1;
  }
  return parsed;
}

function required(value, label) {
  const normalized = String(value || "").trim();
  if (!normalized) usage(`${label} is required`);
  return normalized;
}

function usage(message) {
  if (message) console.error(message);
  console.error("Usage: node scripts/verify-profile-runtime-artifact.mjs --arch <amd64|arm64> --artifact-dir <dir> [--install-dir <dir>]");
  process.exit(2);
}
