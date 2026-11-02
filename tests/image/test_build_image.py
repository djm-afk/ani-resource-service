"""Exercise the real image orchestrator with Docker as the external boundary."""

import contextlib
import importlib.util
import io
import json
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest import mock


spec = importlib.util.spec_from_file_location(
    "build_image", Path(__file__).resolve().parents[2] / "scripts/build-image.py")
image = importlib.util.module_from_spec(spec)
spec.loader.exec_module(image)


class ImageBuildTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        for name in ("Dockerfile", ".dockerignore", "go.mod", "go.sum",
                     "THIRD_PARTY_NOTICES.go-kratos-layout.txt"):
            (self.root / name).write_text(name + "\n")
        for name in ("api/ani/network/v1/network.pb.go", "cmd/ani-resource-service/main.go",
                     "internal/server/server.go", "migrations/embed.go"):
            self.write(name, "package example\n")
        self.write("migrations/0001_initial.sql", 'CREATE TABLE example (id int);\n')
        self.write("migrations/compute/recovery/0001_restore.sql", "SELECT 1;\n")
        self.write("pkg/common/runtime.go", "package common\n")
        self.write("internal/data/sandbox/filehelper/workspace.py", "print('workspace helper')\n")
        self.args = image.arguments(["--image", "registry.example/ani/resource:test",
                                     "--version", "test", "--source-revision", "a" * 40])
        self.image_id = "sha256:" + "1" * 64
        self.manifest_digest = "sha256:" + "2" * 64
        self.commands = []

    def write(self, name, content):
        path = self.root / name
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(content)

    def docker(self, command, **kwargs):
        self.commands.append(command)
        if command[1:3] == ["buildx", "build"]:
            self.context = Path(command[-1])
            self.build_args = dict(command[i + 1].split("=", 1)
                                   for i, arg in enumerate(command) if arg == "--build-arg")
            metadata = Path(command[command.index("--metadata-file") + 1])
            metadata.write_text(json.dumps({"containerimage.config.digest": self.image_id,
                                            "containerimage.digest": self.manifest_digest,
                                            "containerimage.descriptor": {
                                                "digest": self.manifest_digest,
                                                "mediaType": "application/vnd.oci.image.manifest.v1+json"}}))
            return subprocess.CompletedProcess(command, 0)
        return subprocess.CompletedProcess(command, 0, stdout=json.dumps({
            "Id": self.image_id, "Os": "linux", "Architecture": "amd64",
            "Config": {"User": "10001:10001", "Entrypoint": ["/app/bin/server"],
                       "Cmd": ["-conf", "/app/configs"], "Labels": {
                           "org.opencontainers.image.version": "test",
                           "org.opencontainers.image.revision": "a" * 40,
                           "io.ani.source-snapshot": self.build_args["SOURCE_SNAPSHOT"]}}}))

    def build(self, docker=None):
        with contextlib.redirect_stderr(io.StringIO()):
            return image.build(self.root, self.args, docker or self.docker)

    def test_build_uses_isolated_context_and_records_local_not_registry_digest(self):
        for name in ("configs/config.yaml", "internal/server/private.key",
                     "internal/data/sandbox/filehelper/private.key",
                     "internal/server/server_test.go", ".private/token", ".git/config"):
            self.write(name, "do not send\n")
        result = self.build()
        self.assertEqual(result["source_revision"], "a" * 40)
        self.assertIsNone(result["source_dirty"])
        self.assertEqual(result["local_image_id"], self.image_id)
        self.assertEqual(result["local_config_digest"], self.image_id)
        self.assertEqual(result["local_descriptor_digest"], self.manifest_digest)
        self.assertIsNone(result["registry_digest"])
        self.assertFalse(result["pushed"])
        self.assertTrue((self.context / "api/ani/network/v1/network.pb.go").is_file())
        self.assertTrue((self.context / "internal/server/server.go").is_file())
        for name in ("configs", "internal/server/private.key", "internal/server/server_test.go",
                     "internal/data/sandbox/filehelper/private.key",
                     ".private", ".git"):
            self.assertFalse((self.context / name).exists(), name)
        self.assertTrue((self.context / "migrations/0001_initial.sql").is_file())
        self.assertTrue((self.context / "migrations/compute/recovery/0001_restore.sql").is_file())
        self.assertTrue((self.context / "pkg/common/runtime.go").is_file())
        self.assertEqual((self.context / "internal/data/sandbox/filehelper/workspace.py").read_text(),
                         "print('workspace helper')\n")
        manifest = json.loads(Path(result["source_manifest"]).read_text())
        self.assertIn("internal/data/sandbox/filehelper/workspace.py", {row["path"] for row in manifest})
        self.assertIn("--load", self.commands[0])
        self.assertNotIn("--push", self.commands[0])
        self.assertEqual(len(list((self.root / ".work/images").glob("*/result.json"))), 1)

    def test_containerd_index_id_without_config_digest_is_supported(self):
        def docker(command, **kwargs):
            result = self.docker(command, **kwargs)
            if command[1:3] == ["buildx", "build"]:
                path = Path(command[command.index("--metadata-file") + 1])
                data = json.loads(path.read_text())
                del data["containerimage.config.digest"]
                data["containerimage.descriptor"]["mediaType"] = "application/vnd.oci.image.index.v1+json"
                path.write_text(json.dumps(data))
            else:
                data = json.loads(result.stdout)
                data["Id"] = self.manifest_digest
                data["Descriptor"] = {"digest": self.manifest_digest}
                result.stdout = json.dumps(data)
            return result

        result = self.build(docker)
        self.assertEqual(result["local_image_id"], self.manifest_digest)
        self.assertIsNone(result["local_config_digest"])
        self.assertEqual(result["local_descriptor_digest"], self.manifest_digest)
        self.assertEqual(result["local_descriptor_media_type"], "application/vnd.oci.image.index.v1+json")

    def test_snapshot_preserves_nested_go_module_boundaries(self):
        modules = {
            "api/instance/terminal/go.mod": "module example.com/instance/terminal\n\ngo 1.26.7\n",
            "api/instance/terminal/go.sum": "example.com/dependency v1.0.0 h1:fixture\n",
            "api/vm/console/go.mod": "module example.com/vm/console\n\ngo 1.26.7\n",
        }
        for name, content in modules.items():
            self.write(name, content)
        self.write("api/instance/terminal/v1/terminal.pb.go", "package terminalv1\n")
        self.write("api/vm/console/v1/console.pb.go", "package consolev1\n")

        result = self.build()

        for name, content in modules.items():
            self.assertTrue((self.context / name).is_file(), name)
            self.assertEqual((self.context / name).read_text(), content)
        manifest = json.loads(Path(result["source_manifest"]).read_text())
        self.assertTrue(set(modules).issubset({row["path"] for row in manifest}))

    def test_source_is_frozen_before_docker_runs(self):
        original = (self.root / "cmd/ani-resource-service/main.go").read_text()

        def docker(command, **kwargs):
            if command[1:3] == ["buildx", "build"]:
                self.write("cmd/ani-resource-service/main.go", "changed during build\n")
            return self.docker(command, **kwargs)

        result = self.build(docker)
        self.assertEqual((self.context / "cmd/ani-resource-service/main.go").read_text(), original)
        manifest = json.loads(Path(result["source_manifest"]).read_text())
        self.assertTrue(any(row["path"] == "cmd/ani-resource-service/main.go" for row in manifest))

    def test_build_failure_stops_before_inspection_and_success_receipt(self):
        docker = mock.Mock(side_effect=subprocess.CalledProcessError(1, ["docker", "buildx"]))
        with self.assertRaises(subprocess.CalledProcessError):
            self.build(docker)
        self.assertEqual(docker.call_count, 1)
        self.assertEqual(list((self.root / ".work/images").glob("*/result.json")), [])

    def test_tag_replaced_after_build_is_rejected(self):
        def docker(command, **kwargs):
            result = self.docker(command, **kwargs)
            if command[1:3] == ["image", "inspect"]:
                data = json.loads(result.stdout)
                data["Id"] = "sha256:" + "3" * 64
                result.stdout = json.dumps(data)
            return result

        with self.assertRaisesRegex(ValueError, "does not match"):
            self.build(docker)
        self.assertEqual(list((self.root / ".work/images").glob("*/result.json")), [])

    def test_symlink_input_is_rejected_before_docker(self):
        (self.root / "internal/server/linked.go").symlink_to(self.root / "go.mod")
        docker = mock.Mock()
        with self.assertRaisesRegex(ValueError, "symlink"):
            self.build(docker)
        docker.assert_not_called()

    def test_missing_export_revision_is_rejected_before_docker(self):
        self.args.source_revision = ""
        docker = mock.Mock()
        with self.assertRaisesRegex(ValueError, "source-revision"):
            self.build(docker)
        docker.assert_not_called()

    def test_image_requires_explicit_tag(self):
        with contextlib.redirect_stderr(io.StringIO()), self.assertRaises(SystemExit):
            image.arguments(["--image", "registry.example/ani/resource", "--version", "test"])


if __name__ == "__main__":
    unittest.main()
