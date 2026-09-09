# QEMU dependency model for kata-deploy artifacts

## Decision

Downstream QEMU is dynamically linked, permanently. Static linking is out
of scope for security update reasons.

## Dependency inventory

Collected from OCP 4.22 / RHCOS 9.8 with qemu-kvm-core 10.1.0.

QEMU links against ~55 shared libraries. Of these, 48 are present on RHCOS
and 8 are not.

### Private libraries (shipped in the artifact tarball)

| Library | RPM | Purpose |
|---|---|---|
| `libcapstone.so.4` | capstone-4.0.2-13.el9_8 | Disassembly engine |
| `libdaxctl.so.1` | daxctl-libs-82-1.el9 | DAX device control |
| `libndctl.so.6` | ndctl-libs-82-1.el9 | NVDIMM management |
| `libpixman-1.so.0` | pixman-0.40.0-6.el9_3 | 2D pixel manipulation |
| `libpmem.so.1` | libpmem-1.12.1-1.el9 | Persistent memory |
| `libpng16.so.16` | libpng-1.6.37-15.el9_8.2 | PNG image support |
| `libfdt.so.1` | libfdt-1.6.0-7.el9 | Flattened device tree |
| `librdmacm.so.1` | librdmacm-61.0-2.el9 | RDMA connection manager |

All 8 libraries are sourced from RHEL 9 RPMs. `librdmacm` is not in the
RHCOS 9.8 extensions image despite `qemu-kvm-core-10.1.0` linking it
directly.

### Host-provided libraries (~48)

All other QEMU dependencies (glibc, openssl, glib2, gnutls, krb5,
libselinux, etc.) are part of the RHCOS base and receive security
updates through RHCOS updates.

### virtiofsd dependencies

virtiofsd links against 5 libraries, all present on RHCOS:
libc, libcap-ng, libgcc_s, libseccomp. No additional packaging needed.

### Transitive dependency chain

```
qemu-system-x86_64
  -> libpmem.so.1       (private)
    -> libndctl.so.6    (private)
      -> libdaxctl.so.1 (private, no further private deps)
```

All other private libraries depend only on host-provided libraries.

## Library resolution

An exec wrapper sets `LD_LIBRARY_PATH=/var/opt/kata/lib` and execs the
unmodified QEMU binary. This is application-private: the variable applies
to the QEMU process tree only. No host-wide ldconfig entries, no ELF
binary modifications.

```
/opt/kata/bin/qemu-system-x86_64       <- wrapper (shell script)
/opt/kata/bin/qemu-system-x86_64.real  <- unmodified RPM payload
/opt/kata/lib/                         <- 8 unmodified RHEL libraries
```

`LD_LIBRARY_PATH` covers the full transitive chain (libpmem -> libndctl ->
libdaxctl) because it applies to the entire process's library resolution.

patchelf 0.15.0 was tested and reproducibly causes this RHEL QEMU 10.1.0
binary to segfault. See library-resolution.md for the full finding.

## Security update model

| Category | How updates are delivered |
|---|---|
| Host-provided libs (~48) | RHCOS updates. No OSC action needed. |
| Private libs (8) | Artifact image rebuild in Konflux. Same cadence as QEMU updates. |
| QEMU binary | Artifact image rebuild. |
| virtiofsd | Artifact image rebuild. |

When a private library receives a CVE fix, rebuild the artifact image
with the updated RHEL RPM. The operator deploys the new image and
kata-deploy replaces the files on each node.

## Validation

Validated on OCP 4.22 / RHCOS 9.8 (2026-09-06/07):

1. `ldd` on `/var/opt/kata/bin/qemu-system-x86_64.real` with
   `LD_LIBRARY_PATH=/var/opt/kata/lib` resolves all 8 private libraries
   from `/var/opt/kata/lib/` and all host libraries from `/lib64/`
2. `/proc/<pid>/maps` confirms private libs loaded from `/var/opt/kata/lib/`
3. `ldconfig -p | grep /var/opt/kata` returns empty (no host-wide entries)
4. Kata sandbox runs under enforcing SELinux; private libs labeled `lib_t`
5. All private library RPMs have Red Hat provenance (not Fedora)
