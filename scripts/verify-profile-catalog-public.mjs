#!/usr/bin/env node
import fs from "node:fs";
import path from "node:path";

const options = parseArgs(process.argv.slice(2));
if (!options.catalog) usage("--catalog is required");

const catalogPath = path.resolve(options.catalog);
const catalog = JSON.parse(fs.readFileSync(catalogPath, "utf8"));
if (catalog.schema_version !== 1) {
  throw new Error(`unsupported catalog schema_version ${catalog.schema_version}`);
}
if (!Array.isArray(catalog.images) || catalog.images.length === 0) {
  throw new Error("profile catalog has no images");
}

const checked = [];
for (const entry of catalog.images) {
  for (const key of ["profile", "reference", "layer_digest", "layer_media_type"]) {
    if (!entry[key]) throw new Error(`catalog entry is missing ${key}: ${JSON.stringify(entry)}`);
  }
  if (entry.reference.startsWith("https://") || entry.reference.startsWith("http://")) {
    await verifyPublicHTTPArtifact(entry.reference, entry.profile);
  } else {
    const ref = parseOCIReference(entry.reference);
    const manifest = await fetchOCIManifest(ref);
    const layer = findLayer(manifest, entry.layer_media_type, normalizeDigest(entry.layer_digest));
    if (!layer) {
      throw new Error(`${entry.profile}: public OCI manifest does not contain ${entry.layer_media_type} ${entry.layer_digest}`);
    }
  }
  checked.push(`${entry.profile}:${entry.rootfs_size_mb}mb`);
}

console.log(`public profile catalog ok: ${checked.join(", ")}`);

function parseOCIReference(raw) {
  raw = String(raw ?? "").trim();
  let ref;
  const at = raw.lastIndexOf("@");
  if (at >= 0) {
    ref = raw.slice(at + 1);
    raw = raw.slice(0, at);
    const slash = raw.lastIndexOf("/");
    const colon = raw.lastIndexOf(":");
    if (colon > slash) raw = raw.slice(0, colon);
  } else {
    const slash = raw.lastIndexOf("/");
    const colon = raw.lastIndexOf(":");
    if (colon <= slash) throw new Error(`OCI reference must include tag or digest: ${raw}`);
    ref = raw.slice(colon + 1);
    raw = raw.slice(0, colon);
  }
  const firstSlash = raw.indexOf("/");
  if (firstSlash <= 0 || firstSlash === raw.length - 1) {
    throw new Error(`OCI reference must include registry and repository: ${raw}`);
  }
  return {
    registry: raw.slice(0, firstSlash),
    repo: raw.slice(firstSlash + 1),
    ref,
  };
}

async function fetchOCIManifest(ref) {
  const manifestURL = new URL(`https://${ref.registry}/v2/${ref.repo}/manifests/${ref.ref}`);
  const headers = {
    Accept: [
      "application/vnd.oci.artifact.manifest.v1+json",
      "application/vnd.oci.image.manifest.v1+json",
      "application/vnd.docker.distribution.manifest.v2+json",
    ].join(", "),
  };
  let response = await fetch(manifestURL, { headers });
  if (response.status === 401) {
    const token = await fetchAnonymousBearerToken(response.headers.get("www-authenticate"), ref);
    response = await fetch(manifestURL, {
      headers: {
        ...headers,
        Authorization: `Bearer ${token}`,
      },
    });
  }
  if (!response.ok) {
    throw new Error(`fetch public OCI manifest ${manifestURL}: HTTP ${response.status} ${JSON.stringify((await response.text()).trim())}`);
  }
  const manifest = await response.json();
  if (manifest.schemaVersion !== 2) {
    throw new Error(`unsupported OCI manifest schemaVersion ${manifest.schemaVersion}`);
  }
  return manifest;
}

async function verifyPublicHTTPArtifact(reference, profile) {
  const artifactURL = new URL(reference);
  let response = await fetch(artifactURL, { method: "HEAD" });
  if (response.status === 405 || response.status === 403) {
    response = await fetch(artifactURL, { headers: { Range: "bytes=0-0" } });
  }
  if (!response.ok && response.status !== 206) {
    throw new Error(`${profile}: fetch public HTTP artifact ${artifactURL}: HTTP ${response.status} ${JSON.stringify((await response.text()).trim())}`);
  }
}

async function fetchAnonymousBearerToken(challenge, ref) {
  const { realm, params } = parseBearerChallenge(challenge);
  if (!realm) {
    throw new Error("OCI registry requires authentication but did not provide a bearer realm");
  }
  const tokenURL = new URL(realm);
  if (params.service) tokenURL.searchParams.set("service", params.service);
  tokenURL.searchParams.set("scope", params.scope || `repository:${ref.repo}:pull`);
  const response = await fetch(tokenURL);
  if (!response.ok) {
    throw new Error(`fetch anonymous OCI bearer token ${tokenURL}: HTTP ${response.status} ${JSON.stringify((await response.text()).trim())}`);
  }
  const body = await response.json();
  if (body.token) return body.token;
  if (body.access_token) return body.access_token;
  throw new Error("OCI bearer token response did not include a token");
}

function parseBearerChallenge(challenge) {
  const params = {};
  if (!challenge || !challenge.toLowerCase().startsWith("bearer ")) {
    return { realm: "", params };
  }
  for (const match of challenge.matchAll(/([a-zA-Z_]+)="([^"]*)"/g)) {
    params[match[1].toLowerCase()] = match[2];
  }
  return { realm: params.realm || "", params };
}

function findLayer(manifest, mediaType, digest) {
  const descriptors = [...(manifest.layers || []), ...(manifest.blobs || [])];
  return descriptors.find((layer) =>
    layer.digest === digest &&
    layer.mediaType === mediaType
  );
}

function normalizeDigest(value) {
  value = String(value ?? "").trim();
  if (value === "" || value.startsWith("sha256:")) return value;
  return `sha256:${value}`;
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
  console.error("Usage: node scripts/verify-profile-catalog-public.mjs --catalog <profile-images.json>");
  process.exit(2);
}
