{ config, lib, pkgs, ... }:

{
  # ── Firewall ────────────────────────────────────────────────────────
  networking.firewall = {
    enable = true;
  };

  # ── iptables ────────────────────────────────────────────────────────
  # Ensure iptables is available for kiln-vmd to set up NAT / forwarding
  # rules for guest VMs.
  environment.systemPackages = with pkgs; [
    iptables
    iproute2
  ];
}
