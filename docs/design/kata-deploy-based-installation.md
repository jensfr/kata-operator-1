# Proposal: Container-image-based kata deployment

## The problem

We ship kata as an RPM inside the RHCOS extensions image. This creates three problems.

**On HCP, installation is a mess.** HCP nodes have no MCO, so the operator falls back to a privileged DaemonSet that runs `rpm-ostree install` followed by `rpm-ostree apply-live --allow-replacement`. But `apply-live` doesn't apply `/etc` files, doesn't run RPM scriptlets, and doesn't reload SELinux policy. The install script compensates by extracting `/etc` files from RPMs with tar, parsing scriptlets with awk, and calling `load_policy` by hand. Uninstall is worse: `rpm-ostree uninstall` doesn't support `apply-live` at all, so removal is staged for the next reboot while the script manually deletes CRI-O configs. The result is about 100 lines of crash recovery code to handle every combination of staged, live, and booted states after a pod restart.

**CRI-O restart kills the installer pod.** After installing kata, the DaemonSet pod restarts CRI-O to register the new runtime handler. CRI-O manages the pod doing the restart, so the pod dies. Everything critical must happen before that restart. On pod recreation, the script must figure out where it left off.

**Our release dates are locked to OCP z-streams.** The kata RPM ships inside the RHCOS extensions image, which is built as part of the OCP release pipeline. We cannot release the operator until the matching extensions image ships. A kata bug fix that takes a day to write waits weeks for the next z-stream window.

## The proposal

Ship all host-level artifacts in a container image built in Konflux. Use the upstream kata-deploy binary (pinned to 4.2.0) to copy files to the host. The operator dispatches per-node install Jobs using the upstream k8s-job-dispatcher 0.3.0 for fleet orchestration.

### Architecture

```
KataConfig
   |
   v
OSC KataConfig reconciler
   |
   v
k8s-job-dispatcher 0.3.0 (full rollout)
   |
   v
per-node install Job (7 sequential stages)
   |
   v
  1. load-kernel-modules   (kata-deploy binary, combined image)
  2. host-check            (kata-deploy binary, combined image)
  3. selinux-install        (shell, helper image, OSC stage)
  4. artifacts             (kata-deploy binary, combined image)
  5. guest-prep            (shell, helper image, OSC stage)
  6. restorecon-verify     (shell, helper image, OSC stage)
  7. cri                   (kata-deploy binary, combined image)
```

Three dispatcher variants handle the fleet lifecycle:

- **Full rollout**: Installs on all selected nodes. Uses `--cleanup-job-template` for selector convergence (nodes removed from selector get cleaned up).
- **Coverage**: Event-driven (Node create/label/taint changes), replaces upstream's CronJob. Uses `--skip-satisfied-nodes` and `--yield-to-live-run`.
- **Cleanup**: KataConfig deletion. Targets all nodes carrying `katacontainers.io/kata-runtime` regardless of value. Uses `--ignore-node-taints` and `--remove-node-label`.

Two images are used:

- **Combined image**: Upstream kata-deploy 4.2.0 binary with Red Hat artifact tarballs. Used for stages that invoke the kata-deploy binary. Currently uses upstream non-QEMU artifacts; production will use Konflux-built artifacts for all components.
- **Helper image**: Shell-capable UBI9 image for OSC host-integration stages that run shell scripts via `chroot /host` (selinux-install, guest-prep, restorecon-verify).

### MCP independence

KataDeploy does not create, select, mutate, or depend on MachineConfigPools. Node selection is purely Kubernetes label-based via `KataConfig.spec.kataConfigPoolSelector`. Customer-owned MCPs (e.g. `gpu-worker`, `worker-dpu`) remain untouched. MCO continues to manage the node's OS lifecycle independently.

### Upstream dispatcher contract

All three dispatcher variants carry the same upstream common flags:

```
--tracking-label-prefix=kata-deploy-job-dispatcher
--node-label-key=katacontainers.io/kata-runtime
--instance-label-prefix=kata-deploy.katacontainers.io
--require-node-runtime-version
--require-node-machine-id
```

OSC uses the upstream default instance (`kata-deploy.katacontainers.io/default`). Multi-instance coexistence with a separate upstream Helm installation is not currently supported.

### How artifacts reach the host

The combined image contains a tarball (`kata-static-qemu.tar.zst`) assembled at build time from Red Hat RPMs:

- QEMU 10.1.0 (dynamically linked, exec wrapper for library resolution)
- 8 private shared libraries from RHEL RPMs
- Complete Red Hat firmware (edk2-ovmf, seabios, seavgabios, ipxe-roms, qemu-kvm data files)
- virtiofsd
- osbuilder scripts and kata-agent (from kata-containers RPM)
- Boot-time systemd unit (osc-kata-guest-prep.service)

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

The module is pre-compiled as a `.pp` and installed via `semodule -i` in the selinux-install init container. Labels are applied with `restorecon -F -R` after artifact extraction. OSC stages that write to the host are privileged (mapping-only SELinux, no custom allow rules).

### Guest kernel and initrd

OSC generates a host-derived guest by running `kata-osbuilder.sh` with relocated `KATA_LIBEXEC_DIR=/var/opt/kata/libexec/kata-containers`. This produces kernel and initrd under `/var/cache/kata-containers/osbuilder-images/` matching the host's booted kernel.

A systemd unit (`osc-kata-guest-prep.service`) with `Before=kubelet.service` ordering regenerates guest artifacts on boot if the host kernel changes. The unit is installed via offline WantedBy symlink creation (no D-Bus required from the container).

### CRI-O activation

Upstream kata-deploy's `install-stage-cri` command writes a CRI-O drop-in config and restarts CRI-O. CRI-O SIGHUP live reload of runtime handlers works on OCP 4.22 / CRI-O 1.35 (validated 2026-09-04). The SIGHUP path is a separate upstream contribution; the current Job uses the upstream restart behavior.

### Controller reconciliation

The controller uses a fleet-level generation model. It does not track per-node state or inspect partial host state.

- `dispatcherDesiredGeneration`: hash of combined image, helper image, install template, cleanup template, SELinux policy, node selector, runtime handler, dispatcher image, and common dispatcher flags.
- `observedGeneration`: KataConfig annotation recorded after the full-rollout dispatcher Job completes.
- Generation change triggers a new full-rollout dispatcher.

Fleet operation serialization: `hasActiveFleetOperation()` checks for any non-terminal dispatcher Job (rollout, coverage, or cleanup). Cleanup waits for any active rollout or coverage to finish before starting.

The cleanup dispatcher Job has no OwnerReference to the KataConfig (it is part of finalization and must survive independently). It is identified by stable KataConfig-UID-based name and labels, with TTL cleanup after completion.

### RuntimeClass

RuntimeClass `kata` with handler `kata-qemu`. Scheduling uses `katacontainers.io/kata-runtime: "true"` (the label set by the dispatcher after successful install), not MCP-derived labels.

### Deployment modes

- **KataDeploy** (experimental, feature-gated): Job-based installation using kata-deploy and k8s-job-dispatcher. Designed to support both standalone OCP and HCP; HCP validation is pending.
- **MachineConfig** (default): Existing MCO-based flow. Kept for existing customers on standalone OCP.
- Legacy DaemonSet mode (rpm-ostree/apply-live) is kept but deprecated.

### Lifecycle

**Install:** k8s-job-dispatcher creates per-node Jobs for all selected nodes. Each Job runs 7 sequential stages. After all nodes complete, the controller records the observed generation.

**Upgrade:** Controller detects generation mismatch (new image, new template, new dispatcher version), creates new full-rollout dispatcher. The dispatcher uses `--cleanup-job-template` to clean up nodes that are no longer selected.

**Coverage:** KataNodeCoverageReconciler watches Node create/label/taint events. When the fleet has converged (observed == desired) and no fleet operation is active, it creates a coverage dispatcher with `--skip-satisfied-nodes` to install kata on newly eligible nodes.

**Cleanup:** KataConfig deletion triggers a cleanup dispatcher targeting all nodes with `katacontainers.io/kata-runtime` (existence selector). The cleanup pipeline is 3 stages: revert-cri, osc-cleanup (SELinux module, boot service, guest artifacts), remove-artifacts. After cleanup completes, the finalizer is removed.

**Reboot:** Files persist under `/var`. The boot-time systemd unit runs osbuilder before kubelet starts. No operator intervention needed.

## What this fixes

The entire `apply-live` path goes away. No rpm-ostree calls. No `/etc` tar extraction. No scriptlet parsing. No asymmetric uninstall. No staged-vs-live-vs-booted state tracking.

We stop killing our own pod during installation. CRI-O restart happens in the final Job stage, after all other stages complete. The Job pod is not managed by the CRI-O instance it restarts.

We release on our own schedule. A kata fix ships when it is ready, not when the next OCP z-stream opens.

The same code path is designed to support standalone OCP and HCP. One mode instead of three.

Customer-owned MCPs are preserved. A node in a `gpu-worker` or `worker-dpu` MCP stays there; KataDeploy adds kata without touching the MCP structure.

We work upstream first. The kata-deploy binary is unchanged. Downstream builds replace artifacts, not code. The k8s-job-dispatcher is an upstream component; OSC replaces only the trigger (event-driven coverage instead of CronJob) and adds the OpenShift integration stages.

## Validated (2026-09-21)

Validated on OCP 4.22 / RHCOS 9.8 / CRI-O 1.35.6 with kata-deploy 4.2.0 and k8s-job-dispatcher 0.3.0.

### Lifecycle tests (virtlab2400, single-node)

| Test | Result |
|---|---|
| Clean install (dispatcher rollout, 7-stage pipeline) | PASS |
| Kata workload after install | PASS |
| KataConfig deletion (cleanup dispatcher, per-node cleanup, finalizer) | PASS |
| kata-runtime label removed after cleanup | PASS |

### Regression tests (openshift-tests-private, 26 kata tests)

| Result | Count |
|---|---|
| Passed | 12 |
| Skipped | 10 (HCP, FIPS, Eligibility, PeerPod, metrics) |
| Failed | 4 (test framework assumptions, not kata-deploy regressions) |

All workload tests passed: pod deploy/delete, filesystem, logs, sidecar, initContainer, CPU hot-plug, runc coexistence, overhead, scale-down with running workload.

## Open items

1. **Konflux build pipeline**: Build kata-deploy binary and all artifact tarballs from source in Konflux. Currently uses upstream 4.2.0 binary and upstream non-QEMU artifacts.

2. **Multi-node lifecycle tests**: Selector change (3 to 2 node convergence), event-driven coverage (2 to 3), delete-while-rollout-active race proof.

3. **HCP validation**: Same code path, pending cluster access.

4. **MCO coexistence**: Test how MCO node updates/reboots interact with KataDeploy-installed artifacts (persistence, not membership).
