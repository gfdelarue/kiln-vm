{ nixpkgs, system }:

let
  inherit (nixpkgs) lib;
  pkgs = import nixpkgs { inherit system; };
  dummyDaemon = pkgs.writeShellScriptBin "kiln-vmd" "exit 0";

  mkDaemonSystem = extraModule:
    lib.nixosSystem {
      inherit system;
      modules = [
        ../modules/kiln-vmd.nix
        ({ ... }: {
          services.kiln-vmd.package = dummyDaemon;
          services.kiln-vmd.enable = true;
          system.stateVersion = "26.05";
        })
        extraModule
      ];
    };

  moduleFailures = systemConfig:
    map (entry: entry.message) (
      builtins.filter
        (entry: !entry.assertion && lib.hasPrefix "services.kiln-vmd" entry.message)
        systemConfig.config.assertions
    );

  defaults = mkDaemonSystem { };
  defaultEnvironment = defaults.config.systemd.services.kiln-vmd.environment;
  defaultService = defaults.config.systemd.services.kiln-vmd.serviceConfig;
  defaultUnit = defaults.config.systemd.units."kiln-vmd.service".text;
  defaultAfter = defaults.config.systemd.services.kiln-vmd.after;
  defaultAllowedTCPPorts = defaults.config.networking.firewall.allowedTCPPorts;
  defaultAllowedUDPPorts = defaults.config.networking.firewall.allowedUDPPorts;

  customGuestDNSProbe = mkDaemonSystem {
    services.kiln-vmd.guestDNSProbeName = "probe.example.test.";
  };
  invalidGuestDNSProbes = map
    (guestDNSProbeName: mkDaemonSystem {
      services.kiln-vmd = { inherit guestDNSProbeName; };
    })
    [ "" "." "-probe.example." "probe..example." "probe_example." ];
  whitespaceGuestDNSProbe = mkDaemonSystem {
    services.kiln-vmd.guestDNSProbeName = " probe.example.test.";
  };

  remoteDenied = mkDaemonSystem {
    services.kiln-vmd.listenAddr = "0.0.0.0:8080";
  };
  remoteAllowed = mkDaemonSystem {
    services.kiln-vmd = {
      listenAddr = "0.0.0.0:8080";
      allowInsecureRemoteHTTP = true;
    };
  };
  invalidPlaintext = mkDaemonSystem {
    services.kiln-vmd.listenAddr = "not-a-listener";
  };
  invalidPlaintextPort = mkDaemonSystem {
    services.kiln-vmd.listenAddr = "127.0.0.1:65536";
  };
  zeroPlaintextPort = mkDaemonSystem {
    services.kiln-vmd.listenAddr = "127.0.0.1:0";
  };
  whitespacePlaintext = mkDaemonSystem {
    services.kiln-vmd.listenAddr = " 127.0.0.1:8080";
  };
  invalidPlaintextHosts = map
    (listenAddr: mkDaemonSystem {
      services.kiln-vmd = {
        inherit listenAddr;
        allowInsecureRemoteHTTP = true;
      };
    })
    [ "bad host:8080" "999.999.999.999:8080" "[::::]:8080" ];
  validRemoteIPHosts = map
    (listenAddr: mkDaemonSystem {
      services.kiln-vmd = {
        inherit listenAddr;
        allowInsecureRemoteHTTP = true;
      };
    })
    [ "[2001:db8::1]:8080" "[fe80::1%eth0]:8080" "[::ffff:192.0.2.1]:8080" ];
  loopbackVariants = map
    (listenAddr: mkDaemonSystem { services.kiln-vmd = { inherit listenAddr; }; })
    [ "LOCALHOST.:8080" "[0::1]:8080" ];
  mappedLoopbackVariants = map
    (listenAddr: mkDaemonSystem { services.kiln-vmd = { inherit listenAddr; }; })
    [
      "[::ffff:127.0.0.1]:8080"
      "[::ffff:7f00:1]:8080"
      "[0:0:0:0:0:ffff:7f00:1]:8080"
    ];
  mappedNonLoopbackVariants = map
    (listenAddr: mkDaemonSystem { services.kiln-vmd = { inherit listenAddr; }; })
    [
      "[::ffff:192.0.2.1]:8080"
      "[::ffff:c000:201]:8080"
      "[0:0:0:0:0:ffff:c000:201]:8080"
    ];

  validTLS = mkDaemonSystem {
    services.kiln-vmd = {
      # TLS mode must not depend on the unused plaintext listener.
      listenAddr = "unused-in-tls-mode";
      tls = {
        enable = true;
        domain = "kiln-vm.example.test";
        certDir = "/var/lib/kiln-vm/acme-test";
        listenAddr = "0.0.0.0:8443";
        acmeListenAddr = "0.0.0.0:8080";
        email = "kiln-vm@example.test";
      };
    };
  };
  tlsEnvironment = validTLS.config.systemd.services.kiln-vmd.environment;
  tlsMissingDomain = mkDaemonSystem {
    services.kiln-vmd.tls.enable = true;
  };
  tlsInvalidDomains = map
    (domain: mkDaemonSystem {
      services.kiln-vmd.tls = {
        enable = true;
        inherit domain;
      };
    })
    [ "localhost" "https://kiln-vm.example.test" " kiln-vm.example.test" "127.0.0.1" ];
  tlsWhitespaceCertDir = mkDaemonSystem {
    services.kiln-vmd.tls = {
      enable = true;
      domain = "kiln-vm.example.test";
      certDir = " /var/lib/kiln-vm/certs";
    };
  };
  tlsWithPlaintextOptIn = mkDaemonSystem {
    services.kiln-vmd = {
      allowInsecureRemoteHTTP = true;
      tls = {
        enable = true;
        domain = "kiln-vm.example.test";
      };
    };
  };
  tlsSharedAddress = mkDaemonSystem {
    services.kiln-vmd.tls = {
      enable = true;
      domain = "kiln-vm.example.test";
      listenAddr = ":443";
      acmeListenAddr = ":443";
    };
  };
  tlsInvalidAddress = mkDaemonSystem {
    services.kiln-vmd.tls = {
      enable = true;
      domain = "kiln-vm.example.test";
      listenAddr = "not-a-listener";
    };
  };
  tlsInvalidListenerHost = mkDaemonSystem {
    services.kiln-vmd.tls = {
      enable = true;
      domain = "kiln-vm.example.test";
      listenAddr = "bad host:443";
    };
  };
  tlsMalformedIPv6Host = mkDaemonSystem {
    services.kiln-vmd.tls = {
      enable = true;
      domain = "kiln-vm.example.test";
      listenAddr = "[::::]:443";
    };
  };
  tlsInvalidACMEHost = mkDaemonSystem {
    services.kiln-vmd.tls = {
      enable = true;
      domain = "kiln-vm.example.test";
      acmeListenAddr = "999.999.999.999:80";
    };
  };

  withEnvironmentFile = mkDaemonSystem {
    services.kiln-vmd.environmentFile = "/etc/kiln-vm/kiln-vmd.env";
  };

  isoSupported = system == "x86_64-linux";
  iso =
    if isoSupported then
      lib.nixosSystem {
        inherit system;
        modules = [
          ../iso.nix
          ({ ... }: {
            services.kiln-vmd.package = dummyDaemon;
          })
        ];
      }
    else
      null;
  isoPorts = if isoSupported then iso.config.networking.firewall.allowedTCPPorts else [ ];
  isoEnvironment = if isoSupported then iso.config.systemd.services.kiln-vmd.environment else { };
  isoUnit = if isoSupported then iso.config.systemd.units."kiln-vmd.service".text else "";
in
assert lib.assertMsg
  (defaults.config.services.kiln-vmd.listenAddr == "127.0.0.1:5456")
  "kiln-vmd NixOS module must default to a loopback management listener";
assert lib.assertMsg
  (!defaults.config.services.kiln-vmd.allowInsecureRemoteHTTP)
  "kiln-vmd NixOS module must default remote plaintext opt-in to false";
assert lib.assertMsg
  (!defaults.config.services.kiln-vmd.tls.enable)
  "kiln-vmd NixOS module must default TLS to disabled";
assert lib.assertMsg
  (defaults.config.services.kiln-vmd.guestDNSProbeName == "example.com.")
  "kiln-vmd NixOS module must default the provider-owned gateway DNS probe name";
assert lib.assertMsg
  (defaultEnvironment.KILN_VM_LISTEN_ADDR == "127.0.0.1:5456"
    && defaultEnvironment.KILN_VM_ALLOW_INSECURE_REMOTE_HTTP == "false"
    && defaultEnvironment.KILN_VM_TLS_ENABLED == "false"
    && defaultEnvironment.KILN_VM_TLS_LISTEN_ADDR == ":443"
    && defaultEnvironment.KILN_VM_ACME_LISTEN_ADDR == ":80"
    && defaultEnvironment.KILN_VM_GUEST_DNS_PROBE_NAME == "example.com.")
  "kiln-vmd NixOS module must render the safe management transport defaults";
assert lib.assertMsg
  (moduleFailures customGuestDNSProbe == [ ]
    && customGuestDNSProbe.config.systemd.services.kiln-vmd.environment.KILN_VM_GUEST_DNS_PROBE_NAME == "probe.example.test.")
  "kiln-vmd NixOS module must render an explicit gateway DNS probe name";
assert lib.assertMsg
  (lib.all
    (systemConfig: builtins.elem
      "services.kiln-vmd.guestDNSProbeName must be a valid fully qualified ASCII DNS hostname without surrounding whitespace"
      (moduleFailures systemConfig))
    invalidGuestDNSProbes)
  "kiln-vmd NixOS module must reject empty, root, empty-label, underscore, and invalid-label gateway DNS probe names";
assert lib.assertMsg
  (builtins.elem
    "services.kiln-vmd.guestDNSProbeName must be a valid fully qualified ASCII DNS hostname without surrounding whitespace"
    (moduleFailures whitespaceGuestDNSProbe))
  "kiln-vmd NixOS module must reject surrounding gateway DNS probe whitespace that would be emitted verbatim";
assert lib.assertMsg
  (defaultService.Type == "notify"
    && defaultService.NotifyAccess == "main"
    && defaultService.RestartPreventExitStatus == 78
    && lib.hasInfix "Environment=\"KILN_VM_LISTEN_ADDR=127.0.0.1:5456\"" defaultUnit
    && !lib.hasInfix "ExecCondition=" defaultUnit
    && !lib.hasInfix "ExecStartPre=" defaultUnit
    && lib.hasInfix "Type=notify" defaultUnit
    && lib.hasInfix "NotifyAccess=main" defaultUnit
    && lib.hasInfix "RestartPreventExitStatus=78" defaultUnit)
  "kiln-vmd systemd unit must wait for main-process readiness and visibly fail without restarting after configuration exit status 78";
assert lib.assertMsg
  (builtins.elem "firewall.service" defaultAfter
    && !(builtins.elem 53 defaultAllowedTCPPorts)
    && !(builtins.elem 53 defaultAllowedUDPPorts))
  "kiln-vmd must start after the NixOS firewall without globally opening TCP or UDP port 53";
assert lib.assertMsg
  (moduleFailures remoteDenied == [
    "services.kiln-vmd.listenAddr must be loopback-only unless services.kiln-vmd.allowInsecureRemoteHTTP is true"
  ])
  "kiln-vmd NixOS module must reject remote plaintext without explicit opt-in";
assert lib.assertMsg
  (moduleFailures remoteAllowed == [ ]
    && remoteAllowed.config.systemd.services.kiln-vmd.environment.KILN_VM_ALLOW_INSECURE_REMOTE_HTTP == "true"
    && remoteAllowed.config.networking.firewall.allowedTCPPorts == [ ])
  "kiln-vmd NixOS module must pass through explicit remote plaintext opt-in without opening a firewall port";
assert lib.assertMsg
  (builtins.elem
    "services.kiln-vmd.listenAddr must be a syntactically valid host:port address in plaintext mode"
    (moduleFailures invalidPlaintext))
  "kiln-vmd NixOS module must reject a malformed plaintext listener";
assert lib.assertMsg
  (builtins.elem
    "services.kiln-vmd.listenAddr must be a syntactically valid host:port address in plaintext mode"
    (moduleFailures invalidPlaintextPort))
  "kiln-vmd NixOS module must reject a plaintext listener outside the TCP port range";
assert lib.assertMsg
  (builtins.elem
    "services.kiln-vmd.listenAddr must be a syntactically valid host:port address in plaintext mode"
    (moduleFailures zeroPlaintextPort))
  "kiln-vmd NixOS module must reject plaintext port zero";
assert lib.assertMsg
  (builtins.elem
    "services.kiln-vmd.listenAddr must be a syntactically valid host:port address in plaintext mode"
    (moduleFailures whitespacePlaintext))
  "kiln-vmd NixOS module must reject surrounding listener whitespace that would be emitted verbatim";
assert lib.assertMsg
  (lib.all
    (systemConfig: builtins.elem
      "services.kiln-vmd.listenAddr must be a syntactically valid host:port address in plaintext mode"
      (moduleFailures systemConfig))
    invalidPlaintextHosts)
  "kiln-vmd NixOS module must reject invalid plaintext DNS, IPv4-looking, and IPv6 hosts before daemon startup";
assert lib.assertMsg
  (lib.all (systemConfig: moduleFailures systemConfig == [ ]) validRemoteIPHosts)
  "kiln-vmd NixOS module must retain valid ordinary, zoned, and IPv4-mapped IPv6 listener support";
assert lib.assertMsg
  (lib.all (systemConfig: moduleFailures systemConfig == [ ]) loopbackVariants)
  "kiln-vmd NixOS module must accept canonical localhost and literal IPv6 loopback listeners";
assert lib.assertMsg
  (lib.all (systemConfig: moduleFailures systemConfig == [ ]) mappedLoopbackVariants)
  "kiln-vmd NixOS module must recognize dotted, compressed, and expanded IPv4-mapped loopback listeners";
assert lib.assertMsg
  (lib.all
    (systemConfig: moduleFailures systemConfig == [
      "services.kiln-vmd.listenAddr must be loopback-only unless services.kiln-vmd.allowInsecureRemoteHTTP is true"
    ])
    mappedNonLoopbackVariants)
  "kiln-vmd NixOS module must not treat mapped non-loopback IPv4 addresses as loopback";
assert lib.assertMsg
  (moduleFailures validTLS == [ ]
    && tlsEnvironment.KILN_VM_TLS_ENABLED == "true"
    && tlsEnvironment.KILN_VM_TLS_DOMAIN == "kiln-vm.example.test"
    && tlsEnvironment.KILN_VM_TLS_CERT_DIR == "/var/lib/kiln-vm/acme-test"
    && tlsEnvironment.KILN_VM_TLS_LISTEN_ADDR == "0.0.0.0:8443"
    && tlsEnvironment.KILN_VM_ACME_LISTEN_ADDR == "0.0.0.0:8080"
    && tlsEnvironment.KILN_VM_ACME_EMAIL == "kiln-vm@example.test")
  "kiln-vmd NixOS module must render a valid TLS and ACME configuration";
assert lib.assertMsg
  (builtins.elem
    "services.kiln-vmd.tls.domain must be a valid fully qualified ASCII DNS hostname when TLS is enabled"
    (moduleFailures tlsMissingDomain))
  "kiln-vmd NixOS module must require a valid fully qualified TLS domain";
assert lib.assertMsg
  (lib.all
    (systemConfig: builtins.elem
      "services.kiln-vmd.tls.domain must be a valid fully qualified ASCII DNS hostname when TLS is enabled"
      (moduleFailures systemConfig))
    tlsInvalidDomains)
  "kiln-vmd NixOS module must reject single-label, URL, whitespace, and IP TLS domains";
assert lib.assertMsg
  (builtins.elem
    "services.kiln-vmd.tls.certDir must be non-empty and have no surrounding whitespace when TLS is enabled"
    (moduleFailures tlsWhitespaceCertDir))
  "kiln-vmd NixOS module must reject surrounding TLS certificate-directory whitespace";
assert lib.assertMsg
  (builtins.elem
    "services.kiln-vmd.allowInsecureRemoteHTTP cannot be enabled together with services.kiln-vmd.tls.enable"
    (moduleFailures tlsWithPlaintextOptIn))
  "kiln-vmd NixOS module must reject contradictory TLS and insecure HTTP modes";
assert lib.assertMsg
  (builtins.elem
    "services.kiln-vmd.tls.listenAddr and services.kiln-vmd.tls.acmeListenAddr must differ"
    (moduleFailures tlsSharedAddress))
  "kiln-vmd NixOS module must reject identical TLS and ACME listeners";
assert lib.assertMsg
  (builtins.elem
    "services.kiln-vmd.tls.listenAddr must be a syntactically valid host:port address"
    (moduleFailures tlsInvalidAddress))
  "kiln-vmd NixOS module must reject a malformed TLS listener";
assert lib.assertMsg
  (builtins.elem
    "services.kiln-vmd.tls.listenAddr must be a syntactically valid host:port address"
    (moduleFailures tlsInvalidListenerHost))
  "kiln-vmd NixOS module must reject an invalid TLS listener host";
assert lib.assertMsg
  (builtins.elem
    "services.kiln-vmd.tls.listenAddr must be a syntactically valid host:port address"
    (moduleFailures tlsMalformedIPv6Host))
  "kiln-vmd NixOS module must reject a malformed bracketed IPv6 TLS listener host";
assert lib.assertMsg
  (builtins.elem
    "services.kiln-vmd.tls.acmeListenAddr must be a syntactically valid host:port address"
    (moduleFailures tlsInvalidACMEHost))
  "kiln-vmd NixOS module must reject an invalid ACME listener host";
assert lib.assertMsg
  (withEnvironmentFile.config.systemd.services.kiln-vmd.serviceConfig.EnvironmentFile == [
    "/etc/kiln-vm/kiln-vmd.env"
  ])
  "kiln-vmd NixOS module must retain environment-file override support";
assert lib.assertMsg
  (!isoSupported
    || (iso.config.services.kiln-vmd.listenAddr == "127.0.0.1:5456"
      && !iso.config.services.kiln-vmd.allowInsecureRemoteHTTP
      && !iso.config.services.kiln-vmd.tls.enable
      && isoEnvironment.KILN_VM_LISTEN_ADDR == "127.0.0.1:5456"
      && isoEnvironment.KILN_VM_ALLOW_INSECURE_REMOTE_HTTP == "false"
      && isoEnvironment.KILN_VM_TLS_ENABLED == "false"
      && lib.hasInfix "Environment=\"KILN_VM_LISTEN_ADDR=127.0.0.1:5456\"" isoUnit
      && lib.hasInfix "Environment=\"KILN_VM_ALLOW_INSECURE_REMOTE_HTTP=false\"" isoUnit
      && lib.hasInfix "Environment=\"KILN_VM_TLS_ENABLED=false\"" isoUnit
      && !lib.hasInfix "ExecCondition=" isoUnit
      && !lib.hasInfix "ExecStartPre=" isoUnit
      && lib.hasInfix "Type=notify" isoUnit
      && lib.hasInfix "NotifyAccess=main" isoUnit
      && lib.hasInfix "RestartPreventExitStatus=78" isoUnit))
  "kiln-vm ISO must render the safe management transport, readiness, and no-loop validation policy";
assert lib.assertMsg
  (validTLS.config.networking.firewall.allowedTCPPorts == [ ])
  "kiln-vmd NixOS module must not derive firewall openings from TLS listeners";
assert lib.assertMsg
  (!isoSupported
    || (!builtins.elem 80 isoPorts && !builtins.elem 443 isoPorts && !builtins.elem 5456 isoPorts))
  "kiln-vm ISO must not open HTTP, HTTPS, or legacy management ports automatically";
pkgs.runCommand "kiln-vmd-nixos-module-check" { } ''
  touch "$out"
''
