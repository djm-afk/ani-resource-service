#!/usr/bin/env python3
"""Verify the exact first-party provenance inventory, without assigning licenses."""

import base64
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile


def require(condition, message):
    if not condition:
        raise ValueError(message)


def go_json(root, arguments, environment=None):
    result = subprocess.run(
        ["go", *arguments], cwd=root, env=environment,
        check=True, capture_output=True, text=True,
    )
    return json.loads(result.stdout)


def published_module(root, module, version):
    environment = dict(os.environ, GOWORK="off", GOPROXY="direct")
    result = go_json(root, ["mod", "download", "-json", f"{module}@{version}"], environment)
    if not result.get("Origin"):
        # Some proxy cache entries omit VCS metadata. Fetch into an isolated
        # cache rather than treating absent Origin as verified provenance.
        with tempfile.TemporaryDirectory(prefix="ani-provenance.") as temporary:
            environment["GOMODCACHE"] = str(Path(temporary) / "mod")
            result = go_json(root, ["mod", "download", "-json", f"{module}@{version}"], environment)
    return result


def verify(root):
    manifest = json.loads((root / "docs/scaffold/first-party-modules.json").read_text())
    require(manifest.get("schema_version") == 1, "unsupported provenance manifest")
    entries = manifest["modules"]
    inventory = {entry["module"]: entry for entry in entries}
    require(len(entries) == 4 and len(inventory) == 4, "exactly four reviewed module identities are required")
    go_mod = go_json(root, ["mod", "edit", "-json"])
    require(not go_mod.get("Replace"), "replacement modules are not published provenance")
    requirements = {item["Path"]: item["Version"] for item in go_mod["Require"]}
    sums = {}
    for line in (root / "go.sum").read_text().splitlines():
        module, version, checksum = line.split()
        require((module, version) not in sums, "duplicate go.sum identity")
        sums[module, version] = checksum

    bom = json.loads((root / "docs/scaffold/bom.cdx.json").read_text())
    seen = set()
    for component in bom["components"]:
        module = component["name"]
        licenses = component.get("evidence", {}).get("licenses", [])
        if module not in inventory:
            require(licenses and all(item.get("license", {}).get("id") for item in licenses),
                    f"third-party SPDX evidence missing: {module}")
            continue
        require(module not in seen, f"duplicate first-party component: {module}")
        seen.add(module)
        entry = inventory[module]
        version = entry["version"]
        require(entry["license_status"] == "UNDECLARED", f"license status changed: {module}")
        require(not licenses and not component.get("licenses"), f"undeclared license was relabeled: {module}")
        require(component["version"] == version and requirements.get(module) == version,
                f"BOM or go.mod pin differs from reviewed version: {module}")
        for suffix, key in [("", "sum"), ("/go.mod", "go_mod_sum")]:
            require(sums.get((module, version + suffix)) == entry[key], f"go.sum differs: {module}{suffix}")
        expected_digest = base64.b64decode(entry["sum"].removeprefix("h1:"), validate=True).hex()
        require(len(expected_digest) == 64 and any(
            item.get("alg") == "SHA-256" and item.get("content") == expected_digest
            for item in component.get("hashes", [])
        ), f"BOM hash differs from reviewed module sum: {module}")

        downloaded = published_module(root, module, version)
        require(downloaded.get("Path") == module and downloaded.get("Version") == version,
                f"downloaded module identity differs: {module}")
        require(downloaded.get("Sum") == entry["sum"] and downloaded.get("GoModSum") == entry["go_mod_sum"],
                f"published module checksums differ: {module}")
        origin = downloaded.get("Origin", {})
        require(all(origin.get(key, "") == entry["origin"].get(key, "")
                    for key in ("VCS", "URL", "Subdir", "Hash")), f"published Origin differs: {module}")
    require(seen == set(inventory), "reviewed first-party component missing from full BOM")
    print(f"provenance verified: total={len(bom['components'])} first_party_license_undeclared={len(seen)} "
          f"third_party_spdx_evidence={len(bom['components']) - len(seen)}")


if __name__ == "__main__":
    try:
        verify(Path(__file__).resolve().parents[1])
    except (ValueError, KeyError, OSError, subprocess.CalledProcessError) as error:
        print(f"verify-first-party-modules: {error}", file=sys.stderr)
        sys.exit(1)
