#!/usr/bin/env node
import path from "path";
import {
  stampProfileImageMetadata,
  verifyProfileImagePlan,
} from "./lib/profile-image-artifact.mjs";
import fs from "fs";

const options = parseArgs(process.argv.slice(2));
const planPath = requiredPath(options.plan, "--plan");
const metadataPath = requiredPath(options.metadata, "--metadata");
const artifactPath = requiredPath(options.artifact, "--artifact");
const plan = JSON.parse(fs.readFileSync(planPath, "utf8"));
verifyProfileImagePlan(plan);

const metadataName = path.basename(metadataPath);
const entry = plan.entries.find((candidate) => candidate.metadata_name === metadataName);
if (!entry) {
  throw new Error(`${metadataPath} is not part of ${planPath}`);
}

const stamped = stampProfileImageMetadata({ entry, artifactPath, metadataPath });
console.log(JSON.stringify({
  profile: stamped.profile,
  arch: stamped.arch,
  artifact: stamped.artifact,
  release_tag: stamped.release_tag,
  compressed_sha256: stamped.compressed_sha256,
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

function requiredPath(value, label) {
  const normalized = String(value || "").trim();
  if (!normalized) usage(`${label} is required`);
  const resolved = path.resolve(normalized);
  if (!fs.existsSync(resolved) || !fs.statSync(resolved).isFile()) usage(`${label} does not exist: ${resolved}`);
  return resolved;
}

function usage(message) {
  if (message) console.error(message);
  console.error("Usage: node scripts/stamp-profile-image-metadata.mjs --plan <profile-plan.json> --artifact <rootfs.ext4.gz> --metadata <json>");
  process.exit(2);
}
