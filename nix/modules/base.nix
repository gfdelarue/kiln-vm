{ config, lib, pkgs, ... }:

{
  # ── Timezone ────────────────────────────────────────────────────────
  time.timeZone = "UTC";

  # ── Locale ──────────────────────────────────────────────────────────
  i18n.defaultLocale = "en_US.UTF-8";

  # ── SSH ─────────────────────────────────────────────────────────────
  services.openssh = {
    enable = true;
    settings = {
      PermitRootLogin = "prohibit-password";   # key-only for root
      PasswordAuthentication = false;
    };
  };

  # ── Root user ───────────────────────────────────────────────────────
  users.users.root = {
    # No password set – login is SSH-key-only.
    hashedPassword = "!";                       # locked password
    openssh.authorizedKeys.keys = [
      # Add authorised public keys here or via an overlay / host config.
    ];
  };

  # ── Filesystem basics ──────────────────────────────────────────────
  # Ensure /tmp is cleaned on boot.
  boot.tmp.cleanOnBoot = true;

  # Minimal set of system packages expected on every kiln-vm host.
  environment.systemPackages = with pkgs; [
    coreutils
    curl
    htop
    iptables
    jq
    util-linux
  ];

  # ── Nix settings ───────────────────────────────────────────────────
  nix.settings.experimental-features = [ "nix-command" "flakes" ];

  # ── System state version ────────────────────────────────────────────
  system.stateVersion = "24.11";
}
