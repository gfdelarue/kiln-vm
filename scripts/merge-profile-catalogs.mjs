#!/usr/bin/env node
import fs from "node:fs";
import path from "node:path";

const options = parseArgs(process.argv.slice(2));
const catalogs = asArray(options.catalog);
if (catalogs.length === 0 || !options.out) {
  usage("--catalog <profile-images.json>... --out <merged-profile-images.json> are required");
}

const merged = { schema_version: 1, images: [] };
const seen = new Set();
for (const catalogPath of catalogs) {
  const catalog = JSON.parse(fs.readFileSync(path.resolve(catalogPath), "utf8"));
  if (catalog.schema_version !== 1) {
    throw new Error(`${catalogPath}: unsupported schema_version ${catalog.schema_version}`);
  }
  if (!Array.isArray(catalog.images)) {
    throw new Error(`${catalogPath}: images must be an array`);
  }
  for (const entry of catalog.images) {
    const key = [
      entry.profile,
      entry.profile_version,
      entry.fingerprint,
      entry.arch,
      entry.rootfs_size_mb,
    ].join("\0");
    if (seen.has(key)) continue;
    seen.add(key);
    merged.images.push(entry);
  }
}

merged.images.sort((left, right) =>
  String(left.profile).localeCompare(String(right.profile)) ||
  String(left.arch).localeCompare(String(right.arch)) ||
  Number(left.rootfs_size_mb) - Number(right.rootfs_size_mb)
);

const outPath = path.resolve(options.out);
fs.mkdirSync(path.dirname(outPath), { recursive: true });
fs.writeFileSync(outPath, JSON.stringify(merged, null, 2) + "\n");
console.log(`merged profile catalog: ${merged.images.length} images`);

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
  console.error("Usage: node scripts/merge-profile-catalogs.mjs --catalog <profile-images.json>... --out <merged-profile-images.json>");
  process.exit(2);
}
