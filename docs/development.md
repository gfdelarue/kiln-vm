# Development

kiln-vm is a Go module with a small set of Node scripts for packaging and
release checks. The Nix dev shell has every tool the tasks below need.

## Set up

```bash
nix develop
npm ci
```

Without Nix, install Go 1.26, Node 22, [Task](https://taskfile.dev) and
golangci-lint.

## Build

```bash
task build
```

`task build` writes `kiln-vmd`, `kiln-vm`, `kiln-vm-profile-artifact` and
static `amd64` and `arm64` builds of `kiln-vm-agent` to `bin/`.

To run a development daemon, stop any installed `kiln-vmd` first, because two
daemons on one host would claim the same TAP device names. Give the
development daemon its own data directory:

```bash
sudo systemctl stop kiln-vmd
nix build .#guest-kernel -o result-kernel
sudo KILN_VM_DATA_DIR=/tmp/kiln-vm-dev \
  KILN_VM_AGENT_BIN=$PWD/bin/kiln-vm-agent-amd64 \
  KILN_VM_KERNEL_PATH=$PWD/result-kernel \
  ./bin/kiln-vmd
```

The daemon needs root for TAP devices, iptables and loop mounts. It runs
Firecracker from `/usr/bin/firecracker`; set `KILN_VM_FIRECRACKER_BIN` for
another path, such as `$(command -v firecracker)` in the dev shell. It writes its
bootstrap token to `/tmp/kiln-vm-dev/bootstrap-token`.

## Test

The tests run through [Testkit](https://www.npmjs.com/package/@gfdelarue/testkit),
which `npm ci` installs. The default run covers the `unit`, `race` and
`contracts` Go projects and the Node contract tests in `scripts/tests`. CI runs
the same selection:

```bash
npx testkit run
npx testkit run --go-project unit
npx testkit run --file internal/vm/service_test.go
```

Plain `go test ./...` also works for the unit tests.

`task ci` runs everything CI checks: formatting, `go vet`, `govulncheck`,
golangci-lint, the default Testkit run, a compile of every system test tag set,
and the build.

### System tests

The system tests in `internal/systemtest` create real VMs. They need a Linux
host with KVM, a running `kiln-vmd` you start yourself, and a token for it.
Never run them in CI.

```bash
cp .env.systemtest.example .env.systemtest
# set KILN_VM_API_URL and KILN_VM_TOKEN for your daemon
task test:system PROJECT=system
```

`PROJECT` names a Go project in `testkit.config.ts`:

| Project | What it covers |
| --- | --- |
| `system` | Every test under the `system` tag, on the `alpine` profile. |
| `network` | VM networking, isolation and gateway DNS. |
| `network-privileged`, `network-service` | Network tests that need root or restart `kiln-vmd`. |
| `service` | Daemon restart behaviour. Restarts `kiln-vmd`. |
| `distro` | `ubuntu`, `kali` and `ubuntu-docker` profiles. Downloads large images. |
| `capability` | The `ubuntu-docker` Docker capability check. |
| `windows` | A full `windows11` install. Set `KILN_VM_WINDOWS_ISO` to your media. |

`task typecheck:system` compiles every tag set without running anything.

### Nix checks

```bash
task ci:nix
```

`task ci:nix` runs the NixOS module VM test and builds the flake's packages.
The module test boots a NixOS VM, so it takes several minutes.

## Package

```bash
task package:deb
```

`task package:deb` writes a Debian package to `dist/deb/`.
[Release architecture](release-architecture.md) covers how releases build
packages and profile images for each architecture.
