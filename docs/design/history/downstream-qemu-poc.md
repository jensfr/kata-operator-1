# Downstream QEMU integration proof of concept

> **Historical PoC validation (2026-09-05/06).** Superseded by the complete
> Milestone 3 validation in `docs/design/milestone3-validation-record.md`.
> The limitations listed at the end (host ldconfig, upstream firmware
> symlinks, local SELinux overrides, upstream Ubuntu guest) were all
> resolved in subsequent work.

## Result: PASSED

**Date:** 2026-09-05/06
**Test node:** ci-ln-8lxjx6k-1d09d-72w6m-master-0
**Cluster:** OCP 4.22.0-0.nightly-2026-09-02-125503 / RHCOS 9.8 / CRI-O 1.35.6

A kata sandbox runs with dynamically linked RHEL QEMU 10.1.0 under enforcing
SELinux, using unmodified RPM binaries and privately supplied shared libraries.

## Working configuration

### Executables (unmodified RPM payloads)

| File | Hash (sha256) | RPM |
|---|---|---|
| qemu-system-x86_64 | `e346fe47...` | qemu-kvm-core-10.1.0-17.el9_8.5.x86_64 |
| virtiofsd | `acb64ff3...` | virtiofsd-1.13.3-1.el9.x86_64 |

No ELF modifications (no patchelf). Binaries are byte-identical to RPM payload.

### Private libraries (8, all from RHEL RPMs)

| Library | Hash | RPM |
|---|---|---|
| libcapstone.so.4 | `745fed03...` | capstone-4.0.2-13.el9_8 |
| libdaxctl.so.1 | `d4a8944b...` | daxctl-libs-82-1.el9 |
| libfdt.so.1 | `374cb3a3...` | libfdt-1.6.0-7.el9 |
| libndctl.so.6 | `6008487e...` | ndctl-libs-82-1.el9 |
| libpixman-1.so.0 | `ea6ac2e0...` | pixman-0.40.0-6.el9_3 |
| libpmem.so.1 | `4fb7bea7...` | libpmem-1.12.1-1.el9 |
| libpng16.so.16 | `d7dcb093...` | libpng-1.6.37-15.el9_8.2 |
| librdmacm.so.1 | `0a1d3e1d...` | librdmacm-61.0-2.el9 |

### Library resolution

Host-level ldconfig with `/etc/ld.so.conf.d/kata-deploy.conf` containing
`/var/opt/kata/lib`. This is not application-private. The mechanism
for the production artifact is to be determined separately.

### Firmware

RHEL firmware (seabios, seavgabios, edk2-ovmf) placed under
`/var/opt/kata/share/seabios/`, `seavgabios/`, `edk2/ovmf/`.

QEMU's compiled-in data directory includes `/var/opt/kata/share/qemu-kvm/`
(relative to the binary). Symlinks in that directory point to both the
downstream firmware and the upstream kata-deploy firmware for files not
in the downstream set (kvmvapic.bin, pvh.bin, efi-virtio.rom, etc.).

### SELinux

- Packaged module: `osc_kata_deploy` (file-context mappings only)
- Local overrides (4): `share/seabios`, `share/seavgabios`, `share/edk2`,
  `share/qemu-kvm` mapped to `usr_t` (not yet in the packaged module)
- Process domains: QEMU and virtiofsd run as `container_kvm_t` with
  per-pod MCS categories

### Guest kernel and image

Upstream kata-deploy's kernel and image (not downstream OSC guest):
- Kernel: vmlinux-6.18.35-202 (upstream kata)
- Image: kata-ubuntu-noble.image (upstream Ubuntu guest)

### Kata configuration

Upstream kata-deploy's configuration-qemu.toml, unmodified.

## What this established

RHEL's dynamically linked QEMU 10.1.0 can run kata sandboxes on RHCOS 9.8
using privately supplied libraries and enforcing SELinux with the packaged
file-context mappings.

## What this did NOT establish

- Host-derived OSC guest preparation
- Boot-time guest regeneration
- Application-private library resolution (current uses host ldconfig)
- Complete downstream firmware (some files from upstream kata-deploy)
- Packaged SELinux module covering all firmware paths
- patchelf viability (earlier attempt segfaulted; cause not yet isolated)
- Repeat installation, upgrade, or cleanup

## Performance

| Process | RSS |
|---|---|
| QEMU | 284 MB |
| virtiofsd (primary) | 10 MB |
| virtiofsd (listener) | 3.6 MB |
| containerd-shim-kata-v2 | 34 MB |
| **Total kata overhead** | **~332 MB** |

Startup time: ~3 seconds from sandbox creation to running container.

## Next steps

1. Package the complete Red Hat firmware set (replace upstream symlinks)
2. Add firmware path mappings to the SELinux .fc (replace local overrides)
3. Resolve application-private library lookup (separate from ldconfig)
4. Build the standalone staged Job with these proven inputs
