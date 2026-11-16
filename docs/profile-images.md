# Profile images

A profile image is the root filesystem a new VM starts from. kiln-vm caches one
image per profile, profile version and disk size, and copies it into each new
VM. Deleting a VM leaves the cached image in place.

Profile images are a cache that kiln-vm manages. To keep a disk of your own and
create VMs from it, save or build a [VM image](vms.md#save-and-build-vm-images)
instead.

## Where images come from

`KILN_VM_PROFILE_IMAGE_MODE` picks the source:

- `pull`: download a prebuilt image from the trusted catalog. If the catalog
  has no matching image, or the download fails verification, the create fails.
- `local`: build the image on the host from the profile's Dockerfile. This
  needs Docker.
- `auto`: pull when the catalog has a matching image. Build locally when it has
  none, or when the pull fails. kiln-vm logs a warning when it falls back.

The Debian package ships a catalog at `/usr/share/kiln-vm/profile-images.json`
and sets `auto`. The NixOS module defaults to `local`, because Nix builds do not
include the catalog. Set `services.kiln-vmd.profileImageMode` and
`services.kiln-vmd.profileImageCatalog` to change that.

The catalog covers `alpine`, `debian`, `ubuntu` and `ubuntu-docker`. `kali` is
not in it, so a `kali` VM always builds locally and needs Docker on the host. `windows11` comes from `kiln-vm profiles prepare`; see
[VMs](vms.md#run-windows-11).

## How kiln-vm checks a pulled image

Each catalog entry pins the artifact URL, the SHA-256 of the compressed and the
decompressed root filesystem, the profile version, the architecture and a
runtime fingerprint. `kiln-vmd` downloads the artifact, checks both digests,
grows the sparse filesystem if the VM asked for a larger disk, and imports it.

The runtime fingerprint covers what the guest needs to work with this
`kiln-vmd`: the guest agent protocol, the exec protocol, the profile version,
the boot mode and the profile recipe. It does not include the kiln-vm version,
so Debian and Nix builds of the same release can use the same artifacts.

Profile artifacts carry a statically linked `kiln-vm-agent`, so one agent binary
boots on both glibc and musl guests. `kiln-vm-profile-artifact` rejects a
dynamically linked agent.

To pull from a mirror that needs authentication, set
`KILN_VM_PROFILE_IMAGE_TOKEN_FILE` to a file that holds the bearer token.
`KILN_VM_PROFILE_IMAGE_TOKEN` also works, but puts the token in the
environment.

## Manage the cache

```bash
kiln-vm images list
kiln-vm images ensure ubuntu --disk 16G
kiln-vm images refresh ubuntu --disk 16G
kiln-vm images delete <cache-id>
kiln-vm images delete-profile kali
```

`kiln-vm create` runs `images ensure` for you. Run it yourself to download or
build an image before you need it. `images refresh` rebuilds or re-pulls an
image after a profile change.

When cached images exceed `KILN_VM_PROFILE_IMAGE_CACHE_MAX_MB`, unused ones
become reclaimable, and `kiln-vm cleanup` removes them.

## Guest kernel

The Debian package installs the Firecracker guest kernel and
`/usr/share/kiln-vm/guest-kernel.json`. The JSON records the kernel's
architecture, source, config changes and SHA-256. `kiln-vm doctor` checks the
kernel at `KILN_VM_KERNEL_PATH` against it, so a stale or wrong-architecture
kernel shows up before a VM fails to boot.

## Release artifacts

Each release builds the catalog's profile images, writes a catalog that points
at the release's assets, checks the catalog against the built artifacts, and
embeds it in the Debian package. [Release architecture](release-architecture.md)
covers the jobs. Rootfs artifacts never go in git.
