#!/usr/bin/env python3
"""Check image recipes and captured package locks before starting a build."""

import hashlib
import json
from pathlib import Path
import re

ROOT = Path(__file__).resolve().parents[2]


def check(directory, manifest_name, recipe_name):
    manifest = json.loads((directory / manifest_name).read_text())
    if manifest.get("schema") != 1 or manifest.get("platform") != "linux/amd64":
        raise ValueError("Unsupported input lock schema or platform")
    recipe = (directory / recipe_name).read_text()
    external = {}
    stages = set()
    for line in recipe.splitlines():
        if not line.startswith("FROM "):
            continue
        match = re.fullmatch(r"FROM (\S+) AS (\S+)", line)
        if not match:
            raise ValueError("Build stages must have explicit names: " + line)
        image, stage = match.groups()
        if stage in stages:
            raise ValueError("Duplicate build stage: " + stage)
        if image != "scratch" and image not in stages:
            if not re.fullmatch(r"[^@\s]+@sha256:[0-9a-f]{64}", image):
                raise ValueError("Base image is not locked: " + image)
            external[stage] = image
        stages.add(stage)
    if external != manifest["base_images"]:
        raise ValueError("Recipe base images differ from the input lock")
    for stage, lock in manifest["rpms"].items():
        path = directory / lock["file"]
        if path.resolve().parent != (directory / "locks").resolve():
            raise ValueError("RPM lock must be inside locks/")
        data = path.read_bytes()
        if hashlib.sha256(data).hexdigest() != lock["sha256"]:
            raise ValueError("RPM lock checksum differs: " + stage)
        rows = data.decode().splitlines()
        if len(rows) != lock["packages"] or rows != sorted(set(rows)) or not rows:
            raise ValueError("RPM lock is empty, unsorted, or has duplicate rows: " + stage)
        packages = set()
        for row in rows:
            fields = row.split()
            if len(fields) != 3 or not re.fullmatch(r"[A-Za-z0-9_.+~^-]+-[0-9]+:[^\s]+\.(x86_64|noarch)", fields[0]):
                raise ValueError("Invalid RPM identity: " + row)
            if fields[0] in packages or not all(re.fullmatch(r"[0-9a-f]{64}", v) for v in fields[1:]):
                raise ValueError("Duplicate RPM or missing content fingerprint: " + row)
            packages.add(fields[0])
    if "kata" in manifest:
        for arg, expected in [("KATA_SOURCE_REF", manifest["kata"]["ref"]),
                              ("KATA_SOURCE_COMMIT", manifest["kata"]["commit"]),
                              ("GO_VERSION", manifest["go"]["version"]),
                              ("GO_SHA256", manifest["go"]["sha256"])]:
            values = re.findall(r"^ARG " + arg + r"=(\S+)$", recipe, re.MULTILINE)
            if not values or any(value != expected for value in values):
                raise ValueError("Recipe argument differs from lock: " + arg)
        checksums = "".join(digest + "  " + name + "\n" for name, digest in sorted(manifest["dependency_files"].items()))
        if (directory / "locks/kata-source.sha256").read_text() != checksums:
            raise ValueError("Kata dependency file checksums differ from the lock")
    return manifest


def main():
    for directory, manifest, recipe in [
        (ROOT / "scripts/osc-artifacts", "input-locks.json", "Containerfile"),
        (ROOT / "scripts/osc-kata-deploy", "helper-input-locks.json", "Containerfile.helper"),
    ]:
        data = check(directory, manifest, recipe)
        print(f"Verified {recipe}: {len(data['base_images'])} base images, "
              f"{sum(lock['packages'] for lock in data['rpms'].values())} RPM identities")


if __name__ == "__main__":
    main()
