#!/usr/bin/env node
import fs from "fs";
import os from "os";
import path from "path";
import { spawnSync } from "child_process";
import { fileURLToPath } from "url";
import {
  guestKernelConfigDeltas,
  guestKernelConfigURL,
  guestKernelSourceURL,
  guestKernelVersion,
  resolveDebArchitecture,
} from "./lib/architecture.mjs";
import {
  writeGuestKernelMetadata,
} from "./lib/guest-kernel-artifact.mjs";
import {
  curlDownloadArgs,
  curlTextArgs,
} from "./lib/downloads.mjs";

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const options = parseArgs(process.argv.slice(2));
const debArch = options.arch || process.env.DEB_ARCH || hostDebArch();
const target = resolveDebArchitecture(debArch);
const out = path.resolve(options.out || path.join(repoRoot, "dist", "guest-kernel", debArch, "vmlinux"));
const keepWorkDir = options.keepWorkDir || process.env.KILN_VM_KEEP_GUEST_KERNEL_WORKDIR === "1";
const workDir = path.resolve(options.workDir || fs.mkdtempSync(path.join(os.tmpdir(), "kiln-vm-guest-kernel-")));
const jobs = options.jobs || process.env.KILN_VM_GUEST_KERNEL_JOBS || String(Math.max(1, os.cpus().length));

if (target.debArch !== hostDebArch() && !process.env.CROSS_COMPILE) {
  throw new Error(`building the ${target.debArch} guest kernel requires a native ${target.debArch} host or CROSS_COMPILE`);
}

try {
  requireCommands(["curl", "tar", "make", "gcc", "flex", "bison", "bc"]);
  fs.mkdirSync(workDir, { recursive: true });
  const sourceDir = await prepareSource(workDir);
  const buildDir = path.join(workDir, "build");
  fs.mkdirSync(buildDir, { recursive: true });

  const configPath = path.join(buildDir, ".config");
  const baseConfig = downloadText(guestKernelConfigURL(target.debArch));
  fs.writeFileSync(configPath, patchKernelConfig(baseConfig));

  run("make", ["-C", sourceDir, `O=${buildDir}`, `ARCH=${target.linuxKernelArch}`, "olddefconfig"]);
  verifyKernelConfig(path.join(buildDir, ".config"));
  const kernelBuildTarget = target.linuxKernelArch === "arm64" ? "Image" : "vmlinux";
  const kernelBuildOutput = target.linuxKernelArch === "arm64"
    ? path.join(buildDir, "arch", "arm64", "boot", "Image")
    : path.join(buildDir, "vmlinux");
  run("make", ["-C", sourceDir, `O=${buildDir}`, `ARCH=${target.linuxKernelArch}`, `-j${jobs}`, kernelBuildTarget]);

  const builtKernel = kernelBuildOutput;
  if (!fs.existsSync(builtKernel)) throw new Error(`kernel build did not produce ${builtKernel}`);
  fs.mkdirSync(path.dirname(out), { recursive: true });
  fs.copyFileSync(builtKernel, out);
  fs.chmodSync(out, 0o644);
  if (options.metadata) {
    writeGuestKernelMetadata({ debArch: target.debArch, kernelPath: out, metadataPath: path.resolve(options.metadata) });
  }
  console.log(out);
} finally {
  if (!keepWorkDir && !options.workDir) {
    fs.rmSync(workDir, { recursive: true, force: true });
  } else {
    console.error(`guest kernel work dir: ${workDir}`);
  }
}

async function prepareSource(baseDir) {
  if (process.env.KILN_VM_GUEST_KERNEL_SOURCE_DIR) {
    const sourceDir = path.resolve(process.env.KILN_VM_GUEST_KERNEL_SOURCE_DIR);
    if (!fs.existsSync(path.join(sourceDir, "Makefile"))) {
      throw new Error(`KILN_VM_GUEST_KERNEL_SOURCE_DIR is not a Linux source tree: ${sourceDir}`);
    }
    return sourceDir;
  }

  const archive = path.join(baseDir, `linux-${guestKernelVersion}.tar.xz`);
  run("curl", curlDownloadArgs(archive, guestKernelSourceURL()));
  run("tar", ["-xf", archive, "-C", baseDir]);
  const sourceDir = path.join(baseDir, `linux-${guestKernelVersion}`);
  if (!fs.existsSync(path.join(sourceDir, "Makefile"))) throw new Error(`Linux source did not extract to ${sourceDir}`);
  return sourceDir;
}

function patchKernelConfig(config) {
  let patched = config;
  for (const delta of guestKernelConfigDeltas) {
    const [option, value] = delta.split("=");
    patched = setKernelOption(patched, option, value);
  }
  return patched;
}

function setKernelOption(config, option, value) {
  const enabled = new RegExp(`^${escapeRegExp(option)}=.*$`, "m");
  const disabled = new RegExp(`^# ${escapeRegExp(option)} is not set$`, "m");
  if (enabled.test(config)) return config.replace(enabled, `${option}=${value}`);
  if (disabled.test(config)) return config.replace(disabled, `${option}=${value}`);
  return `${config.replace(/\n?$/, "\n")}${option}=${value}\n`;
}

function verifyKernelConfig(configPath) {
  const config = fs.readFileSync(configPath, "utf8");
  for (const option of [
    "CONFIG_NETFILTER_XT_MATCH_ADDRTYPE=y",
    "CONFIG_IP_NF_NAT=y",
    "CONFIG_IP_NF_RAW=y",
    "CONFIG_IP6_NF_RAW=y",
    "CONFIG_BRIDGE=y",
    "CONFIG_VETH=y",
    "CONFIG_OVERLAY_FS=y",
  ]) {
    if (!config.includes(`${option}\n`)) {
      throw new Error(`guest kernel config is missing ${option}`);
    }
  }
}

function requireCommands(commands) {
  for (const command of commands) {
    const result = spawnSync("sh", ["-c", `command -v ${command}`], { stdio: "ignore" });
    if (result.status !== 0) {
      throw new Error(`missing required command ${command}; install kernel build prerequisites first`);
    }
  }
}

function downloadText(url) {
  const result = spawnSync("curl", curlTextArgs(url), { encoding: "utf8" });
  if (result.status !== 0) throw new Error(`curl failed for ${url}: ${result.stderr || result.stdout}`);
  return result.stdout;
}

function run(command, args, options = {}) {
  const result = spawnSync(command, args, { stdio: "inherit", ...options });
  if (result.status !== 0) throw new Error(`Command failed: ${command} ${args.join(" ")}`);
}

function parseArgs(args) {
  const parsed = {};
  for (let i = 0; i < args.length; i++) {
    const arg = args[i];
    if (arg === "--arch") parsed.arch = args[++i];
    else if (arg === "--out") parsed.out = args[++i];
    else if (arg === "--work-dir") parsed.workDir = args[++i];
    else if (arg === "--metadata") parsed.metadata = args[++i];
    else if (arg === "--jobs") parsed.jobs = args[++i];
    else if (arg === "--keep-work-dir") parsed.keepWorkDir = true;
    else throw new Error(`unknown argument ${arg}`);
  }
  return parsed;
}

function hostDebArch() {
  if (process.arch === "x64") return "amd64";
  if (process.arch === "arm64") return "arm64";
  throw new Error(`unsupported host architecture ${process.arch}`);
}

function escapeRegExp(value) {
  return value.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
}
