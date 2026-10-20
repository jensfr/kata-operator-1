"""Run the actual guest-prep verifier with controlled host tool results."""

import os
from pathlib import Path
import subprocess
import tempfile
import textwrap
import unittest

ROOT = Path(__file__).resolve().parents[2]


def verifier(path):
    text = path.read_text()
    body = text.split("<<'VERIFY_GUEST_AGENT'\n", 1)[1]
    return textwrap.dedent(body.split("VERIFY_GUEST_AGENT", 1)[0])


class GuestAgentTests(unittest.TestCase):
    def test_install_verifier_accepts_stripping_and_rejects_stale_or_unreadable_guests(self):
        scripts = [ROOT / "controllers/manifests/dispatcher-install-template.yaml",
                   ROOT / "scripts/osc-kata-deploy/osc-guest-prep.sh"]
        cases = [
            ("same build, stripped guest", "abc123", "abc123", 0, 0, True),
            ("old guest", "abc123", "def456", 0, 0, False),
            ("guest has no build ID", "abc123", "", 0, 0, False),
            ("payload has no build ID", "", "abc123", 0, 0, False),
            ("file failed", "abc123", "abc123", 1, 0, False),
            ("extraction failed after writing ELF", "abc123", "abc123", 0, 1, False),
        ]
        parent = ROOT / "build/_output"
        parent.mkdir(parents=True, exist_ok=True)
        with tempfile.TemporaryDirectory(dir=parent) as directory:
            commands = Path(directory)
            (commands / "file").write_text('''#!/bin/sh
if [ "$2" = - ]; then
    cat >/dev/null
    printf 'ELF, BuildID[sha1]=%s, stripped\\n' "$GUEST_ID"
else
    printf 'ELF, BuildID[sha1]=%s, not stripped\\n' "$AGENT_ID"
fi
exit "$FILE_EXIT"
''')
            (commands / "lsinitrd").write_text('''#!/bin/sh
printf ELF
exit "$EXTRACT_EXIT"
''')
            for command in commands.iterdir():
                command.chmod(0o700)
            for script in scripts:
                for name, agent, guest, file_exit, extract_exit, passes in cases:
                    with self.subTest(script=script.name, case=name):
                        env = dict(os.environ, PATH=str(commands) + os.pathsep + os.environ["PATH"],
                                   AGENT_ID=agent, GUEST_ID=guest, FILE_EXIT=str(file_exit),
                                   EXTRACT_EXIT=str(extract_exit))
                        result = subprocess.run(["bash", "-s", "--", "agent", "initrd"],
                                                input=verifier(script), env=env, text=True, capture_output=True)
                        self.assertEqual(result.returncode == 0, passes, result.stdout + result.stderr)
                        if passes:
                            self.assertIn("Guest agent verified", result.stdout)
                        else:
                            self.assertIn("FATAL:", result.stderr)
                            self.assertNotIn("Guest agent verified", result.stdout)


if __name__ == "__main__":
    unittest.main()
