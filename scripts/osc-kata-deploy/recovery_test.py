#!/usr/bin/env python3
"""Live recovery test on an installed, idle Kata node set. Does not uninstall Kata."""

import argparse
import copy
import json
import os
from pathlib import Path
import time

import validation as v

SELECTOR = "kataConfigPoolSelector"
OBSERVED = "kata.openshift.io/observed-generation"
OWNER = "kata-deploy-job-dispatcher/owner"


def terminal(job):
    return any(c["type"] in ("Complete", "Failed") and c["status"] == "True"
               for c in job.get("status", {}).get("conditions", []))


def change_selector(name, uid, expected, desired):
    config = v.get("kataconfig", name)
    if config["metadata"]["uid"] != uid or config["spec"].get(SELECTOR) != expected:
        raise RuntimeError("KataConfig selector changed outside the test; leaving it intact")
    patch = [{"op": "test", "path": "/metadata/resourceVersion", "value": config["metadata"]["resourceVersion"]}]
    if desired is None:
        patch.append({"op": "remove", "path": "/spec/" + SELECTOR})
    else:
        patch.append({"op": "add", "path": "/spec/" + SELECTOR, "value": desired})
    v.oc("patch", "kataconfig", name, "--type=json", "-p", json.dumps(patch))


def idle(config):
    classes = {i["metadata"]["name"] for i in v.get("runtimeclasses")["items"] if i["handler"] == "kata-qemu"}
    classes.add("kata")
    pods = json.loads(v.oc("get", "pods", "-A", "-o", "json"))["items"]
    active = [p["metadata"]["namespace"] + "/" + p["metadata"]["name"] for p in pods
              if p["spec"].get("runtimeClassName") in classes and p.get("status", {}).get("phase") not in ("Succeeded", "Failed")]
    if active:
        raise RuntimeError("Kata workloads are active: " + ", ".join(active))
    if any(not terminal(j) and (OWNER in j["metadata"].get("labels", {})
           or "kata-deploy/operation" in j["metadata"].get("labels", {})) for j in v.get("jobs", namespace=v.NS)["items"]):
        raise RuntimeError("Another dispatcher or installer is active; retry after it finishes")
    count = config["status"]["kataNodes"]["nodeCount"]
    if not v.installed(config, count):
        raise RuntimeError("KataConfig is not converged")
    return count


def interrupt(name, config_uid, kind, old_generation, known_jobs, count, output, timeout):
    deadline = time.monotonic() + timeout
    deleted = None
    roots = set()
    maximum = 0
    while time.monotonic() < deadline:
        jobs = v.get("jobs", namespace=v.NS)["items"]
        active_nodes = [j for j in jobs if OWNER in j["metadata"].get("labels", {}) and not terminal(j)]
        maximum = max(maximum, len(active_nodes))
        if maximum > 1:
            raise RuntimeError("More than one per-node mutation Job is active")
        rollout = [j for j in jobs if j["metadata"].get("labels", {}).get("kata-deploy/operation") == "rollout"
                   and any(o["uid"] == config_uid for o in j["metadata"].get("ownerReferences", []))
                   and j["metadata"]["uid"] not in known_jobs
                   and j["metadata"]["name"] != "kata-rollout-" + old_generation[:12]]
        for root in rollout:
            roots.add(root["metadata"]["uid"])
            if terminal(root) or deleted:
                continue
            candidates = [root] if kind == "dispatcher" else [
                j for j in active_nodes if j["metadata"].get("labels", {}).get(OWNER) == "kata-" + root["metadata"]["name"].removeprefix("kata-rollout-")]
            pods = v.get("pods", namespace=v.NS)["items"]
            for job in candidates:
                for pod in pods:
                    if not any(o["uid"] == job["metadata"]["uid"] for o in pod["metadata"].get("ownerReferences", [])):
                        continue
                    statuses = pod.get("status", {}).get("initContainerStatuses", []) + pod.get("status", {}).get("containerStatuses", [])
                    if pod["metadata"].get("deletionTimestamp") or not any(s.get("state", {}).get("running") for s in statuses):
                        continue
                    v.snapshot(output, kind + "-before-interruption")
                    deleted = {"pod": pod["metadata"]["name"], "uid": pod["metadata"]["uid"], "job": job["metadata"]["name"]}
                    v.oc("delete", "pod", pod["metadata"]["name"], "-n", v.NS, "--grace-period=60", "--wait=false")
                    print(f"Interrupted {kind}: {deleted['pod']}", flush=True)
                    break
                if deleted:
                    break
        config = v.get("kataconfig", name)
        if config["metadata"]["uid"] != config_uid:
            raise RuntimeError("KataConfig was replaced during the test")
        observed = config["metadata"].get("annotations", {}).get(OBSERVED)
        if observed != old_generation and v.installed(config, count):
            if not deleted:
                raise RuntimeError("Rollout completed before the requested interruption")
            v.snapshot(output, kind + "-recovered")
            return {"case": kind, "deleted": deleted, "rollout_uids": sorted(roots), "max_active_node_jobs": maximum}
        time.sleep(2)
    raise RuntimeError(f"Timed out testing {kind} recovery")


def selector_guard(name, original, count, output, timeout):
    namespace = "osc-katadeploy-selector-guard"
    if v.get("namespace", namespace):
        raise RuntimeError("Selector guard namespace already exists; inspect the previous test")
    node_name = original["status"]["kataNodes"]["installed"][0]
    node = v.get("node", node_name)
    hostname = node["metadata"]["labels"]["kubernetes.io/hostname"]
    v.create_test_namespace(namespace)
    pod = v.workload()
    pod["metadata"]["namespace"] = namespace
    pod["spec"]["nodeSelector"] = {"kubernetes.io/hostname": hostname}
    v.oc("create", "-f", "-", input=json.dumps(pod))
    v.oc("wait", "--for=condition=Ready", "pod/kata-workload", "-n", namespace, "--timeout=180s")
    workload_uid = v.get("pod", "kata-workload", namespace)["metadata"]["uid"]
    original_selector = original["spec"].get(SELECTOR)
    selector = copy.deepcopy(original_selector) or {}
    selector.setdefault("matchExpressions", []).append({"key": "kubernetes.io/hostname", "operator": "NotIn", "values": [hostname]})
    known_jobs = {j["metadata"]["uid"] for j in v.get("jobs", namespace=v.NS)["items"]}
    change_selector(name, original["metadata"]["uid"], original_selector, selector)
    try:
        v.wait("selector removal blocked by the live workload", lambda:
               v.condition(v.get("kataconfig", name), "InProgress").get("reason") == "BlockedByExistingKataPods", 120)
        live = v.get("pod", "kata-workload", namespace)
        assert live["metadata"]["uid"] == workload_uid and not live["metadata"].get("deletionTimestamp"), "Workload was deleted"
        assert v.oc("exec", "kata-workload", "-n", namespace, "--", "cat", "/data/probe").strip() == "kata-ok"
        for job in v.get("jobs", namespace=v.NS)["items"]:
            if job["metadata"]["uid"] not in known_jobs and job["metadata"].get("labels", {}).get("kata-deploy/operation") in ("rollout", "cleanup"):
                raise RuntimeError("Selector cleanup started despite a live workload")
        v.snapshot(output, "selector-removal-blocked")
    finally:
        change_selector(name, original["metadata"]["uid"], selector, original_selector)
        v.wait("selector restored with the workload still running", lambda: v.installed(v.get("kataconfig", name), count), timeout)
    assert v.oc("exec", "kata-workload", "-n", namespace, "--", "cat", "/data/probe").strip() == "kata-ok"
    v.oc("delete", "namespace", namespace, "--timeout=180s")
    return {"case": "selector-removal-guard", "node": node_name, "workload_preserved": True, "original_selector_restored": True}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--name", default="example-kataconfig")
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--timeout", type=int, default=1200)
    args = parser.parse_args()
    os.umask(0o077)
    args.output.mkdir(parents=True, mode=0o700)
    original = v.get("kataconfig", args.name)
    count = idle(original)
    original_selector = original["spec"].get(SELECTOR)
    v.save_json(args.output / "original-kataconfig.json", original)
    results = []
    for kind in ("dispatcher", "installer"):
        current = v.get("kataconfig", args.name)
        v.wait("previous work finished", lambda: all(terminal(j) for j in v.get("jobs", namespace=v.NS)["items"]
               if OWNER in j["metadata"].get("labels", {}) or "kata-deploy/operation" in j["metadata"].get("labels", {})), 180)
        selector = copy.deepcopy(original_selector) or {}
        # Excluding an absent hostname changes the generation, without removing any selected worker.
        excluded = "osc-recovery-" + str(time.time_ns())
        assert all(n["metadata"]["labels"].get("kubernetes.io/hostname") != excluded for n in v.get("nodes")["items"])
        selector.setdefault("matchExpressions", []).append({"key": "kubernetes.io/hostname", "operator": "NotIn", "values": [excluded]})
        generation = current["metadata"]["annotations"][OBSERVED]
        known_jobs = {j["metadata"]["uid"] for j in v.get("jobs", namespace=v.NS)["items"]}
        change_selector(args.name, original["metadata"]["uid"], original_selector, selector)
        try:
            results.append(interrupt(args.name, original["metadata"]["uid"], kind, generation, known_jobs, count, args.output, args.timeout))
        finally:
            before_restore = v.get("kataconfig", args.name)["metadata"].get("annotations", {}).get(OBSERVED)
            change_selector(args.name, original["metadata"]["uid"], selector, original_selector)
            def restored():
                config = v.get("kataconfig", args.name)
                observed = config["metadata"].get("annotations", {}).get(OBSERVED)
                return (v.installed(config, count) and bool(observed) and config["spec"].get(SELECTOR) == original_selector
                        and (observed == generation or observed != before_restore))
            v.wait("original selector restored and converged", restored, args.timeout)
    results.append(selector_guard(args.name, v.get("kataconfig", args.name), count, args.output, args.timeout))
    v.save_json(args.output / "results.json", {"result": "passed", "cases": results})
    print("PASS: Interrupted rollouts recovered; selector removal preserved the workload", flush=True)


if __name__ == "__main__":
    main()
