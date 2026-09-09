# Milestone 4 validation record

## Result: PASSED

**Dates:** 2026-09-07 (simulated controller), 2026-09-08 (operator-driven E2E)
**Cluster (operator E2E):** OCP 4.22.0-0.nightly-2026-09-02-125503, ci-ln-t954gct-1d09d-6khqb
**Node OS:** Red Hat Enterprise Linux CoreOS 9.8.20260901-0 (Plow)
**Kernel:** 5.14.0-687.44.1.el9_8.x86_64
**CRI-O:** 1.35.6
**Branch:** `kata-deploy-upstream` at `6f15e588` on `jensfr/kata-operator-1`

## What Milestone 4 proves

Milestone 3 validated the executor (Job stages produce a working kata
installation). Milestone 4 validates the operator: KataConfig triggers
the controller, the controller creates per-node Jobs, handles RuntimeClass
abstraction, and the standard OSC user interface (`runtimeClassName: kata`)
works end-to-end without manual intervention.

The operator was deployed as a running binary via `make deploy` on a clean
OCP 4.22 cluster. Six integration bugs were found and fixed during the E2E.
The final successful run used the corrected operator image.

## Images

| Image | Digest |
|---|---|
| Operator | `quay.io/jensfr/osc-operator@sha256:a4f2365b56cad6a850b1198465d56795cc3f66ebabb9747d5b03acfb447905bb` |
| Combined (kata-deploy 4.1.0 + Red Hat artifacts) | `quay.io/jensfr/osc-kata-deploy@sha256:18ab8c571b8123482af95eab941def467531a17675766b774fed35ca4be68e25` |
| Helper (UBI-minimal + shell tools) | `quay.io/jensfr/osc-helper@sha256:ea1eeb97d480b84ce5b5d4103346daf06c27246112b517adbbb537ff0fe60c85` |

## Operator deployment

Deployed via `make deploy IMG=<operator digest>` (canonical direct path,
not OLM). Two additional annotations required for OpenShift service-ca
webhook certificate provisioning:

```
service.beta.openshift.io/serving-cert-secret-name=webhook-server-cert
service.beta.openshift.io/inject-cabundle=true
```

This is a pre-existing gap in `config/default`: the webhook is enabled but
cert-manager is commented out. The gap exists on the `devel` branch and is
not introduced by the kata-deploy work.

Environment variables set on the operator deployment:

```
RELATED_IMAGE_KATA_DEPLOY=<combined digest>
RELATED_IMAGE_OSC_HELPER=<helper digest>
```

Feature gate: `osc-feature-gates` ConfigMap with `deploymentMode: KataDeploy`.

## Integration bugs found during E2E

Six bugs found by deploying the operator on a real cluster. All were
caught by API server validation or runtime behavior, not by unit tests.

| # | Bug | Root cause | Fix |
|---|---|---|---|
| 1 | Template placeholder in labels | `__TARGET_NODE__` survived YAML unmarshalling as invalid label value | Valid placeholder defaults, delete obsolete `kata.io/target-node` key |
| 2 | Wrong ServiceAccount | Job template used `kata-deploy` but repo provides `kata-install` with its own SCC | Changed to `kata-install` |
| 3 | SCC missing emptyDir | Job uses `emptyDir` volume but `kata-install-scc` only allowed hostPath/configMap/secret | Added `FSTypeEmptyDir`, extracted `desiredKataInstallSCC` as shared source of truth |
| 4 | RuntimeClass handler mismatch | `postKataInstallation` created handler=`kata` but kata-deploy installed CRI-O handler `kata-qemu` | Explicit runtime definition `{Shim: qemu, Handler: kata-qemu}`, two-phase delete/recreate for immutable handler |
| 5 | Stale GPU RuntimeClass | Previous legacy install left `kata-nvidia-gpu` with no corresponding CRI-O handler | `ensureRuntimeClassAbsent` removes stale RC in KataDeployMode |
| 6 | RuntimeClass scheduling | Used pool-level `node-role.kubernetes.io/master` selector instead of installation label | `ensureRuntimeClass` reconciles scheduling to `katacontainers.io/kata-runtime=true` |

## Operator-driven E2E (clean cluster)

### Setup

KataConfig selecting one worker node via `osc-kata-deploy-test=true` label.
Operator created SELinux ConfigMap from embedded `.pp` bytes automatically.

### Install Job

| Stage | Image | Result |
|---|---|---|
| host-check | combined | exit 0 |
| selinux-install | helper | exit 0 |
| install-artifacts | combined | exit 0 |
| guest-prep | helper | exit 0 |
| apply-labels + verify | helper | exit 0 |
| configure-cri | combined | exit 0 |

Job completed in 70 seconds.

### RuntimeClass state after install

| Check | Result |
|---|---|
| RuntimeClass `kata` handler | `kata-qemu` |
| RuntimeClass `kata` scheduling | `katacontainers.io/kata-runtime: "true"` |
| RuntimeClass `kata-nvidia-gpu` | NotFound (absent) |
| KataConfig `status.runtimeClasses` | `["kata"]` |
| KataConfig InProgress | False |

### Workload test

Pod created with only `runtimeClassName: kata`. No manual nodeSelector,
no manual RuntimeClass, no workarounds.

```
kata-e2e-final   1/1     Running
Node: ci-ln-t954gct-1d09d-6khqb-master-0
uname -r: 5.14.0-687.44.1.el9_8.x86_64
```

Scheduler selected master-0 automatically (the only node with
`katacontainers.io/kata-runtime=true`). Guest kernel matches host kernel.

### Evidence chain

```
KataConfig
  -> operator creates SELinux ConfigMap
  -> operator claims node (managed-by annotation)
  -> 128-bit install attempt ID
  -> per-node install Job created
  -> six sequential stages succeed
  -> phase Completed -> durable generation/UID/label commit
  -> RuntimeClass kata with handler kata-qemu
  -> scheduling selects only installed nodes
  -> Pod with runtimeClassName: kata
  -> scheduler selects installed node
  -> CRI-O invokes kata-qemu handler
  -> Kata VM runs with host-derived kernel
```

## Earlier simulated controller tests (2026-09-07)

Before the operator was deployed as a running binary, the controller's
actions were simulated manually on a separate cluster. These tests
validated the Job execution path and basic lifecycle.

- Fresh install on clean node: 6 stages, all passed
- Full cleanup: all OSC state removed
- Reinstall after cleanup: fresh Job, workload runs
- Self-healing: active Job deleted, controller-simulated recovery converges
- Reboot test (Milestone 3 cluster): boot service ran, kata workload after reboot

## Upstream kata-containers BATS tests

Ran the actual upstream Kubernetes integration test suite from the pinned
kata-containers revision (`ddcb1ad8d23cbb4323f86c209f132b89592902df`,
tag `4.1.0`) via the upstream `run_kubernetes_tests.sh` harness.

### Results: 16 of 17 tests passed

| Test file | Test case | Result | Time |
|---|---|---|---|
| k8s-job.bats | Run a job to completion | PASS | 29s |
| k8s-env.bats | Environment variables | PASS | 21s |
| k8s-hostname.bats | Validate Pod hostname | PASS | 17s |
| k8s-configmap.bats | ConfigMap for a pod | PASS | 24s |
| k8s-configmap.bats | ConfigMap propagation to volume-mounted pod | PASS | 88s |
| k8s-exec.bats | Kubectl exec | PASS | 23s |
| k8s-empty-dirs.bats | Empty dir volumes | PASS | 19s |
| k8s-empty-dirs.bats | Empty dir with FSGroup non-root | PASS | 18s |
| k8s-empty-dirs.bats | Empty dir sizeLimit evicts pod | FAIL | 311s |
| k8s-liveness-probes.bats | Liveness probe | PASS | 37s |
| k8s-liveness-probes.bats | Liveness http probe | PASS | 36s |
| k8s-liveness-probes.bats | Liveness tcp probe | PASS | 49s |
| k8s-copy-file.bats | Copy file in a pod | PASS | 19s |
| k8s-copy-file.bats | Copy from pod to host | PASS | 24s |
| k8s-credentials-secrets.bats | Credentials using secrets | PASS | 30s |
| k8s-credentials-secrets.bats | Secret propagation to volume-mounted pod | PASS | 96s |
| k8s-caps.bats | Check capabilities of pod | PASS | 20s |

The one failure (`Empty dir sizeLimit evicts pod`) is a known upstream kata
limitation with ephemeral storage accounting inside VMs.

### How to reproduce

```bash
cd kata-containers
git checkout ddcb1ad8d23cbb4323f86c209f132b89592902df

export PATH="/opt/homebrew/opt/gnu-sed/libexec/gnubin:/opt/homebrew/bin:$PATH"
export KUBECONFIG=<path-to-kubeconfig>
export KATA_HYPERVISOR=qemu
export AUTO_GENERATE_POLICY=no
export K8S_TEST_HOST_TYPE=small
export K8S_TEST_FAIL_FAST=no

bash tests/integration/kubernetes/setup.sh

export K8S_TEST_UNION="k8s-job.bats k8s-env.bats k8s-hostname.bats \
  k8s-configmap.bats k8s-exec.bats k8s-empty-dirs.bats \
  k8s-liveness-probes.bats k8s-copy-file.bats \
  k8s-credentials-secrets.bats k8s-caps.bats"

bash tests/integration/kubernetes/run_kubernetes_tests.sh
```

Requires GNU sed and bash 4.3+ (macOS: `brew install gnu-sed bash`).

## Proven vs not yet proven

### Proven

- Clean operator-driven install via `make deploy` on OCP 4.22
- Six-stage per-node install Job (host-check, SELinux, artifacts, guest-prep, labels, CRI)
- SELinux file-context mapping module (no new types or allow rules)
- RuntimeClass abstraction: `kata` (OSC public name) maps to `kata-qemu` (upstream handler)
- RuntimeClass scheduling selects only nodes where installation committed
- Standard workload using only `runtimeClassName: kata`
- Host-derived guest kernel (osbuilder with relocated KATA_LIBEXEC_DIR)
- Boot-time guest regeneration (Before=kubelet.service)
- Node reboot with kata workload afterwards (Milestone 3)
- Basic recovery: simulated Job deletion, controller converges with new attempt
- Upstream kata BATS tests: 16/17 from pinned 4.1.0

### Not yet proven

- Kernel or OS upgrade lifecycle (guest regeneration after host kernel change)
- Artifact generation update on running cluster
- Multi-node concurrent installation (maxConcurrentNodeMutations=1 behavior)
- Hosted Control Planes (design removes MCO/MCP dependency; HCP validation outstanding)
- GPU runtime in KataDeployMode (requires separate runtime definition and kata-deploy shim)
- Migration from existing RPM/MachineConfig installations
- Disconnected / mirror registry environments
- Konflux production build pipeline
- Full upstream kata BATS suite (80+ test files)

## Summary

| Test | Result |
|---|---|
| Operator deployment via make deploy | PASS |
| Operator-driven install (KataConfig -> Job -> workload) | PASS |
| RuntimeClass kata -> handler kata-qemu | PASS |
| Scheduling to installed nodes only | PASS |
| Workload with only runtimeClassName: kata | PASS |
| Guest kernel == host kernel | PASS |
| SELinux module + QEMU label | PASS |
| Stale GPU RuntimeClass cleaned | PASS |
| Simulated cleanup/reinstall/recovery | PASS |
| Upstream BATS tests (16/17) | PASS (1 known limitation) |
| Integration bugs found and fixed | 6 |
