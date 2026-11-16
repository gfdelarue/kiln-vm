{ config, lib, pkgs, ... }:

let
  cfg = config.services.kiln-vmd;
  defaultProfileImageCatalog = "/usr/share/kiln-vm/profile-images.json";
  trim = lib.strings.trim;
  listenAddressParts = address:
    builtins.match "([[][^]]+[]]|[^:]*):([0-9]+)" (trim address);
  ipv4Parts = host:
    builtins.match "([0-9]+)[.]([0-9]+)[.]([0-9]+)[.]([0-9]+)" host;
  isValidIPv4Octet = octet:
    builtins.stringLength octet <= 3
    && (octet == "0" || !lib.hasPrefix "0" octet)
    && lib.strings.toIntBase10 octet <= 255;
  isValidIPv4Address = host:
    let parts = ipv4Parts host;
    in parts != null && lib.all isValidIPv4Octet parts;
  looksLikeIPv4Address = host:
    builtins.match "[0-9.]+" host != null
    && lib.count (character: character == ".") (lib.stringToCharacters host) == 3;
  isValidDNSHostname = host:
    let
      hostname = lib.toLower (lib.removeSuffix "." host);
      labels = lib.splitString "." hostname;
      isValidLabel = label:
        builtins.stringLength label >= 1
        && builtins.stringLength label <= 63
        && builtins.match "[a-z0-9]([a-z0-9-]*[a-z0-9])?" label != null;
    in
    builtins.stringLength hostname >= 1
    && builtins.stringLength hostname <= 253
    && lib.all isValidLabel labels;
  ipv6WithMappedIPv4Normalized = address:
    let
      pieces = lib.splitString ":" address;
      tail = if pieces == [ ] then "" else lib.last pieces;
      tailParts = ipv4Parts tail;
      tailIsIPv4 = tailParts != null && lib.all isValidIPv4Octet tailParts;
      octets = if tailIsIPv4 then map lib.strings.toIntBase10 tailParts else [ ];
      high = if tailIsIPv4 then
        (builtins.elemAt octets 0) * 256 + builtins.elemAt octets 1
      else
        0;
      low = if tailIsIPv4 then
        (builtins.elemAt octets 2) * 256 + builtins.elemAt octets 3
      else
        0;
    in
    if builtins.length pieces >= 2 && tailIsIPv4 then
      lib.concatStringsSep ":" (
        lib.init pieces ++ [ (lib.toHexString high) (lib.toHexString low) ]
      )
    else
      null;
  parseOrdinaryIPv6Address = address:
    let
      attempt =
        if lib.hasInfix "/" address then
          { success = false; value = null; }
        else
          builtins.tryEval (
            let parsed = lib.network.ipv6.fromString address;
            in builtins.deepSeq parsed parsed
          );
    in
    if attempt.success then attempt.value.address else null;
  parseIPv6Address = address:
    let
      zoneParts = lib.splitString "%" address;
      addressWithoutZone = if zoneParts == [ ] then "" else builtins.head zoneParts;
      zoneIsValid =
        builtins.length zoneParts == 1
        || (builtins.length zoneParts == 2
          && builtins.match "[A-Za-z0-9_.-]+" (builtins.elemAt zoneParts 1) != null);
      normalizedMappedAddress = ipv6WithMappedIPv4Normalized addressWithoutZone;
      addressForParser =
        if normalizedMappedAddress == null then addressWithoutZone else normalizedMappedAddress;
    in
    if zoneIsValid then parseOrdinaryIPv6Address addressForParser else null;
  isValidIPv6Address = address:
    parseIPv6Address address != null;
  isValidBracketedIPLiteral = host:
    let
      length = builtins.stringLength host;
      inner = if length >= 2 then builtins.substring 1 (length - 2) host else "";
    in
    lib.hasPrefix "[" host
    && lib.hasSuffix "]" host
    && (
      isValidIPv4Address inner
      || (lib.hasInfix ":" inner && isValidIPv6Address inner)
    );
  isValidListenHost = host:
    host == ""
    || isValidIPv4Address host
    || isValidBracketedIPLiteral host
    || (!looksLikeIPv4Address host && isValidDNSHostname host);
  isIPv4LoopbackHost = host:
    let parts = ipv4Parts host;
    in
    parts != null
    && lib.all isValidIPv4Octet parts
    && builtins.elemAt parts 0 == "127";
  isIPv6LoopbackHost = host:
    let
      length = builtins.stringLength host;
      inner = if length >= 2 then builtins.substring 1 (length - 2) host else "";
      parsed =
        if lib.hasPrefix "[" host && lib.hasSuffix "]" host then
          parseIPv6Address inner
        else
          null;
      pieces = if parsed == null then [ ] else lib.splitString ":" parsed;
      isNativeLoopback = parsed == "0:0:0:0:0:0:0:1";
      mappedHigh =
        if builtins.length pieces == 8 then
          lib.fromHexString (builtins.elemAt pieces 6)
        else
          -1;
      isMappedLoopback =
        builtins.length pieces == 8
        && lib.all (piece: piece == "0") (lib.take 5 pieces)
        && builtins.elemAt pieces 5 == "ffff"
        && mappedHigh >= 32512
        && mappedHigh <= 32767;
    in
    isNativeLoopback || isMappedLoopback;
  isSyntacticListenAddress = address:
    let
      parts = listenAddressParts address;
      host = if parts == null then "" else builtins.elemAt parts 0;
      port = if parts == null then -1 else lib.strings.toIntBase10 (builtins.elemAt parts 1);
    in
    address == trim address
    && parts != null
    && isValidListenHost host
    && port >= 1
    && port <= 65535;
  isLoopbackListenAddress = address:
    let
      parts = listenAddressParts address;
      host = if parts == null then "" else lib.toLower (builtins.elemAt parts 0);
      hostWithoutTrailingDot = lib.removeSuffix "." host;
    in
    hostWithoutTrailingDot == "localhost"
    || isIPv4LoopbackHost host
    || isIPv6LoopbackHost host;
  isValidTLSDomain = value:
    let
      domain = lib.toLower (lib.removeSuffix "." value);
      labels = lib.splitString "." domain;
      isValidLabel = label:
        builtins.stringLength label >= 1
        && builtins.stringLength label <= 63
        && builtins.match "[a-z0-9]([a-z0-9-]*[a-z0-9])?" label != null;
    in
    value == trim value
    && builtins.stringLength domain <= 253
    && builtins.length labels >= 2
    && builtins.match "[0-9]+([.][0-9]+){3}" domain == null
    && lib.all isValidLabel labels;
in
{
  options.services.kiln-vmd = {
    enable = lib.mkEnableOption "kiln-vmd VM manager daemon";

    package = lib.mkOption {
      type = lib.types.package;
      description = "The kiln-vmd package to use.";
    };

    listenAddr = lib.mkOption {
      type = lib.types.str;
      default = "127.0.0.1:5456";
      description = ''
        Plaintext management address (host:port). This is used only when
        services.kiln-vmd.tls.enable is false. Non-loopback addresses require
        services.kiln-vmd.allowInsecureRemoteHTTP.
      '';
    };

    allowInsecureRemoteHTTP = lib.mkOption {
      type = lib.types.bool;
      default = false;
      description = ''
        Explicitly allow the bearer-authenticated management API to bind a
        non-loopback plaintext HTTP address. Prefer TLS for remote access.
      '';
    };

    tls = {
      enable = lib.mkEnableOption "TLS for the kiln-vmd management API";

      domain = lib.mkOption {
        type = lib.types.str;
        default = "";
        description = "DNS domain served by the TLS management listener.";
      };

      certDir = lib.mkOption {
        type = lib.types.str;
        default = "${cfg.dataDir}/certs";
        description = "Runtime directory used for the ACME certificate cache.";
      };

      listenAddr = lib.mkOption {
        type = lib.types.str;
        default = ":443";
        description = "Address (host:port) for the TLS management listener.";
      };

      acmeListenAddr = lib.mkOption {
        type = lib.types.str;
        default = ":80";
        description = ''
          Address (host:port) for ACME HTTP-01 challenges and HTTPS redirects.
          It never serves the plaintext management API.
        '';
      };

      email = lib.mkOption {
        type = lib.types.str;
        default = "";
        description = "Optional account email supplied to the ACME provider.";
      };
    };

    dataDir = lib.mkOption {
      type = lib.types.str;
      default = "/var/lib/kiln-vm";
      description = "Directory where kiln-vmd stores its state (DB, rootfs images, etc.).";
    };

    kernelPath = lib.mkOption {
      type = lib.types.str;
      default = "/var/lib/kiln-vm/kernel/vmlinux";
      description = "Path to the guest vmlinux kernel image.";
    };

    firecrackerBin = lib.mkOption {
      type = lib.types.str;
      default = "${pkgs.firecracker}/bin/firecracker";
      description = "Path to the firecracker binary.";
    };

    agentBinPath = lib.mkOption {
      type = lib.types.str;
      default = "${cfg.package}/bin/kiln-vm-agent";
      description = "Path to the kiln-vm-agent binary injected into guest rootfs images.";
    };

    bridgeCIDR = lib.mkOption {
      type = lib.types.str;
      default = "10.231.0.0/16";
      description = "CIDR range used for the VM bridge network.";
    };

    guestDNSProbeName = lib.mkOption {
      type = lib.types.str;
      default = "example.com.";
      description = ''
        Stable external A-record name used by kiln-vm's provider-owned gateway
        DNS readiness probe. The queried name is excluded from public consumer
        metrics and operation diagnostics, but remains in kiln-vm-private probe data.
      '';
    };

    maxRunningVMs = lib.mkOption {
      type = lib.types.int;
      default = 8;
      description = "Maximum number of VM processes kiln-vm may run at once.";
    };

    maxTotalVCPUs = lib.mkOption {
      type = lib.types.int;
      default = 8;
      description = "Maximum total vCPUs across running/creating/stopping VMs.";
    };

    maxTotalMemoryMB = lib.mkOption {
      type = lib.types.int;
      default = 8192;
      description = "Maximum total memory in MiB across running/creating/stopping VMs.";
    };

    maxVMDiskMB = lib.mkOption {
      type = lib.types.ints.between 128 (1024 * 1024);
      default = 512 * 1024;
      description = "Maximum requested VM root disk size in MiB.";
    };

    imageBuildTimeout = lib.mkOption {
      type = lib.types.str;
      default = "30m";
      description = "Maximum duration for a single image build.";
    };

    profileImageMode = lib.mkOption {
      type = lib.types.enum [ "auto" "pull" "local" ];
      default = "local";
      description = ''
        VM profile image source policy.  "auto" uses trusted profile
        images when the catalog has a matching entry and falls back to
        local builds only when no trusted entry exists.  "pull" requires
        a trusted catalog entry.  "local" always builds locally.
      '';
    };

    profileImageCatalog = lib.mkOption {
      type = lib.types.str;
      default = defaultProfileImageCatalog;
      description = ''
        Local path or HTTP(S) URL for the trusted VM profile image
        catalog. Configure this with profileImageTokenFile for private
        artifact hosts.
      '';
    };

    profileImageCacheMaxMB = lib.mkOption {
      type = lib.types.ints.unsigned;
      default = 8192;
      description = ''
        Maximum retained warm VM profile-image cache in MiB. Unused profile
        variants above this budget are reported as reclaimable by kiln-vm cleanup;
        profile images still used by machines remain protected.
      '';
    };

    profileImageTokenFile = lib.mkOption {
      type = lib.types.nullOr lib.types.str;
      default = null;
      description = ''
        Optional root-readable file containing a bearer token for private
        profile image catalogs and artifacts. The token is read by kiln-vmd and
        is not placed directly in the Nix store.
      '';
    };

    profileImagePullTimeout = lib.mkOption {
      type = lib.types.str;
      default = "30m";
      description = "Maximum duration for pulling a trusted VM profile image.";
    };

    bootstrapTokenFile = lib.mkOption {
      type = lib.types.str;
      default = "${cfg.dataDir}/bootstrap-token";
      description = "Root-readable file where kiln-vmd writes its initial bootstrap API token.";
    };

    libvirtConnectURI = lib.mkOption {
      type = lib.types.str;
      default = "qemu:///system";
      description = "Libvirt connection URI used for qemu-backed profiles such as windows11.";
    };

    libvirtNetwork = lib.mkOption {
      type = lib.types.str;
      default = "kiln-vm";
      description = "Libvirt NAT network for Windows VMs. kiln-vmd creates and starts the kiln-vm network itself; any other name must already exist and be active.";
    };

    libvirtCIDR = lib.mkOption {
      type = lib.types.str;
      default = "10.232.0.0/24";
      description = "IPv4 range kiln-vmd gives the kiln-vm libvirt network when it creates it. Must not overlap bridgeCIDR.";
    };

    windowsProfileDir = lib.mkOption {
      type = lib.types.str;
      default = "${cfg.dataDir}/windows-profiles";
      description = "Directory where prepared Windows profile media and cached base disks are stored.";
    };

    environmentFile = lib.mkOption {
      type = lib.types.nullOr lib.types.path;
      default = null;
      description = ''
        Optional path to an environment file (e.g. /etc/kiln-vm/env) that
        will be loaded by the systemd unit.  Useful for secrets and
        overrides.
      '';
    };
  };

  config = lib.mkIf cfg.enable {
    assertions = [
      {
        assertion = cfg.tls.enable || isSyntacticListenAddress cfg.listenAddr;
        message = "services.kiln-vmd.listenAddr must be a syntactically valid host:port address in plaintext mode";
      }
      {
        assertion =
          cfg.tls.enable
          || cfg.allowInsecureRemoteHTTP
          || isLoopbackListenAddress cfg.listenAddr;
        message = "services.kiln-vmd.listenAddr must be loopback-only unless services.kiln-vmd.allowInsecureRemoteHTTP is true";
      }
      {
        assertion = !cfg.tls.enable || !cfg.allowInsecureRemoteHTTP;
        message = "services.kiln-vmd.allowInsecureRemoteHTTP cannot be enabled together with services.kiln-vmd.tls.enable";
      }
      {
        assertion = !cfg.tls.enable || isValidTLSDomain cfg.tls.domain;
        message = "services.kiln-vmd.tls.domain must be a valid fully qualified ASCII DNS hostname when TLS is enabled";
      }
      {
        assertion =
          !cfg.tls.enable
          || (cfg.tls.certDir == trim cfg.tls.certDir && cfg.tls.certDir != "");
        message = "services.kiln-vmd.tls.certDir must be non-empty and have no surrounding whitespace when TLS is enabled";
      }
      {
        assertion = !cfg.tls.enable || isSyntacticListenAddress cfg.tls.listenAddr;
        message = "services.kiln-vmd.tls.listenAddr must be a syntactically valid host:port address";
      }
      {
        assertion = !cfg.tls.enable || isSyntacticListenAddress cfg.tls.acmeListenAddr;
        message = "services.kiln-vmd.tls.acmeListenAddr must be a syntactically valid host:port address";
      }
      {
        assertion = !cfg.tls.enable || trim cfg.tls.listenAddr != trim cfg.tls.acmeListenAddr;
        message = "services.kiln-vmd.tls.listenAddr and services.kiln-vmd.tls.acmeListenAddr must differ";
      }
      {
        assertion = cfg.guestDNSProbeName == trim cfg.guestDNSProbeName && isValidDNSHostname cfg.guestDNSProbeName;
        message = "services.kiln-vmd.guestDNSProbeName must be a valid fully qualified ASCII DNS hostname without surrounding whitespace";
      }
    ];

    boot.kernel.sysctl."net.ipv4.ip_forward" = 1;

    virtualisation.docker = {
      enable = true;
      package = lib.mkDefault pkgs.docker_29;
    };
    virtualisation.libvirtd.enable = true;
    virtualisation.libvirtd.qemu.swtpm.enable = true;

    environment.systemPackages = [
      cfg.package
      pkgs.curl
      pkgs.e2fsprogs
      pkgs.firecracker
      pkgs.iproute2
      pkgs.iptables
      pkgs.libvirt
      pkgs.mtools
      pkgs.qemu_kvm
      pkgs.swtpm
      pkgs.util-linux
      pkgs.virt-manager
    ];

    systemd.services.kiln-vmd = {
      description = "kiln-vm VM manager";
      # The daemon reconciles its own narrowly scoped INPUT hook. Ordering after
      # the host firewall avoids an unnecessary first-pass repair at boot; this
      # does not delegate rule ownership to the NixOS firewall or open :53.
      after = [ "network.target" "firewall.service" ];
      wantedBy = [ "multi-user.target" ];
      path = [
        pkgs.coreutils
        pkgs.curl
        config.virtualisation.docker.package
        pkgs.e2fsprogs
        pkgs.firecracker
        pkgs.gnutar
        pkgs.iproute2
        pkgs.iptables
        pkgs.libvirt
        pkgs.mtools
        pkgs.openssh
        pkgs.qemu_kvm
        pkgs.swtpm
        pkgs.util-linux
        pkgs.virt-manager
      ];
      unitConfig = {
        StartLimitBurst = 3;
        StartLimitIntervalSec = 60;
      };

      environment = {
        KILN_VM_LISTEN_ADDR   = cfg.listenAddr;
        KILN_VM_ALLOW_INSECURE_REMOTE_HTTP = lib.boolToString cfg.allowInsecureRemoteHTTP;
        KILN_VM_TLS_ENABLED = lib.boolToString cfg.tls.enable;
        KILN_VM_TLS_DOMAIN = cfg.tls.domain;
        KILN_VM_TLS_CERT_DIR = cfg.tls.certDir;
        KILN_VM_TLS_LISTEN_ADDR = cfg.tls.listenAddr;
        KILN_VM_ACME_LISTEN_ADDR = cfg.tls.acmeListenAddr;
        KILN_VM_ACME_EMAIL = cfg.tls.email;
        KILN_VM_DATA_DIR      = cfg.dataDir;
        KILN_VM_KERNEL_PATH   = cfg.kernelPath;
        KILN_VM_FIRECRACKER_BIN = cfg.firecrackerBin;
        KILN_VM_AGENT_BIN     = cfg.agentBinPath;
        KILN_VM_BRIDGE_CIDR   = cfg.bridgeCIDR;
        KILN_VM_GUEST_DNS_PROBE_NAME = cfg.guestDNSProbeName;
        KILN_VM_MAX_RUNNING_VMS = toString cfg.maxRunningVMs;
        KILN_VM_MAX_TOTAL_VCPUS = toString cfg.maxTotalVCPUs;
        KILN_VM_MAX_TOTAL_MEMORY_MB = toString cfg.maxTotalMemoryMB;
        KILN_VM_MAX_VM_DISK_MB = toString cfg.maxVMDiskMB;
        KILN_VM_IMAGE_BUILD_TIMEOUT = cfg.imageBuildTimeout;
        KILN_VM_PROFILE_IMAGE_MODE = cfg.profileImageMode;
        KILN_VM_PROFILE_IMAGE_CATALOG = cfg.profileImageCatalog;
        KILN_VM_PROFILE_IMAGE_CACHE_MAX_MB = toString cfg.profileImageCacheMaxMB;
        KILN_VM_PROFILE_IMAGE_PULL_TIMEOUT = cfg.profileImagePullTimeout;
        KILN_VM_BOOTSTRAP_TOKEN_FILE = cfg.bootstrapTokenFile;
        KILN_VM_LIBVIRT_CONNECT_URI = cfg.libvirtConnectURI;
        KILN_VM_LIBVIRT_NETWORK = cfg.libvirtNetwork;
        KILN_VM_LIBVIRT_CIDR = cfg.libvirtCIDR;
        KILN_VM_WINDOWS_PROFILE_DIR = cfg.windowsProfileDir;
      } // lib.optionalAttrs (cfg.profileImageTokenFile != null) {
        KILN_VM_PROFILE_IMAGE_TOKEN_FILE = cfg.profileImageTokenFile;
      };

      serviceConfig = {
        ExecStart = "${cfg.package}/bin/kiln-vmd";
        Type = "notify";
        NotifyAccess = "main";
        Restart = "always";
        RestartPreventExitStatus = 78;
        RestartSec = 5;
        TimeoutStopSec = 30;
        KillMode = "control-group";
        KillSignal = "SIGTERM";
        SendSIGKILL = true;
        MemoryAccounting = true;
        TasksAccounting = true;
        Delegate = true;

        # kiln-vmd needs root privileges to create TAP devices and
        # manipulate iptables rules.
        DynamicUser = false;

        StateDirectory = "kiln-vm";
        StateDirectoryMode = "0750";

        # Load optional env-file for secrets / overrides.
        EnvironmentFile = lib.optional (cfg.environmentFile != null)
          cfg.environmentFile;
      };
    };
  };
}
