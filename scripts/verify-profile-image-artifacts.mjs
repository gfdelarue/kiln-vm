#!/usr/bin/env node
import {
  verifyProfileImagePlanArtifacts,
} from "./lib/profile-image-artifact.mjs";

const options = parseArgs(process.argv.slice(2));
const metadataOnly = Boolean(options["metadata-only"]);
const plan = verifyProfileImagePlanArtifacts({
  planPath: required(options.plan, "--plan"),
  artifactDir: required(options["artifact-dir"], "--artifact-dir"),
  metadataOnly,
});

console.log(JSON.stringify({
  release_tag: plan.release_tag,
  arch: plan.arch,
  metadata_only: metadataOnly,
  images: plan.entries.map((entry) => entry.artifact_name),
}, null, 2));

function parseArgs(args) {
  const parsed = {};
  for (let index = 0; index < args.length; index += 1) {
    const arg = args[index];
    if (!arg.startsWith("--")) usage(`unexpected argument: ${arg}`);
    const key = arg.slice(2);
    if (key === "metadata-only") {
      parsed[key] = true;
      continue;
    }
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
  console.error("Usage: node scripts/verify-profile-image-artifacts.mjs --plan <profile-plan.json> --artifact-dir <dir> [--metadata-only]");
  process.exit(2);
}
