#!/usr/bin/env node
import assert from "node:assert/strict";
import crypto from "node:crypto";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";
import {
  writeGuestKernelMetadata,
} from "./lib/guest-kernel-artifact.mjs";

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const workDir = fs.mkdtempSync(path.join(os.tmpdir(), "kiln-vm-deb-guest-kernel-contract-"));
const debPath = path.join(repoRoot, "dist", "deb", "kiln_vm_0.0.0_amd64.deb");

try {
  const kernel = writeExecutableFixture("vmlinux", "synthetic verified guest kernel\n", 0o644);
  const firecracker = writeExecutableFixture("firecracker", "#!/bin/sh\nexit 0\n");
  const agent = writeExecutableFixture("kiln-vm-agent", "#!/bin/sh\nexit 0\n");
  const metadata = path.join(workDir, "guest-kernel.json");
  writeGuestKernelMetadata({ debArch: "amd64", kernelPath: kernel, metadataPath: metadata });

  fs.rmSync(debPath, { force: true });
  run(process.execPath, ["scripts/build-deb.mjs"], {
    cwd: repoRoot,
    env: {
      ...process.env,
      KILN_VM_DEB_VERSION: "0.0.0",
      DEB_ARCH: "amd64",
      FIRECRACKER_BIN: firecracker,
      KILN_VM_AGENT_BIN: agent,
      KILN_VM_KERNEL_PATH: kernel,
      KILN_VM_GUEST_KERNEL_METADATA_SOURCE: metadata,
    },
  });

  const root = path.join(workDir, "root");
  run("dpkg-deb", ["-x", debPath, root]);
  assert.equal(fs.readFileSync(path.join(root, "usr", "lib", "kiln-vm", "vmlinux"), "utf8"), "synthetic verified guest kernel\n");
  assert.ok(fs.existsSync(path.join(root, "usr", "share", "kiln-vm", "kiln-vmd.env.defaults")));
  assert.ok(!fs.existsSync(path.join(root, "etc", "kiln-vm", "kiln-vmd.env")));

  const embedded = JSON.parse(fs.readFileSync(path.join(root, "usr", "share", "kiln-vm", "guest-kernel.json"), "utf8"));
  assert.equal(embedded.artifact_kind, "kiln_vm_guest_kernel");
  assert.equal(embedded.deb_arch, "amd64");
  assert.equal(embedded.kiln_vm_version, "0.0.0");
  assert.equal(embedded.packaged_path, "/usr/lib/kiln-vm/vmlinux");
  assert.equal(embedded.sha256, crypto.createHash("sha256").update("synthetic verified guest kernel\n").digest("hex"));
  assert.match(embedded.release_tag, /^guest-kernel-v6\.1\.155-/);

  console.log("debian guest kernel contract ok");
} finally {
  fs.rmSync(workDir, { recursive: true, force: true });
  fs.rmSync(debPath, { force: true });
}

function writeExecutableFixture(name, content, mode = 0o755) {
  const file = path.join(workDir, name);
  fs.writeFileSync(file, content, { mode });
  return file;
}

function run(command, args, options = {}) {
  const result = spawnSync(command, args, { stdio: "inherit", ...options });
  if (result.status !== 0) throw new Error(`Command failed: ${command} ${args.join(" ")}`);
}
