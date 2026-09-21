# SELinux packaging validation record

> **Historical initial validation (2026-09-05).** This records the first
> packaged-module test against upstream statically linked QEMU. The
> limitations listed at the end were all resolved in subsequent work.

## Subsequent validation (2026-09-06/07)

All items listed under "does NOT cover" in this record were validated
during Milestone 3:

- Fresh extraction on a clean node: validated
- Reboot persistence: labels survive reboot
- Downstream dynamically linked QEMU with lib_t mappings: validated
- Firmware path mappings in the packaged .fc: added and tested
- Full E2E with host-derived guest and boot service: validated

See `docs/design/milestone3-validation-record.md` for the complete evidence.

## Test: packaged mapping module against upstream artifact fixture

**Date:** 2026-09-05
**Result:** PASSED

### Environment

- Cluster: cluster-bot OCP 4.22.0-0.nightly-2026-09-02-125503
- Node OS: Red Hat Enterprise Linux CoreOS 9.8.20260901-0 (Plow)
- Kernel: 5.14.0-687.44.1.el9_8.x86_64
- CRI-O: 1.35.6
- container-selinux: 2.245.0-1.el9
- selinux-policy-targeted: 38.1.55-3.el9_8 (policy version 33)
- kata-deploy: quay.io/kata-containers/kata-deploy:4.1.0 (upstream, unmodified)

### Module

- Source: `scripts/osc-artifacts/selinux/osc_kata_deploy.te` + `.fc`
- Built with: checkmodule + semodule_package in Fedora 41 container
- `.pp` sha256: `4d11ce2695e3b583361d870ee10a474ae1bd4811eae55eab5cb8f8d8a9aa70c7`
- Commit: e5df5ea4

### Test node

Worker: `ci-ln-nl6w002-1d09d-hk7zw-worker-northcentralus-jwffh`

Pre-test state:
- No local `semanage fcontext` overrides (`semanage fcontext -l -C` empty)
- No osc/kata modules loaded (`semodule -l | grep osc` empty)
- Files had residual `bin_t` from earlier `chcon` experiment
- `restorecon -R` reset files to `var_t` before module installation

### Commands

```
# Install module
semodule -i osc_kata_deploy.pp

# Apply labels
restorecon -F -R -v /var/opt/kata/

# Verify
matchpathcon -V <path>   # for each checked path
```

### CHECK 1: Policy lookup (matchpathcon, before label application)

| Path | Expected type | Result |
|---|---|---|
| `/var/opt/kata/bin/containerd-shim-kata-v2` | `container_runtime_exec_t` | Correct |
| `/var/opt/kata/bin/qemu-system-x86_64` | `qemu_exec_t` | Correct |
| `/var/opt/kata/bin/kata-runtime` | `bin_t` | Correct |
| `/var/opt/kata/libexec/virtiofsd` | `bin_t` | Correct |

### CHECK 2: Label application (after restorecon -F -R)

| Path | Actual label | Expected | Match |
|---|---|---|---|
| `/var/opt/kata/bin/containerd-shim-kata-v2` | `container_runtime_exec_t:s0` | `container_runtime_exec_t:s0` | Yes |
| `/var/opt/kata/bin/qemu-system-x86_64` | `qemu_exec_t:s0` | `qemu_exec_t:s0` | Yes |
| `/var/opt/kata/bin/kata-runtime` | `bin_t:s0` | `bin_t:s0` | Yes |
| `/var/opt/kata/libexec/virtiofsd` | `bin_t:s0` | `bin_t:s0` | Yes |
| `/var/opt/kata/` (dir) | `usr_t:s0` | `usr_t:s0` | Yes |
| `/var/opt/kata/bin/` (dir) | `bin_t:s0` | `bin_t:s0` | Yes |
| `/var/opt/kata/libexec/` (dir) | `bin_t:s0` | `bin_t:s0` | Yes |
| `/var/opt/kata/share/defaults/kata-containers/` (dir) | `etc_t:s0` | `etc_t:s0` | Yes |
| `/var/opt/kata/share/kata-containers/` (dir) | `container_ro_file_t:s0` | `container_ro_file_t:s0` | Yes |
| `/var/opt/kata/share/kata-qemu/` (dir) | `usr_t:s0` | `usr_t:s0` | Yes |
| `configuration-qemu.toml` (symlink) | `etc_t:s0` | `etc_t:s0` | Yes |
| `vmlinux-6.18.35-202` (guest kernel) | `container_ro_file_t:s0` | `container_ro_file_t:s0` | Yes |
| `edk2-x86_64-code.fd` (firmware) | `usr_t:s0` | `usr_t:s0` | Yes |

All seven `matchpathcon -V` checks passed.

### CHECK 3: Workload process contexts

A kata pod ran successfully on the test node. Process contexts:

| Process | Full context |
|---|---|
| virtiofsd | `system_u:system_r:container_kvm_t:s0:c13,c268` |
| qemu-system-x86 | `system_u:system_r:container_kvm_t:s0:c13,c268` |

Both processes run in `container_kvm_t` domain with MCS categories assigned
to the pod. The categories (`c13,c268`) were observed for this specific pod;
a different pod would receive different categories. This observation confirms
the expected domain assignment, not cross-pod isolation enforcement (which
would require a separate multi-pod test).

### Scope and limitations

This validation covers:
- The packaged `.pp` module supplies the intended file-context mappings
- `restorecon -F` applies those mappings to the inventoried file types
- A kata workload starts under enforcing SELinux with expected process domains

This validation does NOT cover:
- Fresh extraction (files already existed from a prior kata-deploy installation)
- Reboot persistence (not tested)
- Downstream dynamically linked QEMU and its library labels
- Directory and symlink rules added in the `.fc` beyond the seven checked paths
- Cross-pod MCS isolation enforcement
- The `osc_monitor` module's interaction with the relocated paths
