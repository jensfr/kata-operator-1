#!/bin/sh
# OSC guest preparation: generate host-derived kata guest artifacts.
#
# Runs as an init container in the install Job, after install-stage-artifacts
# has delivered the osbuilder scripts and kata-agent to /var/opt/kata/.
#
# Uses the host's kernel and dracut to build an initrd containing the
# kata-agent. The output goes to /var/cache/kata-containers/osbuilder-images/
# where the kata configuration references it.
#
# Also installs a systemd unit for boot-time regeneration after kernel updates.

set -eu

KATA_DIR=/var/opt/kata
OSBUILDER="${KATA_DIR}/libexec/kata-containers/osbuilder/kata-osbuilder.sh"
BOOT_UNIT="${KATA_DIR}/share/systemd/kata-osbuilder-generate.service"
HOST_UNIT_DIR=/etc/systemd/system

echo "=== OSC guest preparation ==="

# Verify osbuilder is installed
if ! chroot /host test -x "${OSBUILDER}"; then
  echo "FATAL: osbuilder not found at ${OSBUILDER}"
  exit 1
fi

# Run osbuilder with relocated KATA_LIBEXEC_DIR
# Rebuild on installation so agent upgrades reach the guest on the same kernel.
# -u: update/remove existing images first
echo "Running kata-osbuilder.sh..."
chroot /host env \
  KATA_LIBEXEC_DIR="${KATA_DIR}/libexec/kata-containers" \
  "${OSBUILDER}" -u

echo "Guest artifacts generated."

# Verify output
if chroot /host test -L /var/cache/kata-containers/osbuilder-images/kata.kernel && \
   chroot /host test -e /var/cache/kata-containers/osbuilder-images/kata.initrd; then
  echo "kata.kernel -> $(chroot /host readlink /var/cache/kata-containers/osbuilder-images/kata.kernel)"
  echo "kata.initrd exists"
else
  echo "FATAL: guest artifacts not generated"
  exit 1
fi

chroot /host /bin/bash -s -- "${KATA_DIR}/libexec/kata-containers/agent/usr/bin/kata-agent" /var/cache/kata-containers/osbuilder-images/kata.initrd <<'VERIFY_GUEST_AGENT'
set -euo pipefail
agent_info=$(file -b "$1") || { echo "FATAL: cannot inspect installed kata-agent" >&2; exit 1; }
guest_info=$(lsinitrd -f usr/bin/kata-agent "$2" | file -b -) || { echo "FATAL: cannot extract guest kata-agent" >&2; exit 1; }
agent_id=$(printf '%s\n' "$agent_info" | sed -n 's/.*BuildID\[sha1\]=\([[:xdigit:]]*\).*/\1/p')
guest_id=$(printf '%s\n' "$guest_info" | sed -n 's/.*BuildID\[sha1\]=\([[:xdigit:]]*\).*/\1/p')
if [ -z "$agent_id" ] || [ -z "$guest_id" ]; then
  echo "FATAL: kata-agent ELF build ID missing from installed payload or guest initrd" >&2
  exit 1
fi
if [ "$agent_id" != "$guest_id" ]; then
  echo "FATAL: guest kata-agent mismatch: installed=$agent_id guest=$guest_id" >&2
  exit 1
fi
echo "Guest agent verified: ELF build ID $guest_id"
VERIFY_GUEST_AGENT

# Install boot-time regeneration unit with correct ordering
if [ -f "/host${BOOT_UNIT}" ]; then
  # Write an OSC-specific unit with Before=kubelet ordering
  cat > "/host${HOST_UNIT_DIR}/osc-kata-guest-prep.service" <<UNIT
[Unit]
Description=OSC: Generate Kata guest image for host kernel
Before=kubelet.service
After=local-fs.target

[Service]
Type=oneshot
Environment=KATA_LIBEXEC_DIR=${KATA_DIR}/libexec/kata-containers
ExecStart=${OSBUILDER} -c -u

[Install]
WantedBy=kubelet.service
UNIT

  chroot /host systemctl daemon-reload
  chroot /host systemctl enable osc-kata-guest-prep.service
  echo "Boot-time regeneration unit installed and enabled."
else
  echo "WARNING: boot unit template not found, skipping boot-helper."
fi

# Update kata configuration to use host-derived guest paths
KATA_CONF="/var/opt/kata/share/defaults/kata-containers/runtimes/qemu/configuration-qemu.toml"
if chroot /host test -f "$KATA_CONF"; then
  chroot /host sed -i \
    -e 's|^kernel = ".*"|kernel = "/var/cache/kata-containers/osbuilder-images/kata.kernel"|' \
    -e 's|^image = ".*"|initrd = "/var/cache/kata-containers/osbuilder-images/kata.initrd"|' \
    -e '/^image = /d' \
    "$KATA_CONF"
  echo "Kata config updated to use host-derived guest paths."
else
  echo "WARNING: kata config not found at $KATA_CONF"
fi

echo "=== Guest preparation complete ==="
