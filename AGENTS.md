# kiln-vm

kiln-vm runs persistent microVMs on a Linux host. `kiln-vmd` owns VM
lifecycle, networking and storage and serves an HTTP API. `kiln-vm` is the CLI
for that API. `kiln-vm-agent` runs inside Linux guests and talks to the host
over vsock.

## Layout

- `cmd/`: the four binaries, `kiln-vmd`, `kiln-vm`, `kiln-vm-agent` and
  `kiln-vm-profile-artifact`.
- `internal/server`, `internal/api`: HTTP routes and handlers.
- `internal/vm`: VM records, profiles, power policies and profile images.
- `internal/machine`: the Firecracker and libvirt backends, TAP networking,
  isolation and the gateway DNS proxy.
- `internal/image`: root filesystem builds and the image store.
- `internal/store`: the SQLite database.
- `internal/cli`: the `kiln-vm` commands.
- `internal/systemtest`: tests that create real VMs.
- `nix/`: the NixOS module, its VM test and the guest kernel.
- `scripts/`: Debian packaging and release tooling, with Node contract tests in
  `scripts/tests`.

## Keep the scope

kiln-vm manages persistent VMs on one host. Linux guests run on Firecracker.
libvirt and QEMU are only for Windows guests. New features should fit that
scope; propose anything larger in an issue first.

## Test

Run `npm ci` once, then:

- `npx testkit run` runs the default `unit`, `race` and `contracts` projects
  and the Node contract tests, the same selection CI runs. Narrow it with
  `--file <path>` or `--go-project <name>`, then run the defaults before you
  push.
- `task ci` runs formatting, vet, govulncheck, golangci-lint, the default
  tests, a compile of every system test tag set, and the build.
- The system projects in `testkit.config.ts` create real VMs. They need a KVM
  host and a `kiln-vmd` you start yourself. Never add them to CI.
- Never skip a failing test, weaken an assertion, or assert broken behaviour.

[Development](docs/development.md) covers setup, system tests, Nix checks and
packaging.

## Conventions

- The store schema has one version. Until the first stable release, change
  `internal/store/migrations.go` in place rather than adding migrations.
- Every host resource kiln-vm creates carries a kiln-vm name: TAP devices
  `kv<N>`, the cgroup `/sys/fs/cgroup/kiln-vm`, libvirt domains `kiln-vm-*`
  and the libvirt network `kiln-vm`. Never modify a resource kiln-vm cannot
  prove it owns.
- After a `go.mod` change, update `goVendorHash` in `flake.nix`.
- Nix flakes only see files git tracks. Run `git add` before `nix build`.
