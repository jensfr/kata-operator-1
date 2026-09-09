# Milestone 3 validation record

## Result: PASSED

**Date:** 2026-09-06/07
**Cluster:** OCP 4.22.0-0.nightly-2026-09-02-125503
**Node OS:** Red Hat Enterprise Linux CoreOS 9.8.20260901-0 (Plow)
**Kernel:** 5.14.0-687.44.1.el9_8.x86_64
**CRI-O:** 1.35.6

## Images

| Image | Digest |
|---|---|
| Combined (kata-deploy 4.1.0 + Red Hat artifacts) | `quay.io/jensfr/osc-kata-deploy@sha256:18ab8c571b8123482af95eab941def467531a17675766b774fed35ca4be68e25` |
| Helper (UBI-minimal + shell tools) | `quay.io/jensfr/osc-helper@sha256:ea1eeb97d480b84ce5b5d4103346daf06c27246112b517adbbb537ff0fe60c85` |
| Upstream pin | `quay.io/kata-containers/kata-deploy:4.1.0` (git ddcb1ad8d23cbb4323f86c209f132b89592902df) |

## Test results

### Installation (6-stage Job on clean node)

| Stage | Image | Result |
|---|---|---|
| host-check | combined | exit 0 |
| selinux-install | helper | exit 0 |
| install-artifacts | combined | exit 0 |
| guest-prep | helper | exit 0 |
| apply-labels + verify | helper | exit 0 |
| configure-cri | combined | exit 0 |

### Guest preparation

- osbuilder ran with `KATA_LIBEXEC_DIR=/var/opt/kata/libexec/kata-containers`
- `kata.kernel` -> `/usr/lib/modules/5.14.0-687.44.1.el9_8.x86_64/vmlinuz`
- `kata.initrd` generated (40M)
- Boot service unit (`osc-kata-guest-prep.service`) installed and enabled
  via WantedBy symlink in `kubelet.service.wants/`

### Kata configuration

```
kernel = "/var/cache/kata-containers/osbuilder-images/kata.kernel"
initrd = "/var/cache/kata-containers/osbuilder-images/kata.initrd"
```

No `image =` line present.

### SELinux verification (apply-labels stage)

All paths verified with `matchpathcon -V`:

| Path | Expected | Result |
|---|---|---|
| `/var/opt/kata/bin/qemu-system-x86_64` | `qemu_exec_t` | verified |
| `/var/opt/kata/bin/qemu-system-x86_64.real` | `qemu_exec_t` | verified |
| `/var/opt/kata/libexec/virtiofsd` | `bin_t` | verified |
| `/var/opt/kata/share/qemu-kvm/bios-256k.bin` | `usr_t` | verified |
| `/var/cache/kata-containers/osbuilder-images/kata.kernel` | (host default) | verified |
| `/var/cache/kata-containers/osbuilder-images/kata.initrd` | (host default) | verified |

### Kata workload (before reboot)

```
kata-test   1/1     Running
uname -r: 5.14.0-687.44.1.el9_8.x86_64
```

Guest kernel matches host kernel.

### Reboot test

Node rebooted via `systemctl reboot`. After ~6 minutes, node returned to
Ready status.

**Boot service log:**

```
Sep 07 08:26:22 systemd[1]: Starting OSC: Generate Kata guest image for host kernel...
Sep 07 08:26:22 kata-osbuilder.sh[1119]: + INFO: Nothing to do. The OSBUILDER images are already installed
Sep 07 08:26:22 systemd[1]: Finished OSC: Generate Kata guest image for host kernel.
```

The boot service ran before kubelet. It detected existing images matched
the booted kernel and exited without rebuilding (idempotent).

**Post-reboot state:**

| Check | Result |
|---|---|
| Boot service | `osc-kata-guest-prep.service`: loaded, enabled, exit 0/SUCCESS |
| kata.kernel | -> `/usr/lib/modules/5.14.0-687.44.1.el9_8.x86_64/vmlinuz` |
| kata.initrd | exists (kernel-versioned symlink) |
| Kata config | `kernel =` and `initrd =` point to host-derived paths |
| CRI-O handler | `99-kata-deploy` config present |
| Boot unit symlink | `/etc/systemd/system/kubelet.service.wants/osc-kata-guest-prep.service` |

### Kata workload (after reboot)

```
kata-post-reboot   1/1     Running
uname -r: 5.14.0-687.44.1.el9_8.x86_64
Linux version 5.14.0-687.44.1.el9_8.x86_64 (mockbuild@x86-64-03.build.eng.rdu2.redhat.com)
```

Guest kernel matches host kernel. No operator intervention after reboot.

## Earlier lifecycle tests (2026-09-06, separate cluster)

| Test | Result |
|---|---|
| Idempotent reinstall | PASS |
| Interrupted fresh install + recovery | PASS |
| Interrupted CRI-stage + recovery | PASS |
| Full cleanup (upstream stages + SELinux removal) | PASS |
| Reinstall after cleanup | PASS |
| Kata workload with explicit resources after each operation | PASS |

Key properties proven:
- Partial artifact mutation state converges on re-application
- Partial CRI activation state converges on re-application
- The reconciler can safely re-run the full Job without knowing where the
  previous attempt stopped

## Known issues

**BestEffort exit-137:** Pods without explicit memory resources exit with
SIGKILL. Reproduces equivalently with upstream and downstream QEMU on
OCP 4.22. Not downstream-specific. Tracked separately; see
`docs/design/history/besteffort-investigation.md`.

**Privileged containers:** All host-mutating stages run with
`privileged: true`. OpenShift SELinux denies `container_t` write on
`var_lock_t` (/run/lock). This is a documented prototype decision;
narrower SCC + targeted SELinux policy is future work.

## Pinned references

- Upstream kata-deploy: v4.1.0, git `ddcb1ad8d23cbb4323f86c209f132b89592902df`
- kata-containers RPM: `kata-containers-3.31.0-5.rhaos4.22.el9.x86_64`
- QEMU: `qemu-kvm-core-10.1.0` (RHEL 9, dynamically linked)
- SELinux module: `osc_kata_deploy.pp` (file-context mappings only)
