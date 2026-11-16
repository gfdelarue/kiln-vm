#!/usr/bin/env node
import fs from "fs";
import path from "path";
import { fileURLToPath } from "url";

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const tagArg = process.argv.find((arg) => arg.startsWith("--tag="));
const tag = tagArg ? tagArg.slice("--tag=".length) : process.env.GITHUB_REF_NAME || "";
const expectedVersion = tag.startsWith("v") ? tag.slice(1) : tag;

if (!expectedVersion) {
  throw new Error("release tag is required");
}

const goVersionSource = fs.readFileSync(path.join(repoRoot, "internal", "version", "version.go"), "utf8");
const goVersion = matchOne(goVersionSource, /const\s+Version\s*=\s*"([^"]+)"/, "internal/version/version.go");
if (goVersion !== expectedVersion) {
  throw new Error(`internal/version/version.go is ${goVersion}, expected ${expectedVersion} from tag ${tag}`);
}

const flakeSource = fs.readFileSync(path.join(repoRoot, "flake.nix"), "utf8");
const flakeVersions = [...flakeSource.matchAll(/version\s*=\s*"([^"]+)";/g)].map((match) => match[1]);
if (flakeVersions.length === 0) {
  throw new Error("flake.nix has no package version declarations");
}
const mismatches = flakeVersions.filter((version) => version !== expectedVersion);
if (mismatches.length > 0) {
  throw new Error(`flake.nix package versions are [${flakeVersions.join(", ")}], expected all ${expectedVersion}`);
}

console.log(`release version verified: ${expectedVersion}`);

function matchOne(source, pattern, label) {
  const match = source.match(pattern);
  if (!match) throw new Error(`unable to read version from ${label}`);
  return match[1];
}
