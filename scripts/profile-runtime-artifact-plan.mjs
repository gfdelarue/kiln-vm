#!/usr/bin/env node
import fs from "fs";
import path from "path";
import {
  profileRuntimeArtifactPlan,
} from "./lib/profile-runtime-artifact.mjs";

const options = parseArgs(process.argv.slice(2));
const plan = profileRuntimeArtifactPlan(options.arch || process.env.DEB_ARCH || "amd64");

if (options.out) {
  const out = path.resolve(options.out);
  fs.mkdirSync(path.dirname(out), { recursive: true });
  fs.writeFileSync(out, `${JSON.stringify(plan, null, 2)}\n`);
}

if (options["github-output"]) {
  const output = path.resolve(options["github-output"]);
  fs.appendFileSync(output, [
    `release_tag=${plan.release_tag}`,
    `metadata_name=${plan.metadata_name}`,
    `spec_fingerprint=${plan.spec_fingerprint}`,
    ...plan.binaries.map((binary) => `artifact_${binary.name.replaceAll("-", "_")}=${binary.artifact_name}`),
    ...plan.notices.map((notice) => `artifact_notice_${notice.name.replaceAll("-", "_")}=${notice.artifact_name}`),
    "",
  ].join("\n"));
}

console.log(JSON.stringify(plan, null, 2));

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

function usage(message) {
  if (message) console.error(message);
  console.error("Usage: node scripts/profile-runtime-artifact-plan.mjs --arch <amd64|arm64> [--out <json>] [--github-output <path>]");
  process.exit(2);
}
