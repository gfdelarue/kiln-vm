{ config, lib, pkgs, modulesPath, ... }:

{
  imports = [
    # NixOS ISO base profile – provides the live CD / installer infrastructure.
    # `modulesPath` is set automatically by nixpkgs.lib.nixosSystem and points
    # to <nixpkgs>/nixos/modules, so this works cleanly inside a flake.
    "${modulesPath}/installer/cd-dvd/iso-image.nix"

    # kiln-vm modules
    ./modules/base.nix
    ./modules/kiln-vmd.nix
    ./modules/firecracker.nix
    ./modules/networking.nix
    ./modules/tls.nix
  ];

  # ── ISO image settings ──────────────────────────────────────────────
  image.baseName = lib.mkForce "kiln-vm-${config.system.nixos.release}";

  isoImage = {
    volumeID = "KILN";
    makeEfiBootable = true;
    makeUsbBootable = true;
  };

  # Boot loader (ISO needs a bootloader config even though it's live)
  boot.loader.grub.memtest86.enable = false;

  # ── Enable kiln-vm services ───────────────────────────────────────────
  services.kiln-vmd = {
    enable = true;
    firecrackerBin = "${pkgs.firecracker}/bin/firecracker";
  };

  kiln-vm.firecracker.enable = true;

  # ── Extra system packages on the ISO ────────────────────────────────
  environment.systemPackages = with pkgs; [
    config.services.kiln-vmd.package
    firecracker
    vim
    tmux
    git
  ];
}
