#!/usr/bin/env python3
"""Development deployment and live validation. Uses Python's standard library and oc."""

import argparse
import datetime
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import time

ROOT = Path(__file__).resolve().parents[2]
NS = "openshift-sandboxed-containers-operator"
TEST_NS = "osc-katadeploy-e2e"
RUNTIME_LABEL = "katacontainers.io/kata-runtime"
MONITOR = "openshift-sandboxed-containers-monitor"
IMAGES = {
    "operator": os.environ.get("OPERATOR_IMG"),
    "installer": os.environ.get("KATA_DEPLOY_IMG"),
    "helper": os.environ.get("HELPER_IMG"),
    "monitor": os.environ.get("MONITOR_IMG"),
}


def run(*args, **kwargs):
    return subprocess.run(args, check=True, text=True, capture_output=True, **kwargs).stdout


def oc(*args, **kwargs):
    request_timeout = "0" if args[0] in ("wait", "rollout", "debug", "exec", "delete") else "30s"
    return run("oc", "--request-timeout=" + request_timeout, *args, **kwargs)


def get(kind, name=None, namespace=None):
    args = ["get", kind]
    if name:
        args.append(name)
    if namespace:
        args += ["-n", namespace]
    # NotFound is distinct from an API connection or authorization error.
    result = oc(*args, "--ignore-not-found", "-o", "json")
    if result.strip():
        return json.loads(result)
    return None if name else {"apiVersion": "v1", "kind": "List", "items": []}


def apply(obj):
    return oc("apply", "-f", "-", input=json.dumps(obj))


def condition(obj, name):
    return next((c for c in obj.get("status", {}).get("conditions", []) if c["type"] == name), {})


def installed(config, count):
    nodes = (config or {}).get("status", {}).get("kataNodes", {})
    return (count > 0 and nodes.get("nodeCount") == count
            and nodes.get("readyNodeCount") == count
            and condition(config or {}, "InProgress").get("status") == "False")


def wait(description, check, timeout=900):
    print(f"Waiting: {description}", flush=True)
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        value = check()
        if value:
            print(f"PASS: {description}", flush=True)
            return value
        time.sleep(5)
    raise RuntimeError(f"Timed out: {description}")


def save_json(path, obj):
    path.write_text(json.dumps(obj, indent=2) + "\n")


def render(output):
    """Copy config before applying overrides so a developer's image override stays intact."""
    if not all(IMAGES.values()):
        raise RuntimeError("Image variables missing; run deploy-katadeploy.sh or e2e-test.sh")
    directory = output / "render"
    directory.mkdir()
    shutil.copytree(ROOT / "config", directory / "config",
                    ignore=shutil.ignore_patterns("cloud-api-adaptor"))
    peerpod = ROOT / "config/peerpods/podvm/cloud-api-adaptor/src/peerpod-ctrl/chart/crds/confidentialcontainers.org_peerpods.yaml"
    if not peerpod.is_file():
        raise RuntimeError("PeerPod CRD missing. Initialize the cloud-api-adaptor submodule first.")
    shutil.copyfile(peerpod, directory / "peerpods.yaml")
    deployment = {
        "apiVersion": "apps/v1", "kind": "Deployment", "metadata": {"name": "controller-manager", "namespace": NS},
        "spec": {"template": {"spec": {"containers": [{
            "name": "manager", "image": IMAGES["operator"],
            "env": [{"name": name, "value": IMAGES[key]} for name, key in [
                ("RELATED_IMAGE_KATA_DEPLOY", "installer"), ("RELATED_IMAGE_OSC_HELPER", "helper"),
                ("RELATED_IMAGE_KATA_MONITOR", "monitor")]],
        }]}}},
    }
    patches = [deployment, {
        "apiVersion": "v1", "kind": "Service", "metadata": {"name": "webhook-service", "namespace": NS, "annotations": {
            "service.beta.openshift.io/serving-cert-secret-name": "webhook-server-cert"}},
    }, {
        "apiVersion": "admissionregistration.k8s.io/v1", "kind": "ValidatingWebhookConfiguration",
        "metadata": {"name": "validating-webhook-configuration", "annotations": {
            "service.beta.openshift.io/inject-cabundle": "true"}},
    }]
    save_json(directory / "feature-gates.json", {
        "apiVersion": "v1", "kind": "ConfigMap", "metadata": {"name": "osc-feature-gates", "namespace": NS},
        "data": {"deploymentMode": "KataDeploy"},
    })
    save_json(directory / "Kustomization", {
        "apiVersion": "kustomize.config.k8s.io/v1beta1", "kind": "Kustomization", "namespace": NS,
        "resources": ["config/default", "peerpods.yaml", "feature-gates.json"],
        "patches": [{"patch": json.dumps(patch)} for patch in patches],
    })
    kustomize = os.environ.get("KUSTOMIZE", str(ROOT / "bin/kustomize"))
    if not Path(kustomize).is_file() and not shutil.which(kustomize):
        raise RuntimeError("kustomize missing. Run make kustomize, or set KUSTOMIZE.")
    manifest = output / "operator-install.yaml"
    manifest.write_text(run(kustomize, "build", str(directory)))
    return manifest


def webhook_ready():
    secret = get("secret", "webhook-server-cert", NS) or {}
    hooks = (get("validatingwebhookconfiguration", "validating-webhook-configuration") or {}).get("webhooks", [])
    return secret.get("type") == "kubernetes.io/tls" and bool(hooks) and all(
        w.get("clientConfig", {}).get("caBundle") for w in hooks)


def deploy(manifest):
    print(f"Applying {manifest}", flush=True)
    oc("apply", "-f", str(manifest))
    oc("rollout", "status", "deployment/controller-manager", "-n", NS, "--timeout=180s")
    wait("webhook certificate and CA injection", webhook_ready, 180)


def kataconfig(name, nodes=None):
    spec = {"enablePeerPods": False}
    if nodes:
        spec["kataConfigPoolSelector"] = {"matchExpressions": [{
            "key": "kubernetes.io/hostname", "operator": "In", "values": nodes}]}
    return {"apiVersion": "kataconfiguration.openshift.io/v1", "kind": "KataConfig",
            "metadata": {"name": name}, "spec": spec}


def namespace_ready(name):
    namespace = get("namespace", name) or {}
    return bool(namespace.get("metadata", {}).get("annotations", {}).get("openshift.io/sa.scc.uid-range")) and bool(
        get("serviceaccount", "default", name))


def create_test_namespace(name):
    oc("create", "namespace", name)
    wait("namespace SCC allocation and service account", lambda: namespace_ready(name), 180)


def cleanup_jobs(jobs, uid):
    return [j for j in jobs["items"] if j["metadata"].get("labels", {}).get("kata-deploy/operation") == "cleanup"
            and j["metadata"].get("labels", {}).get("kata-deploy/kataconfig-uid") == uid]


def snapshot(output, phase):
    resources = {}
    for kind, namespace in [("kataconfig", None), ("nodes", None), ("runtimeclasses", None),
                            ("jobs", NS), ("pods", NS), ("daemonsets", NS)]:
        resources[kind] = get(kind, namespace=namespace)
    save_json(output / f"{phase}.json", resources)
    try:
        (output / f"{phase}-operator.log").write_text(oc(
            "logs", "deployment/controller-manager", "-n", NS, "--tail=300"))
    except subprocess.CalledProcessError:
        pass


HOST_INSTALLED = r"""set -eu
test -c /dev/kvm
test "$(getenforce)" = Enforcing
for f in containerd-shim-kata-v2 kata-runtime qemu-system-x86_64; do test -x /var/opt/kata/bin/$f; done
test -s /var/cache/kata-containers/osbuilder-images/kata.kernel
test -s /var/cache/kata-containers/osbuilder-images/kata.initrd
# Upstream rootfs.sh strips the guest copy; the ELF build ID survives stripping.
host_agent_id=$(file -b /var/opt/kata/libexec/kata-containers/agent/usr/bin/kata-agent | sed -n 's/.*BuildID\[sha1\]=\([[:xdigit:]]*\).*/\1/p')
guest_agent_id=$(lsinitrd -f usr/bin/kata-agent /var/cache/kata-containers/osbuilder-images/kata.initrd | file -b - | sed -n 's/.*BuildID\[sha1\]=\([[:xdigit:]]*\).*/\1/p')
test -n "$host_agent_id" && test "$host_agent_id" = "$guest_agent_id" || { echo "Guest initrd contains a different kata-agent or lacks an ELF build ID"; exit 1; }
echo "Guest agent ELF build ID: $guest_agent_id"
test -L /etc/systemd/system/kubelet.service.wants/osc-kata-guest-prep.service
test -f /etc/crio/crio.conf.d/99-kata-deploy
ls -Z /var/opt/kata/bin/containerd-shim-kata-v2 /var/opt/kata/bin/qemu-system-x86_64.real /var/cache/kata-containers/osbuilder-images/kata.initrd
stat -Lc '%C' /var/opt/kata/bin/containerd-shim-kata-v2 | grep -q ':container_runtime_exec_t:'
stat -Lc '%C' /var/opt/kata/bin/qemu-system-x86_64.real | grep -q ':qemu_exec_t:'
stat -Lc '%C' /var/cache/kata-containers/osbuilder-images/kata.initrd | grep -q ':container_ro_file_t:'
/var/opt/kata/bin/kata-runtime --version
for module in osc_monitor osc_kata_deploy; do semodule -l | grep -E "^${module}([[:space:]]|$)"; done
echo HOST_CHECK_PASSED
"""
HOST_REMOVED = r"""set -eu
for f in /var/opt/kata/bin/containerd-shim-kata-v2 /etc/crio/crio.conf.d/99-kata-deploy /etc/systemd/system/osc-kata-guest-prep.service /etc/systemd/system/kubelet.service.wants/osc-kata-guest-prep.service /var/cache/kata-containers/osbuilder-images/kata.initrd; do
    if test -e "$f" || test -L "$f"; then echo "Unexpected leftover: $f"; exit 1; fi
done
if semodule -l | grep -Eq '^(osc_monitor|osc_kata_deploy)([[:space:]]|$)'; then exit 1; fi
echo HOST_CHECK_PASSED
"""


def host_checks(nodes, output, removed=False):
    for node in nodes:
        result = oc("debug", "node/" + node["metadata"]["name"], "--", "chroot", "/host", "/bin/sh", "-c",
                    HOST_REMOVED if removed else HOST_INSTALLED)
        (output / f"host-{'removed' if removed else 'installed'}-{node['metadata']['name']}.log").write_text(result)
        if "HOST_CHECK_PASSED" not in result:
            raise RuntimeError("Host check did not finish")


def workload():
    return {"apiVersion": "v1", "kind": "Pod", "metadata": {"name": "kata-workload", "namespace": TEST_NS}, "spec": {
        "runtimeClassName": "kata", "restartPolicy": "Never",
        "securityContext": {"runAsNonRoot": True, "seccompProfile": {"type": "RuntimeDefault"}},
        "containers": [{"name": "test", "image": "registry.access.redhat.com/ubi9/ubi-minimal:9.6",
                        "command": ["sh", "-ec", "uname -r; echo kata-ok >/data/probe; sleep 3600"],
                        "securityContext": {"allowPrivilegeEscalation": False, "capabilities": {"drop": ["ALL"]}},
                        "resources": {"requests": {"memory": "128Mi", "cpu": "100m"},
                                      "limits": {"memory": "512Mi", "cpu": "1"}},
                        "readinessProbe": {"exec": {"command": ["cat", "/data/probe"]}},
                        "volumeMounts": [{"name": "data", "mountPath": "/data"}]}],
        "volumes": [{"name": "data", "emptyDir": {}}],
    }}


def e2e(args, output):
    configs = get("kataconfig") if get("crd", "kataconfigs.kataconfiguration.openshift.io") else None
    if configs and configs["items"]:
        raise RuntimeError("KataConfig already exists. Use a clean cluster for the E2E install/uninstall test.")
    if get("namespace", TEST_NS):
        raise RuntimeError(f"Namespace {TEST_NS} already exists. Inspect it before another test.")
    workers = [n for n in get("nodes")["items"] if "node-role.kubernetes.io/worker" in n["metadata"].get("labels", {})
               and condition(n, "Ready").get("status") == "True" and not n["spec"].get("unschedulable")]
    names = args.nodes.split(",") if args.nodes else [n["metadata"]["name"] for n in workers[:1]]
    nodes = [n for n in workers if n["metadata"]["name"] in names]
    if not names or len(nodes) != len(set(names)):
        raise RuntimeError("Select Ready, schedulable workers. Pass comma-separated node names with --nodes.")
    hostnames = [n["metadata"]["labels"]["kubernetes.io/hostname"] for n in nodes]
    # Let kata-deploy load KVM modules on capable CPUs. Do not require them to be loaded already.
    for node in nodes:
        if node["status"]["nodeInfo"]["architecture"] != "amd64":
            raise RuntimeError("The pinned development payload supports amd64 workers")
        oc("debug", "node/" + node["metadata"]["name"], "--", "chroot", "/host", "sh", "-ec",
           "test -c /dev/kvm || grep -Eq '(vmx|svm)' /proc/cpuinfo")
    deploy(render(output))
    create_test_namespace(TEST_NS)
    config = kataconfig(args.name, hostnames)
    save_json(output / "kataconfig.json", config)
    oc("create", "-f", "-", input=json.dumps(config))
    wait("all selected nodes installed", lambda: installed(get("kataconfig", args.name), len(nodes)), args.timeout)
    oc("rollout", "status", "daemonset/" + MONITOR, "-n", NS, "--timeout=180s")
    monitor = get("daemonset", MONITOR, NS)["status"]
    assert monitor.get("desiredNumberScheduled") == len(nodes) and monitor.get("numberReady") == len(nodes), "Monitor coverage is incomplete"
    rc = get("runtimeclass", "kata")
    if rc["handler"] != "kata-qemu" or rc["scheduling"]["nodeSelector"].get(RUNTIME_LABEL) != "true":
        raise RuntimeError("RuntimeClass handler or scheduling is wrong")
    host_checks(nodes, output)
    apply(workload())
    oc("wait", "--for=condition=Ready", "pod/kata-workload", "-n", TEST_NS, "--timeout=180s")
    pod = get("pod", "kata-workload", TEST_NS)
    guest = oc("exec", "kata-workload", "-n", TEST_NS, "--", "uname", "-r").strip()
    host = get("node", pod["spec"]["nodeName"])["status"]["nodeInfo"]["kernelVersion"]
    assert pod["spec"]["nodeName"] in names, "Workload scheduled outside the selection"
    assert guest == host, f"Guest kernel {guest} differs from host {host}"
    assert oc("exec", "kata-workload", "-n", TEST_NS, "--", "cat", "/data/probe").strip() == "kata-ok"
    snapshot(output, "installed")
    uid = get("kataconfig", args.name)["metadata"]["uid"]
    oc("delete", "kataconfig", args.name, "--wait=false")
    wait("uninstall blocked by the live Kata workload", lambda:
         condition(get("kataconfig", args.name) or {}, "InProgress").get("reason") == "BlockedByExistingKataPods", 120)
    blocked = get("kataconfig", args.name)
    assert blocked["metadata"].get("finalizers"), "Uninstall dropped its finalizer"
    assert not get("pod", "kata-workload", TEST_NS)["metadata"].get("deletionTimestamp"), "Workload was deleted"
    assert not cleanup_jobs(get("jobs", namespace=NS), uid), "Cleanup started despite the running workload"
    assert oc("exec", "kata-workload", "-n", TEST_NS, "--", "cat", "/data/probe").strip() == "kata-ok"
    snapshot(output, "blocked-delete")
    oc("delete", "pod/kata-workload", "-n", TEST_NS, "--timeout=120s")
    oc("wait", "--for=delete", "kataconfig/" + args.name, f"--timeout={args.timeout}s")
    wait("RuntimeClass removed", lambda: not get("runtimeclass", "kata"), 120)
    wait("monitor removed", lambda: not get("daemonset", MONITOR, NS), 120)
    for name in names:
        assert RUNTIME_LABEL not in get("node", name)["metadata"].get("labels", {}), "Runtime label remains"
    host_checks(nodes, output, removed=True)
    snapshot(output, "removed")
    oc("delete", "namespace", TEST_NS, "--timeout=120s")
    save_json(output / "results.json", {
        "result": "passed", "images": IMAGES, "nodes": names, "guest_kernel": guest,
        "checks": ["install-status", "monitor", "host-files", "runtimeclass", "workload-exec-emptydir",
                   "guest-kernel", "uninstall-guard", "workload-preserved", "cleanup"],
        "operator_left_running": True,
    })


def verify_transition(before, after, expect_kernel_change):
    for field in ("server", "node", "node_uid", "kataconfig_uid"):
        if before[field] != after[field]:
            raise RuntimeError(f"Baseline belongs to a different {field}")
    if before["boot_id"] == after["boot_id"]:
        raise RuntimeError("Node has not rebooted since the baseline")
    if expect_kernel_change and before["kernel"] == after["kernel"]:
        raise RuntimeError("Host kernel did not change; this does not prove a kernel upgrade")


def lifecycle(args, output):
    if not args.node:
        raise RuntimeError("Lifecycle validation needs --node")
    node = get("node", args.node)
    config = get("kataconfig", args.name)
    count = (config or {}).get("status", {}).get("kataNodes", {}).get("nodeCount", 0)
    if not installed(config, count) or node["metadata"].get("labels", {}).get(RUNTIME_LABEL) != "true":
        raise RuntimeError("Kata must be installed and converged on the named node")
    state = {
        "server": oc("whoami", "--show-server").strip(), "node": args.node,
        "node_uid": node["metadata"]["uid"], "kataconfig_uid": config["metadata"]["uid"],
        "boot_id": node["status"]["nodeInfo"]["bootID"],
        "kernel": node["status"]["nodeInfo"]["kernelVersion"],
    }
    if args.after:
        verify_transition(json.loads(args.after.read_text()), state, args.expect_kernel_change)
        boot_check = r'''set -eu
test "$(systemctl show osc-kata-guest-prep.service -p Result --value)" = success
test "$(systemctl show osc-kata-guest-prep.service -p ExecMainStatus --value)" = 0
test "$(systemctl show osc-kata-guest-prep.service -p ExecMainStartTimestampMonotonic --value)" -gt 0
journalctl -b -u osc-kata-guest-prep.service --no-pager -n 50
echo BOOT_SERVICE_PASSED
'''
        result = oc("debug", "node/" + args.node, "--", "chroot", "/host", "sh", "-ec", boot_check)
        (output / "boot-service.log").write_text(result)
        if "BOOT_SERVICE_PASSED" not in result:
            raise RuntimeError("Guest preparation did not run successfully during this boot")
    host_checks([node], output)
    namespace = "osc-katadeploy-lifecycle"
    if get("namespace", namespace):
        raise RuntimeError(f"Namespace {namespace} already exists; inspect the previous run")
    create_test_namespace(namespace)
    pod = workload()
    pod["metadata"]["namespace"] = namespace
    pod["spec"]["nodeSelector"] = {"kubernetes.io/hostname": node["metadata"]["labels"]["kubernetes.io/hostname"]}
    oc("create", "-f", "-", input=json.dumps(pod))
    oc("wait", "--for=condition=Ready", "pod/kata-workload", "-n", namespace, "--timeout=180s")
    guest = oc("exec", "kata-workload", "-n", namespace, "--", "uname", "-r").strip()
    assert guest == state["kernel"], "Guest does not use the current host kernel"
    assert oc("exec", "kata-workload", "-n", namespace, "--", "cat", "/data/probe").strip() == "kata-ok"
    snapshot(output, "lifecycle")
    oc("delete", "namespace", namespace, "--timeout=180s")
    save_json(output / "baseline.json", state)
    save_json(output / "results.json", {"result": "passed", "state": state, "guest_kernel": guest,
              "phase": "after-kernel-upgrade" if args.expect_kernel_change else "after-reboot" if args.after else "baseline"})
    print("PASS: Host checks and a fresh Kata workload", flush=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=["deploy", "e2e", "lifecycle"])
    parser.add_argument("--output", type=Path, help="New evidence directory (default: build/_output/kata-deploy-<time>)")
    parser.add_argument("--name", default="example-kataconfig", help="KataConfig name")
    parser.add_argument("--nodes", help="Comma-separated Ready worker names (E2E)")
    parser.add_argument("--node", help="Installed node to check (lifecycle)")
    parser.add_argument("--after", type=Path, help="Baseline JSON captured before reboot or upgrade (lifecycle)")
    parser.add_argument("--expect-kernel-change", action="store_true", help="Require a different host kernel (lifecycle)")
    parser.add_argument("--timeout", type=int, default=1200, help="Installation/cleanup timeout in seconds")
    mode = parser.add_mutually_exclusive_group()
    mode.add_argument("--render", action="store_true", help="Render manifests without cluster access")
    mode.add_argument("--operator-only", action="store_true", help="Apply the operator without creating a KataConfig")
    mode.add_argument("--undeploy", action="store_true", help="Delete the named KataConfig and wait; keep the operator for diagnostics")
    args = parser.parse_args()
    if args.command != "deploy" and (args.render or args.operator_only or args.undeploy):
        parser.error("Deployment options belong to deploy-katadeploy.sh")
    if args.nodes and args.command != "e2e":
        parser.error("--nodes belongs to e2e-test.sh")
    if args.command != "lifecycle" and (args.node or args.after or args.expect_kernel_change):
        parser.error("Lifecycle options belong to lifecycle-test.sh")
    if args.expect_kernel_change and not args.after:
        parser.error("--expect-kernel-change needs --after")
    os.umask(0o077)
    stamp = datetime.datetime.now(datetime.timezone.utc).strftime("%Y%m%dT%H%M%S%fZ")
    output = (args.output or ROOT / "build/_output" / ("kata-deploy-" + stamp)).resolve()
    if output.exists():
        parser.error("Output directory must be new, to preserve previous evidence")
    output.mkdir(parents=True, mode=0o700)
    os.chmod(output, 0o700)
    print(f"Evidence: {output}", flush=True)
    try:
        if args.command == "e2e":
            e2e(args, output)
        elif args.command == "lifecycle":
            lifecycle(args, output)
        elif args.undeploy:
            oc("delete", "kataconfig", args.name, "--ignore-not-found", f"--timeout={args.timeout}s")
            print("KataConfig cleanup finished. Operator remains available for diagnostics.")
        else:
            manifest = render(output)
            if args.render:
                print(manifest)
                return
            deploy(manifest)
            if not args.operator_only:
                config = kataconfig(args.name)
                save_json(output / "kataconfig.json", config)
                oc("create", "-f", "-", input=json.dumps(config))
                print(f"Created {args.name}. Watch: oc get kataconfig {args.name} -w")
    except (subprocess.CalledProcessError, RuntimeError, AssertionError) as error:
        # Leave failed resources in place for inspection. Never strip finalizers or delete workloads.
        print(f"FAIL: {error}", file=sys.stderr)
        if isinstance(error, subprocess.CalledProcessError):
            print(error.stderr, file=sys.stderr)
        if not args.render:
            try:
                snapshot(output, "failure")
            except Exception as capture_error:
                print(f"Could not capture cluster state: {capture_error}", file=sys.stderr)
        raise SystemExit(1)


if __name__ == "__main__":
    main()
