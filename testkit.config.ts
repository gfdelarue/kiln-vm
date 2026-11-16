import { defineConfig } from "@gfdelarue/testkit/config";

const systemtest = ["./internal/systemtest"];

const focusedPackages = [
  "./internal/config",
  "./internal/store",
  "./internal/middleware",
  "./internal/machine",
  "./internal/guestagent",
  "./cmd/kiln-vm-agent",
  "./internal/dnsprobe",
  "./internal/image",
  "./internal/vm",
  "./internal/metrics",
  "./internal/disk",
  "./internal/api",
  "./internal/server",
  "./internal/version",
];

const pureSystemContracts = [
  "TestCompatibleHealthErrorAcceptsMatchingDaemon",
  "TestCompatibleHealthErrorRejectsStaleDaemon",
  "TestNetworkLinkInspectionDistinguishesMissingFromFailure",
  "TestNetworkGatewayDNSResponseValidationRequiresNoErrorAddressAnswer",
  "TestNetworkGatewayDNSBringdownTargetMatchersAreExact",
  "TestNetworkGatewayDNSFirewallHelperParsesCustomCIDRAdmissionAndBroadGuard",
  "TestNetworkGatewayDNSFirewallHelperParsesDynamicV2OwnershipAndOrdering",
  "TestNetworkGatewayDNSFirewallHelperRejectsOwnerAndChainCollisions",
  "TestNetworkGatewayDNSFirewallHelperValidatesDurableOwnerFile",
  "TestDaemonRestartGuardAllowsExactOwnedStableRestartFixtures",
  "TestDaemonRestartGuardClassifiesProviderRecoveryRisk",
  "TestDaemonRestartGuardExactOwnershipCannotBypassUnsafeWork",
  "TestDaemonRestartGuardKeepsUnownedStableResourcesBlocking",
  "TestDaemonRestartGuardUsesExactIDsNotNames",
];

const racePackages = [
  "./internal/store",
  "./internal/middleware",
  "./internal/machine",
  "./internal/guestagent",
  "./cmd/kiln-vm-agent",
  "./internal/dnsprobe",
  "./internal/vm",
  "./internal/disk",
  "./internal/api",
  "./internal/server",
];

export default defineConfig({
  nix: {
    checks: ["kiln-vmd-nixos-module"],
    defaultChecks: [],
  },
  areas: {
    "control-plane": {
      name: "Daemon, API and state",
      paths: [
        "cmd/kiln-vmd/**",
        "internal/api/**",
        "internal/apierr/**",
        "internal/auth/**",
        "internal/config/**",
        "internal/disk/**",
        "internal/hostfs/**",
        "internal/limits/**",
        "internal/metrics/**",
        "internal/middleware/**",
        "internal/models/**",
        "internal/naming/**",
        "internal/refid/**",
        "internal/sdnotify/**",
        "internal/server/**",
        "internal/store/**",
        "internal/version/**",
      ],
    },
    cli: { name: "kiln-vm CLI", paths: ["cmd/kiln-vm/**", "internal/cli/**"] },
    "machine-backends": {
      name: "Machine backends",
      paths: [
        "internal/machine/**",
        "internal/dnsprobe/**",
        "internal/libvirtaccess/**",
        "internal/libvirtdomain/**",
      ],
    },
    "vm-guest": {
      name: "VMs and guest agent",
      paths: [
        "cmd/kiln-vm-agent/**",
        "internal/vm/**",
        "internal/guestagent/**",
        "internal/guestexec/**",
        "internal/execution/**",
        "internal/execwire/**",
      ],
    },
    images: {
      name: "Images and VM profiles",
      paths: ["internal/image/**", "internal/windowsprofile/**", "cmd/kiln-vm-profile-artifact/**"],
    },
    "release-tooling": {
      name: "Release and packaging tooling",
      paths: ["scripts/**", "nix/**", "flake.nix", "third_party/**"],
    },
    "test-harness": { name: "System test harness", paths: ["internal/systemtest/**"] },
  },
  go: {
    defaultProjects: ["unit", "race", "contracts"],
    projects: {
      unit: { packages: ["./..."] },
      race: { packages: racePackages, race: true },
      contracts: {
        packages: systemtest,
        tags: ["system", "service"],
        run: "^(TestCompatibleHealthError.*|TestNetworkLinkInspectionDistinguishesMissingFromFailure|TestNetworkGatewayDNS(ResponseValidationRequiresNoErrorAddressAnswer|BringdownTargetMatchersAreExact|FirewallHelper.*)|TestDaemonRestartGuard.*)$",
        cases: { "./internal/systemtest": pureSystemContracts },
      },
      focused: { packages: focusedPackages },
      "management-transport": {
        packages: [
          "./cmd/kiln-vmd",
          "./internal/config",
          "./internal/server",
          "./internal/cli",
          "./internal/sdnotify",
        ],
      },
      system: {
        packages: systemtest,
        tags: ["system"],
        timeoutSeconds: 2700,
      },
      network: {
        packages: systemtest,
        tags: ["system"],
        run: "^TestNetwork",
        timeoutSeconds: 2700,
      },
      "gateway-dns-system": {
        packages: systemtest,
        tags: ["system"],
        run: "^(TestNetworkGatewayDNSResponseValidationRequiresNoErrorAddressAnswer|TestNetworkVMGatewayDNS)",
        timeoutSeconds: 2700,
      },
      "gateway-dns-service": {
        packages: systemtest,
        tags: ["system", "privileged", "service"],
        run: "^TestNetworkServiceGatewayDNS",
        timeoutSeconds: 3300,
      },
      "network-privileged": {
        packages: systemtest,
        tags: ["system", "privileged"],
        run: "^TestNetworkPrivileged",
        timeoutSeconds: 1500,
      },
      "network-service": {
        packages: systemtest,
        tags: ["system", "privileged", "service"],
        run: "^TestNetworkService",
        timeoutSeconds: 1500,
      },
      service: {
        packages: systemtest,
        tags: ["system", "service"],
        run: "TestService",
        timeoutSeconds: 900,
      },
      distro: {
        packages: systemtest,
        tags: ["system", "distro"],
        run: "TestDistro",
        timeoutSeconds: 5400,
      },
      capability: {
        packages: systemtest,
        tags: ["system", "distro"],
        run: "TestVMUbuntuDockerCapabilityVerification",
        timeoutSeconds: 4500,
      },
      windows: {
        packages: systemtest,
        tags: ["system", "windowsprofile"],
        run: "TestWindowsProfile",
        timeoutSeconds: 9000,
      },
    },
  },
});
