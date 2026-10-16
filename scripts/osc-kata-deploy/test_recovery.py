import unittest
from unittest.mock import patch

import recovery_test as r


class RecoverySafetyTests(unittest.TestCase):
    def test_selector_restore_does_not_overwrite_external_change(self):
        for config in [{"metadata": {"uid": "replacement"}, "spec": {}},
                       {"metadata": {"uid": "original"}, "spec": {r.SELECTOR: {"matchLabels": {"other": "work"}}}}]:
            with patch.object(r.v, "get", return_value=config), patch.object(r.v, "oc") as command:
                with self.assertRaisesRegex(RuntimeError, "changed outside the test"):
                    r.change_selector("test", "original", None, {"matchLabels": {"test": "true"}})
                command.assert_not_called()


if __name__ == "__main__":
    unittest.main()
