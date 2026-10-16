"""Checks for false success and unsafe cleanup in the development validation scripts."""

from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

import validation as v


class ValidationTests(unittest.TestCase):
    def test_partial_or_missing_status_never_passes(self):
        for config in [None, {}, {"status": {"kataNodes": {"readyNodeCount": 2, "nodeCount": 2}}},
                       {"status": {"kataNodes": {"readyNodeCount": 1, "nodeCount": 2},
                                   "conditions": [{"type": "InProgress", "status": "False"}]}}]:
            self.assertFalse(v.installed(config, 2))

    def test_install_condition_is_found_by_type(self):
        config = {"status": {"kataNodes": {"readyNodeCount": 2, "nodeCount": 2}, "conditions": [
            {"type": "Other", "status": "True"}, {"type": "InProgress", "status": "False"}]}}
        self.assertTrue(v.installed(config, 2))
        self.assertFalse(v.installed(config, 0))

    def test_old_cleanup_job_does_not_fail_new_guard_check(self):
        def job(uid):
            return {"metadata": {"labels": {"kata-deploy/operation": "cleanup", "kata-deploy/kataconfig-uid": uid}}}
        self.assertEqual(v.cleanup_jobs({"items": [job("old")]}, "current"), [])
        self.assertEqual(len(v.cleanup_jobs({"items": [job("old"), job("current")]}, "current")), 1)

    def test_reboot_and_upgrade_need_real_identity_and_kernel_changes(self):
        before = dict(server="api", node="worker-a", node_uid="node-uid", kataconfig_uid="config-uid",
                      boot_id="first-boot", kernel="first-kernel")
        with self.assertRaisesRegex(RuntimeError, "has not rebooted"):
            v.verify_transition(before, before, False)
        rebooted = dict(before, boot_id="second-boot")
        v.verify_transition(before, rebooted, False)
        with self.assertRaisesRegex(RuntimeError, "kernel did not change"):
            v.verify_transition(before, rebooted, True)
        upgraded = dict(rebooted, kernel="second-kernel")
        v.verify_transition(before, upgraded, True)
        with self.assertRaisesRegex(RuntimeError, "different node_uid"):
            v.verify_transition(before, dict(upgraded, node_uid="replacement-node"), True)

    def test_missing_webhook_configuration_is_not_ready(self):
        with patch.object(v, "get", side_effect=[{"type": "kubernetes.io/tls"}, None]):
            self.assertFalse(v.webhook_ready())

    def test_namespace_waits_for_scc_allocation_and_service_account(self):
        namespace = {"metadata": {"annotations": {"openshift.io/sa.scc.uid-range": "100000/10000"}}}
        with patch.object(v, "get", return_value={"metadata": {}}):
            self.assertFalse(v.namespace_ready("test"))
        with patch.object(v, "get", side_effect=[namespace, None]):
            self.assertFalse(v.namespace_ready("test"))
        with patch.object(v, "get", side_effect=[namespace, {"metadata": {"name": "default"}}]):
            self.assertTrue(v.namespace_ready("test"))

    def test_api_error_is_not_treated_as_resource_absent(self):
        with patch.object(v, "oc", side_effect=subprocess.CalledProcessError(1, ["oc"], stderr="Forbidden")):
            for name in (None, "kata"):
                with self.assertRaises(subprocess.CalledProcessError):
                    v.get("runtimeclass", name)

    def test_empty_oc_list_after_cleanup_is_distinct_from_missing_object(self):
        for output in ("", "\n"):
            with patch.object(v, "oc", return_value=output):
                self.assertEqual(v.get("kataconfig")["items"], [])
                self.assertIsNone(v.get("kataconfig", "example-kataconfig"))

    def test_failed_uninstall_keeps_operator_and_workloads(self):
        parent = v.ROOT / "build/_output"
        parent.mkdir(parents=True, exist_ok=True)
        with tempfile.TemporaryDirectory(dir=parent) as directory:
            output = Path(directory) / "uninstall"
            with patch("sys.argv", ["validation.py", "deploy", "--undeploy", "--output", str(output)]), \
                    patch.object(v, "snapshot"), patch.object(v, "oc", side_effect=
                    subprocess.CalledProcessError(1, ["oc"], stderr="KataConfig cleanup blocked")) as command:
                with self.assertRaises(SystemExit) as error:
                    v.main()
                self.assertEqual(error.exception.code, 1)
                self.assertEqual(command.call_args_list, [unittest.mock.call(
                    "delete", "kataconfig", "example-kataconfig", "--ignore-not-found", "--timeout=1200s")])

    def test_render_preserves_checkout_and_sets_all_images(self):
        # Use the actual kustomize build, including the developer's current image transform.
        parent = v.ROOT / "build/_output"
        parent.mkdir(parents=True, exist_ok=True)
        manager = v.ROOT / "config/manager/kustomization.yaml"
        before = manager.read_bytes()
        images = {key: f"quay.io/test/{key}@sha256:" + "a" * 64 for key in v.IMAGES}
        with tempfile.TemporaryDirectory(dir=parent) as directory, patch.dict(v.IMAGES, images):
            manifest = v.render(Path(directory)).read_text()
            for image in images.values():
                self.assertIn(image, manifest)
            self.assertIn("service.beta.openshift.io/serving-cert-secret-name: webhook-server-cert", manifest)
            self.assertIn("peerpods.confidentialcontainers.org", manifest)
            self.assertIn("deploymentMode: KataDeploy", manifest)
        self.assertEqual(manager.read_bytes(), before)


if __name__ == "__main__":
    unittest.main()
