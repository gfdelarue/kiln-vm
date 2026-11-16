#!/usr/bin/env node
import {
  verifyGuestKernelArtifact,
} from "./lib/guest-kernel-artifact.mjs";

const options = parseArgs(process.argv.slice(2));
const metadata = verifyGuestKernelArtifact({
  debArch: required(options.arch, "--arch"),
  kernelPath: required(options.kernel, "--kernel"),
  metadataPath: required(options.metadata, "--metadata"),
});

console.log(JSON.stringify({
  artifact_name: metadata.artifact_name,
  metadata_name: metadata.metadata_name,
  release_tag: metadata.release_tag,
  sha256: metadata.sha256,
  size_bytes: metadata.size_bytes,
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
  console.error("Usage: node scripts/verify-guest-kernel-artifact.mjs --arch <amd64|arm64> --kernel <vmlinux> --metadata <json>");
  process.exit(2);
}
