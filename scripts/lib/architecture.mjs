export const supportedDebArchitectures = ["amd64", "arm64"];
export const guestKernelVersion = "6.1.155";
export const guestKernelConfigDeltas = [
  "CONFIG_IP_NF_RAW=y",
  "CONFIG_IP6_NF_RAW=y",
];

export const standardProfileImages = [
  { profile: "alpine", rootfsSizeMB: 128 },
  { profile: "ubuntu", rootfsSizeMB: 2048 },
  { profile: "debian", rootfsSizeMB: 2048 },
  { profile: "ubuntu-docker", rootfsSizeMB: 4096 },
];

const architectures = new Map([
  ["amd64", {
    debArch: "amd64",
    goArch: "amd64",
    catalogArch: "amd64",
    firecrackerArch: "x86_64",
    kernelArch: "x86_64",
    linuxKernelArch: "x86",
    dockerPlatform: "linux/amd64",
    qemuSystemPackage: "qemu-system-x86",
  }],
  ["arm64", {
    debArch: "arm64",
    goArch: "arm64",
    catalogArch: "arm64",
    firecrackerArch: "aarch64",
    kernelArch: "aarch64",
    linuxKernelArch: "arm64",
    dockerPlatform: "linux/arm64",
    qemuSystemPackage: "qemu-system-arm",
  }],
]);

export function resolveDebArchitecture(value) {
  const normalized = String(value || "amd64").trim();
  const architecture = architectures.get(normalized);
  if (!architecture) {
    throw new Error(`unsupported Debian architecture ${JSON.stringify(normalized)}; supported: ${supportedDebArchitectures.join(", ")}`);
  }
  return architecture;
}

export function firecrackerReleaseURL(version, debArch) {
  const arch = resolveDebArchitecture(debArch);
  return `https://github.com/firecracker-microvm/firecracker/releases/download/${version}/firecracker-${version}-${arch.firecrackerArch}.tgz`;
}

export function firecrackerReleaseBinaryPath(version, debArch) {
  const arch = resolveDebArchitecture(debArch);
  return {
    releaseName: `firecracker-${version}-${arch.firecrackerArch}.tgz`,
    releaseDir: `release-${version}-${arch.firecrackerArch}`,
    binaryName: `firecracker-${version}-${arch.firecrackerArch}`,
  };
}

export function guestKernelSourceURL() {
  return `https://cdn.kernel.org/pub/linux/kernel/v6.x/linux-${guestKernelVersion}.tar.xz`;
}

export function guestKernelConfigURL(debArch) {
  const arch = resolveDebArchitecture(debArch);
  return `https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/v1.15/${arch.kernelArch}/vmlinux-${guestKernelVersion}.config`;
}
