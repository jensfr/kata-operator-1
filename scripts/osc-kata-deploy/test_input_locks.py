"""Check build-input drift and fail-closed package installation."""

import importlib.util
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[2]
ARTIFACT = ROOT / "scripts/osc-artifacts"
HELPER = ROOT / "scripts/osc-kata-deploy"
spec = importlib.util.spec_from_file_location("input_locks", ARTIFACT / "check-input-locks.py")
locks = importlib.util.module_from_spec(spec)
spec.loader.exec_module(locks)
fetch_spec = importlib.util.spec_from_file_location("fetch_locked_rpms", ARTIFACT / "fetch-locked-rpms.py")
fetch = importlib.util.module_from_spec(fetch_spec)
fetch_spec.loader.exec_module(fetch)


class InputLockTests(unittest.TestCase):
    def temporary(self):
        parent = ROOT / "build/_output"
        parent.mkdir(parents=True, exist_ok=True)
        return tempfile.TemporaryDirectory(dir=parent)

    def test_committed_recipes_and_inventories_match(self):
        locks.check(ARTIFACT, "input-locks.json", "Containerfile")
        locks.check(HELPER, "helper-input-locks.json", "Containerfile.helper")

    def test_floating_base_and_changed_package_inventory_fail_before_build(self):
        with self.temporary() as directory:
            path = Path(directory)
            shutil.copytree(HELPER / "locks", path / "locks")
            for name in ("helper-input-locks.json", "Containerfile.helper"):
                shutil.copyfile(HELPER / name, path / name)
            recipe = path / "Containerfile.helper"
            original = recipe.read_text()
            manifest = json.loads((path / "helper-input-locks.json").read_text())
            recipe.write_text(original.replace(manifest["base_images"]["helper"], "registry.access.redhat.com/ubi9/ubi-minimal:latest"))
            with self.assertRaisesRegex(ValueError, "not locked"):
                locks.check(path, "helper-input-locks.json", "Containerfile.helper")
            recipe.write_text(original)
            rpm_lock = path / "locks/helper.rpms"
            rpm_lock.write_text(rpm_lock.read_text().replace("0:", "1:", 1))
            with self.assertRaisesRegex(ValueError, "checksum differs"):
                locks.check(path, "helper-input-locks.json", "Containerfile.helper")

    def test_locked_source_arguments_reject_overrides(self):
        manifest = json.loads((ARTIFACT / "input-locks.json").read_text())
        command = ["python3", str(ARTIFACT / "locks/verify-source.py"), str(ARTIFACT / "input-locks.json"),
                   "--ref", manifest["kata"]["ref"], "--commit", manifest["kata"]["commit"],
                   "--go-version", manifest["go"]["version"], "--go-sha256", manifest["go"]["sha256"]]
        self.assertEqual(subprocess.run(command, capture_output=True).returncode, 0)
        for flag in ("--ref", "--commit", "--go-version", "--go-sha256"):
            changed = command.copy()
            changed[changed.index(flag) + 1] = "changed"
            self.assertNotEqual(subprocess.run(changed, capture_output=True).returncode, 0, flag)

    def test_package_content_and_transitive_dependency_drift_fail(self):
        row = "example-0:1.0-1.el9.x86_64 " + "a" * 64 + " " + "b" * 64 + "\n"
        with self.temporary() as directory:
            path = Path(directory)
            (path / "expected.rpms").write_text(row)
            (path / "rpm").write_text('#!/bin/sh\nprintf "%s" "$ACTUAL_RPMS"\n')
            (path / "manager").write_text('#!/bin/sh\nexit "$MANAGER_EXIT"\n')
            for name in ("rpm", "manager"):
                (path / name).chmod(0o700)
            for script in (ARTIFACT / "locks/install-rpms.sh", HELPER / "locks/install-rpms.sh"):
                for name, actual, manager_exit, success in [
                    ("match", row, 0, True),
                    ("cached match skips package access", row, 1, True),
                    ("same version, different content", row.replace("a" * 64, "c" * 64), 0, False),
                    ("extra dependency", row + row.replace("example-", "extra-"), 0, False),
                    ("unavailable package", "", 1, False),
                ]:
                    with self.subTest(script=str(script), case=name):
                        env = dict(os.environ, PATH=str(path) + os.pathsep + os.environ["PATH"],
                                   ACTUAL_RPMS=actual, MANAGER_EXIT=str(manager_exit))
                        result = subprocess.run(["sh", str(script), str(path / "expected.rpms"), str(path / "manager")],
                                                env=env, capture_output=True, text=True)
                        self.assertEqual(result.returncode == 0, success, result.stdout + result.stderr)
                        self.assertEqual("Verified locked RPM set" in result.stdout, success)
                env = dict(os.environ, PATH=str(path) + os.pathsep + os.environ["PATH"],
                           ACTUAL_RPMS=row.replace("a" * 64, "c" * 64), MANAGER_EXIT="0")
                result = subprocess.run(["sh", str(script), str(path / "expected.rpms"), "--verify-only"],
                                        env=env, capture_output=True, text=True)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("FATAL:", result.stderr)

    def test_repository_override_replaces_defaults_and_requires_its_mount(self):
        row = "example-0:1.0-1.el9.x86_64 " + "a" * 64 + " " + "b" * 64 + "\n"
        with self.temporary() as directory:
            path = Path(directory)
            (path / "expected.rpms").write_text(row)
            (path / "actual.rpms").write_text("")
            (path / "rpm").write_text('#!/bin/sh\ncat "$ACTUAL_FILE"\n')
            (path / "manager").write_text(
                '#!/bin/sh\nprintf "%s\\n" "$@" >> "$CALLS_FILE"\n'
                'case " $* " in *" install "*) cp "$EXPECTED_FILE" "$ACTUAL_FILE";; esac\n'
            )
            for name in ("rpm", "manager"):
                (path / name).chmod(0o700)
            repo = path / "repos"
            repo.mkdir()
            (repo / "osc.repo").write_text("[locked-build]\nenabled=1\n")
            env = dict(os.environ, PATH=str(path) + os.pathsep + os.environ["PATH"],
                       ACTUAL_FILE=str(path / "actual.rpms"), EXPECTED_FILE=str(path / "expected.rpms"),
                       CALLS_FILE=str(path / "calls"), OSC_RPM_REPOS_DIR=str(repo), OSC_RPM_REPOS_REQUIRED="1")
            command = ["sh", str(ARTIFACT / "locks/install-rpms.sh"), str(path / "expected.rpms"), str(path / "manager")]
            result = subprocess.run(command, env=env, capture_output=True, text=True)
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
            self.assertEqual((path / "calls").read_text().splitlines().count("--setopt=reposdir=" + str(repo)), 2)
            (path / "actual.rpms").write_text("")
            (path / "calls").unlink()
            (repo / "osc.repo").unlink()
            result = subprocess.run(command, env=env, capture_output=True, text=True)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("repository configuration is missing or empty", result.stderr)
            self.assertFalse((path / "calls").exists())

            (repo / "osc.repo").write_text("[changed-repository]\nenabled=1\n")
            env["OSC_RPM_REPOS_SHA256"] = "0" * 64
            result = subprocess.run(command, env=env, capture_output=True, text=True)
            self.assertNotEqual(result.returncode, 0)
            self.assertFalse((path / "calls").exists())

    def test_repository_build_does_not_supply_old_entitlements(self):
        with self.temporary() as directory:
            path = Path(directory)
            repo = path / "source.repo"
            repo.write_text("[locked-build]\nenabled=1\n")
            ca = path / "ca.pem"
            ca.write_text("test CA\n")
            cache = path / "cache"
            (cache / "repodata").mkdir(parents=True)
            metadata = cache / "repodata/repomd.xml"
            metadata.write_text("test repository metadata\n")
            cache.chmod(0o700)
            metadata.chmod(0o600)
            (path / "podman").write_text('#!/bin/sh\nprintf "%s\\n" "$@" > "$CALLS_FILE"\n')
            (path / "podman").chmod(0o700)
            env = dict(os.environ, PATH=str(path) + os.pathsep + os.environ["PATH"], CALLS_FILE=str(path / "calls"),
                       OSC_RPM_REPOS_FILE=str(repo), OSC_RPM_CA_FILE=str(ca), OSC_RPM_CACHE_DIR=str(cache))
            result = subprocess.run(["bash", str(ARTIFACT / "build-local.sh"), "-t", "localhost/osc-test:repos"],
                                    env=env, capture_output=True, text=True)
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
            args = (path / "calls").read_text()
            self.assertIn("id=rpm-repositories,src=" + str(repo), args)
            self.assertIn("id=rpm-ca,src=" + str(ca), args)
            self.assertIn("OSC_RPM_REPOS_REQUIRED=1", args)
            self.assertIn("OSC_RPM_CACHE_SHA256=", args)
            self.assertNotIn("id=entitlement", args)
            context = Path(args.splitlines()[-1])
            self.assertEqual(context.stat().st_mode & 0o777, 0o700)
            self.assertEqual((context / "rpm-cache/repodata/repomd.xml").stat().st_mode & 0o444, 0o444)
            self.assertEqual(metadata.stat().st_mode & 0o777, 0o600)
            for name in (".containerignore", ".dockerignore"):
                self.assertIn("!podman-build-secret*", (context / name).read_text())
                self.assertNotIn("!podman-build-secret*", (ARTIFACT / name).read_text())

    def test_downloaded_rpm_content_is_checked_before_installation(self):
        name = "gcc-c++-0:11.5.0-14.el9.x86_64"
        row = name + " " + "a" * 64 + " " + "b" * 64
        self.assertEqual(fetch.package_name(name), "gcc-c++-11.5.0-14.el9.x86_64")
        with self.temporary() as directory:
            for actual, valid in [(row, True), (row.replace("a" * 64, "c" * 64), False), (row + "\n" + row, False)]:
                with self.subTest(valid=valid), patch.object(fetch.subprocess, "run") as run:
                    run.return_value.stdout = actual + "\n"
                    if valid:
                        fetch.verify_packages(Path(directory), {name: row}, "test@sha256:base")
                    else:
                        with self.assertRaisesRegex(ValueError, "digests differ"):
                            fetch.verify_packages(Path(directory), {name: row}, "test@sha256:base")


if __name__ == "__main__":
    unittest.main()
