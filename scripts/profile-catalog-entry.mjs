#!/usr/bin/env node
import fs from "node:fs";
import path from "node:path";

const options = parseArgs(process.argv.slice(2));
if (!options.metadata || !options.reference || !options.out) {
  usage("--metadata <file> --reference <oci-ref@digest> --out <catalog.json> are required");
}

const metadata = JSON.parse(fs.readFileSync(options.metadata, "utf8"));
const outPath = path.resolve(options.out);
const catalog = fs.existsSync(outPath)
  ? JSON.parse(fs.readFileSync(outPath, "utf8"))
  : { schema_version: 1, images: [] };
if (catalog.schema_version !== 1) {
  throw new Error(`unsupported catalog schema_version ${catalog.schema_version}`);
}
const entry = {
  profile: metadata.profile,
  profile_version: metadata.profile_version,
  fingerprint: metadata.fingerprint,
  arch: metadata.arch,
  rootfs_size_mb: metadata.rootfs_size_mb,
  reference: options.reference,
  layer_digest: metadata.compressed_sha256,
  rootfs_sha256: metadata.rootfs_sha256,
  layer_media_type: metadata.layer_media_type,
  source: options.source || (options.reference.startsWith("http://") || options.reference.startsWith("https://") ? "https" : "oci"),
  created_at: metadata.created_at,
};

catalog.images = [
  ...catalog.images.filter((candidate) =>
    candidate.profile !== entry.profile ||
    candidate.profile_version !== entry.profile_version ||
    candidate.fingerprint !== entry.fingerprint ||
    candidate.arch !== entry.arch ||
    candidate.rootfs_size_mb !== entry.rootfs_size_mb
  ),
  entry,
].sort((left, right) =>
  left.profile.localeCompare(right.profile) ||
  left.arch.localeCompare(right.arch) ||
  left.rootfs_size_mb - right.rootfs_size_mb
);
fs.mkdirSync(path.dirname(outPath), { recursive: true });
fs.writeFileSync(outPath, JSON.stringify(catalog, null, 2) + "\n");

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
  console.error("Usage: node scripts/profile-catalog-entry.mjs --metadata <file> --reference <ref-or-url> --out <catalog.json> [--source https|oci]");
  process.exit(2);
}
