# Installer and helper build inputs

The source installer uses `input-locks.json` and `locks/`. The helper has its
own `helper-input-locks.json` and `locks/` in `scripts/osc-kata-deploy` so each
Konflux build context contains its inputs.

The locks cover Linux amd64:

- Every external base image uses its architecture manifest digest. Those
  manifests were checked against the image configs from the successful build.
- Every installed RPM, including base packages and transitive dependencies,
  has an exact NEVRA, header SHA256, and payload SHA256. Signing-key pseudo
  packages are excluded from the inventory. There are 931 stage entries for
  the installer and 118 for the helper.
- Go 1.26.7 has the archive checksum from the
  [Go download API](https://go.dev/dl/?mode=json&include=all).
  `GOTOOLCHAIN=local` prevents an automatic toolchain download.
- Kata source must match commit `c7351e797efff8bfc6bd73da0eb1909be12e2cfe`.
  `Cargo.lock`, the runtime `go.mod`, and `go.sum` have recorded checksums.
  Go uses readonly modules. Cargo fetches with `--locked`; agent compilation
  runs offline and must leave the lockfile unchanged. The installer uses
  `--frozen`.
- Build arguments that disagree with the source or Go lock fail before source
  compilation. To change these inputs, update the recipe and lock together.

`check-input-locks.py` checks the recipe, inventory checksums, and source
arguments before either local build starts. The RPM installer first checks
whether the complete inventory already matches. Otherwise it requests the
exact package identities and checks the full inventory after installation.
Missing versions, extra dependencies, and changed package content fail the
build. The script's `--verify-only` mode checks an existing package layer
without contacting repositories.

Images built with these recipes carry the input manifest and RPM inventories.
The installer stores them under
`/opt/kata-artifacts/provenance`. The helper carries them under
`/usr/share/osc-build-inputs`. Local wrappers stage only the recipe and named
inputs under `build/_output`; old credentials and unrelated local files stay
outside the build context.

## Build and check

```bash
python3 scripts/osc-artifacts/check-input-locks.py
./scripts/osc-artifacts/build-local.sh --build-arg VERSION=1.14 \
  -t localhost/osc-kata-deploy:locked-inputs
./scripts/osc-kata-deploy/build-helper.sh --build-arg VERSION=1.14 \
  -t localhost/osc-helper:locked-inputs
```

RHEL package installation needs an accessible package source. The default
local build uses entitlement secrets for the RHEL CDN. To use a retained
mirror instead, provide a local DNF repository file and, if needed, its CA:

```bash
OSC_RPM_REPOS_FILE="$PWD/build/_output/local-rhel.repo" \
OSC_RPM_CA_FILE="$PWD/build/_output/local-rhel-ca.pem" \
./scripts/osc-artifacts/build-local.sh --build-arg VERSION=1.14 \
  -t localhost/osc-kata-deploy:locked-inputs
```

The repository file replaces the default repositories in the three RHEL
package stages. Its URLs must point to repositories that retain the locked
versions. A repository entry can set
`sslcacert=/run/osc-rpm-repos/ca.pem` to use the supplied CA. Repository and CA
files are temporary secret mounts; this mode does not supply the old
entitlement files. A missing requested repository mount fails the build.
The CentOS agent stage keeps its own repositories. Package identity and
content checks apply to both source modes.

If a compose has dropped older versions, `fetch-locked-rpms.py` can fetch the
signed Brew copies that the pinned base images do not already contain:

```bash
python3 scripts/osc-artifacts/fetch-locked-rpms.py \
  --brew-hub "$BREW_HUB" --brew-root "$BREW_ROOT" \
  --ca-file "$BUILD_CA_FILE" --output "$PWD/build/_output/locked-rpms"
```

The fetcher verifies every downloaded RPM's NEVRA, header SHA256, and payload
SHA256 before installation. It fails if Brew lacks a signed copy or returns
different package content. Create normal DNF metadata with `createrepo_c` in
that directory, then set `OSC_RPM_CACHE_DIR` to it. The local repository entry
must use `baseurl=file:///run/osc-rpm-cache/`. The wrapper copies this cache
into its private build context and mounts it read-only during package
installation through a scratch image input. Direct context mounts failed
with a permission error on the local Podman VM; the image input works
without changing SELinux settings. This does not
replace base-image package layers with old compiler images. Hashes of the
repository file and metadata participate in the build
cache key. Internal endpoints and credentials stay outside Git.

Locks do not make packages available when a repository denies access or
removes a version.
Release builds need a repository or mirror that retains the locked RPMs.
There is no fallback to a newer package or an unlocked base tag.

These recipes still fetch source, modules, crates, and packages over the
network. They are not hermetic builds and do not yet guarantee byte-identical
OCI images or tarballs; timestamp normalization remains separate work.
The existing operator Dockerfile and official `.tekton` files are outside
this installer/helper locking change.

## Updating inputs

Choose and verify the new Kata commit, SDK checksum, and amd64 base manifests.
Update the recipe defaults, `versions.yaml`, and input manifest together.
Build the intended package stages, then capture their inventories with:

```bash
podman run --rm --platform linux/amd64 --network none \
  --entrypoint rpm "$STAGE_IMAGE" -qa \
  --qf '%{NAME}-%{EPOCHNUM}:%{VERSION}-%{RELEASE}.%{ARCH} %{SHA256HEADER} [%{PAYLOADDIGEST}]\n' \
  | grep -v '^gpg-pubkey-' | LC_ALL=C sort > "$RPM_LOCK"
```

Review the package and content changes. Update each inventory's row count
and SHA256 in its input manifest. For a Kata update, regenerate the dependency
file checksums from that verified checkout. Run the local lock checks, source
and helper builds, image contracts, and live validation before changing the
deployment digests. A cached package build must be recorded as such; it does
not prove fresh repository access.
