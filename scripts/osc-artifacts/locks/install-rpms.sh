#!/bin/sh
# Install the captured package set, then check RPM header and payload identities.
set -efu
lock=$1
manager=${2:---verify-only}
test -s "$lock"
inventory() {
    rpm -qa --qf '%{NAME}-%{EPOCHNUM}:%{VERSION}-%{RELEASE}.%{ARCH} %{SHA256HEADER} [%{PAYLOADDIGEST}]\n' | grep -v '^gpg-pubkey-' | LC_ALL=C sort
}
expected=$(cat "$lock")
if [ "$(inventory)" = "$expected" ]; then
    echo "Verified locked RPM set: $lock"
    exit 0
fi
if [ "$manager" = --verify-only ]; then
    echo "FATAL: installed RPM versions or content differ from $lock" >&2
    exit 1
fi
repo_directory=${OSC_RPM_REPOS_DIR:-/run/osc-rpm-repos}
if [ "${OSC_RPM_REPOS_REQUIRED:-0}" = 1 ] && [ ! -s "$repo_directory/osc.repo" ]; then
    echo "FATAL: requested RPM repository configuration is missing or empty" >&2
    exit 1
fi
if [ -n "${OSC_RPM_REPOS_SHA256:-}" ]; then
    printf '%s  %s\n' "$OSC_RPM_REPOS_SHA256" "$repo_directory/osc.repo" | sha256sum -c -
fi
if [ -n "${OSC_RPM_CACHE_SHA256:-}" ]; then
    printf '%s  %s\n' "$OSC_RPM_CACHE_SHA256" /run/osc-rpm-cache/repodata/repomd.xml | sha256sum -c -
fi
run_manager() {
    if [ -s "$repo_directory/osc.repo" ]; then
        "$manager" "--setopt=reposdir=$repo_directory" "$@"
    else
        "$manager" "$@"
    fi
}
# Each lock row contains NEVRA, header SHA256, and payload SHA256.
# Package names contain no whitespace or shell patterns.
# shellcheck disable=SC2046
set -- $(cut -d ' ' -f 1 "$lock")
run_manager install -y --nodocs --setopt=install_weak_deps=0 "$@"
run_manager clean all
actual=$(inventory)
if [ "$actual" != "$expected" ]; then
    echo "FATAL: installed RPM versions or content differ from $lock" >&2
    printf '%s\n' "$actual" >&2
    exit 1
fi
echo "Verified locked RPM set: $lock"
