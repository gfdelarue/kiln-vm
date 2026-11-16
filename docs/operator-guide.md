# Operator guide

This guide covers running `kiln-vmd` on a host: configuration, upgrades,
storage, networking and health checks. [VMs](vms.md) covers using VMs once the
daemon runs.

## Check the host

Run the doctor after you install or upgrade:

```bash
kiln-vm doctor
kiln-vm doctor --json
kiln-vm doctor --profile windows11 --media /path/to/Win11.iso
```

The doctor checks the CPU architecture, KVM, the host tools kiln-vm needs,
Firecracker, the guest kernel's metadata and digest, the profile catalog, the
resource limits, free host capacity, and whether the daemon and CLI versions
match. Each warning and failure comes with a fix.

Some checks read `/etc/kiln-vm/kiln-vmd.env`. If your user cannot read that
file, the doctor says its result is not authoritative and reports those
checks as warnings. Run `sudo kiln-vm doctor` for the full result.

## Configure the daemon

`kiln-vmd` reads its configuration from environment variables. The Debian
package keeps them in `/etc/kiln-vm/kiln-vmd.env`. The NixOS module sets them
from `services.kiln-vmd.*` options, and `services.kiln-vmd.environmentFile`
adds a file of extra variables.

| Variable | Default | Purpose |
| --- | --- | --- |
| `KILN_VM_LISTEN_ADDR` | `127.0.0.1:5456` | API listener when TLS is off. |
| `KILN_VM_ALLOW_INSECURE_REMOTE_HTTP` | `false` | Server opt-in for a non-loopback plaintext listener. |
| `KILN_VM_TLS_ENABLED` | `false` | Serve the API over HTTPS with ACME. See [Management transport and TLS](tls-setup.md). |
| `KILN_VM_DATA_DIR` | `/var/lib/kiln-vm` | Database, images, VM disks and network state. |
| `KILN_VM_BRIDGE_CIDR` | `10.231.0.0/16` | Range kiln-vm splits into one `/30` per Firecracker VM. |
| `KILN_VM_MAX_RUNNING_VMS` | `8` | Running VMs the host admits. |
| `KILN_VM_MAX_TOTAL_VCPUS` | `8` | CPU allocation the host admits across running VMs. |
| `KILN_VM_MAX_TOTAL_MEMORY_MB` | `8192` | Memory the host admits across running VMs. |
| `KILN_VM_POOLED_CPU_LIMIT` | `0` | CPUs shared by `--cpu-kind pooled` VMs. 0 turns pooling off. |
| `KILN_VM_MAX_VM_DISK_MB` | `524288` | Largest root disk a VM may request. |
| `KILN_VM_PROFILE_IMAGE_MODE` | `auto` | `auto`, `pull` or `local`. See [Profile images](profile-images.md). |
| `KILN_VM_PROFILE_IMAGE_CACHE_MAX_MB` | `8192` | Cached profile images above this size become reclaimable. |
| `KILN_VM_IMAGE_BUILD_DIR` | `KILN_VM_DATA_DIR/build` | Scratch space for local image builds. |
| `KILN_VM_IMAGE_MIN_FREE_MB` | `2048` | Free space an image build needs before it starts. |
| `KILN_VM_GUEST_DNS_SERVERS` | unset | Give guests these DNS servers instead of the gateway proxy. |
| `KILN_VM_LIBVIRT_CONNECT_URI` | `qemu:///system` | libvirt connection for Windows VMs. |
| `KILN_VM_LIBVIRT_NETWORK` | `kiln-vm` | libvirt network for Windows VMs. kiln-vmd creates and starts `kiln-vm` itself. Any other network must already exist. |
| `KILN_VM_LIBVIRT_CIDR` | `10.232.0.0/24` | Range for the `kiln-vm` libvirt network. Must not overlap `KILN_VM_BRIDGE_CIDR`. |
| `KILN_VM_WINDOWS_PROFILE_DIR` | `KILN_VM_DATA_DIR/windows-profiles` | Where prepared Windows images live. |
| `KILN_VM_LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error`. |

The Debian package ships a catalog of prebuilt images and keeps `auto`, so it
pulls the cataloged profiles and builds the rest locally with Docker.

`kiln-vmd` validates its configuration at start. An invalid configuration exits
with status 78, and the packaged unit sets `RestartPreventExitStatus=78`, so a
bad setting does not cause a restart loop. Read the reason in the journal:

```bash
sudo systemctl status --no-pager kiln-vmd.service
sudo journalctl -u kiln-vmd.service -n 100 --no-pager
```

After you edit `/etc/kiln-vm/kiln-vmd.env`, restart the service:

```bash
sudo systemctl restart kiln-vmd.service
```

## Pick a management transport

`kiln-vmd` serves its API in one of three modes:

1. Loopback HTTP, the default, for a CLI on the same host.
2. Remote HTTPS with an ACME certificate, for clients on other machines.
3. Remote plaintext HTTP, only when the server sets
   `KILN_VM_ALLOW_INSECURE_REMOTE_HTTP=true` and each client opts in with
   `kiln-vm login --allow-insecure-http` or
   `KILN_VM_ALLOW_INSECURE_HTTP=true`.

[Management transport and TLS](tls-setup.md) covers each mode.

## Configure the CLI

`kiln-vm login` saves the API URL and token. These environment variables
override the saved values:

- `KILN_VM_API_URL`: the API URL.
- `KILN_VM_TOKEN`: the API token.
- `KILN_VM_ALLOW_INSECURE_HTTP`: `true` to send the token to a remote
  plaintext URL.
- `KILN_VM_HTTP_TIMEOUT`: request timeout, for example `10m` when a first
  profile image build is slow.

On first start, `kiln-vmd` writes a bootstrap token to
`KILN_VM_DATA_DIR/bootstrap-token`, readable only by root. Set
`KILN_VM_BOOTSTRAP_TOKEN_FILE` to write it somewhere else. Log in with it, then
create named tokens and delete the bootstrap token. `--expires-in` takes
seconds, and 0 means the token never expires:

```bash
kiln-vm tokens create --name laptop --expires-in 2592000
kiln-vm tokens list
kiln-vm tokens delete <id>
```

## Upgrade the Debian package

The package keeps its defaults in `/usr/share/kiln-vm/kiln-vmd.env.defaults`
and your configuration in `/etc/kiln-vm/kiln-vmd.env`. On install, the package
copies the defaults into a new configuration file. On upgrade, it adds any
default variable your file does not set and leaves every existing line alone,
including the listener and the insecure-HTTP opt-in. It never adds
`KILN_VM_ALLOW_INSECURE_REMOTE_HTTP=true`.

On a host running systemd, the package starts `kiln-vmd` on first install and
restarts it on upgrade through Debian's service helpers, which honour
`policy-rc.d`. In a chroot, it does not start the service. A failed start or
restart fails the package configuration step.

Stopping `kiln-vmd` stops every running Linux VM. Windows VMs keep running
under libvirt, and the new daemon adopts them. After the upgrade, the daemon
starts the Linux VMs again as [power modes](vms.md#power-modes) describe.

## Manage storage

VM disks are sparse. A 64 GiB VM uses only the blocks the guest has written,
and kiln-vm keeps the holes when it copies a cached profile image into a new
VM.

Local image builds use `KILN_VM_IMAGE_BUILD_DIR` for scratch space. Point it at
a larger filesystem if the data directory is small:

```bash
KILN_VM_IMAGE_BUILD_DIR=/mnt/large/kiln-vm-build
KILN_VM_IMAGE_MIN_FREE_MB=4096
```

Finished images still live under `KILN_VM_DATA_DIR/images`.

See what kiln-vm stores and what it can reclaim:

```bash
kiln-vm disk usage
```

The report lists cached profile images, VM disks, image build scratch space and
kiln-vm temp files. It warns when a kiln-vm filesystem has less than 8 GiB
free or is at least 85% full, or when reclaimable data reaches 32 GiB.

Remove what kiln-vm can prove it no longer needs:

```bash
kiln-vm cleanup --dry-run
kiln-vm cleanup
```

Cleanup removes unreferenced image and VM directories, superseded profile
images, unused profile images over `KILN_VM_PROFILE_IMAGE_CACHE_MAX_MB`, and
kiln-vm temp files older than an hour. It keeps anything a VM uses and any
mounted root filesystem. It does not touch Docker or anything outside the
kiln-vm data directory.

## Watch the host

```bash
kiln-vm node health
kiln-vm node capacity
kiln-vm metrics
kiln-vm metrics <vm>
```

`kiln-vm metrics` without a VM shows a node summary. These are live snapshots;
kiln-vm keeps no history. The API serves the same data at `GET /node/health`,
`GET /node/capacity`, `GET /metrics/summary` and `GET /vms/{ref}/metrics`.

`kiln-vm node capacity` shows each limit, what running VMs use, and what is
left. kiln-vm refuses to start a VM that would exceed a limit.

## Networking

Each Firecracker VM gets a TAP device named `kv<N>` and a `/30` from
`KILN_VM_BRIDGE_CIDR`. kiln-vm NATs guest traffic out of the host's default
route. Each TAP carries the interface alias `kiln-vm-public-network:v1:...`,
and kiln-vm refuses to modify or delete a `kv<N>` link without that alias.

`KILN_VM_BRIDGE_CIDR` must be an aligned IPv4 `/29` or larger. Choose a range
that does not overlap your LAN, VPN or container networks.

### Gateway DNS

By default, guests use a DNS proxy that `kiln-vmd` runs on each VM's gateway
address. The proxy forwards to the resolvers in the host's `resolv.conf`, read
once when `kiln-vmd` starts. A local stub such as systemd-resolved at
`127.0.0.53` works, and later changes behind the stub reach guests. To give
guests fixed resolvers instead, set `KILN_VM_GUEST_DNS_SERVERS`.

Every resolver must sit outside `KILN_VM_BRIDGE_CIDR`. If one does not,
`kiln-vmd` refuses to start rather than route a query into a VM.

kiln-vm opens port 53 only from each guest to its own gateway. It installs
these rules in an iptables chain named `KVD_<id>`, hooked from `INPUT` with the
comment `kiln-vm-gateway-dns:v2:<owner>:input`. The owner ID lives in
`KILN_VM_DATA_DIR/network/gateway-dns-firewall.owner`. kiln-vm changes only
rules that carry its owner ID and fails closed on anything else.

`kiln-vm metrics` reports the proxy's state for each VM. `cleanup_pending`
means kiln-vm still has network state to remove and will retry.

### Remove leftover network state

Stop or delete VMs with `kiln-vm` before you uninstall, and wait for
`cleanup_pending` to clear. If `kiln-vmd` can no longer run, list what it left:

```bash
ip -o link show | grep 'alias kiln-vm-public-network'
sudo iptables -w 5 -S INPUT | grep kiln-vm-gateway-dns
sudo iptables -w 5 -S | grep -E 'KVD_|KVISO-'
sudo iptables -w 5 -t nat -S POSTROUTING | grep kiln-vm
```

Delete only rules and links that carry kiln-vm's comments and aliases. Never
flush a whole chain you did not create.

If you ran Windows VMs, delete their libvirt domains and the `kiln-vm` network:

```bash
sudo virsh list --all --name | grep '^kiln-vm-'
sudo virsh net-destroy kiln-vm
sudo virsh net-undefine kiln-vm
```
