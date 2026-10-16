import unittest
from unittest.mock import patch

import multinode_test as m


class MachineSetSafetyTests(unittest.TestCase):
    def test_scale_refuses_replaced_or_externally_scaled_machineset(self):
        original = {"metadata": {"name": "workers", "uid": "original"}}
        for current in [{"metadata": {"uid": "replacement"}, "spec": {"replicas": 2}},
                        {"metadata": {"uid": "original"}, "spec": {"replicas": 3}}]:
            with patch.object(m.v, "get", return_value=current), patch.object(m.v, "oc") as command:
                with self.assertRaisesRegex(RuntimeError, "changed outside the test"):
                    m.scale(original, 2, 1)
                command.assert_not_called()


if __name__ == "__main__":
    unittest.main()
