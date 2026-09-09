# Proposal: Container-image-based kata deployment

## The problem

We ship kata as an RPM inside the RHCOS extensions image. This creates three problems.

**On HCP, installation is a mess.** HCP nodes have no MCO, so the operator falls back to a privileged DaemonSet that runs `rpm-ostree install` followed by `rpm-ostree apply-live --allow-replacement`. But `apply-live` doesn't apply `/etc` files, doesn't run RPM scriptlets, and doesn't reload SELinux policy. The install script compensates by extracting `/etc` files from RPMs with tar, parsing scriptlets with awk, and calling `load_policy` by hand. Uninstall is worse: `rpm-ostree uninstall` doesn't support `apply-live` at all, so removal is staged for the next reboot while the script manually deletes CRI-O configs. The result is about 100 lines of crash recovery code to handle every combination of staged, live, and booted states after a pod restart.

**CRI-O restart kills the installer pod.** After installing kata, the DaemonSet pod restarts CRI-O to register the new runtime handler. CRI-O manages the pod doing the restart, so the pod dies. Everything critical must happen before that restart. On pod recreation, the script must figure out where it left off.

**Our release dates are locked to OCP z-streams.** The kata RPM ships inside the RHCOS extensions image, which is built as part of the OCP release pipeline. We cannot release the operator until the matching extensions image ships. A kata bug fix that takes a day to write waits weeks for the next z-stream window.

## The proposal

Ship all host-level artifacts in a container image built in Konflux. Use the upstream kata-deploy Rust binary (unchanged, pinned at v4.1.0) to copy files to the host. The operator creates per-node install Jobs using kata-deploy's staged installation commands.

### Architecture

```
KataConfig / OSC reconciler
        |
        v
per-node install Job (6 sequential stages)
        |
        v
  1. host-check           (kata-deploy binary, combined image)
  2. selinux-install       (shell, helper image)
  3. install-stage-artifacts (kata-deploy binary, combined image)
  4. guest-prep            (shell, helper image)
  5. apply-labels + verify (shell, helper image)
  6. configure-cri         (kata-deploy binary, combined image)
```

Two images are used:

- **Combined image**: Unchanged upstream kata-deploy 4.1.0 binary layered with Red Hat artifact tarballs. Used for stages that invoke the kata-deploy binary (host-check, install-artifacts, configure-cri). This image is distroless.
- **Helper image**: Shell-capable UBI-minimal image for host-integration stages that run shell scripts via `chroot /host` (selinux-install, guest-prep, apply-labels). In production, the OSC operator image itself fills this role.

### How artifacts reach the host

The combined image contains a single tarball (`kata-static-qemu.tar.zst`) assembled at build time from Red Hat RPMs:

- QEMU 10.1.0 (dynamically linked, exec wrapper for library resolution)
- 8 private shared libraries from RHEL RPMs
- Complete Red Hat firmware (edk2-ovmf, seabios, seavgabios, ipxe-roms, qemu-kvm data files)
- virtiofsd
- osbuilder scripts and kata-agent (from kata-containers RPM)
- Boot-time systemd unit (osc-kata-guest-prep.service)
- Downstream guest configuration (osc-guest-paths.toml)

The upstream `install-stage-artifacts` command extracts this tarball to `/opt/kata/` on the host, which is `/var/opt/kata/` on RHCOS (writable, persists across reboots and OS upgrades).

No rpm-ostree state to track. Files are files. Remove them to uninstall.

### QEMU and library resolution

Downstream QEMU is dynamically linked, permanently. Static linking is out of scope for security update reasons.

A minimal exec wrapper sets `LD_LIBRARY_PATH=/var/opt/kata/lib` and execs the unmodified QEMU binary. This is application-private: the variable applies to the QEMU process tree only, not to the host-wide loader configuration. All 8 private libraries and the QEMU binary are byte-identical to their RHEL RPM payloads.

### SELinux

A mapping-only SELinux policy module assigns existing types to relocated paths under `/var/opt/kata/`. No new types. No new allow rules. The mappings reproduce the labels that the baseline RPM installation assigns:

| Pattern | Type | Baseline match |
|---|---|---|
| `containerd-shim-kata-v2` | `container_runtime_exec_t` | RPM shim binary |
| `qemu-system-.*` | `qemu_exec_t` | RPM QEMU binary |
| `bin/`, `libexec/` | `bin_t` | `/usr/bin`, `/usr/libexec` |
| `lib/` | `lib_t` | `/usr/lib64` |
| `share/defaults/kata-containers/` | `etc_t` | `/etc/kata-containers` |
| `share/kata-containers/` | `container_ro_file_t` | `/var/cache/kata-containers` |
| Firmware paths | `usr_t` | `/usr/share/edk2`, `/usr/share/seabios` |

The module is pre-compiled as a `.pp` and installed via `semodule -i` in the selinux-install init container. Labels are applied with `restorecon -F -R` after artifact extraction.

### Guest kernel and initrd

OSC generates a host-derived guest by running `kata-osbuilder.sh` with relocated `KATA_LIBEXEC_DIR=/var/opt/kata/libexec/kata-containers`. This produces kernel and initrd under `/var/cache/kata-containers/osbuilder-images/` matching the host's booted kernel. The kata configuration is patched to reference these paths instead of the upstream prebuilt guest.

A systemd unit (`osc-kata-guest-prep.service`) with `Before=kubelet.service` ordering regenerates guest artifacts on boot if the host kernel changes. The unit is installed via offline WantedBy symlink creation (no D-Bus required from the container).

For confidential containers, pre-built guest kernels with published reference values will be shipped in the tarball. The osbuilder path applies to bare-metal kata-qemu only.

### CRI-O activation

Upstream kata-deploy's `install-stage-cri` command writes a CRI-O drop-in config and restarts CRI-O. CRI-O SIGHUP live reload of runtime handlers works on OCP 4.22 / CRI-O 1.35 (validated 2026-09-04). The SIGHUP path is a separate upstream contribution; the current Job uses the upstream restart behavior.

### Controller reconciliation

The controller uses a desired/observed generation model. It does not inspect partial host state or try to determine which install stage completed.

- `desiredGeneration`: hash of combined image digest, SELinux version, guest-prep version
- `observedGeneration`: per-node annotation recorded after Job success and verification
- `nodeUID`: per-node annotation to detect node replacement (same name, new UID)
- Phases: Installing, Ready, Failed, Cleaning

Reconcile logic for each selected node:

```
observed generation == desired generation AND same node UID
    -> Ready (skip)

otherwise
    -> ensure install Job exists
    -> watch Job
    -> Job succeeds -> verify -> record observed generation
    -> Job fails or disappears -> reapply same desired generation
```

The controller never grows code like "if QEMU exists but CRI-O config doesn't, do X." That would recreate the installer logic this design removes. The proven Job is idempotent and interrupt-recoverable.

### Deployment modes

- **KataDeploy** (experimental, feature-gated): Job-based installation using kata-deploy. Designed to support both standalone OCP and HCP; HCP validation is pending.
- **MachineConfig** (default): Existing MCO-based flow. Kept for existing customers on standalone OCP.
- Legacy DaemonSet mode (rpm-ostree/apply-live) is deprecated.

### Lifecycle

**Install:** Per-node Job runs 6 sequential stages. After completion, the controller records the observed generation and labels the node.

**Upgrade:** Controller detects generation mismatch (new image), creates new install Jobs. The idempotent Job overwrites artifacts with updated versions.

**Cleanup:** Per-node cleanup Job runs upstream cleanup stages plus OSC-owned resource removal (SELinux module, boot unit, guest artifacts, labels).

**Reboot:** Files persist under `/var`. The boot-time systemd unit runs osbuilder before kubelet starts. No operator intervention needed.

**Node replacement:** Controller detects UID change, treats the node as new, creates a fresh install Job.

## What this fixes

The entire `apply-live` path goes away. No rpm-ostree calls. No `/etc` tar extraction. No scriptlet parsing. No asymmetric uninstall. No staged-vs-live-vs-booted state tracking.

We stop killing our own pod during installation. CRI-O restart happens in the final Job stage, after all other stages complete. The Job pod is not managed by the CRI-O instance it restarts.

We release on our own schedule. A kata fix ships when it is ready, not when the next OCP z-stream opens.

The same code path is designed to support standalone OCP and HCP (HCP validation pending). One mode instead of three.

We work upstream first. The kata-deploy binary is unchanged. Downstream builds replace artifacts, not code.

## Validated architecture (Milestone 3, 2026-09-07)

Validated on OCP 4.22.0-0.nightly / RHCOS 9.8 / CRI-O 1.35.6.

| Test | Result |
|---|---|
| Clean node install (6-stage Job) | PASS |
| Downstream dynamic RHEL QEMU 10.1.0 | PASS |
| Exec wrapper with private libraries | PASS |
| Enforcing SELinux (mapping-only module) | PASS |
| Idempotent reinstall | PASS |
| Partial artifact recovery | PASS |
| Partial CRI recovery | PASS |
| Full cleanup and reinstall | PASS |
| Host-derived guest (osbuilder) | PASS |
| Boot service before kubelet | PASS |
| Node reboot | PASS |
| Kata workload after reboot (host kernel) | PASS |

Guest kernel inside the kata VM after reboot matches the host kernel (`5.14.0-687.44.1.el9_8.x86_64`). The boot-time unit ran before kubelet, verified existing guest artifacts matched the booted kernel, and exited successfully.

BestEffort exit-137: reproduces equivalently with upstream and downstream QEMU on OCP 4.22. Not downstream-specific. Tracked separately.

## Open questions

1. Migration path for nodes with kata installed via RPM. Need to remove RPM-layered packages and install file-based artifacts without disrupting running workloads.

2. Cleanup Job template. Symmetric with install: upstream cleanup stages plus SELinux module removal, boot unit removal, guest artifact removal.

3. Konflux pipeline for the combined image. Needs entitlement certs and kata-containers RPM as build inputs.
