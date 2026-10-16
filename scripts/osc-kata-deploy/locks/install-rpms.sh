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
# Each lock row contains NEVRA, header SHA256, and payload SHA256.
# Package names contain no whitespace or shell patterns.
# shellcheck disable=SC2046
set -- $(cut -d ' ' -f 1 "$lock")
"$manager" install -y --nodocs --setopt=install_weak_deps=0 "$@"
"$manager" clean all
actual=$(inventory)
if [ "$actual" != "$expected" ]; then
    echo "FATAL: installed RPM versions or content differ from $lock" >&2
    printf '%s\n' "$actual" >&2
    exit 1
fi
echo "Verified locked RPM set: $lock"
