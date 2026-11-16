{ config, lib, pkgs, ... }:

let
  cfg = config.kiln-vm.firecracker;

  guestKernel = pkgs.callPackage ../guest-kernel.nix { };
in
{
  options.kiln-vm.firecracker = {
    enable = lib.mkEnableOption "Firecracker VMM and guest kernel";
  };

  config = lib.mkIf cfg.enable {
    # Make the firecracker binary available system-wide.
    environment.systemPackages = [ pkgs.firecracker ];

    # Point kiln-vmd at the immutable kernel path so NixOS rebuilds switch
    # guest kernels atomically with the service configuration.
    services.kiln-vmd.kernelPath = lib.mkDefault "${guestKernel}";

    # Keep the directory present for operators who override
    # services.kiln-vmd.kernelPath to a mutable local kernel.
    systemd.tmpfiles.rules = [
      "d /var/lib/kiln-vm/kernel 0755 root root -"
    ];
  };
}
