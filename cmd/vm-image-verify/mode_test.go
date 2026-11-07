package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRunFilesystemDoesNotJudgeReadOnlyByPermissionBits is the #4 guard from the PR
// review: CDI v1.66.1 finishes a filesystem import with os.Chmod(dataFile, 0660)
// (pkg/importer/data-processor.go, resize), and mounting the volume read-only does not
// rewrite those bits. A normally imported ISO must therefore never be rejected because
// of the mode. The verifier reads content with O_RDONLY; deployment acceptance
// owns read-only mount enforcement.
func TestRunFilesystemDoesNotJudgeReadOnlyByPermissionBits(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "disk.img")
	if e := os.WriteFile(file, []byte("volume"), 0o660); e != nil {
		t.Fatal(e)
	}
	previous := filesystemSource
	filesystemSource = file
	t.Cleanup(func() { filesystemSource = previous })

	out := runFilesystem("2b1f0c2e-6d3a-4f57-9a10-3f7f5c1d8a11", "INSTALL_ISO", "Filesystem", report{})
	if strings.Contains(out.ReadOnlyProof, "world-writable") {
		t.Fatalf("a CDI-produced 0660 file was rejected by its permission bits: %+v", out)
	}
}

// A writable source file can be read and verified without any write-open probe.
// The mount policy itself is validated once in deployment acceptance.
func TestFilesystemContentValidationWithoutReadOnlyProof(t *testing.T) {
	file := filepath.Join(t.TempDir(), "disk.img")
	content := make([]byte, 65536)
	copy(content[32768:], []byte{1, 'C', 'D', '0', '0', '1', 1})
	copy(content[34816:], []byte{255, 'C', 'D', '0', '0', '1', 1})
	if err := os.WriteFile(file, content, 0o660); err != nil {
		t.Fatal(err)
	}
	old := filesystemSource
	filesystemSource = file
	t.Cleanup(func() { filesystemSource = old })
	result := run("image-fixture", "INSTALL_ISO", "Filesystem")
	if !result.Pass || result.FSType != "iso9660" || result.Checksum == "" || result.ReadOnlyProof != "" {
		t.Fatal(result)
	}
	invalid := make([]byte, 65536)
	if err := os.WriteFile(file, invalid, 0o660); err != nil {
		t.Fatal(err)
	}
	rejected := run("image-fixture", "INSTALL_ISO", "Filesystem")
	if rejected.Pass || rejected.Reason != "SOURCE_VALIDATION_FAILED" {
		t.Fatal(rejected)
	}
}
