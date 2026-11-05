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
// of the mode; the read-only proof stays the real write-open probe in probeReadOnly.
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
