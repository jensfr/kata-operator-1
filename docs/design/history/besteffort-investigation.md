# BestEffort exit-137 investigation

> **Historical investigation record (2026-09-06).** Milestone 3 is now
> complete. The BestEffort issue is tracked separately from the installer
> work.

## Finding

BestEffort pods (no memory requests/limits) exit with 137 (SIGKILL)
immediately after starting. Pods with explicit memory resources run
stably. This affects upstream and downstream QEMU equally on
kata 4.1.0 / OCP 4.22.

## 2x2 comparison (OCP 4.22 / RHCOS 9.8, 2026-09-06)

### First cluster (flawed comparison)

| Stack | BestEffort | Explicit resources |
|---|---|---|
| Upstream QEMU 11.0.1 | Running | Running |
| Downstream QEMU 10.1.0 | Exit 137 | Running |

This comparison was misleading: the upstream node was tested after an
incomplete SELinux setup, different from the downstream test conditions.

### Second cluster (fair comparison, same SELinux on both)

| Stack | BestEffort | Explicit resources |
|---|---|---|
| Upstream QEMU 11.0.1 | **Exit 137** | Running |
| Downstream QEMU 10.1.0 | **Exit 137** | Running |

Both stacks behave identically.

QEMU command lines were compared: practically identical arguments,
same kernel, same guest image, same devices, same memory (2048M).
No relevant downstream/upstream QEMU argv difference explains the
observed behavior.

## Conclusion

Reproduces equivalently with the tested upstream and downstream stacks
on OCP 4.22 / RHCOS 9.8. Not downstream-specific. Tracked separately
from the installer work.

- The exec wrapper is ruled out as a cause
- The installer path is not the cause
- The issue is not host OOM (no MemoryPressure)
- Our downstream RHEL QEMU integration has no known behavioral
  regression compared to upstream

## What remains unresolved

- Root cause of BestEffort exit-137 on kata 4.1.0 / OCP 4.22
- Guest-visible memory/cgroup state
- Whether this reproduces on other OCP versions or kata versions
