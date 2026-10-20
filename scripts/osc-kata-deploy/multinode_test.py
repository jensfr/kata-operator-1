#!/usr/bin/env python3
"""Selector and Machine API lifecycle tests on a disposable cluster."""

import argparse
import json
import os
from pathlib import Path
import time

import recovery_test as r
import validation as v

MACHINE_NS = "openshift-machine-api"
MACHINES = "machines.machine.openshift.io"
MACHINESETS = "machinesets.machine.openshift.io"
NAMESPACE = "osc-katadeploy-multinode"
DELETE_ANNOTATION = "machine.openshift.io/delete-machine"


def workers():
    return sorted([n for n in v.get("nodes")["items"]
                   if "node-role.kubernetes.io/worker" in n["metadata"].get("labels", {})],
                  key=lambda n: n["metadata"]["name"])


def status(name, nodes):
    config = v.get("kataconfig", name)
    return v.installed(config, len(nodes)) and set(config["status"]["kataNodes"]["installed"]) == set(nodes)


def check_pod(name, uid=None):
    pod = v.get("pod", name, NAMESPACE)
    assert not pod["metadata"].get("deletionTimestamp"), "Test workload was deleted"
    if uid:
        assert pod["metadata"]["uid"] == uid, "Test workload was replaced"
    assert v.oc("exec", name, "-n", NAMESPACE, "--", "cat", "/data/probe").strip() == "kata-ok"
    kernel = v.oc("exec", name, "-n", NAMESPACE, "--", "uname", "-r").strip()
    assert kernel == v.get("node", pod["spec"]["nodeName"])["status"]["nodeInfo"]["kernelVersion"]
    return pod["metadata"]["uid"]


def create_pod(name, node):
    pod = v.workload()
    pod["metadata"].update(name=name, namespace=NAMESPACE)
    pod["spec"]["nodeSelector"] = {"kubernetes.io/hostname": node["metadata"]["labels"]["kubernetes.io/hostname"]}
    v.oc("create", "-f", "-", input=json.dumps(pod))
    v.oc("wait", "--for=condition=Ready", "pod/" + name, "-n", NAMESPACE, "--timeout=300s")
    return check_pod(name)


def selector_test(args, nodes):
    names = [n["metadata"]["name"] for n in nodes]
    excluded, retained = nodes[:2]
    config = v.get("kataconfig", args.name)
    uid = config["metadata"]["uid"]
    selector = {"matchExpressions": [{"key": "kubernetes.io/hostname", "operator": "NotIn",
                "values": [excluded["metadata"]["labels"]["kubernetes.io/hostname"]]}]}
    removed_uid = create_pod("excluded", excluded)
    retained_uid = create_pod("retained", retained)
    jobs = {j["metadata"]["uid"] for j in v.get("jobs", namespace=v.NS)["items"]}
    r.change_selector(args.name, uid, None, selector)
    v.wait("selector shrink blocked by a live workload", lambda:
           v.condition(v.get("kataconfig", args.name), "InProgress").get("reason") == "BlockedByExistingKataPods", 120)
    check_pod("excluded", removed_uid)
    check_pod("retained", retained_uid)
    assert not any(j["metadata"]["uid"] not in jobs and j["metadata"].get("labels", {}).get("kata-deploy/operation") in ("rollout", "cleanup")
                   for j in v.get("jobs", namespace=v.NS)["items"]), "Mutation started while selector removal was blocked"
    v.snapshot(args.output, "selector-blocked")
    v.oc("delete", "pod/excluded", "-n", NAMESPACE, "--timeout=120s")
    v.wait("excluded worker cleaned and retained workers installed", lambda: status(args.name, names[1:]), args.timeout)
    v.host_checks([excluded], args.output, removed=True)
    check_pod("retained", retained_uid)
    v.snapshot(args.output, "selector-shrunk")
    r.change_selector(args.name, uid, selector, None)
    v.wait("selector restored on all workers", lambda: status(args.name, names), args.timeout)
    restored = v.get("kataconfig", args.name)
    assert restored["metadata"]["generation"] == config["metadata"]["generation"] + 2, "Recording completion changed the spec generation"
    assert restored["metadata"]["annotations"][r.OBSERVED] != config["metadata"]["annotations"][r.OBSERVED], "Restoring the selector reused an old rollout"
    v.host_checks([excluded], args.output)
    create_pod("restored", excluded)
    check_pod("retained", retained_uid)
    v.snapshot(args.output, "selector-expanded")
    for name in ("restored", "retained"):
        v.oc("delete", "pod/" + name, "-n", NAMESPACE, "--timeout=120s")
    return {"case": "selector-shrink-expand", "excluded": names[0], "retained_workload_preserved": True,
            "fresh_rollout_after_restore": True, "spec_generation_changes": 2}


def owned_machines(machineset):
    return [m for m in v.get(MACHINES, namespace=MACHINE_NS)["items"]
            if any(o["uid"] == machineset["metadata"]["uid"] for o in m["metadata"].get("ownerReferences", []))]


def scale(machineset, expected, desired):
    current = v.get(MACHINESETS, machineset["metadata"]["name"], MACHINE_NS)
    if current["metadata"]["uid"] != machineset["metadata"]["uid"] or current["spec"]["replicas"] != expected:
        raise RuntimeError("MachineSet changed outside the test; refusing to scale")
    patch = [{"op": "test", "path": "/metadata/resourceVersion", "value": current["metadata"]["resourceVersion"]},
             {"op": "replace", "path": "/spec/replicas", "value": desired}]
    v.oc("patch", MACHINESETS, current["metadata"]["name"], "-n", MACHINE_NS, "--type=json", "-p", json.dumps(patch))


def machine_test(args, nodes, machineset):
    baseline = machineset["spec"]["replicas"]
    existing = owned_machines(machineset)
    if baseline < 1 or len(existing) != baseline or any(m["status"].get("phase") != "Running" for m in existing):
        raise RuntimeError("MachineSet must have its baseline replicas running")
    if any(DELETE_ANNOTATION in m["metadata"].get("annotations", {}) for m in existing):
        raise RuntimeError("A Machine already has a deletion annotation")
    known = {m["metadata"]["uid"] for m in existing}
    original_nodes = {n["metadata"]["name"]: n["metadata"]["uid"] for n in nodes}
    generation = v.get("kataconfig", args.name)["metadata"]["annotations"][r.OBSERVED]
    v.save_json(args.output / "original-machineset.json", machineset)
    scale(machineset, baseline, baseline + 1)
    added = None
    added_node = None
    try:
        def new_machine():
            new = [m for m in owned_machines(machineset) if m["metadata"]["uid"] not in known]
            if len(new) > 1:
                raise RuntimeError("Unexpected extra Machines; refusing automatic scale down")
            return new[0] if new else None
        added = v.wait("one new Machine", new_machine, args.timeout)
        v.save_json(args.output / "added-machine.json", added)
        def new_node():
            machine = v.get(MACHINES, added["metadata"]["name"], MACHINE_NS)
            node_name = machine.get("status", {}).get("nodeRef", {}).get("name")
            node = v.get("node", node_name) if node_name else None
            return node if node and v.condition(node, "Ready").get("status") == "True" else None
        node = v.wait("new worker Ready", new_node, args.timeout)
        added_node = node["metadata"]["name"]
        names = list(original_nodes) + [node["metadata"]["name"]]
        v.wait("coverage installs the new worker", lambda: status(args.name, names), args.timeout)
        assert v.get("kataconfig", args.name)["metadata"]["annotations"][r.OBSERVED] == generation, "Node addition changed the rollout generation"
        v.host_checks([node], args.output)
        create_pod("added-worker", node)
        v.snapshot(args.output, "worker-added")
        v.oc("delete", "pod/added-worker", "-n", NAMESPACE, "--timeout=120s")
    finally:
        # Only the Machine created by this scale-up may be removed.
        if added is None:
            raise RuntimeError("No added Machine was recorded. Inspect the saved MachineSet before restoring replicas")
        current = owned_machines(machineset)
        assert {m["metadata"]["uid"] for m in current} == known | {added["metadata"]["uid"]}, "Machine set membership changed; inspect it before scaling down"
        for machine in current:
            if machine["metadata"]["uid"] in known and DELETE_ANNOTATION in machine["metadata"].get("annotations", {}):
                raise RuntimeError("An original Machine was marked for deletion; refusing scale down")
        v.oc("annotate", MACHINES, added["metadata"]["name"], "-n", MACHINE_NS, DELETE_ANNOTATION + "=true")
        scale(machineset, baseline + 1, baseline)
        v.wait("only the added Machine removed", lambda: not v.get(MACHINES, added["metadata"]["name"], MACHINE_NS), args.timeout)
        if added_node:
            v.wait("added Node removed", lambda: not v.get("node", added_node), args.timeout)
    v.wait("status returns to the original worker set", lambda: status(args.name, original_nodes), args.timeout)
    for name, uid in original_nodes.items():
        assert v.get("node", name)["metadata"]["uid"] == uid, "An original worker was replaced"
    assert v.get("kataconfig", args.name)["metadata"]["annotations"][r.OBSERVED] == generation
    v.snapshot(args.output, "worker-removed")
    return {"case": "real-worker-add-remove", "added_machine": added["metadata"]["name"], "generation_unchanged": True, "baseline_replicas_restored": baseline}


def delete_while_active(args):
    v.wait("previous fleet work finished", lambda: all(r.terminal(j) for j in v.get("jobs", namespace=v.NS)["items"]
           if r.OWNER in j["metadata"].get("labels", {}) or "kata-deploy/operation" in j["metadata"].get("labels", {})), args.timeout)
    config = v.get("kataconfig", args.name)
    uid = config["metadata"]["uid"]
    known = {j["metadata"]["uid"] for j in v.get("jobs", namespace=v.NS)["items"]}
    excluded = "osc-delete-active-" + str(time.time_ns())
    assert all(n["metadata"]["labels"].get("kubernetes.io/hostname") != excluded for n in workers())
    selector = {"matchExpressions": [{"key": "kubernetes.io/hostname", "operator": "NotIn", "values": [excluded]}]}
    r.change_selector(args.name, uid, None, selector)

    def installer_running():
        jobs = {j["metadata"]["uid"] for j in v.get("jobs", namespace=v.NS)["items"]
                if r.OWNER in j["metadata"].get("labels", {}) and j["metadata"]["uid"] not in known and not r.terminal(j)}
        for pod in v.get("pods", namespace=v.NS)["items"]:
            if any(o["uid"] in jobs for o in pod["metadata"].get("ownerReferences", [])):
                statuses = pod.get("status", {}).get("initContainerStatuses", []) + pod.get("status", {}).get("containerStatuses", [])
                if any(s.get("state", {}).get("running") for s in statuses):
                    return pod["metadata"]["name"]
        return None

    pod = v.wait("installer running before KataConfig deletion", installer_running, args.timeout)
    v.snapshot(args.output, "delete-during-rollout-before")
    roots = {j["metadata"]["uid"] for j in v.get("jobs", namespace=v.NS)["items"]
             if j["metadata"].get("labels", {}).get("kata-deploy/operation") in ("rollout", "coverage")}
    v.oc("delete", "kataconfig", args.name, "--wait=false")

    def finalized():
        jobs = v.get("jobs", namespace=v.NS)["items"]
        active = [j for j in jobs if not r.terminal(j)]
        assert sum(r.OWNER in j["metadata"].get("labels", {}) for j in active) <= 1, "Concurrent node mutation Jobs"
        fleet = [j for j in jobs if j["metadata"].get("labels", {}).get("kata-deploy/operation") in ("rollout", "coverage")]
        assert all(j["metadata"]["uid"] in roots for j in fleet), "A new install dispatcher started after deletion"
        cleanup = v.cleanup_jobs({"items": jobs}, uid)
        assert not cleanup or all(r.terminal(j) for j in fleet), "Cleanup started before active fleet work finished"
        return not v.get("kataconfig", args.name)

    v.wait("active rollout finishes before cleanup and finalization", finalized, args.timeout)
    v.wait("RuntimeClass removed", lambda: not v.get("runtimeclass", "kata"), 120)
    v.wait("monitor removed", lambda: not v.get("daemonset", v.MONITOR, v.NS), 120)
    v.snapshot(args.output, "delete-during-rollout-after")
    return {"case": "delete-during-rollout", "active_installer_pod": pod, "cleanup_serialized": True}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--machineset", required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--name", default="example-kataconfig")
    parser.add_argument("--timeout", type=int, default=1800)
    args = parser.parse_args()
    os.umask(0o077)
    args.output.mkdir(parents=True, mode=0o700)
    if v.get("kataconfig")["items"] or v.get("namespace", NAMESPACE):
        raise RuntimeError("Use a disposable cluster with no KataConfig or previous test namespace")
    nodes = workers()
    if len(nodes) < 2 or any(v.condition(n, "Ready").get("status") != "True" or n["spec"].get("unschedulable") for n in nodes):
        raise RuntimeError("At least two Ready, schedulable workers are required")
    if v.get("machineautoscalers.autoscaling.openshift.io", namespace=MACHINE_NS)["items"]:
        raise RuntimeError("Disable automatic MachineSet scaling before this test")
    machineset = v.get(MACHINESETS, args.machineset, MACHINE_NS)
    if not machineset or machineset["metadata"].get("labels", {}).get("machine.openshift.io/cluster-api-machine-role") == "master":
        raise RuntimeError("Select a worker MachineSet")
    for machine in owned_machines(machineset):
        assert machine.get("status", {}).get("nodeRef", {}).get("name") in {n["metadata"]["name"] for n in nodes}, "MachineSet contains a non-worker or unready Machine"
    v.create_test_namespace(NAMESPACE)
    v.oc("create", "-f", "-", input=json.dumps(v.kataconfig(args.name)))
    v.wait("all workers installed", lambda: status(args.name, [n["metadata"]["name"] for n in nodes]), args.timeout)
    results = [selector_test(args, nodes), machine_test(args, nodes, machineset), delete_while_active(args)]
    v.host_checks(nodes, args.output, removed=True)
    v.oc("delete", "namespace", NAMESPACE, "--timeout=180s")
    v.save_json(args.output / "results.json", {"result": "passed", "cases": results})
    print("PASS: Selector changes, real worker addition/removal, and deletion during rollout", flush=True)


if __name__ == "__main__":
    main()
