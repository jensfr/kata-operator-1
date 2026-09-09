# SELinux file-context mappings for kata-deploy installation paths

## Problem

Upstream kata-deploy installs files to `/opt/kata/` (which is `/var/opt/kata/`
on RHCOS). These files inherit the `var_t` label from the parent directory.
The existing container-selinux policy defines `container_kvm_t` with
permissions to execute files labeled as `exec_type` members (e.g., `bin_t`,
`qemu_exec_t`, `container_runtime_exec_t`) and read files labeled as
`base_ro_file_type` members (e.g., `lib_t`, `usr_t`). Files labeled `var_t`
fall outside these permitted types.

The solution is file-context mappings that assign the relocated files the
same labels their RPM-installed counterparts have. No new types. No new
allow rules.

## Baseline inventory

Collected on virtlab725 (RHEL 9.6, container-selinux 2.237.0,
selinux-policy-targeted 38.1.53, osc_monitor module loaded) and
cluster-bot OCP 4.22 nightly (RHCOS 9.8, container-selinux 2.245.0).

### RPM-installed file labels (virtlab725 baseline)

| File | Path | SELinux type | Type attribute |
|---|---|---|---|
| Runtime shim | `/usr/bin/containerd-shim-kata-v2` | `container_runtime_exec_t` | `exec_type` |
| QEMU | `/usr/libexec/qemu-kvm` | `qemu_exec_t` | `exec_type` |
| virtiofsd | `/usr/libexec/virtiofsd` | `bin_t` | `exec_type` |
| kata-runtime | `/usr/bin/kata-runtime` | `bin_t` | `exec_type` |
| Shared libraries | `/usr/lib64/libpmem.so.*` | `lib_t` | `base_ro_file_type` |
| Runtime config | `/etc/kata-containers/*` | `etc_t` | |
| CRI-O drop-in | `/etc/crio/crio.conf.d/50-kata` | `container_config_t` | |
| Guest artifacts | `/var/cache/kata-containers/*` | `container_ro_file_t` | |
| Packaged data | `/usr/share/kata-containers/*` | `usr_t` | `base_ro_file_type` |
| OVMF firmware | `/usr/share/edk2/ovmf/*` | `usr_t` | `base_ro_file_type` |
| BIOS firmware | `/usr/share/seabios/*` | `usr_t` | `base_ro_file_type` |

### Running process contexts (virtlab725, kata pod active)

| Process | Context |
|---|---|
| virtiofsd | `system_u:system_r:container_kvm_t:s0:c489,c629` |
| qemu-kvm | `system_u:system_r:container_kvm_t:s0:c489,c629` |
| crio | `system_u:system_r:container_runtime_t:s0` |

### Relevant allow rules from host policy

```
allow container_kvm_t exec_type:file entrypoint;
allow container_kvm_t base_ro_file_type:file { execute execute_no_trans map };
allow container_kvm_t qemu_exec_t:file { execute execute_no_trans getattr ioctl lock map open read };
allow container_kvm_t container_file_t:file { ... execute execute_no_trans ... };
allow domain lib_t:file { execute map };
allow domain base_ro_file_type:file { getattr ioctl lock open read };
```

`container_kvm_t` can use ANY `exec_type` member as entrypoint and execute
ANY `base_ro_file_type` member. `bin_t`, `qemu_exec_t`, and
`container_runtime_exec_t` are all members of `exec_type`.

## Artifact layout (upstream kata-deploy 4.1.0 on RHCOS)

| Path | Content | Default label |
|---|---|---|
| `/var/opt/kata/bin/containerd-shim-kata-v2` | Runtime shim (executable) | `var_t` |
| `/var/opt/kata/bin/qemu-system-x86_64` | QEMU (executable) | `var_t` |
| `/var/opt/kata/bin/kata-runtime` | Kata runtime (executable) | `var_t` |
| `/var/opt/kata/bin/kata-monitor` | Monitor (executable) | `var_t` |
| `/var/opt/kata/bin/kata-collect-data.sh` | Debug script (executable) | `var_t` |
| `/var/opt/kata/libexec/virtiofsd` | Filesystem daemon (executable) | `var_t` |
| `/var/opt/kata/libexec/nydusd` | Nydus daemon (executable) | `var_t` |
| `/var/opt/kata/share/defaults/kata-containers/*.toml` | Runtime configuration | `var_t` |
| `/var/opt/kata/share/kata-containers/vmlinux*` | Guest kernel | `var_t` |
| `/var/opt/kata/share/kata-containers/kata-*.img` | Guest rootfs/initrd | `var_t` |
| `/var/opt/kata/share/kata-qemu/qemu/*` | QEMU firmware (OVMF, SeaBIOS) | `var_t` |
| `/var/opt/kata/lib/kata-qemu/` | QEMU data (upstream static build) | `var_t` |

Note: upstream kata-deploy 4.x ships statically linked binaries. No shared
libraries under `lib/`. The Red Hat build will ship dynamically linked QEMU
with shared libraries under `/var/opt/kata/lib/`.

## Proposed file-context mappings

Each mapping reproduces the label that the baseline RPM installation assigns
to the equivalent file. Where the upstream layout groups things differently
(e.g., config and firmware under `share/`), the mapping follows the
baseline's label for that category of content.

### Executables

| Pattern | Type | Baseline match |
|---|---|---|
| `/var/opt/kata/bin/containerd-shim-kata-v2` | `container_runtime_exec_t` | `/usr/bin/containerd-shim-kata-v2` |
| `/var/opt/kata/bin/qemu-system-.*` | `qemu_exec_t` | `/usr/bin/qemu-system-.*` (existing rule) |
| `/var/opt/kata/libexec/virtiofsd` | `bin_t` | `/usr/libexec/virtiofsd` |
| `/var/opt/kata/libexec/nydusd` | `bin_t` | No RPM baseline; `bin_t` as default executable |
| `/var/opt/kata/bin/kata-runtime` | `bin_t` | `/usr/bin/kata-runtime` |
| `/var/opt/kata/bin/kata-monitor` | `bin_t` | No RPM baseline; `bin_t` as default executable |
| `/var/opt/kata/bin/kata-collect-data.sh` | `bin_t` | `/usr/bin/kata-collect-data.sh` |

### Shared libraries (Red Hat build only)

| Pattern | Type | Baseline match |
|---|---|---|
| `/var/opt/kata/lib(/.*)?` | `lib_t` | `/usr/lib64/*` |

### Configuration

| Pattern | Type | Baseline match |
|---|---|---|
| `/var/opt/kata/share/defaults/kata-containers(/.*)?` | `etc_t` | `/etc/kata-containers/*` |

The baseline labels runtime configuration as `etc_t`. These `.toml` files
serve the same role (kata runtime config) at a different path.

### Guest artifacts

| Pattern | Type | Baseline match |
|---|---|---|
| `/var/opt/kata/share/kata-containers(/.*)?` | `container_ro_file_t` | `/var/cache/kata-containers/*` |

Upstream prebuilt guest kernels, initrds, and rootfs images. The baseline
labels these as `container_ro_file_t`.

OSC host-derived guest artifacts (kernel and initrd generated by osbuilder)
are stored under `/var/cache/kata-containers/osbuilder-images/`, not under
`/var/opt/kata/share/kata-containers/`. The `/var/cache/kata-containers/`
path already receives `container_ro_file_t` from the base policy.

### Firmware and packaged data

| Pattern | Type | Baseline match |
|---|---|---|
| `/var/opt/kata/share/kata-qemu(/.*)?` | `usr_t` | `/usr/share/edk2/*`, `/usr/share/seabios/*` |
| `/var/opt/kata/share/bash-completion(/.*)?` | `usr_t` | `/usr/share/bash-completion/*` |

### Directories

| Pattern | Type | Baseline match |
|---|---|---|
| `/var/opt/kata` | `usr_t` | Installation root; read-only data default |
| `/var/opt/kata/bin` | `bin_t` | `/usr/bin` |
| `/var/opt/kata/libexec` | `bin_t` | `/usr/libexec` |

## Mapping precedence

File-context rules are evaluated most-specific-first when supplied via a
policy module's `.fc` file. Local `semanage fcontext` overrides take
precedence over module-supplied rules regardless of specificity.

The compatibility package should supply mappings via a policy module's
file-context file, not via `semanage fcontext -a` calls. This avoids
precedence surprises and makes the mappings removable by uninstalling the
module.

The generic `bin_t` rules for `/var/opt/kata/bin(/.*)?` must not override
the specific `container_runtime_exec_t` and `qemu_exec_t` rules. In a
module `.fc` file, policy-supplied mappings use specificity-based matching
(not simply longest-pattern-wins). Tests must verify resolved contexts
explicitly:

- `containerd-shim-kata-v2` resolves to `container_runtime_exec_t`
- `qemu-system-x86_64` resolves to `qemu_exec_t`
- `kata-runtime` resolves to `bin_t`
- `virtiofsd` resolves to `bin_t`

## Validation status

### Completed: proof of concept (2026-09-05)

Seven representative paths verified using local `semanage fcontext` mappings
on one cluster-bot worker (OCP 4.22 / RHCOS 9.8). A fresh kata workload ran
under enforcing SELinux; QEMU and virtiofsd observed in `container_kvm_t`
with per-pod MCS categories. This established that mapping-only compatibility
is sufficient; no new types or allow rules are justified.

### Completed: packaging acceptance test (2026-09-05)

Built `.te`/`.fc` as a compiled `.pp` module in a Fedora container via
`checkmodule` + `semodule_package`. Installed on a clean node with no
local `semanage fcontext` overrides. All seven `matchpathcon -V` checks
passed using module-supplied mappings only. See
`docs/design/history/selinux-initial-validation.md`.

### Completed: downstream artifact validation (2026-09-06/07)

Validated with downstream dynamically linked RHEL QEMU 10.1.0 and 8
private libraries labeled `lib_t`. Firmware paths (seabios, seavgabios,
edk2, qemu-kvm data) added to the `.fc` and validated. Complete Milestone 3
E2E including reboot persistence. See `docs/design/milestone3-validation-record.md`.

## Validation checklist

1. **Correct files.** After extraction and `restorecon -F -R /var/opt/kata/`:
   `matchpathcon -V` on each inventoried path. Use `-F` to override
   `container_file_t` labels from kata-deploy's container context.

2. **Correct processes.** Start a kata pod, verify virtiofsd and QEMU run as
   `container_kvm_t` with MCS categories. Compare full contexts (user, role,
   type, range) against virtlab725 baseline.

3. **Persistent behavior.** Reboot, verify labels survive. Re-extract
   (simulate upgrade), run `restorecon -F`, verify labels are reapplied.

4. **No experimental overrides.** Before final testing, confirm no local
   `semanage fcontext` customizations, no `chcon` residue, no loaded
   modules from earlier experiments (`semodule -l | grep osc`). Remove
   only identified experimental overrides, not all local customizations.

5. **Ownership and permissions.** Verify extracted files are owned by root
   and not writable by the workload process (`container_kvm_t`).

6. **Packaging test.** Build module in a build container. Install on a node
   with no local fcontext overrides. Verify resolved contexts match the
   design table. Run a fresh workload.

## Open items

1. The `osc_monitor` module on virtlab725: what does it add? Its rules may
   overlap with or depend on specific file paths. Check whether it needs
   updating for the relocated paths.

2. CRI-O drop-in config at `/etc/crio/crio.conf.d/99-kata-deploy`: labeled
   `container_config_t` in the baseline. When kata-deploy writes this file,
   does it get the right label from the parent directory's context? The
   parent `/etc/crio/crio.conf.d/` should already have the right context.

3. ~~Red Hat build: dynamically linked QEMU adds shared libraries under
   `/var/opt/kata/lib/`.~~ **Resolved.** The `lib_t` mapping covers the
   private libraries. Validated with the actual RHEL QEMU 10.1.0 binary
   and 8 private libraries.
