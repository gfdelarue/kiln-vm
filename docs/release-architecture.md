# Release architecture

kiln-vm release jobs must treat target architecture as explicit workflow data. A
runner's CPU architecture is only a routing capability; it is not allowed to
silently define the target artifact unless the job matrix says so and the output
is verified against that target.

Every job runs on a GitHub-hosted runner. A job must not build an `amd64`
artifact on an `arm64` runner, or the reverse, unless a cross-build path is
deliberately implemented and the release gates prove the output matches the
declared target.

## Jobs

| Job | Target shape | Current runner contract | Notes |
| --- | --- | --- | --- |
| `validate` | Architecture-neutral | `ubuntu-latest` | Verifies version, package contracts, artifact naming/fingerprints, and provider release gates. It must not produce target-shaped binaries. |
| `build` | Cross-build-safe Go binaries | `ubuntu-latest` with explicit `GOOS`/`GOARCH` matrix | Builds `kiln-vm` for `linux` and `darwin`, and `kiln-vmd` for `linux`, each on `amd64` and `arm64`. It does not copy a host runtime into a target package. |
| `release-notices` | Architecture-neutral | `ubuntu-latest` | Packages the pinned third-party notices that accompany raw release binaries. It must not build or reshape target artifacts. |
| `guest-kernel` | Native-routed target artifact | `amd64` on `ubuntu-latest`, `arm64` on `ubuntu-24.04-arm` | Produces the Firecracker guest kernel and metadata for one Debian architecture. Keep native routing unless a cross-kernel path is proven and guarded by the same metadata verification. |
| `profile-images` | Native-routed target artifact | `amd64` on `ubuntu-latest`, `arm64` on `ubuntu-24.04-arm` | Produces profile runtime tooling and VM profile image metadata/artifacts for one Debian architecture. Cache hits may skip building, but the downloaded runtime/profile artifacts must verify against the target plan before use. |
| `profile-catalog` | Architecture-neutral aggregation | `ubuntu-latest` | Merges already-produced per-architecture catalogs. It must not create target-shaped artifacts. |
| `deb` | Native-routed package assembly | `amd64` on `ubuntu-latest`, `arm64` on `ubuntu-24.04-arm` | Assembles one Debian package from already-verified target guest-kernel, profile runtime, and profile catalog artifacts, then verifies embedded architecture metadata. |
| `release` | Architecture-neutral publishing | `ubuntu-latest` | Publishes already-built binaries, debs, the merged catalog, and accompanying notices. It must not build or reshape target artifacts. |

## Artifact rules

- Matrix `arch` values are Debian architecture names: `amd64` and `arm64`.
- Architecture mapping lives in `scripts/lib/architecture.mjs`; workflow YAML
  should consume those scripts rather than duplicate mapping rules.
- Guest kernels are versioned release assets keyed by kernel version, build
  recipe, target architecture, architecture mapping, and required config deltas.
- Profile runtime tooling is a versioned release asset keyed by Go version,
  target architecture, runtime source/import graph, build flags, and profile
  artifact recipe inputs. Its manifest and cache include the pinned third-party
  notices required by the bundled runtime tooling; cache verification checks their names,
  module versions, checksums, sizes, install paths, and exact source bytes.
- VM profile images are versioned release assets keyed by profile recipe,
  runtime compatibility, rootfs shape, target architecture, and artifact recipe.
- Debian packages must embed target-matching guest-kernel metadata and profile
  catalogs. The release workflow must verify those bytes after package
  assembly.
- Raw cross-platform `kiln-vm` and `kiln-vmd` release binaries must be accompanied by the pinned
  `miekg/dns` and `golang.org/x/net` notices as architecture-neutral release
  assets.

## Gates

`scripts/tests/release-architecture-contract.test.mjs` is the release-contract
gate. It checks architecture mappings, artifact naming/fingerprints, cached
runtime/profile workflow structure, and this document's job-boundary markers.
It is part of the default Testkit run, so CI runs it on every pull request and
push to `main`, and the release workflow runs it in `validate` before any
target artifacts are built.
