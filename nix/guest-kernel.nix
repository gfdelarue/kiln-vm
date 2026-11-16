{ fetchurl, lib, linuxKernel, runCommand, stdenv }:

let
  supportedHost = stdenv.hostPlatform.system == "x86_64-linux";

  baseConfig = fetchurl {
    url = "https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/v1.15/x86_64/vmlinux-6.1.155.config";
    sha256 = "1hzpprkjh7g0v9d11pb5faiw10n835vg505bm7wk2wgycap2ljq2";
  };

  configFile = runCommand "kiln-vm-guest-kernel.config" { } ''
    cp ${baseConfig} "$out"
    substituteInPlace "$out" \
      --replace-fail '# CONFIG_IP_NF_RAW is not set' 'CONFIG_IP_NF_RAW=y' \
      --replace-fail '# CONFIG_IP6_NF_RAW is not set' 'CONFIG_IP6_NF_RAW=y'
  '';

  kernel = (linuxKernel.manualConfig {
    inherit (linuxKernel.kernels.linux_6_1) src version;
    modDirVersion = lib.versions.pad 3 linuxKernel.kernels.linux_6_1.version;
    configfile = configFile;
    allowImportFromDerivation = true;
  }).overrideAttrs (old: {
    postInstall = (old.postInstall or "") + ''
      cp vmlinux $out/vmlinux
    '';
  });
in
assert lib.assertMsg supportedHost "kiln-vm Nix guest kernel currently supports x86_64-linux only; use the official apt/deb package for ARM64 hosts.";
runCommand "kiln-vm-guest-vmlinux-${kernel.version}" {
  passthru = {
    inherit configFile kernel;
  };
} ''
  cp ${kernel}/vmlinux $out
''
