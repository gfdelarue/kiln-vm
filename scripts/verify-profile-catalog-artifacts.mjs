#!/usr/bin/env node
import crypto from "node:crypto";
import fs from "node:fs";
import path from "node:path";

const options = parseArgs(process.argv.slice(2));
for (const key of ["catalog", "artifact-dir", "base-url"]) {
  if (!options[key]) usage(`--${key} is required`);
}

const catalog = JSON.parse(fs.readFileSync(path.resolve(options.catalog), "utf8"));
if (catalog.schema_version !== 1) {
  throw new Error(`unsupported catalog schema_version ${catalog.schema_version}`);
}
if (!Array.isArray(catalog.images) || catalog.images.length === 0) {
  throw new Error("profile catalog has no images");
}

const artifactDir = path.resolve(options["artifact-dir"]);
const baseURL = trimTrailingSlash(options["base-url"]);
const checked = [];
for (const entry of catalog.images) {
  if (!entry.reference?.startsWith(`${baseURL}/`)) {
    throw new Error(`${entry.profile}: reference ${entry.reference} does not start with ${baseURL}/`);
  }
  if (entry.source !== "https") {
    throw new Error(`${entry.profile}: source ${entry.source} is not https`);
  }
  const fileName = path.basename(new URL(entry.reference).pathname);
  const artifactPath = path.join(artifactDir, fileName);
  if (!fs.existsSync(artifactPath)) {
    throw new Error(`${entry.profile}: missing artifact ${artifactPath}`);
  }
  const compressedSHA256 = `sha256:${await hashFile(artifactPath, "sha256")}`;
  if (compressedSHA256 !== normalizeDigest(entry.layer_digest)) {
    throw new Error(`${entry.profile}: artifact digest ${compressedSHA256} does not match catalog ${entry.layer_digest}`);
  }
  checked.push(`${entry.profile}:${entry.rootfs_size_mb}mb`);
}

console.log(`profile catalog artifacts ok: ${checked.join(", ")}`);

function hashFile(filePath, algorithm) {
  return new Promise((resolve, reject) => {
    const hash = crypto.createHash(algorithm);
    const stream = fs.createReadStream(filePath);
    stream.on("data", (chunk) => hash.update(chunk));
    stream.on("error", reject);
    stream.on("end", () => resolve(hash.digest("hex")));
  });
}

function normalizeDigest(value) {
  value = String(value ?? "").trim();
  if (value === "" || value.startsWith("sha256:")) return value;
  return `sha256:${value}`;
}

function trimTrailingSlash(value) {
  return String(value).replace(/\/+$/, "");
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
    parsed[key] = value;
  }
  return parsed;
}

function usage(message) {
  if (message) console.error(message);
  console.error("Usage: node scripts/verify-profile-catalog-artifacts.mjs --catalog <profile-images.json> --artifact-dir <dir> --base-url <public-base-url>");
  process.exit(2);
}
