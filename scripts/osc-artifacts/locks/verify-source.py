#!/usr/bin/env python3
"""Reject build-argument overrides that disagree with the committed input lock."""

import argparse
import json

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument("manifest")
parser.add_argument("--ref", required=True)
parser.add_argument("--commit", required=True)
parser.add_argument("--go-version")
parser.add_argument("--go-sha256")
args = parser.parse_args()
with open(args.manifest) as stream:
    manifest = json.load(stream)
for actual, expected, name in [
    (args.ref, manifest["kata"]["ref"], "KATA_SOURCE_REF"),
    (args.commit, manifest["kata"]["commit"], "KATA_SOURCE_COMMIT"),
]:
    if actual != expected:
        parser.error(name + " differs from input-locks.json; update the lock for a new build input")
if args.go_version is not None or args.go_sha256 is not None:
    if args.go_version != manifest["go"]["version"] or args.go_sha256 != manifest["go"]["sha256"]:
        parser.error("Go SDK arguments differ from input-locks.json")
print("Verified locked source and toolchain arguments")
