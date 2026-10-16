#!/usr/bin/env python3
"""Render inactive Konflux build definitions for the jensfr fork. Does not contact a cluster."""

import argparse
import datetime
import json
from pathlib import Path
import re

ROOT = Path(__file__).resolve().parents[3]
TEMPLATES = Path(__file__).resolve().parent / "templates"
GIT_URL = "https://github.com/jensfr/kata-operator-1"


def prepare(output, tenant, application, image_prefix, branch):
    if tenant == "ose-osc-tenant" or image_prefix.startswith("quay.io/redhat-user-workloads/"):
        raise ValueError("Use your own tenant and registry namespace for this fork prototype")
    for value in (tenant, application):
        if not re.fullmatch(r"[a-z0-9]([-a-z0-9]*[a-z0-9])?", value) or len(value) > 63:
            raise ValueError("Tenant and application must be Kubernetes DNS labels")
    if not re.fullmatch(r"[a-zA-Z0-9./_-]+", image_prefix) or not re.fullmatch(r"[a-zA-Z0-9./_-]+", branch):
        raise ValueError("Invalid registry prefix or branch")
    output.mkdir(parents=True, mode=0o700)
    pipeline_dir = output / ".tekton"
    pipeline_dir.mkdir()
    pipeline = (ROOT / ".tekton/build-pipeline.yaml").read_text()
    old = "metadata:\n  name: build-pipeline\n"
    if pipeline.count(old) != 1:
        raise ValueError("Shared pipeline metadata changed; inspect before rendering")
    pipeline = pipeline.replace(old, f"metadata:\n  name: kata-deploy-build\n  namespace: {tenant}\n")
    (pipeline_dir / "kata-deploy-build.yaml").write_text(pipeline)
    components = []
    for kind, context, recipe in [("installer", "scripts/osc-artifacts", "Containerfile"),
                                  ("helper", "scripts/osc-kata-deploy", "Containerfile.helper")]:
        component = "kata-prototype-" + kind
        image = "osc-kata-deploy" if kind == "installer" else "osc-helper"
        values = {"GIT_URL": GIT_URL, "TENANT": tenant, "APPLICATION": application,
                  "IMAGE_PREFIX": image_prefix, "BRANCH": branch, "COMPONENT": component}
        for event in ("pull-request", "push"):
            text = (TEMPLATES / kind / (event + ".yaml.in")).read_text()
            for key, value in values.items():
                text = text.replace("@" + key + "@", value)
            if re.search(r"@[A-Z_]+@", text):
                raise ValueError("Unresolved pipeline template token")
            (pipeline_dir / (component + "-" + event + ".yaml")).write_text(text)
        components.append({
            "apiVersion": "appstudio.redhat.com/v1alpha1", "kind": "Component",
            "metadata": {"name": component, "namespace": tenant, "annotations": {
                "build.appstudio.openshift.io/request": "configure-pac-no-mr", "git-provider": "github", "git-provider-url": "https://github.com"}},
            "spec": {"application": application, "componentName": component,
                     "source": {"git": {"url": GIT_URL, "revision": branch, "context": context, "dockerfileUrl": recipe}},
                     "containerImage": image_prefix + "/" + image},
        })
    application_resource = {"apiVersion": "appstudio.redhat.com/v1alpha1", "kind": "Application",
                            "metadata": {"name": application, "namespace": tenant}, "spec": {"displayName": application}}
    (output / "components.json").write_text(json.dumps(
        {"apiVersion": "v1", "kind": "List", "items": [application_resource] + components}, indent=2) + "\n")
    return pipeline_dir


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--namespace", required=True, help="Your own Konflux tenant namespace")
    parser.add_argument("--application", default="kata-deploy-prototype")
    parser.add_argument("--image-prefix", default="quay.io/jensfr")
    parser.add_argument("--branch", default="kata-deploy-upstream")
    parser.add_argument("--output", type=Path)
    args = parser.parse_args()
    stamp = datetime.datetime.now(datetime.timezone.utc).strftime("%Y%m%dT%H%M%S%fZ")
    output = args.output or ROOT / "build/_output" / ("kata-konflux-fork-" + stamp)
    if output.exists():
        parser.error("Output directory must be new")
    try:
        pipeline = prepare(output.resolve(), args.namespace, args.application, args.image_prefix, args.branch)
    except ValueError as error:
        parser.error(str(error))
    print(f"Prepared fork build definitions: {pipeline.parent}")


if __name__ == "__main__":
    main()
