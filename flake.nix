{
  description = "kiln-vm – Firecracker microVM manager";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-26.05";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs = { self, nixpkgs, flake-utils, ... }:
    let
      # Systems we build packages for
      supportedSystems = [ "x86_64-linux" "aarch64-linux" ];

      # Per-system outputs (packages, devShells)
      perSystem = flake-utils.lib.eachSystem supportedSystems (system:
        let
          pkgs = import nixpkgs { inherit system; };
          buildGoModule = pkgs.buildGoModule.override { go = pkgs.go_1_26; };
          goVendorHash = "sha256-kL79sHQV/9deoADIx3S8UYUXO4/YB0u3MK9hTgvHkvQ=";
        in
        {
          checks.kiln-vmd-nixos-module = import ./nix/tests/kiln-vmd-module.nix {
            inherit nixpkgs system;
          };

          packages = {
            guest-kernel = pkgs.callPackage ./nix/guest-kernel.nix { };

            kiln-vmd = buildGoModule {
              pname = "kiln-vmd";
              version = "0.1.0";
              src = ./.;
              env.CGO_ENABLED = "0";
              subPackages = [
                "cmd/kiln-vmd"
                "cmd/kiln-vm-agent"
              ];
              postInstall = ''
                install -Dm444 third_party/github.com/miekg/dns/LICENSE \
                  "$out/share/doc/kiln-vmd/third-party/miekg-dns.LICENSE"
                install -Dm444 third_party/golang.org/x/net/LICENSE \
                  "$out/share/doc/kiln-vmd/third-party/golang-x-net.LICENSE"
              '';

              # The project vendors its dependencies or uses the Go module
              # proxy.  Set this to the correct SRI hash after the first
              # build, or use `null` when deps are vendored in-tree.
              vendorHash = goVendorHash;

              meta = {
                description = "kiln-vm – daemon that manages Firecracker microVMs";
                mainProgram = "kiln-vmd";
                license = pkgs.lib.licenses.asl20;
              };
            };

            kiln-vm = buildGoModule {
              pname = "kiln-vm";
              version = "0.1.0";
              src = ./.;
              env.CGO_ENABLED = "0";
              subPackages = [ "cmd/kiln-vm" ];
              vendorHash = goVendorHash;
              postInstall = ''
                install -Dm444 third_party/github.com/miekg/dns/LICENSE \
                  "$out/share/doc/kiln-vm/third-party/miekg-dns.LICENSE"
                install -Dm444 third_party/golang.org/x/net/LICENSE \
                  "$out/share/doc/kiln-vm/third-party/golang-x-net.LICENSE"
              '';

              meta = {
                description = "kiln-vm CLI – manage Firecracker microVMs";
                mainProgram = "kiln-vm";
                license = pkgs.lib.licenses.asl20;
              };
            };

            kiln-vm-agent = buildGoModule {
              pname = "kiln-vm-agent";
              version = "0.1.0";
              src = ./.;
              env.CGO_ENABLED = "0";
              subPackages = [ "cmd/kiln-vm-agent" ];
              vendorHash = goVendorHash;
              postInstall = ''
                install -Dm444 third_party/golang.org/x/net/LICENSE \
                  "$out/share/doc/kiln-vm-agent/third-party/golang-x-net.LICENSE"
              '';

              meta = {
                description = "kiln-vm guest agent";
                mainProgram = "kiln-vm-agent";
                license = pkgs.lib.licenses.asl20;
              };
            };

            default = self.packages.${system}.kiln-vmd;
          };

          devShells.default = pkgs.mkShell {
            buildInputs = with pkgs; [
              # Go toolchain
              go_1_26

              # Firecracker VMM
              firecracker

              # Container tooling (rootfs builds)
              docker_29

              # SQLite CLI (useful for inspecting the state db)
              sqlite

              # Linting / formatting
              golangci-lint
              gotools          # provides goimports

              go-task
              nodejs_22
            ];

            shellHook = ''
              echo "kiln-vm dev shell ready  (Go $(go version | cut -d' ' -f3))"
            '';
          };
        }
      );

      # NixOS-specific outputs (modules, ISO) -- only x86_64-linux
      nixosOutputs =
        let
          system = "x86_64-linux";
          pkgs = import nixpkgs { inherit system; };
        in
        {
          # Re-usable NixOS modules.
          nixosModules = {
            kiln-vmd = { lib, pkgs, ... }:
              let
                hostPackages = self.packages.${pkgs.stdenv.hostPlatform.system};
              in
              {
                imports = [ ./nix/modules/kiln-vmd.nix ];
                services.kiln-vmd.package = lib.mkDefault hostPackages.kiln-vmd;
                services.kiln-vmd.kernelPath = lib.mkDefault "${hostPackages.guest-kernel}";
              };
            firecracker = import ./nix/modules/firecracker.nix;
          };

          # Bootable ISO image: `nix build .#iso`
          packages.${system}.iso =
            let
              isoConfig = nixpkgs.lib.nixosSystem {
                inherit system;
                modules = [
                  ./nix/iso.nix
                  {
                    services.kiln-vmd.package = self.packages.${system}.kiln-vmd;
                    services.kiln-vmd.kernelPath = "${self.packages.${system}.guest-kernel}";
                  }
                ];
              };
            in
            isoConfig.config.system.build.isoImage;
        };
    in
    # Merge per-system outputs with NixOS-only outputs
    perSystem // {
      inherit (nixosOutputs) nixosModules;

      # Merge packages: keep all per-system packages and add the iso for
      # x86_64-linux.
      packages = perSystem.packages // {
        x86_64-linux = (perSystem.packages.x86_64-linux or {}) // {
          iso = nixosOutputs.packages.x86_64-linux.iso;
        };
      };
    };
}
