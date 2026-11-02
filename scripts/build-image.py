#!/usr/bin/env python3
"""Build one local production image and emit its machine-readable receipt."""

import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile


SERVICE = "ani-resource-service"
SOURCE = "https://github.com/zhangzhe-ctrl/ani-resource-service"
DIGEST = re.compile(r"sha256:[0-9a-f]{64}\Z")


def arguments(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--image", required=True, help="image repository with an explicit tag")
    parser.add_argument("--version", required=True)
    parser.add_argument("--platform", default="linux/amd64", choices=["linux/amd64", "linux/arm64"])
    parser.add_argument("--target", default="service", choices=["service"])
    parser.add_argument("--builder-image", default="golang:1.26.10-bookworm")
    parser.add_argument("--runtime-image", default="gcr.io/distroless/static-debian12:nonroot")
    parser.add_argument("--goproxy", default="https://goproxy.cn,https://proxy.golang.org,direct")
    parser.add_argument("--source-revision", default="", help="required for an exported source tree without Git")
    parser.add_argument("--output-dir", default=".work/images")
    parser.add_argument("--docker", default="docker")
    args = parser.parse_args(argv)
    if not re.fullmatch(r"[a-z0-9][a-z0-9._:/-]*:[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}", args.image):
        parser.error("--image must include an explicit tag and cannot be a digest")
    if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9._+-]{0,127}", args.version):
        parser.error("--version must be a bounded version or commit identifier")
    if args.source_revision and not re.fullmatch(r"[0-9a-f]{40}", args.source_revision):
        parser.error("--source-revision must be a full Git commit SHA")
    return args


def source_revision(root, supplied):
    result = subprocess.run(["git", "-C", str(root), "rev-parse", "--show-toplevel"],
                            capture_output=True, text=True)
    if result.returncode == 0 and Path(result.stdout.strip()).resolve() == root:
        revision = subprocess.check_output(["git", "-C", str(root), "rev-parse", "HEAD"], text=True).strip()
        if supplied and supplied != revision:
            raise ValueError("source revision does not match this checkout")
        dirty = bool(subprocess.check_output(
            ["git", "-C", str(root), "status", "--porcelain", "--untracked-files=normal"], text=True))
        return revision, dirty
    if not supplied:
        raise ValueError("exported source requires --source-revision; its snapshot is recorded separately")
    return supplied, None


def snapshot(root, destination):
    # Do not send the checkout, configs, credentials, tests, caches or Git to Docker.
    files = [Path(name) for name in ("Dockerfile", ".dockerignore", "go.mod", "go.sum",
                                    "THIRD_PARTY_NOTICES.go-kratos-layout.txt",
                                    "internal/data/sandbox/filehelper/workspace.py")]
    for directory in ("api", "cmd", "internal", "pkg", "migrations"):
        files.extend(path.relative_to(root) for path in (root / directory).rglob("*.go")
                     if not path.name.endswith("_test.go"))
        # Preserve nested module boundaries so their packages are not flattened
        # into this service's module alongside its pinned API dependencies.
        for name in ("go.mod", "go.sum"):
            files.extend(path.relative_to(root) for path in (root / directory).rglob(name))
    # All domains embed their own migration history, including compute recovery SQL.
    files.extend(path.relative_to(root) for path in (root / "migrations").rglob("*.sql"))
    manifest = []
    digest = hashlib.sha256()
    for relative in sorted(files):
        source = root / relative
        if any(parent.is_symlink() for parent in [source, *source.parents] if parent != root):
            raise ValueError(f"symlink is not a supported build input: {relative}")
        data = source.read_bytes()
        target = destination / relative
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_bytes(data)
        target.chmod(0o644)
        row = {"path": relative.as_posix(), "sha256": hashlib.sha256(data).hexdigest()}
        manifest.append(row)
        digest.update((row["path"] + "\0" + row["sha256"] + "\n").encode())
    return "sha256:" + digest.hexdigest(), manifest


def build(root, args, docker_run=subprocess.run):
    revision, dirty = source_revision(root, args.source_revision)
    output = Path(args.output_dir)
    if not output.is_absolute():
        output = root / output
    output.mkdir(parents=True, exist_ok=True)
    run = Path(tempfile.mkdtemp(prefix="image-", dir=output))
    context = run / "context"
    context.mkdir()
    snapshot_digest, manifest = snapshot(root, context)
    (run / "source-manifest.json").write_text(json.dumps(manifest, indent=2) + "\n")
    metadata_file = run / "buildkit.json"
    command = [args.docker, "buildx", "build", "--load", "--progress=plain",
               "--platform", args.platform, "--target", args.target,
               "--tag", args.image, "--metadata-file", str(metadata_file)]
    for key, value in {"BUILDER_IMAGE": args.builder_image, "RUNTIME_IMAGE": args.runtime_image,
                       "VERSION": args.version, "GOPROXY": args.goproxy,
                       "SOURCE_REVISION": revision, "SOURCE_SNAPSHOT": snapshot_digest}.items():
        command.extend(["--build-arg", key + "=" + value])
    command.append(str(context))
    environment = dict(os.environ, BUILDX_METADATA_PROVENANCE="max")
    docker_run(command, check=True, env=environment, stdout=sys.stderr)
    metadata = json.loads(metadata_file.read_text())
    inspected = docker_run([args.docker, "image", "inspect", args.image, "--format", "{{json .}}"],
                           check=True, capture_output=True, text=True)
    image = json.loads(inspected.stdout)
    descriptor_digest = metadata.get("containerimage.digest")
    config_digest = metadata.get("containerimage.config.digest")
    if not DIGEST.fullmatch(descriptor_digest or ""):
        raise ValueError("missing or invalid image descriptor digest returned by BuildKit")
    if config_digest is not None and not DIGEST.fullmatch(config_digest):
        raise ValueError("invalid image config digest returned by BuildKit")
    descriptor = metadata.get("containerimage.descriptor") or {}
    if descriptor.get("digest", descriptor_digest) != descriptor_digest:
        raise ValueError("BuildKit descriptor digest is inconsistent")
    image_id = image.get("Id")
    # Classic Docker identifies images by config digest. The containerd image
    # store can use an OCI index digest, with no config digest in build metadata.
    inspected_digest = (image.get("Descriptor") or {}).get("digest")
    if (not DIGEST.fullmatch(image_id or "")
            or (image_id not in {config_digest, descriptor_digest}
                and inspected_digest != descriptor_digest)):
        raise ValueError("loaded image does not match the BuildKit result")
    config = image["Config"]
    expected_labels = {"org.opencontainers.image.version": args.version,
                       "org.opencontainers.image.revision": revision,
                       "io.ani.source-snapshot": snapshot_digest}
    if any(config.get("Labels", {}).get(k) != v for k, v in expected_labels.items()):
        raise ValueError("loaded image source/version labels do not match this build")
    if (config.get("Entrypoint") != ["/app/bin/server"]
            or config.get("Cmd") != ["-conf", "/app/configs"]
            or config.get("User") != "10001:10001"
            or image["Os"] + "/" + image["Architecture"] != args.platform):
        raise ValueError("loaded image does not satisfy the production image contract")
    result = {"schema_version": 1, "service": SERVICE, "source": SOURCE, "source_revision": revision,
              "source_dirty": dirty, "source_snapshot": snapshot_digest,
              "version": args.version, "target": args.target, "platform": args.platform,
              "image": args.image, "local_image_id": image_id,
              "local_config_digest": config_digest,
              "local_descriptor_digest": descriptor_digest,
              "local_descriptor_media_type": descriptor.get("mediaType"),
              "registry_digest": None, "pushed": False,
              "builder_image": args.builder_image, "runtime_image": args.runtime_image,
              "buildkit_metadata": str(metadata_file), "source_manifest": str(run / "source-manifest.json")}
    receipt = run / "result.json"
    receipt.write_text(json.dumps(result, ensure_ascii=False, indent=2) + "\n")
    print(f"image receipt: {receipt}", file=sys.stderr)
    return result


def main():
    try:
        result = build(Path(__file__).resolve().parent.parent, arguments())
    except (OSError, ValueError, KeyError, subprocess.CalledProcessError) as error:
        print(f"image build failed: {error}", file=sys.stderr)
        return 1
    print(json.dumps(result, ensure_ascii=False))
    return 0


if __name__ == "__main__":
    sys.exit(main())
