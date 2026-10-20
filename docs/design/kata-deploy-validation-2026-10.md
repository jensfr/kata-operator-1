# KataDeploy validation, October 2026

This record covers the current source-based Kata 4.2.0 payload. The September
milestone records describe earlier images and remain historical evidence.

## Completed live tests

| Environment | Image | Result |
|---|---|---|
| Virtlab2400, OpenShift 4.22.16, 2026-10-15 | Operator `7dc59800fd30`, installer `522713e3686d` | Install, status 1/1, Kata workload, exec, emptyDir, monitor metrics, guarded uninstall, cleanup, and recreation passed |
| Virtlab2400, OpenShift 4.22.16, 2026-10-16 | Operator `27405bd754fb`, installer `522713e3686d` | Physical node reboot, boot service, SELinux labels, status 1/1, and a fresh Kata VM with exec and emptyDir passed |
| Virtlab2400, OpenShift 4.22.16 to 4.22.18, 2026-10-16 | Operator `27405bd754fb`, installer `522713e3686d` | New host kernel, regenerated initrd at boot, host files and SELinux checks, status 1/1, and a fresh Kata VM with exec and emptyDir passed |
| Virtlab2400, OpenShift 4.22.18, 2026-10-16 | Operator `27405bd754fb`, installer `522713e3686d` | Dispatcher and installer pod interruption recovered; at most one node mutation Job active; selector removal blocked and preserved its live workload |
| Azure Cluster Bot, OpenShift 4.22.18, two workers, 2026-10-16 | Operator `27405bd754fb`, installer `522713e3686d` | Install, status 2/2, monitors, host and SELinux checks, real Kata VM, exec, emptyDir, guest kernel, guarded uninstall, and cleanup passed |
| Azure Cluster Bot, final two-worker E2E, 2026-10-16 | Patched private operator `16cc1528141a`, installer `522713e3686d` | Install, status 2/2, monitors, host and SELinux checks, real Kata VM, exec, emptyDir, guest kernel, workload preservation during blocked uninstall, and full cleanup passed |
| Azure Cluster Bot, published standard operator, two workers, 2026-10-16 | Operator `e8feb39e5af6`, installer `522713e3686d` | Default-pin E2E passed: install, status 2/2, monitors, host and SELinux checks, real Kata VM, exec, emptyDir, guest kernel, guarded uninstall, preserved workload, and full cleanup |
| Azure Cluster Bot, three workers, 2026-10-16 | Patched operator, private build `34d4ca230d6c` then `16cc1528141a`; installer `522713e3686d` | Selector shrink and expansion, real MachineSet scale from 3 to 4 and back, Kata VM on the new worker, unchanged coverage generation, and serialized deletion during rollout passed |
| ROSA HCP 4.22.18, two workers, 2026-10-16 | Operator `27405bd754fb`, installer `01918db46c5f` | Installation and cleanup on both workers, status 2/2, monitors 2/2, Prometheus scraping, and pending-workload uninstall guard passed |
| Virtlab2400, source installer/helper upgrade, OpenShift 4.22.18, 2026-10-16 | Operator `4cf0a61684a2`, installer `a9c0e0727bb2`, helper `55ea8ffb66c5` | Existing installation upgraded; KataConfig UID, spec, and generation preserved; refreshed guest agent, status 1/1, SELinux, and a fresh Kata VM with exec, emptyDir, and matching host kernel passed |
| Azure Cluster Bot, OpenShift 4.22.19, 2026-10-20 | Operator digest `facd85bd54eb`, locked installer digest `a7fb3db3fcd2`, helper digest `59e48faa1135` | Two-worker E2E and three-worker lifecycle passed: clean install, real Kata VM, guest identity, guarded uninstall, selector shrink/expansion, real worker addition/removal, unchanged coverage generation, deletion during rollout, and full host cleanup |

The HCP workers were `m7i.xlarge` without `/dev/kvm`. The normal installer
reported the missing virtualization backend. The HCP deployment test used a
temporary image that accepted only that exact failure from the kernel-module
stage. It did not validate VM execution or kernel-module persistence. The
normal installer was restored after cleanup. No hardware check bypass was
added to the operator or the checked-in test scripts.

The reboot test retained the node and KataConfig UIDs and changed the node's
boot ID. `osc-kata-guest-prep.service` ran successfully during the new boot.
The existing images matched host kernel `5.14.0-687.50.1.el9_8.x86_64`, so the
service reused them. A fresh Kata VM reported that same kernel. Reboot did
not create a per-node install Job; a coverage dispatcher skipped the satisfied
node. This tests persistence across a reboot with the same kernel.

The OpenShift update changed RHCOS from `9.8.20260922-1` to
`9.8.20261006-0` and the kernel from `5.14.0-687.50.1.el9_8.x86_64` to
`5.14.0-687.55.1.el9_8.x86_64`. The boot journal records a successful initrd
rebuild before kubelet started. A fresh Kata VM used the new host kernel.
The node and KataConfig UIDs remained the same.

All three Azure workers were `Standard_D4as_v5` with `/dev/kvm` and CPU
virtualization flags exposed. The two-worker E2E run used the normal installer
without a hardware-check exception. Its Kata VM used host kernel
`5.14.0-687.55.1.el9_8.x86_64`.

The multi-node run found a selector regression in operator `27405bd754fb`.
Changing the selector from three workers to two and back reused a completed
rollout Job. The excluded worker stayed uninstalled and status remained 2/3.
The fix includes the KataConfig spec generation in the rollout hash. Recording
completion now patches only metadata with an optimistic lock, so nullable spec
fields do not cause another rollout.

The patched controller recovered the stalled installation. Machine API then
added a fourth worker. Event-driven coverage installed Kata without changing
the observed rollout hash. A fresh Kata VM passed exec and guest-kernel checks
on that worker, and all four monitors were ready. The test removed only the
added Machine, restored the original three replicas, and preserved every
original node UID. Deleting KataConfig during a later installer stage waited
for the active rollout before cleanup. No new install dispatcher started after
deletion, at most one node mutation Job ran at a time, and host cleanup passed.

These patched operator images were built in the disposable cluster's private
registry. They were not published to Quay. The installer and helper remained
the public digests in `images.env`; the new local source builds were not used.
After explicit approval, the standard operator image was published as
`quay.io/jensfr/osc-operator:selector-revert-20261016`, digest
`sha256:e8feb39e5af66131376354bbada90ed0122947a45608d4332f20934e005848c4`.
An anonymous registry read verified the manifest digest and its config matched
the locally built image. The shared scripts pinned this operator at that stage.
At that point approval covered only the operator. The source installer and
helper were published later for the Virtlab upgrade described below.
The published standard operator then passed the two-worker E2E using that
default pin. It pulled and started successfully, ran a real Kata VM, preserved
the workload while uninstall was blocked, and completed host cleanup after the
test removed its own workload. The operator remained running for diagnostics.

A fresh three-worker selector test then passed on final private operator
`16cc1528141a` without restarting the controller. The retained workload kept
its pod UID and exec access through shrink and expansion. Restoring the
selector created a different rollout hash. Spec generation changed from 2 to
4, exactly once for each selector patch. Completion acknowledgement did not
change the spec. The test ended with successful host cleanup on all workers.

The last HCP run used source commit `a023c5a7` and checked both workers for
runtime binaries, host-derived guest images, CRI-O handler, boot service,
and SELinux labels. Deleting KataConfig with a pending Kata pod retained
the finalizer, reported `BlockedByExistingKataPods`, preserved the pod, and
started no cleanup dispatcher. Removing that pod allowed full cleanup.

The source-built installer and UBI helper were later published as
`quay.io/jensfr/osc-kata-deploy:fork-source-20261016` and
`quay.io/jensfr/osc-helper:fork-source-20261016`. Anonymous registry reads
verified both manifest digests and their local image identities. Virtlab2400
then upgraded using those exact digests, without replacing or deleting its
KataConfig. Its UID, spec, selector, and spec generation 10 stayed unchanged.

This upgrade exposed an OSC guest-preparation bug. Installation used the
osbuilder cache check, which considers the kernel but not the agent. The new
agent reached the host while the guest kept the old initrd. Installation now
forces a rebuild. Boot preparation still reuses images for an unchanged kernel.
Upstream rootfs preparation strips the guest agent, so validation compares ELF
build IDs rather than raw checksums. The rebuilt guest and source payload both
reported build ID `66ef3e01080af6f8dd67cee2a9c6e44817504cd5`.

The operator fix was built from a clean export with the committed submodule,
using the cached standard builder and `make build`, including `go vet` and
CGO-enabled `aws,azure` compilation. Its UBI container passed `--help`.
The published digest is
`sha256:4cf0a61684a24861b3d5ede326082b2c70821e2f3c8d2f187848bfda2b2de775`.
With that operator, source installer `a9c0e0727bb2`, and helper `55ea8ffb66c5`,
status converged to 1/1. Host files, SELinux, guest-agent identity, and a fresh
Kata VM passed. Exec and emptyDir worked, and the guest used host kernel
`5.14.0-687.55.1.el9_8.x86_64`. The test removed only its temporary namespace.
Virtlab's installation remained in place. No reboot or uninstall was performed.

The normal image digests are pinned in
[`images.env`](../../scripts/osc-kata-deploy/images.env). Reproduction commands
and script behavior are documented in
[`scripts/osc-kata-deploy/README.md`](../../scripts/osc-kata-deploy/README.md).

## Local validation

Controller tests and a full Go build passed before the branch was pushed on
2026-10-16. They also passed in an exported source tree using the committed
cloud-api-adaptor gitlink `784b3d7dff17abd465b5996fb56d2be7f368d618`.
The developer checkout used a different submodule revision, so both were checked.

The normal operator Dockerfile also built successfully from a clean export
with that committed gitlink. It passed `go vet` and the CGO-enabled
`linux/amd64` build with `aws,azure` tags. The packaged binary passed `--help`
on UBI. This is the operator image published after approval. The earlier live
tests used the private Kata-only controller build.

The development scripts passed 12 Python checks, shell syntax, shellcheck, and
a real Kustomize render. The fork Konflux renderer passed three checks and
rendered PipelineRun validation. Both helper and complete installer images
built locally on October 16. The installer, shim, and agent provenance all
match Kata commit `c7351e797efff8bfc6bd73da0eb1909be12e2cfe`.
Local image builds do not prove a Konflux tenant run. The later Virtlab
upgrade checks the published source images on a live cluster.

Generated DeepCopy code is ignored by Git. A clean source export needs:

```bash
go tool sigs.k8s.io/controller-tools/cmd/controller-gen \
  object:headerFile=hack/boilerplate.go.txt 'paths={./api/v1,./cmd/manager,./controllers}'
go test -mod=readonly ./controllers/... -count=1
go build -mod=readonly ./...
```

## Guest identity checks and fixed build inputs

The install Job now verifies the guest agent before boot-service and CRI-O
configuration. A mismatch, missing build ID, or failed extraction terminates
`guest-prep`. Its termination message uses the failed container's logs.
The controller reads that detail only from Pods owned by the current Job UID.
A regression checks that an older success cannot hide this failure, that
`failedToInstall` names the node, and that the installation-success metric is 0.
A Pod from a previous Job with the same name cannot supply the error detail.

Read-only host checks on Virtlab accepted the installed agent and rejected a
different agent or missing initrd. Real Kubernetes Jobs repeated the matching
and mismatching cases with a read-only host mount and temporary scratch volume.
The mismatch exited 1, retained the FATAL message in Pod status, and never
started the following container. Only those test Jobs were removed.

The published operator is `quay.io/jensfr/osc-operator:agent-check-20261016`,
digest `sha256:facd85bd54eb2975d277e79625317da070670505599ffac32b77dd9ed76202ef`.
The normal CGO-enabled `aws,azure` build and container startup check passed.
Virtlab upgraded with its KataConfig UID, spec, selector, and generation
unchanged, then passed host checks and a fresh Kata VM with exec and emptyDir.

The locked helper was published as
`quay.io/jensfr/osc-helper:locked-inputs-20261016`, digest
`sha256:59e48faa11351b5935d7f05e2dda2336feacf019c8b73e778a2328395a0f3ce2`.
An anonymous manifest read matched its config to the local image. With the new
operator and unchanged installer `a9c0e0727bb2`, Virtlab passed another
preserved-installation rollout. The actual install Job logged a verified guest
agent build ID. Host checks, SELinux, monitor readiness, and a fresh Kata VM
passed. The original node, boot ID, KataConfig, and existing failed workload
remained in place. The scripts pin this tested operator/helper combination.

The installer and helper recipes now lock architecture-specific base manifests,
Go SDK content, source dependencies, and the full installed RPM inventory with
header and payload hashes. The helper built successfully from its locked
inputs. The full installer rebuilt and passed its payload and UBI contracts
using verified existing package/compiler caches. That validation image stays
local and records its cache inputs. It is not fresh package-install evidence.

A fresh canonical installer build stopped at RHEL BaseOS metadata with HTTP
403 on October 16. The existing entitlement was within its validity period
and matched its key; a direct request with Red Hat's CA reproduced 403. This blocked that day's
fresh package check. At that point the public installer pin
stayed at `a9c0e0727bb2`; the cached validation build was not a deployment default.

Controller tests, 17 development-script Python checks, three Konflux renderer
checks, shell syntax, and shellcheck passed. The new tests reject floating
bases, changed package locks, source/SDK argument overrides, changed RPM
content at the same version, and extra dependencies.

## Fresh installer build through an RPM mirror, October 17

The canonical installer recipe passed a fresh local build using signed RPMs
from Brew. A fixed RHEL compose was reachable through the VPN, but lacked 17
older package identities from the locks. The fetcher downloaded the 168 signed
RPMs needed beyond the pinned RHEL base images and verified their archive
digests, NEVRAs, header SHA256, and payload SHA256 before installation.

Each RHEL stage installed those RPMs from a local repository with GPG checks
enabled. All four complete inventories matched: shim/runtime 293, CentOS agent
288, RHEL components 212, and assembler 138, for 931 stage entries. The CentOS
stage used its existing public repositories. Source compilation used the same
Kata commit and Go SDK locks. No old compiler image replaced a package stage.
The newly built stages were reused to assemble the final image.

Local image `localhost/osc-kata-deploy:internal-locked-20261017` has image ID
`c44799fe68d2932e8210e014984cf1129f3dda29e2f0fd4ad400506c782f164a`.
All four component tarballs passed extraction and file checks. The image
contains matching source refs for shim, agent, and installer at
`c7351e797efff8bfc6bd73da0eb1909be12e2cfe`. The rebuilt agent's SHA256 and GNU
build ID match the previously validated agent; its highest required GLIBC
symbol version is 2.34. The installer passed its UBI startup check and an
offline run reported `kata-deploy 0.1.0`, the upstream crate version. Build
repository files, the RPM cache, and entitlement files are absent from the
final image.

Twenty development-script tests, three Konflux renderer tests, input-lock
checks, shell syntax, and shellcheck passed. New cases cover repository
replacement, missing mounts, repository checksum drift, RPM content checks,
private secret transport, and readable package copies inside private build
contexts. Direct context mounts failed with a Podman permission error; mounting
the package data from a scratch image input passed without changing SELinux
settings. Both container ignore files now include the lock inputs.

At the end of the October 17 run, the new image remained local and deployment
pins still referenced the previously validated public images. That run did not
change a cluster or an official pipeline. The
[build-input guide](../../scripts/osc-artifacts/INPUT-LOCKS.md)
documents the mirror and local RPM cache options. Evidence is retained under
`build/_output/kata-deploy-internal-build-20261017`.

## Locked installer lifecycle validation, October 20

After approval, the October 17 installer was published as
`quay.io/jensfr/osc-kata-deploy:internal-locked-20261020`, digest
`sha256:a7fb3db3fcd2f24e47ea8e427c1abb6b8640eecab806c84fce2cd76fd5d0e3ed`.
An anonymous registry read matched its manifest and local image identity.
The operator and helper stayed at digests `facd85bd54eb` and `59e48faa1135`.

The disposable Azure cluster ran OpenShift 4.22.19 with three amd64
`Standard_D4as_v5` workers. All exposed `/dev/kvm` and CPU virtualization
flags. The normal installer retained its hardware checks. The two-worker
E2E passed installation status 2/2, monitor readiness, host files and SELinux,
RuntimeClass scheduling, guest agent identity, exec, and emptyDir checks.
The VM's kernel matched host kernel `5.14.0-687.57.1.el9_8.x86_64`.
Deleting KataConfig preserved the running workload and reported
`BlockedByExistingKataPods`. After the test removed its workload, finalization,
host cleanup, runtime-label removal, and RuntimeClass/monitor removal passed.

The multi-node test installed all three workers, shrank the selector, and
expanded it again. A live workload blocked removal of its worker. The retained
workload kept its pod UID and exec access throughout the selector changes.
Restoring the selector created a fresh rollout and installed the excluded worker.

Machine API scaled one worker MachineSet from one replica to two, increasing
the total worker count from three to four. Coverage installed Kata on the new
worker without changing the observed rollout generation. A fresh Kata VM
passed there. Only the added Machine was marked for deletion and removed.
The original replica count and every original worker UID were preserved.
An independent final audit also matched all MachineSet UIDs and replica counts
against the pre-test baseline.

Deleting KataConfig during an active installer stage waited for fleet work to
finish before cleanup. No new install dispatcher started after deletion, and
at most one node mutation Job ran at a time. Final host cleanup passed on all
original workers. All remained Ready; the test namespaces, KataConfig,
RuntimeClass, monitors, and runtime labels were removed. The operator remains
available for diagnostics.

Both scripts exited 0. Evidence is retained under
`build/_output/kata-deploy-azure-validation-20261020`, including
`e2e/results.json`, `multinode/results.json`, and `final-cluster-audit.json`.
The default scripts now pin this tested installer digest. Reboot and OS-update
evidence above comes from earlier installer images. Virtlab was not changed
during the October 20 run.

## Remaining live evidence

HCP still needs a real Kata workload on workers with KVM. A Konflux tenant run
and customer-owned worker MCP membership checks are also pending. The new
source-image clean-install, guarded-uninstall, and multi-worker lifecycle
checks passed on Azure on October 20.

Virtlab2400's API was temporarily unreachable at the start of the October 16
run. It recovered before testing. The advertised OpenShift 4.22.18 update
completed with the new node OS and no degraded cluster operators.
The previous ROSA HCP API hostname no longer resolves.
