#!/bin/bash
# osc-lite (crun + libosc-lite) installation script for OpenShift Sandboxed Containers
#
# Dev preview: extracts binaries from container image, copies to host
# Tech preview: will use rpm-ostree install from extension image
#
# Required env vars:
#   KRUN_IMAGE  - container image with osc-lite binaries
#   NODE_NAME   - name of the node being configured
#
# Usage: osc-osc-lite-install.sh install|uninstall

set -euo pipefail

HOST=/host
BINDIR=${HOST}/usr/local/bin
LIBDIR=${HOST}/usr/local/lib64
CRIOCONF=${HOST}/etc/crio/crio.conf.d
LDCONF=${HOST}/etc/ld.so.conf.d

source /scripts/lib.sh

wait_for_crio_handler() {
    local handler=$1
    local max_attempts=30
    for i in $(seq 1 $max_attempts); do
        if chroot /host crictl info 2>/dev/null | grep -q "$handler"; then
            echo "CRI-O handler '$handler' is ready"
            return 0
        fi
        echo "Waiting for CRI-O handler '$handler'... ($i/$max_attempts)"
        sleep 2
    done
    echo "WARNING: CRI-O handler '$handler' not detected after $max_attempts attempts"
    return 1
}

install() {
    echo "=== Installing osc-lite ==="

    # Create target directories
    mkdir -p ${BINDIR} ${LIBDIR} ${CRIOCONF} ${LDCONF}

    # Extract osc-lite binary and libraries from container image
    echo "Extracting osc-lite artifacts from ${KRUN_IMAGE}..."
    local extract_dir="/tmp/osc-lite-extract"
    mkdir -p ${extract_dir}

    extract_container_image \
        "${KRUN_IMAGE}" \
        "/opt/krun/bin/crun /opt/krun/lib64" \
        "${extract_dir}" \
        "/tmp/regauth/auth.json"

    # Install binary
    echo "Installing osc-lite binary..."
    cp ${extract_dir}/crun ${BINDIR}/osc-lite
    chmod +x ${BINDIR}/osc-lite

    # Install libraries
    echo "Installing libosc-lite libraries..."
    cp ${extract_dir}/lib64/libosc-lite* ${LIBDIR}/ 2>/dev/null || true
    cp ${extract_dir}/lib64/libosc-litefw* ${LIBDIR}/ 2>/dev/null || true

    # Configure library path
    echo "/usr/local/lib64" > ${LDCONF}/osc-lite.conf
    chroot /host ldconfig

    # Verify library is loadable
    echo "Verifying..."
    chroot /host bash -c 'LD_LIBRARY_PATH=/usr/local/lib64 ldd /usr/local/bin/osc-lite' 2>/dev/null | grep osc-lite || true

    # Deploy CRI-O runtime handler config
    echo "Deploying CRI-O config..."
    cp /files/50-osc-lite ${CRIOCONF}/50-osc-lite
    chmod 0644 ${CRIOCONF}/50-osc-lite

    # Restart CRI-O (reload does not pick up new runtime handlers)
    echo "Restarting CRI-O..."
    chroot /host systemctl restart crio

    # Wait for handler to be available
    wait_for_crio_handler "osc-lite"

    # Verify
    echo "=== osc-lite installation complete ==="
    chroot /host /usr/local/bin/osc-lite --version || echo "WARNING: osc-lite version check failed"

    # Clean up
    rm -rf ${extract_dir}
}

uninstall() {
    echo "=== Uninstalling osc-lite ==="

    # Remove CRI-O config
    rm -f ${CRIOCONF}/50-osc-lite

    # Restart CRI-O to remove the handler
    echo "Restarting CRI-O..."
    chroot /host systemctl restart crio || true

    # Remove binaries and libraries
    rm -f ${BINDIR}/osc-lite
    rm -f ${LIBDIR}/libosc-lite*
    rm -f ${LIBDIR}/libosc-litefw*
    rm -f ${LDCONF}/osc-lite.conf
    chroot /host ldconfig || true

    echo "=== osc-lite uninstallation complete ==="
}

# Main
ACTION=${1:-}
case "${ACTION}" in
    install)
        : "${KRUN_IMAGE:?KRUN_IMAGE env var is required}"
        : "${NODE_NAME:?NODE_NAME env var is required}"
        install
        echo "Installer running. osc-lite is ready."
        exec sleep infinity
        ;;
    uninstall)
        uninstall
        ;;
    *)
        echo "Usage: $0 install|uninstall"
        exit 1
        ;;
esac
