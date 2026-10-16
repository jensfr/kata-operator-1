#!/bin/bash
# Stage only build inputs. Podman remote transports secret mounts through temporary context files.
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
python3 "$SCRIPT_DIR/check-input-locks.py"
build_secrets=()
if [ -n "${OSC_RPM_REPOS_FILE:-}" ]; then
    test -s "$OSC_RPM_REPOS_FILE" || { echo "FATAL: OSC_RPM_REPOS_FILE is missing or empty" >&2; exit 1; }
    repo_sha=$(python3 -c 'import hashlib,sys; print(hashlib.sha256(open(sys.argv[1],"rb").read()).hexdigest())' "$OSC_RPM_REPOS_FILE")
    build_secrets+=(--secret "id=rpm-repositories,src=$OSC_RPM_REPOS_FILE" --build-arg OSC_RPM_REPOS_REQUIRED=1 --build-arg "OSC_RPM_REPOS_SHA256=$repo_sha")
    if [ -n "${OSC_RPM_CA_FILE:-}" ]; then
        test -s "$OSC_RPM_CA_FILE" || { echo "FATAL: OSC_RPM_CA_FILE is missing or empty" >&2; exit 1; }
        build_secrets+=(--secret "id=rpm-ca,src=$OSC_RPM_CA_FILE")
    fi
else
    if [ -n "${OSC_RPM_CA_FILE:-}" ]; then
        echo "FATAL: OSC_RPM_CA_FILE requires OSC_RPM_REPOS_FILE" >&2
        exit 1
    fi
    build_secrets+=(--secret "id=entitlement-key,src=${ENTITLEMENT_KEY:-$HOME/.osc-build-secrets/entitlement-key.pem}")
    build_secrets+=(--secret "id=entitlement,src=${ENTITLEMENT_CERT:-$HOME/.osc-build-secrets/entitlement.pem}")
fi
umask 077
mkdir -p "$REPO_ROOT/build/_output"
context="$(mktemp -d "$REPO_ROOT/build/_output/kata-build.XXXXXX")"
cleanup_secrets() {
    find "$context" -maxdepth 1 -type f -name 'podman-build-secret*' -delete
}
trap cleanup_secrets EXIT
cp "$SCRIPT_DIR/Containerfile" "$SCRIPT_DIR/shim-components.json" "$SCRIPT_DIR/versions.yaml" "$SCRIPT_DIR/input-locks.json" "$context/"
cp "$SCRIPT_DIR/.containerignore" "$SCRIPT_DIR/.dockerignore" "$context/"
# Podman remote creates secret transport files after this context is staged.
# Permit them only here; old files in the source directory stay excluded.
for ignore in "$context/.containerignore" "$context/.dockerignore"; do
    printf '\n!podman-build-secret*\n' >> "$ignore"
done
cp -r "$SCRIPT_DIR/locks" "$context/locks"
mkdir -p "$context/rpm-cache"
cp "$SCRIPT_DIR/rpm-cache/.gitignore" "$context/rpm-cache/"
if [ -n "${OSC_RPM_CACHE_DIR:-}" ]; then
    test -n "${OSC_RPM_REPOS_FILE:-}" || { echo "FATAL: OSC_RPM_CACHE_DIR requires OSC_RPM_REPOS_FILE" >&2; exit 1; }
    test -s "$OSC_RPM_CACHE_DIR/repodata/repomd.xml" || { echo "FATAL: RPM cache has no repository metadata" >&2; exit 1; }
    cp -R "$OSC_RPM_CACHE_DIR/." "$context/rpm-cache/"
    # Build containers can use a different UID from the context owner.
    # The parent context stays private; only its package copy is readable.
    chmod -R a+rX "$context/rpm-cache"
    cache_sha=$(python3 -c 'import hashlib,sys; print(hashlib.sha256(open(sys.argv[1],"rb").read()).hexdigest())' "$context/rpm-cache/repodata/repomd.xml")
    build_secrets+=(--build-arg "OSC_RPM_CACHE_SHA256=$cache_sha")
fi
mkdir -p "$context/osc-guest-support/dracut.conf.d"
cp "$SCRIPT_DIR/osc-guest-support/kata-osbuilder.sh" "$context/osc-guest-support/"
cp "$SCRIPT_DIR/osc-guest-support/dracut.conf.d/15-dracut.conf" "$context/osc-guest-support/dracut.conf.d/"
echo "Build context: $context"
podman build --layers --platform linux/amd64 \
    "${build_secrets[@]}" \
    "$@" -f "$context/Containerfile" "$context"
