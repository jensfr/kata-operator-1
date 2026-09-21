# Library resolution for downstream QEMU

Decision: exec wrapper with `LD_LIBRARY_PATH`. This is the validated,
final approach.

## Finding: patchelf 0.15.0 segfaults QEMU 10.1.0

Controlled comparison in a single build container (UBI 9.6 + RHEL repos):

| Case | Binary | Result |
|---|---|---|
| Original RPM payload | qemu-kvm-core-10.1.0-17.el9_8.5 | --version succeeds |
| After `patchelf --set-rpath /var/opt/kata/lib` | Same RPM, patchelf 0.15.0 (EPEL) | Segfault (exit 139) |

The patchelf 0.15.0 transformation reorders program headers (14 to 15),
moves the INTERP and LOAD segments, and adds GNU_RELRO and GNU_STACK
headers. The binary grows from 32290368 to 32798096 bytes. The resulting
binary segfaults immediately.

This is consistent with known patchelf issues on complex ELF layouts
(NixOS/patchelf#568 and similar reports).

The private libraries (libpmem, libndctl) were also patched successfully
in the build container but were not tested in isolation on the target.
The executable transformation alone reproduces the crash.

## Solution: exec wrapper

A minimal shell wrapper sets `LD_LIBRARY_PATH=/var/opt/kata/lib` and
`exec`s into the unmodified QEMU binary. This is application-private:
the variable applies to the QEMU process and its inherited children,
not to the host-wide loader configuration.

```
/opt/kata/bin/qemu-system-x86_64       <- wrapper (shell script)
/opt/kata/bin/qemu-system-x86_64.real  <- unmodified RPM payload
/opt/kata/lib/                         <- unmodified RHEL private libraries
```

The wrapper:
```sh
#!/bin/sh
set -eu
export LD_LIBRARY_PATH=/var/opt/kata/lib
exec /var/opt/kata/bin/qemu-system-x86_64.real "$@"
```

This covers the full transitive dependency chain (libpmem -> libndctl ->
libdaxctl) without modifying any library, because `LD_LIBRARY_PATH`
applies to the entire process's library resolution, not just direct deps.

### Validated on OCP 4.22 (2026-09-06)

Clean node, no ldconfig entries, no local SELinux overrides, no patchelf.
SELinux module from the packaged .pp only.

- Kata pod runs as `container_kvm_t` with per-pod MCS categories
- `/proc/<pid>/exe` points to `qemu-system-x86_64.real`
- `/proc/<pid>/maps` shows 7 private libs from `/var/opt/kata/lib/`
  and host libs from `/lib64/`
- `ldconfig -p | grep /var/opt/kata` returns empty
- No SELinux denials
- virtiofsd also runs as `container_kvm_t`

### Why this works under SELinux

The wrapper script has `qemu_exec_t` label (from the `.fc` mapping for
`/var/opt/kata/bin/qemu-system-.*`). `container_kvm_t` has entrypoint
permission on `exec_type` members. The shell interpreter (`/bin/sh`,
labeled `shell_exec_t`) is also an `exec_type` member. After `exec`,
the process runs the real QEMU binary (also `qemu_exec_t`).

`LD_LIBRARY_PATH` is respected because the process is not in secure
execution mode: the wrapper and real binary have the same SELinux context,
and no setuid/setgid bits are involved.

### Why this is preferred over patchelf

- QEMU and all libraries are byte-identical to RHEL RPM payloads
- Debug symbols and Build-IDs match the original RPMs
- The wrapper is a build artifact, not a host-side workaround
- No ELF modification means no risk of corrupting complex layouts

### Why this is preferred over ldconfig

- Application-private (no host-wide loader cache changes)
- No cleanup needed (the wrapper is part of the artifact tarball)
- Does not affect other applications on the node

## Notes

The patchelf 0.15.0 segfault is confirmed and reproducible. A newer
patchelf version (0.18.0 on Fedora 41) may handle this binary correctly
but has not been tested. This is not blocking; the exec wrapper approach
is validated and preferred for its simplicity and auditability.
