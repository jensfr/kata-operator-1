#!/usr/bin/env python3
"""Fetch signed Brew RPMs that the pinned RHEL base images do not contain."""

import argparse
from concurrent.futures import ThreadPoolExecutor, as_completed
import hashlib
import json
from pathlib import Path
import socket
import ssl
import subprocess
from urllib.parse import quote, urlsplit
import xmlrpc.client

ROOT = Path(__file__).resolve().parent
QUERY = "%{NAME}-%{EPOCHNUM}:%{VERSION}-%{RELEASE}.%{ARCH} %{SHA256HEADER} [%{PAYLOADDIGEST}]\n"


def package_name(nevra):
    name, epoch_version, release_arch = nevra.rsplit("-", 2)
    return name + "-" + epoch_version.split(":", 1)[1] + "-" + release_arch


def multicall(hub, method, values):
    results = []
    for start in range(0, len(values), 100):
        calls = [{"methodName": method, "params": [value]} for value in values[start:start + 100]]
        for result in hub.multiCall(calls):
            if not isinstance(result, list) or len(result) != 1:
                raise ValueError("Brew lookup failed: " + method)
            results.append(result[0])
    return results


def needed_packages(manifest):
    wanted = {}
    inventories = {}
    for stage in ("kata-builder", "rhel-builder", "assembler"):
        image = manifest["base_images"][stage]
        if image not in inventories:
            result = subprocess.run(
                ["podman", "run", "--rm", "--platform", "linux/amd64", "--network", "none",
                 "--entrypoint", "rpm", image, "-qa", "--qf", QUERY],
                check=True, capture_output=True, text=True,
            )
            inventories[image] = set(result.stdout.splitlines())
        for row in (ROOT / manifest["rpms"][stage]["file"]).read_text().splitlines():
            if row in inventories[image]:
                continue
            name = row.split()[0]
            if name in wanted and wanted[name] != row:
                raise ValueError("Stage locks disagree about package content: " + name)
            wanted[name] = row
    return wanted


def verify_packages(directory, wanted, image):
    files = ["/rpms/" + package_name(name) + ".rpm" for name in sorted(wanted)]
    command = ["podman", "run", "--rm", "--platform", "linux/amd64", "--network", "none",
               "--volume", str(directory.resolve()) + ":/rpms:ro", "--entrypoint", "rpm", image]
    subprocess.run(command + ["-K", "--nosignature", *files], check=True, capture_output=True, text=True)
    result = subprocess.run(
        command + ["-qp", "--qf", QUERY, *files], check=True, capture_output=True, text=True,
    )
    if sorted(result.stdout.splitlines()) != sorted(wanted.values()):
        raise ValueError("Downloaded RPM header or payload digests differ from the locks")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--brew-hub", required=True)
    parser.add_argument("--brew-root", required=True)
    parser.add_argument("--ca-file", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--jobs", type=int, default=4)
    args = parser.parse_args()
    for value in (args.brew_hub, args.brew_root):
        parsed = urlsplit(value)
        if parsed.scheme != "https" or parsed.username or parsed.password or parsed.query or parsed.fragment:
            parser.error("Brew endpoints must be HTTPS URLs without credentials or query strings")
    if not 1 <= args.jobs <= 8:
        parser.error("--jobs must be between 1 and 8")
    socket.setdefaulttimeout(30)
    manifest = json.loads((ROOT / "input-locks.json").read_text())
    wanted = needed_packages(manifest)
    names = sorted(wanted)
    print(f"Resolving {len(names)} signed RPMs from Brew", flush=True)
    hub = xmlrpc.client.ServerProxy(args.brew_hub, context=ssl.create_default_context(cafile=str(args.ca_file)))
    rpms = multicall(hub, "getRPM", [package_name(name) for name in names])
    if any(rpm is None for rpm in rpms):
        missing = [name for name, rpm in zip(names, rpms) if rpm is None]
        raise ValueError("Pinned RPMs missing from Brew: " + ", ".join(missing))
    ids = sorted({rpm["build_id"] for rpm in rpms})
    builds = dict(zip(ids, multicall(hub, "getBuild", ids)))
    signatures = multicall(hub, "queryRPMSigs", [rpm["id"] for rpm in rpms])
    args.output.mkdir(parents=True, exist_ok=True, mode=0o700)
    (args.output / "expected.rpms").write_text("".join(wanted[name] + "\n" for name in names))
    present = {name: row for name, row in wanted.items() if (args.output / (package_name(name) + ".rpm")).is_file()}
    if present:
        verify_packages(args.output, present, manifest["base_images"]["kata-builder"])
        print(f"Verified {len(present)} RPMs from the previous download", flush=True)
    previous = {}
    sources_file = args.output / "sources.json"
    if sources_file.exists():
        previous = {entry["nevra"]: entry for entry in json.loads(sources_file.read_text())}

    def fetch(item):
        name, rpm, sigs = item
        actual = f"{rpm['name']}-{rpm.get('epoch') or 0}:{rpm['version']}-{rpm['release']}.{rpm['arch']}"
        if actual != name:
            raise ValueError("Brew returned a different package identity: " + name)
        build = builds[rpm["build_id"]]
        volume = build.get("volume_name", "DEFAULT")
        prefix = "" if volume in (None, "DEFAULT") else "/vol/" + quote(volume, safe="")
        base = args.brew_root.rstrip("/") + prefix + "/packages/" + "/".join(
            quote(str(build[key]), safe="") for key in ("name", "version", "release"))
        keys = sorted({sig["sigkey"] for sig in sigs if sig.get("sigkey")})
        if not keys:
            raise ValueError("No signed Brew copy found: " + name)
        filename = package_name(name) + ".rpm"
        path = args.output / filename
        old = previous.get(name)
        if name in present:
            checksum = hashlib.sha256(path.read_bytes()).hexdigest()
            if old and old.get("lock") == wanted[name] and old.get("sha256") == checksum:
                return old
            return {"nevra": name, "lock": wanted[name], "source": "previous download, verified against lock",
                    "sha256": checksum}
        temporary = path.with_suffix(".rpm.part")
        last_error = ""
        for key in keys:
            url = base + "/data/signed/" + quote(key, safe=",") + "/" + rpm["arch"] + "/" + filename
            result = subprocess.run(
                ["curl", "--fail", "--silent", "--show-error", "--location", "--proto-redir", "=https",
                 "--retry", "4", "--retry-all-errors", "--retry-delay", "1",
                 "--connect-timeout", "10", "--max-time", "600", "--continue-at", "-",
                 "--output", str(temporary), url], capture_output=True, text=True,
            )
            if result.returncode == 0:
                temporary.replace(path)
                return {"nevra": name, "lock": wanted[name], "url": url,
                        "sha256": hashlib.sha256(path.read_bytes()).hexdigest()}
            last_error = result.stderr.strip()
        temporary.unlink(missing_ok=True)
        raise ValueError("Could not download a signed Brew copy: " + name + ": " + last_error)

    completed = []
    errors = []
    with ThreadPoolExecutor(max_workers=args.jobs) as pool:
        pending = [pool.submit(fetch, item) for item in zip(names, rpms, signatures)]
        for future in as_completed(pending):
            try:
                entry = future.result()
            except Exception as error:
                errors.append(str(error))
                print("Download failed: " + str(error), flush=True)
                continue
            completed.append(entry)
            sources_file.write_text(json.dumps(completed, indent=2) + "\n")
            print(f"Downloaded {len(completed)}/{len(names)}: {entry['nevra']}", flush=True)
    if errors:
        raise ValueError("\n".join(errors))
    verify_packages(args.output, wanted, manifest["base_images"]["kata-builder"])
    print(f"Verified locked header and payload digests for {len(wanted)} downloaded RPMs", flush=True)


if __name__ == "__main__":
    main()
