# VMs

A kiln-vm VM is a named machine with its own root disk. The disk survives
`kiln-vm stop`, `kiln-vm start` and `kiln-vmd` restarts. Linux profiles run on
Firecracker. The `windows11` profile runs on libvirt and QEMU.

## Profiles

`kiln-vm profiles` lists the profiles the daemon offers. Sizes you omit on
`kiln-vm create` come from the profile:

| Profile | Runtime | Disk | vCPUs | Memory | Notes |
| --- | --- | --- | --- | --- | --- |
| `alpine` | Firecracker | 1 GiB | 1 | 512 MiB | Small VM for smoke tests and light tooling. |
| `debian` | Firecracker | 8 GiB | 2 | 2 GiB | Debian stable. |
| `ubuntu` | Firecracker | 8 GiB | 2 | 2 GiB | Ubuntu LTS. |
| `kali` | Firecracker | 32 GiB | 2 | 4 GiB | Kali rolling, headless. |
| `ubuntu-docker` | Firecracker | 64 GiB | 4 | 8 GiB | Ubuntu with Docker, rsync, SSH, PostgreSQL client tools and build essentials. Declares the `docker` capability. |
| `windows11` | libvirt-qemu | 64 GiB | 2 | 4 GiB | Needs `kiln-vm profiles prepare windows11` first. |

## Create a VM

```bash
kiln-vm create dev --profile debian
kiln-vm create app --profile ubuntu-docker --disk 128G --memory-mb 12288 --cpu 6 --vcpus 6
```

`--disk` takes MiB, or a size with an `M` or `G` suffix. `--cpu` sets the VM's
CPU-time quota, for example `2`, `0.5` or `500m`. The guest's vCPU count comes
from the profile unless you pass `--vcpus`. To see the resolved plan without
creating anything, add `--dry-run`.

The first VM from a profile downloads or builds the profile's base image, which
can take a few minutes. Later VMs copy the cached image. To pay that cost ahead
of time, run `kiln-vm images ensure <profile> --disk <size>`.
[Profile images](profile-images.md) covers the cache.

## Label a VM

Labels are key-value pairs that tools use to find their own VMs:

```bash
kiln-vm create job-42 --profile alpine --label owner=kiln-run --label job=42
kiln-vm list --label owner=kiln-run
kiln-vm list --label owner=kiln-run --label job
```

A `--label` filter on `kiln-vm list` is either `key=value` or `key`. A bare key
matches any VM that has that label. Repeated filters must all match.

A key is 1 to 63 characters of lowercase letters, digits, `.`, `_`, `/` and
`-`. It starts and ends with a letter or a digit. A value is at most 255 bytes.
A VM has at most 32 labels. kiln-vm stores labels in its database, so they
survive restarts. You set labels when you create the VM and cannot change them
later.

## Save and build VM images

A VM image is a root disk you can create new VMs from. It is separate from the
cached [profile images](profile-images.md). You can save one from a stopped VM
or build one from a Dockerfile:

```bash
kiln-vm stop dev
kiln-vm vm-images save dev dev-snapshot
kiln-vm vm-images build tools -f Dockerfile --context . --label owner=kiln-run
kiln-vm vm-images list --label owner=kiln-run
kiln-vm vm-images get tools
kiln-vm vm-images delete dev-snapshot
```

`vm-images save` copies the VM's root disk. The VM must be stopped. The image
keeps the VM's profile and disk size. Only Firecracker VMs can be saved. A
saved image holds everything on the VM's disk, including its SSH host keys and
any credentials you put there. VMs created from it share those files.

`vm-images build` runs `docker build` on the host. The context directory is
sent to `kiln-vmd` as a tar archive of at most 96 MiB. `--profile` picks the
Firecracker Linux profile that VMs from the image use. It defaults to `debian`.
`--disk` defaults to the profile's disk size. kiln-vm adds its guest agent and
init to the built filesystem, as it does for profile images. A build stops
when the client disconnects or after `KILN_VM_IMAGE_BUILD_TIMEOUT`, which
defaults to 30 minutes. The image's `resolved` field records the repository
digest that each `FROM` line resolved to. kiln-vm reads the digests from the
BuildKit output, or from the host's Docker image cache with the classic
builder. A `FROM` line that uses a build argument, such as `FROM ${BASE}`,
gets no entry.

Create a VM from an image with `--image` instead of `--profile`:

```bash
kiln-vm create dev2 --image dev-snapshot
kiln-vm create big --image tools --disk 64G
```

The VM uses the image's profile. Its disk defaults to the image's size and
cannot be smaller. `kiln-vm list --json` and `kiln-vm get --json` show the
image a VM came from.

`kiln-vm vm-images delete` always removes the image from the list. Each VM has
its own copy of the disk, so VMs created from the image keep working. kiln-vm
deletes the image's disk file when the last of those VMs is deleted.

## Use a VM

Run a command. A single argument runs as a shell command line. Several
arguments are quoted and joined, with POSIX shell quoting on Linux and as a
PowerShell `&` call on Windows:

```bash
kiln-vm exec dev 'uname -a'
kiln-vm exec dev ls -la /root
```

Everything after the VM name belongs to the guest command, flags included:

```bash
kiln-vm exec dev sh -c 'exit 3'
```

When standard input is a pipe or a file, `kiln-vm exec` streams it to the guest
command and closes the guest's stdin at EOF:

```bash
tar czf - ./src | kiln-vm exec dev 'mkdir -p /src && tar xzf - -C /src'
```

For an interactive terminal, use `kiln-vm shell dev`.

To copy a file in, use `kiln-vm upload`. The guest file gets the local file's
permissions unless you pass `--mode`. Windows guests ignore the mode. kiln-vm
creates missing parent directories:

```bash
kiln-vm upload dev ./build/app /usr/local/bin/app
kiln-vm upload dev ./config.toml /etc/app/config.toml --mode 600
```

Linux guests run `kiln-vm-agent`, which handles exec, shell, file uploads,
forwarding and shutdown over vsock. Guests need no SSH server and no open port.

## Reach a service in the VM

```bash
kiln-vm forward dev 3000
kiln-vm forward dev 3000 --listen 127.0.0.1:8080
```

`kiln-vm forward` listens on `--listen`, prints the bound address, and serves
until you press Ctrl-C. The default, `127.0.0.1:0`, is a free loopback port.
Each local connection opens a WebSocket to the kiln-vm API, and the guest agent
connects it to `127.0.0.1:<port>` in the guest. If nothing listens there, the
agent tries `[::1]:<port>`.

A forward needs no ingress rule, so it works for `isolated-web` VMs. An open
forward connection counts as use: it wakes the VM when the power policy allows
it and keeps the VM from going idle.

## Power modes

```bash
kiln-vm create app --profile ubuntu-docker --power on_demand --idle 10m
kiln-vm power app
kiln-vm power app --mode warm --idle 15m
kiln-vm power app --mode manual
```

| Mode | Starts with `kiln-vmd` | Wakes on use | Stops when idle |
| --- | --- | --- | --- |
| `manual` | If it was running | No | No |
| `always` | Yes | Yes | No |
| `on_demand` | No | Yes | Yes |
| `warm` | Yes | Yes | Yes |

Use means `exec`, `shell`, `upload` and `forward` connections. A `manual` VM
must already be running, or the command fails with "does not wake on use".

`--idle` applies to `on_demand` and `warm`, and defaults to 10 minutes.
`kiln-vmd` checks for idle VMs every 30 seconds.

When `kiln-vmd` stops, it stops every running Linux VM. Windows VMs run under
libvirt and keep running. When `kiln-vmd` starts, it adopts the Windows VMs
that are still running. It then starts the `always` and `warm` VMs, and every
`manual` VM that was running when `kiln-vmd` stopped or crashed. A `manual` VM
you stopped yourself stays stopped.

## Stop, start and delete

```bash
kiln-vm stop dev
kiln-vm start dev
kiln-vm delete dev
```

`kiln-vm stop` asks the guest to sync and power off. If the guest does not stop
in time, kiln-vm ends the VM process. `kiln-vm delete` removes the VM and its
root disk. It keeps the cached profile image and any VM image the VM came
from.

## Restrict network access

```bash
kiln-vm create sandbox --profile debian --network-policy isolated-web
```

An `isolated-web` VM can reach public IPv4 addresses on TCP ports 80 and 443
and the DNS server on its gateway. kiln-vm drops everything else: host
services, private and reserved ranges, unsolicited inbound traffic, spoofed
source addresses and all IPv6. You can still reach services in
the VM with `kiln-vm forward`. `isolated-web` is available on Firecracker
profiles only.

## Files kiln-vm writes in the guest

On create and on every start of a Firecracker VM, kiln-vm writes these files in
the VM's root disk before it boots:

- `/etc/hostname`: replaced with the VM name in lower case, with spaces and
  underscores turned into hyphens and other characters dropped.
- `/etc/hosts`: kiln-vm adds missing `localhost` and hostname entries and keeps
  the rest. It leaves a file larger than 1 MiB unchanged.
- `/etc/resolv.conf`: replaced with the gateway address when the gateway DNS
  proxy is on, or with `KILN_VM_GUEST_DNS_SERVERS` when that is set.

kiln-vm writes no other guest file. It writes nothing in a `windows11` VM's
disk at start.

## Verify capabilities

A profile can declare capabilities that kiln-vm checks inside the guest. Until
a declared capability passes, kiln-vm refuses `exec`, `shell` and file uploads
on that VM. `kiln-vm get` shows each capability's state.

```bash
kiln-vm verify app --capability docker
kiln-vm verify app --capability docker --force
```

The `docker` capability on `ubuntu-docker` checks that the Docker daemon
starts, that the iptables raw and nat tables exist, and that a container on a
user-defined network can resolve DNS and publish a port. It runs a small local
image with `--pull=never`, so it does not need Docker Hub.

## Run Windows 11

kiln-vm does not ship Windows installation media or license keys. Supply your
own ISO, and optionally a product key:

```bash
kiln-vm doctor --profile windows11 --media /path/to/Win11.iso
kiln-vm profiles prepare windows11 --media /path/to/Win11.iso
kiln-vm create win --profile windows11
kiln-vm exec win '$PSVersionTable.PSVersion.ToString()'
kiln-vm shell win
```

`kiln-vm profiles prepare` runs an unattended install and caches the result
under `KILN_VM_WINDOWS_PROFILE_DIR`. It takes up to 90 minutes; change the limit
with `--timeout-seconds`. New `windows11` VMs copy the cached image. To rebuild
it from new media or settings, add `--refresh`. To pass a key, use
`--product-key-file <path>` rather than `--product-key`, so the key stays out of
your shell history.

The prepared image is Windows 11 with OpenSSH Server, the virtio-win drivers
and the QEMU guest agent. Its disk and network use virtio. kiln-vmd downloads a pinned virtio-win
release and checks its SHA-256 before the install. To use your own copy, pass
`--virtio-media <path>`.

kiln-vm reaches Windows guests over SSH. `exec`, `shell`, `upload` and
`forward` work as they do on Linux. `exec` runs its command in Windows PowerShell 5.1 with UTF-8 input and
output. The exit code is the one the command passes to `exit`. Without one, it
is 0 when the last statement succeeded, the last native command's exit code
when that failed, and 1 otherwise. `forward` tunnels through SSH to the guest's
loopback interface, so the Windows firewall needs no rule for the port.

On a VM's first boot, kiln-vmd gives it its own identity through the QEMU
guest agent. The guest authorizes only that VM's SSH key, generates new SSH
host keys, sets a random password on the `kiln-vm` account and renames itself.
Windows then restarts once. The computer name is up to 10 characters of the VM
name and the first four characters of the VM ID, for example `win-1e1a`.
kiln-vmd pins the host key that the guest reports over the agent channel and
refuses SSH to any other key. `kiln-vm create` and `kiln-vm start` return once
SSH accepts the VM's key.

VMs made from one prepared image still share the Windows machine SID. Domain
members that share a SID can fail to authenticate to each other, so don't join
more than one of them to the same Active Directory domain.

`kiln-vm stop` asks the guest agent to shut Windows down and waits two minutes
before it forces the VM off. The `isolated-web` policy is Linux-only.

Windows VMs attach to a libvirt network named `kiln-vm` on `10.232.0.0/24`.
kiln-vmd creates it on first use. Change the range with
`KILN_VM_LIBVIRT_CIDR`.

The host needs QEMU, OVMF, swtpm, virt-install, mtools and libvirt. The Debian
package recommends them. `kiln-vm doctor --profile windows11` checks them.

## Share a CPU pool

By default each VM gets its own CPU quota from `--cpu`. To let several
Firecracker VMs share a fixed budget instead, set `KILN_VM_POOLED_CPU_LIMIT` to
the number of host CPUs in the pool and create VMs with `--cpu-kind pooled`:

```bash
kiln-vm create worker --profile ubuntu-docker --cpu-kind pooled --cpu 8
```

A pooled VM exposes `--cpu` guest vCPUs and can use any spare capacity in the
pool. The cgroup `/sys/fs/cgroup/kiln-vm/pool` enforces the pool's quota. The
pool counts against `KILN_VM_MAX_TOTAL_VCPUS` once, whatever the number of VMs
in it. `KILN_VM_POOLED_CPU_LIMIT` defaults to 0, which turns pooling off, and
cannot exceed `KILN_VM_MAX_TOTAL_VCPUS`.

## HTTP API

The CLI wraps the HTTP API. Every route needs the bearer token. The routes for
labels and VM images are:

| Route | Body or query | Result |
| --- | --- | --- |
| `POST /vms` | `labels`, and `image` as an alternative to `profile` | The VM, with `labels` and `image` |
| `GET /vms` | `label=key=value` or `label=key`, repeatable | VMs that match every filter |
| `GET /vms/{ref}` | | The VM, with `labels` and `image` |
| `POST /vm-images` | `{name, from_vm, labels}` or `{name, dockerfile, context, profile, disk_size_mb, labels}` | `201` and the image |
| `GET /vm-images` | `label`, as for `GET /vms` | Images that match every filter |
| `GET /vm-images/{ref}` | | The image |
| `DELETE /vm-images/{ref}` | | `204` |

`context` is a base64-encoded tar archive. A VM image has the fields `id`,
`name`, `profile`, `source` (`vm` or `dockerfile`), `disk_size_mb`, `labels`,
`created_at`, and `resolved` when known. `resolved` maps each `FROM` reference
to a repository digest such as `docker.io/library/alpine@sha256:...`.
A `{ref}` is a name, an ID, or a unique ID prefix of at least 4 characters.
Responses omit `labels` and `image` when they are empty.

Invalid labels or filters, an unknown image, and a request that sets both or
neither of `from_vm` and `dockerfile` return `400`. Saving a VM that is not
stopped returns `409`. A failed `docker build` returns `500` with the code
`docker_build_failed` and the end of the build output in
`diagnostics[0].stderr_tail`.

## Limits

kiln-vm VMs are headless. GUI desktops, USB passthrough and host Wi-Fi adapters
are out of scope.
