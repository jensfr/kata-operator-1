import json
from pathlib import Path
import tempfile
import unittest

import prepare


class ForkPrototypeTests(unittest.TestCase):
    def test_official_tenant_is_rejected_before_writing_files(self):
        with self.assertRaises(ValueError):
            prepare.prepare(Path("unused"), "ose-osc-tenant", "prototype", "quay.io/jensfr", "kata-deploy-upstream")

    def test_render_targets_fork_and_leaves_official_pipelines_untouched(self):
        parent = prepare.ROOT / "build/_output"
        parent.mkdir(parents=True, exist_ok=True)
        before = {p: p.read_bytes() for p in (prepare.ROOT / ".tekton").glob("*") if p.is_file()}
        with tempfile.TemporaryDirectory(dir=parent) as directory:
            output = Path(directory) / "prototype"
            pipeline_dir = prepare.prepare(output, "my-test-tenant", "kata-test", "quay.io/jensfr", "kata-deploy-upstream")
            components = json.loads((output / "components.json").read_text())["items"]
            for component in components[1:]:
                self.assertEqual(component["metadata"]["namespace"], "my-test-tenant")
                self.assertEqual(component["metadata"]["annotations"]["build.appstudio.openshift.io/request"], "configure-pac-no-mr")
                self.assertEqual(component["spec"]["source"]["git"]["url"], prepare.GIT_URL)
                self.assertTrue(component["spec"]["containerImage"].startswith("quay.io/jensfr/"))
            for pipeline in pipeline_dir.glob("kata-prototype-*.yaml"):
                text = pipeline.read_text()
                self.assertNotIn("ose-osc-tenant", text)
                self.assertNotIn("@COMPONENT@", text)
                self.assertIn(prepare.GIT_URL, text)
                self.assertIn("namespace: my-test-tenant", text)
            self.assertEqual(len(list(pipeline_dir.glob("*.yaml"))), 5)
        self.assertEqual(before, {p: p.read_bytes() for p in before})

    def test_branch_cannot_inject_yaml(self):
        with self.assertRaises(ValueError):
            prepare.prepare(Path("unused"), "my-tenant", "prototype", "quay.io/jensfr", 'branch"\nother: value')


if __name__ == "__main__":
    unittest.main()
