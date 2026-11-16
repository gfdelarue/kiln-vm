#!/usr/bin/env node
import fs from "fs";
import path from "path";
import { spawnSync } from "child_process";
import {
  createProfileImagePlan,
} from "./lib/profile-image-artifact.mjs";
import {
  standardProfileImages,
} from "./lib/architecture.mjs";

const options = parseArgs(process.argv.slice(2));
const arch = required(options.arch || process.env.PROFILE_ARCH || process.env.DEB_ARCH, "--arch");
const fingerprintTool = requiredPath(options["fingerprint-tool"], "--fingerprint-tool");
const agentBin = requiredPath(options["agent-bin"], "--agent-bin");

const fingerprints = standardProfileImages.map(({ profile }) => profileFingerprint({
  fingerprintTool,
  agentBin,
  arch,
  profile,
}));
const plan = createProfileImagePlan({ arch, fingerprints });

if (options.out) {
  const out = path.resolve(options.out);
  fs.mkdirSync(path.dirname(out), { recursive: true });
  fs.writeFileSync(out, `${JSON.stringify(plan, null, 2)}\n`);
}

if (options.tsv) {
  const tsv = path.resolve(options.tsv);
  fs.mkdirSync(path.dirname(tsv), { recursive: true });
  fs.writeFileSync(tsv, plan.entries.map((entry) => [
    entry.profile,
    entry.rootfs_size_mb,
    entry.artifact_name,
    entry.metadata_name,
  ].join("\t")).join("\n") + "\n");
}

if (options["github-output"]) {
  const output = path.resolve(options["github-output"]);
  fs.appendFileSync(output, [
    `release_tag=${plan.release_tag}`,
    `arch=${plan.arch}`,
    "",
  ].join("\n"));
}

console.log(JSON.stringify(plan, null, 2));

function profileFingerprint({ fingerprintTool, agentBin, arch, profile }) {
  const result = spawnSync(fingerprintTool, [
    "--fingerprint",
    "--profile", profile,
    "--agent-bin", agentBin,
    "--arch", arch,
  ], { encoding: "utf8" });
  if (result.status !== 0) {
    throw new Error(`profile fingerprint ${profile} failed:\n${result.stdout}${result.stderr}`);
  }
  try {
    return JSON.parse(result.stdout);
  } catch (error) {
    throw new Error(`profile fingerprint ${profile} returned invalid JSON: ${error.message}\n${result.stdout}`);
  }
}

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

function requiredPath(value, label) {
  const resolved = path.resolve(required(value, label));
  if (!fs.existsSync(resolved) || !fs.statSync(resolved).isFile()) usage(`${label} does not exist: ${resolved}`);
  return resolved;
}

function usage(message) {
  if (message) console.error(message);
  console.error("Usage: node scripts/profile-image-artifact-plan.mjs --arch <amd64|arm64> --fingerprint-tool <kiln-vm-profile-artifact> --agent-bin <kiln-vm-agent> [--out <json>] [--tsv <tsv>] [--github-output <path>]");
  process.exit(2);
}
