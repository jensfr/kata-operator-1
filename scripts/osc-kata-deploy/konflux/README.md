# Konflux prototype for the fork

These templates target `https://github.com/jensfr/kata-operator-1` and a
tenant supplied at render time. They are outside `.tekton`, so committing
them does not activate builds. The renderer writes files under
`build/_output`; it does not apply resources or edit existing pipelines.

```bash
python3 scripts/osc-kata-deploy/konflux/prepare.py \
  --namespace "$MY_KONFLUX_TENANT" \
  --output build/_output/konflux-fork
```

The output contains a copy of the shared build pipeline named
`kata-deploy-build`, four PR/push PipelineRuns, and `components.json` for a
separate prototype application. The default branch is `kata-deploy-upstream`
and image prefix is `quay.io/jensfr`. All build platforms are `linux/x86_64`.
Inspect the rendered files before copying the new PipelineRuns into `.tekton`
on a fork test branch and onboarding the application in your own tenant.
The component manifests use `configure-pac-no-mr` so onboarding keeps the
prepared PipelineRuns. See [Konflux onboarding](https://konflux-ci.dev/docs/building/creating-github/).

| Component | Context | Recipe | Output |
|---|---|---|---|
| `kata-prototype-installer` | `scripts/osc-artifacts` | `Containerfile`, final `installer` target | Installer binary and runtime artifact tarballs |
| `kata-prototype-helper` | `scripts/osc-kata-deploy` | `Containerfile.helper` | Shell tools for OSC host stages |

The installer recipe builds the shim, runtime, agent, osbuilder, and installer
from Kata 4.2.0. Both source clones must match commit
`c7351e797efff8bfc6bd73da0eb1909be12e2cfe`. QEMU, virtiofsd, firmware, and
private libraries come from RHEL RPMs. The final UBI stage executes
`kata-deploy --help` and checks the payload contract. The `artifacts` target
still exports just the payload for development builds.

The prototype uses `hermetic: "false"`: its recipe fetches source,
crates, Go dependencies, and RPMs. Installer and helper inputs now have
[base-image, package-content, SDK, and source locks](../../osc-artifacts/INPUT-LOCKS.md).
Release work still needs retained RPM repositories, source-image checks,
timestamp normalization, and release policy validation.

## Subscription access

The installer needs RHEL packages. The pinned buildah task uses the
`activation-key` secret in your own tenant, or falls back to
`etc-pki-entitlement`. Configure the required RHEL repositories through
that tenant's subscription. Keep credentials outside Git. See the
[Konflux activation key instructions](https://konflux-ci.dev/docs/building/activation-keys-subscription/).

The task bundle was inspected at digest
`sha256:f34d44fd81608a76644b3af2784ee9d3ffb7eae57e33242b0b1ef1aeca57c9ac`.
Its defaults are `ACTIVATION_KEY=activation-key` and
`ENTITLEMENT_SECRET=etc-pki-entitlement`.

## Local build

```bash
./scripts/osc-artifacts/build-local.sh --build-arg VERSION=1.14 \
  -t localhost/osc-kata-deploy:source

./scripts/osc-artifacts/build-local.sh --target artifacts \
  -t localhost/osc-kata-artifacts:source

./scripts/osc-kata-deploy/build-helper.sh --build-arg VERSION=1.14 \
  -t localhost/osc-helper:source
```

`build-local.sh` stages named build inputs in a private directory under
`build/_output`. This excludes old `podman-build-secret*` files from the source
directory. Podman receives subscription files as build secrets. Set
`ENTITLEMENT_CERT` and `ENTITLEMENT_KEY` to override the defaults under
`~/.osc-build-secrets`.
For local builds through a retained package mirror, use `OSC_RPM_REPOS_FILE`
and optional `OSC_RPM_CA_FILE` as described in the
[build input guide](../../osc-artifacts/INPUT-LOCKS.md). This does not change
the tenant's repositories or pipeline configuration.

After a prototype Konflux build succeeds, test its digests on a disposable
cluster. Bundle image references, component nudges, and changes to the
official pipelines belong in a later review.
