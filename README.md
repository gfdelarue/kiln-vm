# kiln-vm

kiln-vm runs persistent microVMs on a Linux host. Each VM keeps its root disk
across stops, starts and daemon restarts, so you can install tools, keep state
and come back to it later.

Linux VMs run on [Firecracker](https://firecracker-microvm.github.io/) and boot
in seconds. Windows 11 VMs run on libvirt and QEMU from installation media you
supply.

```bash
kiln-vm create dev --profile debian
kiln-vm shell dev
kiln-vm exec dev 'apt-get update && apt-get install -y ripgrep'
kiln-vm stop dev
```

## What you get

- A daemon, `kiln-vmd`, that owns VM lifecycle, networking and storage on one
  host and serves an authenticated HTTP API.
- A CLI, `kiln-vm`, for that API.
- A guest agent, `kiln-vm-agent`, that runs inside Linux VMs. It handles exec,
  shell, file uploads, port forwarding, shutdown and readiness over vsock, so
  guests need no SSH server.
- Ready-made profiles: `alpine`, `debian`, `ubuntu`, `kali`, `ubuntu-docker`
  and `windows11`.
- Power modes that start VMs with the daemon, wake them when you use them, and
  stop them when they sit idle.
- An `isolated-web` network policy that limits a Linux guest to public HTTP,
  HTTPS and gateway DNS.

## Install

kiln-vm needs an x86_64 Linux host with KVM (`/dev/kvm`).

### Debian and Ubuntu

The `.deb` package installs `kiln-vmd`, `kiln-vm`, the guest agent,
Firecracker, a guest kernel and a systemd unit. It starts `kiln-vmd` on
loopback:

```bash
KILN_VM_LISTEN_ADDR=127.0.0.1:5456
KILN_VM_ALLOW_INSECURE_REMOTE_HTTP=false
```

Install it, then read the bootstrap token and log in:

```bash
sudo apt install ./kiln-vm_<version>_amd64.deb
sudo cat /var/lib/kiln-vm/bootstrap-token
kiln-vm login --api-url http://127.0.0.1:5456 --token <token>
kiln-vm doctor
```

### NixOS

The flake exports a NixOS module:

```nix
{
  inputs.kiln-vm.url = "github:gfdelarue/kiln-vm";

  outputs = { nixpkgs, kiln-vm, ... }: {
    nixosConfigurations.host = nixpkgs.lib.nixosSystem {
      system = "x86_64-linux";
      modules = [
        kiln-vm.nixosModules.kiln-vmd
        { services.kiln-vmd.enable = true; }
      ];
    };
  };
}
```

### From source

```bash
nix develop
task build
```

[Development](docs/development.md) covers the build, test and package tasks.

## Use it

Create a VM from a profile. Omitted sizes come from the profile:

```bash
kiln-vm profiles
kiln-vm create box --profile ubuntu --disk 16G --memory-mb 4096 --cpu 2
```

Open a shell, run a command, or pipe a file in:

```bash
kiln-vm shell box
kiln-vm exec box 'uname -a'
tar czf - ./src | kiln-vm exec box 'mkdir -p /src && tar xzf - -C /src'
```

Reach a service inside the VM from your machine:

```bash
kiln-vm forward box 8080
```

Let the VM sleep when idle and wake when you use it:

```bash
kiln-vm power box --mode on_demand --idle 15m
```

Stop, start and delete:

```bash
kiln-vm stop box
kiln-vm start box
kiln-vm delete box
```

Save a stopped VM as an image, or build one from a Dockerfile, and create VMs
from it:

```bash
kiln-vm vm-images save box box-image
kiln-vm vm-images build tools -f Dockerfile --context .
kiln-vm create box2 --image box-image --label owner=me
```

[VMs](docs/vms.md) is the full reference for profiles, power modes, networking,
labels, VM images and guest access.

## Documentation

- [VMs](docs/vms.md): lifecycle, profiles, labels, VM images, power, networking
  and guest access.
- [Operator guide](docs/operator-guide.md): configuration, storage, metrics,
  upgrades and troubleshooting.
- [Management transport and TLS](docs/tls-setup.md): local, HTTPS and
  plaintext remote access.
- [Profile images](docs/profile-images.md): how base images are pulled,
  built and cached.
- [Development](docs/development.md): building, testing and packaging.
- [Release architecture](docs/release-architecture.md): how release
  artifacts are built.

## License

kiln-vm is licensed under the [Apache License 2.0](LICENSE).
