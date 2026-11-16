#!/usr/bin/env node
import fs from "node:fs";
import path from "node:path";
import { spawnSync } from "node:child_process";

const options = parseArgs(process.argv.slice(2));
for (const key of ["catalog", "agent-bin", "fingerprint-tool"]) {
  if (!options[key]) usage(`--${key} is required`);
}

const catalogPath = path.resolve(options.catalog);
const agentBin = path.resolve(options["agent-bin"]);
const fingerprintTool = path.resolve(options["fingerprint-tool"]);
const arch = options.arch || "";
const requiredProfiles = asArray(options["required-profile"]);
const catalog = JSON.parse(fs.readFileSync(catalogPath, "utf8"));
if (catalog.schema_version !== 1) {
  throw new Error(`unsupported catalog schema_version ${catalog.schema_version}`);
}
if (!Array.isArray(catalog.images) || catalog.images.length === 0) {
  throw new Error("profile catalog has no images");
}

const checked = [];
const entries = arch ? catalog.images.filter((entry) => entry.arch === arch) : catalog.images;
if (entries.length === 0) {
  throw new Error(arch ? `profile catalog has no images for arch ${arch}` : "profile catalog has no images");
}

for (const profile of requiredProfiles) {
  if (!entries.some((entry) => entry.profile === profile)) {
    throw new Error(`profile catalog is missing required ${arch ? `${arch} ` : ""}profile ${profile}`);
  }
}

for (const entry of entries) {
  const expected = profileFingerprint(entry.profile);
  if (entry.profile_version !== expected.profile_version) {
    throw new Error(`${entry.profile}: catalog profile_version ${entry.profile_version} does not match runtime profile_version ${expected.profile_version}`);
  }
  if (entry.arch !== expected.arch) {
    throw new Error(`${entry.profile}: catalog arch ${entry.arch} does not match runtime arch ${expected.arch}`);
  }
  if (entry.fingerprint !== expected.fingerprint) {
    throw new Error(`${entry.profile}: catalog fingerprint ${entry.fingerprint} does not match packaged runtime fingerprint ${expected.fingerprint}`);
  }
  checked.push(`${entry.profile}:${entry.rootfs_size_mb}mb`);
}

console.log(`profile catalog ok${arch ? ` (${arch})` : ""}: ${checked.join(", ")}`);

function profileFingerprint(profile) {
  const args = [
    "--profile",
    profile,
    "--agent-bin",
    agentBin,
    "--fingerprint",
  ];
  if (arch) args.push("--arch", arch);
  const result = spawnSync(fingerprintTool, args, { encoding: "utf8" });
  if (result.status !== 0) {
    throw new Error(`fingerprint ${profile} failed: ${result.stderr || result.stdout}`);
  }
  return JSON.parse(result.stdout);
}

function parseArgs(args) {
  const parsed = {};
  for (let index = 0; index < args.length; index += 1) {
    const arg = args[index];
    if (!arg.startsWith("--")) usage(`unexpected argument: ${arg}`);
    const key = arg.slice(2);
    const value = args[index + 1];
    if (!value || value.startsWith("--")) usage(`missing value for ${arg}`);
    index += 1;
    if (parsed[key] === undefined) parsed[key] = value;
    else if (Array.isArray(parsed[key])) parsed[key].push(value);
    else parsed[key] = [parsed[key], value];
  }
  return parsed;
}

function asArray(value) {
  if (value === undefined) return [];
  return Array.isArray(value) ? value : [value];
}

function usage(message) {
  if (message) console.error(message);
  console.error("Usage: node scripts/verify-profile-catalog.mjs --catalog <profile-images.json> --agent-bin <kiln-vm-agent> --fingerprint-tool <kiln-vm-profile-artifact> [--arch amd64|arm64] [--required-profile <profile>...]");
  process.exit(2);
}
