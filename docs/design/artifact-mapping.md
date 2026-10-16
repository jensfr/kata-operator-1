# Artifact mapping: upstream Kata/CoCo to Red Hat builds

Maps the current plain Kata prototype inputs and records proposed CoCo
integration. The fork build templates do not register production components.

## Plain Kata (host-derived guest)

| Upstream artifact | Upstream source | RH equivalent | RH owner |
|---|---|---|---|
| `kata-static-shim-v2-go.tar.zst` | kata-containers `src/runtime` (Go) | Build from pinned kata source | osc-kata-artifacts |
| `kata-static-qemu.tar.zst` | QEMU upstream | RHEL RPMs (qemu-kvm-core, libs, firmware) | osc-kata-artifacts |
| `kata-static-virtiofsd.tar.zst` | virtiofsd upstream | RHEL RPM (virtiofsd) | osc-kata-artifacts |
| `kata-static-kernel.tar.zst` | Kata kernel build | Not shipped; host kernel used via guest-prep | N/A |
| `kata-static-rootfs-initrd.tar.zst` | Kata rootfs/initrd build | Not shipped; generated on-node by guest-prep | N/A |
| `kata-static-nydus.tar.zst` | Nydus snapshotter | Not shipped initially | N/A |
| Guest builder (`kata-osbuilder.sh`) | RPM spec / CCA | OSC-specific wrapper, calls upstream osbuilder | OSC copy in osc-artifacts; CCA integration pending |
| kata-agent | kata-containers `src/agent` (Rust) | Built from the same pinned Kata source on CentOS Stream 9, GNU-linked | osc-artifacts build recipe |
| kata-agent.service, kata-containers.target | kata-containers `src/agent` | Generated from kata source (make kata-agent.service) | osc-kata-artifacts |
| Boot service | OSC-specific | `osc-kata-guest-prep.service` | osc-kata-artifacts |
| Dracut config | RPM spec / CCA | `15-dracut.conf` (RHCOS modules) | OSC copy in osc-artifacts; CCA integration pending |

## Confidential Containers

| Upstream artifact | Upstream source | RH equivalent | RH owner |
|---|---|---|---|
| CoCo extension (AA, CDH, image-rs) | `confidential-containers/guest-components` | RH CoCo guest components | confidential-compute-artifacts (CCA) |
| `rootfs-image-coco-extension` | guest-components EROFS+dm-verity | CCA-built measured extension | CCA |
| Pre-built measured guest | Kata kernel + rootfs + CoCo extension | CCA-built, measured, reference values published | CCA |
| OVMF-SEV / OVMF-TDX | Kata firmware build | RH-supplied TEE firmware | CCA / RHEL |
| Trustee (KBS, AS, RVPS) | `confidential-containers/trustee` | RH Trustee images | CCA |
| `kata-osbuilder.sh` (CoCo variant) | CCA | Pre-builds measured initrds per runtime class | CCA |

## Shared between Kata and CoCo

| Component | Shared how | Notes |
|---|---|---|
| containerd-shim-kata-v2 | Same binary, different configs per shim | Config selects TEE, guest type |
| kata-agent | Same source, embedded in all guests | CoCo guests add guest-components alongside |
| kata-deploy binary | Same installer for all variants | `shim-components.json` selects which tarballs |
| k8s-job-dispatcher | Same dispatcher | Template differs per install/cleanup |
| osbuilder scripts | Upstream dracut/initrd-builder shared | CCA adds policies, guest-components, NVIDIA modules |

## Key design boundaries

The current build copies `kata-osbuilder.sh` and the dracut configuration
from `scripts/osc-artifacts/osc-guest-support`. Consuming these from a CCA
artifact image remains an integration decision.

**Plain Kata uses host-derived guest**: no pre-built kernel/initrd shipped. Guest-prep runs `kata-osbuilder.sh` on the node at install time and on boot.

**CoCo uses pre-built measured guest**: initrds are generated at build time by CCA with known kernel, agent, guest-components, policies. Reference values are published with the artifacts. Guest-prep does not run on the node for CoCo.

**`shim-components.json` is the connecting point**: both plain Kata and CoCo use the same upstream schema. Each shim variant lists its required tarballs. The same `kata-deploy install-stage-artifacts` extracts them.

## Konflux component mapping

```
Fork prototype: kata-prototype-installer
  Inputs:
    - Kata 4.2.0 source, verified against its commit
      (shim, runtime, config, agent, osbuilder, systemd units, installer)
    - RHEL RPMs (QEMU, virtiofsd, firmware, libs)
    - OSC copy of kata-osbuilder.sh and dracut configuration
  Output:
    - Final installer with 4 tarballs for plain Kata qemu/x86_64
    - Intermediate "artifacts" target for separate development assembly

Fork prototype: kata-prototype-helper
  Output:
    - Shell tools for OSC host integration stages

Planned CCA integration:
  Inputs:
    - kata-containers source @ pinned ref
    - guest-components source @ pinned ref
    - NVIDIA drivers
    - RHEL kernel
  Output:
    - Pre-built measured initrds per runtime class
    - Guest-components tarballs
    - kata-osbuilder.sh (canonical)
    - Reference values

Separately: RH k8s-job-dispatcher build
```

The templates and renderer are in
[`scripts/osc-kata-deploy/konflux`](../../scripts/osc-kata-deploy/konflux/README.md).
The current prototype is a connected build on the user's fork. Production
pipeline changes, component registration, and bundle image references remain
separate work.

## Open decisions

1. Should `kata-osbuilder.sh` and `15-dracut.conf` be consumed from a CCA artifact image instead of copied into our repo?
2. When CoCo artifacts are added to our payload, do they come as additional tarballs in `shim-components.json` or as a separate image input to `osc-kata-deploy`?
3. Which upstream Kata/CoCo integration tests should run against our RH-built artifacts on OpenShift?
