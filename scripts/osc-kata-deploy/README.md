# KataDeploy development validation

These scripts install the experimental KataDeploy mode directly, without OLM.
Use a disposable OpenShift cluster with KVM on its workers. The E2E test
installs and removes Kata, restarts CRI-O through the upstream installer,
and leaves the operator running.

The scripts need `oc`, Python 3.9 or newer, the initialized cloud-api-adaptor submodule,
and `make kustomize`. They copy deployment configuration into a new evidence
directory under `build/_output`. They do not edit `config/manager` or change
the current context. Set `KUBECONFIG` before running a cluster command.

## Images

`images.env` pins the operator, source-built Kata 4.2.0 installer, UBI helper,
and monitor used in the October validation. The installer recipe is
[`scripts/osc-artifacts/Containerfile`](../osc-artifacts/Containerfile);
the helper supplies shell tools used by OSC integration stages. Shim, agent,
osbuilder, and installer use Kata commit
`c7351e797efff8bfc6bd73da0eb1909be12e2cfe`. The upstream installer reports
its Rust crate version, `0.1.0`, through `--version`. Set `OPERATOR_IMG`,
`KATA_DEPLOY_IMG`, `HELPER_IMG`, or `MONITOR_IMG` to override a digest. The normal installer includes the
hardware checks. The temporary HCP installer that skipped the missing-KVM
error is not a default or a supported test mode.

Build recipes and their fixed inputs are described in
[the input-lock guide](../osc-artifacts/INPUT-LOCKS.md). The installer default
is the locked source build made on October 17 with verified signed Brew RPMs.
It was published on October 20 and passed clean installation, guarded
uninstall, selector changes, real worker addition/removal, and deletion during
an active rollout on Azure OpenShift 4.22.19. All three original workers had
KVM; their UIDs and MachineSet replica counts were preserved. See the
[October validation record](../../docs/design/kata-deploy-validation-2026-10.md).

## Deploy

```bash
make kustomize
export KUBECONFIG=/path/to/kubeconfig

# Inspect the manifest without contacting the cluster.
./scripts/osc-kata-deploy/deploy-katadeploy.sh --render

# Install the operator and create example-kataconfig for the worker nodes.
./scripts/osc-kata-deploy/deploy-katadeploy.sh
oc get kataconfig -w
```

Use `--operator-only` when a KataConfig already exists. The script waits for
the Deployment rollout, serving certificate, and webhook CA injection before
creating a KataConfig. It also installs the PeerPod CRD needed by the manager,
even when peer pods are disabled.

To uninstall Kata:

```bash
./scripts/osc-kata-deploy/deploy-katadeploy.sh --undeploy --name example-kataconfig
```

This deletes only the named KataConfig and waits for finalization. Running
Kata workloads block deletion. The operator, namespace, CRDs, and RBAC stay
available for diagnostics. The script does not remove finalizers or delete
workload pods. Remove the operator separately after host cleanup completes.

## E2E install, workload, and uninstall

```bash
# Select one Ready, schedulable worker automatically.
./scripts/osc-kata-deploy/e2e-test.sh

# Or select workers explicitly.
./scripts/osc-kata-deploy/e2e-test.sh --nodes worker-a,worker-b
```

The test refuses an existing KataConfig or `osc-katadeploy-e2e` namespace.
It checks for a KVM device or CPU virtualization flags before deployment,
and lets kata-deploy load missing modules. It then verifies:

- All selected nodes appear in `readyNodeCount`, with `InProgress=False`.
- The monitor DaemonSet is ready and RuntimeClass `kata` selects installed nodes.
- Host binaries, guest images, CRI-O configuration, and the boot service exist,
  with SELinux enforcing and the expected file labels and policy modules.
- A restricted-SCC-compatible Kata pod runs, supports exec, and writes an emptyDir.
- Its guest kernel matches the kernel of its scheduled node.
- The agent embedded in the initrd has the same ELF build ID as the installed
  payload. Upstream strips the guest copy, which changes its SHA256.
- Deleting KataConfig reports `BlockedByExistingKataPods`, retains the finalizer,
  preserves the workload, and starts no cleanup dispatcher for that KataConfig UID.
- Deleting the test workload allows finalization, RuntimeClass and monitor removal,
  runtime label removal, and host file and SELinux module cleanup.

On failure, resources remain for inspection. The evidence directory contains
the rendered manifest, KataConfig, snapshots, controller logs, and host checks.
A successful run writes `results.json`. Credentials are not copied into the
directory. Do not commit evidence containing cluster details.

## Guest image verification failures

The install Job compares the installed agent and guest initrd ELF build IDs
before enabling the boot service or configuring CRI-O. A missing, unreadable,
or different guest agent fails `guest-prep`; later stages do not run.
The failed container records its last log lines in its termination message.
Once the Job exhausts its retries, the controller reports the detail in
KataConfig's `InProgress=True`,
`Reason=Failed`, and lists the node in `failedToInstall`.
`kata_config_installation_success` stays 0. The controller accepts termination
messages only from Pods owned by the current Job UID. After Job/Pod cleanup,
the persistent dispatcher result still reports failure; retain container logs
if their detail is needed later.

```bash
oc get kataconfig example-kataconfig -o yaml
oc get jobs -n openshift-sandboxed-containers-operator
oc logs -n openshift-sandboxed-containers-operator job/"$JOB" -c guest-prep
```

## Local regression checks

```bash
PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover \
  -s scripts/osc-kata-deploy -p 'test_*.py' -v
bash -n scripts/osc-kata-deploy/{deploy-katadeploy,e2e-test}.sh
go test ./controllers/... -count=1
go build ./...
```

The Python tests exercise rendering against the actual checkout and check
that missing status, API failures, and old cleanup Jobs cannot produce a
false pass. They do not replace live cluster tests.

## Installed-cluster checks, reboot, and kernel upgrade

Installation rebuilds the guest initrd even when the kernel is unchanged, so
an agent upgrade reaches new VMs. Boot preparation can reuse an initrd for
the same kernel and rebuilds it when the host kernel changes.

`lifecycle-test.sh` checks an existing installation. It does not reboot or
upgrade the cluster. It creates a temporary namespace and Kata pod on the
named node, then removes them after success. Use a new output directory for
each phase. A failed phase keeps its test pod for inspection.

```bash
./scripts/osc-kata-deploy/lifecycle-test.sh --node "$NODE" \
  --output build/_output/lifecycle-before

# Reboot a disposable test node. On SNO this interrupts the API and all workloads.
oc debug node/"$NODE" -- chroot /host systemctl reboot

# Wait for API access and node readiness to return, then:
./scripts/osc-kata-deploy/lifecycle-test.sh --node "$NODE" \
  --after build/_output/lifecycle-before/baseline.json \
  --output build/_output/lifecycle-after-reboot
```

The after check requires a new boot ID on the same cluster, node UID, and
KataConfig UID. It checks that guest preparation ran successfully during
this boot, then starts a fresh Kata VM and compares guest and host kernels.

For an OS update, capture another baseline, apply an advertised OpenShift
update through the cluster's normal update mechanism, wait for convergence,
and run the same after check with `--expect-kernel-change`. A reboot into the
same kernel cannot pass as kernel upgrade evidence. Do not replace the host
kernel by hand.

## Rollout recovery and selector guard

To test dispatcher and installer interruption on an installed, idle node set:

```bash
./scripts/osc-kata-deploy/recovery-test.sh \
  --output build/_output/kata-recovery
```

The script triggers two rollouts by adding a selector expression that excludes
an absent hostname. The selected node set stays the same. It deletes a running
dispatcher pod in one rollout and a pod executing an installer stage in the
other. It checks convergence and that at most one node mutation Job is active.
It restores the original selector after each case. Active Kata workloads or
another fleet operation stop the test before it changes the selector.
It then starts a test workload and excludes its node from the selector. The
controller must report `BlockedByExistingKataPods`, preserve the pod, and
start no cleanup. Restoring the selector must return status to ready while
that workload still supports exec. The test removes its own namespace after
these checks pass.

## Multi-node lifecycle checks

Use at least two disposable workers. Install the operator, then run:

```bash
./scripts/osc-kata-deploy/deploy-katadeploy.sh --operator-only
oc get machinesets.machine.openshift.io -n openshift-machine-api
./scripts/osc-kata-deploy/multinode-test.sh --machineset "$WORKER_MACHINESET" \
  --output build/_output/kata-multinode
```

The test requires no existing KataConfig, test namespace, or MachineAutoscaler.
It installs Kata on all workers, checks selector shrink and expansion with live
workloads, then scales the named worker MachineSet up by one. Coverage must
install the new worker without changing the rollout generation. A fresh Kata
VM must run there. The test marks only its added Machine for deletion and
restores the original replica count. It verifies that the original worker UIDs
survive and that status drops the removed worker. It then starts a new rollout
and deletes KataConfig while an installer stage runs. Cleanup must wait for
active work, and no new install dispatcher may start after deletion. The test
checks host cleanup and removes its namespace. Failure preserves evidence and
test resources; scale restoration refuses unexpected MachineSet or Machine changes.

The deletion annotation and scale-down sequence follow
[OpenShift machine management](https://docs.redhat.com/en/documentation/openshift_container_platform/4.21/html-single/machine_management/machine_management).
Snapshots and manager logs are saved at each transition.

| Case | Action | Required evidence |
|---|---|---|
| Selector shrink | Patch `kataConfigPoolSelector` to exclude an installed worker | A live Kata pod on that worker blocks cleanup and survives; after deleting it, that worker is cleaned and retained workers stay ready |
| Selector expansion | Add another worker to `kataConfigPoolSelector` | A new rollout installs it; completed and total counts increase together |
| Node addition | Add a worker matching an unchanged selector | A coverage dispatcher installs it without changing the observed generation |
| Node removal | Remove an idle selected worker through its infrastructure owner | Total and completed counts shrink; no stale node remains in status |
| Interrupted rollout | Delete an active dispatcher pod, then an active per-node installer pod in a separate run | The controller retries, only one node mutation runs at a time, and installation converges |
| Delete during rollout | Delete KataConfig while installation is active | No new rollout starts; cleanup waits for active work and retains finalizers until cleanup completes |

Node addition and removal need infrastructure access. Removing a Node object
alone does not test replacement of a real worker. The controller regression
tests cover selector workload guards, stale results, node events, and
serialization; they do not prove host behavior during these transitions.
