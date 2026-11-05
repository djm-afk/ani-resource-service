// Command vm-image-verify is the fixed read-only validator that runs inside the
// image validation Job. It accepts no image, command, path or plugin from a
// caller: the only inputs are the three flags the Job template hard-codes.
//
// It proves three things and reports them as one bounded JSON document through
// the pod termination message: the source opens read-only and refuses a write
// open, the full-length SHA-256 of the addressable content, and for an ISO the
// filesystem signature reported by blkid.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

const (
	blockSource    = "/dev/source"
	terminationLog = "/dev/termination-log"
	maxReportBytes = 4096
	// A qcow2 header may legitimately be followed by sparse tail; the accepted
	// minimum is one 64 KiB cluster rather than an arbitrary magic length.
	minSourceBytes = 65536
)

// filesystemSource is the single ordinary file the validator may read. It is a
// package variable, not a constant, so the behaviour tests can point it at a
// temporary file; production always reads the mounted path below.
var filesystemSource = "/source/disk.img"

type report struct {
	Pass          bool   `json:"pass"`
	ImageID       string `json:"image_id"`
	Kind          string `json:"kind"`
	VolumeMode    string `json:"volume_mode"`
	Checksum      string `json:"checksum"`
	Bytes         int64  `json:"bytes"`
	FSType        string `json:"fs_type"`
	ReadOnlyProof string `json:"read_only_proof"`
	Reason        string `json:"reason"`
	ObservedAt    string `json:"observed_at"`
}

func main() {
	imageID := flag.String("image-id", "", "image identity this Job was created for")
	kind := flag.String("kind", "", "DISK_IMAGE or INSTALL_ISO")
	volumeMode := flag.String("volume-mode", "", "Block or Filesystem")
	flag.Parse()
	result := run(*imageID, *kind, *volumeMode)
	encoded, err := json.Marshal(result)
	if err != nil {
		encoded = []byte(`{"pass":false,"reason":"SOURCE_VALIDATION_FAILED"}`)
	}
	if len(encoded) > maxReportBytes {
		encoded = encoded[:maxReportBytes]
	}
	// The report is the only channel back, so it is written before any exit code.
	if writeErr := os.WriteFile(terminationLog, encoded, 0o600); writeErr != nil {
		fmt.Fprintln(os.Stderr, string(encoded))
	}
	if !result.Pass {
		os.Exit(1)
	}
}

func run(imageID, kind, volumeMode string) report {
	out := report{ImageID: imageID, Kind: kind, VolumeMode: volumeMode, ObservedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	if imageID == "" || (kind != "DISK_IMAGE" && kind != "INSTALL_ISO") {
		out.Reason = "VM_IMAGE_INVALID_ARGUMENT"
		return out
	}
	switch {
	case kind == "DISK_IMAGE" && volumeMode != "Block":
		out.Reason = "VM_IMAGE_INVALID_ARGUMENT"
		return out
	case kind == "INSTALL_ISO" && volumeMode != "Filesystem":
		out.Reason = "VM_IMAGE_INVALID_ARGUMENT"
		return out
	}
	if volumeMode == "Block" {
		return runBlock(imageID, kind, volumeMode, out)
	}
	return runFilesystem(imageID, kind, volumeMode, out)
}

// runBlock proves the read-only contract on the device itself. The write probe
// opens the exact same node with O_WRONLY and O_RDWR and must not succeed; the
// host block layer's read-only bit alone is never treated as the proof.
func runBlock(imageID, kind, volumeMode string, out report) report {
	out.VolumeMode, out.Pass = "Block", false
	info, err := os.Stat(blockSource)
	if err != nil {
		out.Reason = "SOURCE_VALIDATION_FAILED"
		return out
	}
	if info.Mode()&os.ModeDevice == 0 || info.Mode()&os.ModeCharDevice != 0 {
		// A regular file or a char device at the block path is an identity
		// change, not a validation failure: the Job mounted the wrong volume.
		out.Reason = "SOURCE_IDENTITY_CHANGED"
		return out
	}
	reason, proof, err := probeReadOnly(blockSource)
	if err != nil {
		out.Reason, out.ReadOnlyProof = reason, proof
		return out
	}
	out.ReadOnlyProof = proof
	file, err := os.OpenFile(blockSource, os.O_RDONLY, 0)
	if err != nil {
		out.Reason = "SOURCE_VALIDATION_FAILED"
		return out
	}
	defer file.Close()
	digest, size, err := digestStream(file)
	if err != nil {
		out.Reason = "SOURCE_VALIDATION_FAILED"
		return out
	}
	if size < minSourceBytes {
		out.Reason = "SOURCE_VALIDATION_FAILED"
		return out
	}
	out.Checksum, out.Bytes, out.Pass = digest, size, true
	return out
}

// runFilesystem reads the fixed ordinary file only. A symlink, directory or
// device node at that path is an identity change, not a validation result.
func runFilesystem(imageID, kind, volumeMode string, out report) report {
	out.VolumeMode, out.Pass = "Filesystem", false
	info, err := os.Lstat(filesystemSource)
	if err != nil || !info.Mode().IsRegular() {
		out.Reason = "SOURCE_IDENTITY_CHANGED"
		if err != nil && os.IsNotExist(err) {
			out.Reason = "SOURCE_VALIDATION_FAILED"
		}
		return out
	}
	// CDI finishes a filesystem import with os.Chmod(dataFile, 0660) and a read-only
	// mount leaves those bits on disk, so the mode says nothing about writability.
	// probeReadOnly below is the only proof used.
	reason, proof, err := probeReadOnly(filesystemSource)
	if err != nil {
		out.Reason, out.ReadOnlyProof = reason, proof
		return out
	}
	out.ReadOnlyProof = proof
	file, err := os.OpenFile(filesystemSource, os.O_RDONLY, 0)
	if err != nil {
		out.Reason = "SOURCE_VALIDATION_FAILED"
		return out
	}
	digest, size, err := digestStream(file)
	file.Close()
	if err != nil {
		out.Reason = "SOURCE_VALIDATION_FAILED"
		return out
	}
	// The hash covers the whole CDI-processed file, which may be longer than the
	// bytes the uploader sent because of qcow2 padding or an explicit resize.
	if size < minSourceBytes {
		out.Reason = "SOURCE_VALIDATION_FAILED"
		return out
	}
	// The signature check is the fixed argv form, run without a shell.
	command := exec.Command("blkid", "-p", "-s", "TYPE", "-o", "value", "--", filesystemSource)
	command.Env = []string{"LC_ALL=C", "PATH=/usr/sbin:/usr/bin:/sbin:/bin"}
	var stdout, stderr strings.Builder
	command.Stdout, command.Stderr = &stdout, &stderr
	if err = command.Run(); err != nil {
		out.Reason = "SOURCE_VALIDATION_FAILED"
		if exit := new(exec.ExitError); errors.As(err, &exit) {
			switch exit.ExitCode() {
			case 2, 4:
				out.Reason = "SOURCE_VALIDATION_FAILED"
			default:
				out.Reason = "VM_IMAGE_DEPENDENCY_UNAVAILABLE"
			}
		}
		return out
	}
	signature := strings.TrimSpace(stdout.String())
	if signature != "iso9660" && signature != "udf" {
		out.Reason = "SOURCE_VALIDATION_FAILED"
		return out
	}
	out.Checksum, out.Bytes, out.FSType, out.Pass = digest, size, signature, true
	return out
}

// probeReadOnly attempts the two write modes on the exact source path with no
// O_CREATE, so the attempt can neither fabricate a file nor a device node. It
// never writes a byte; the proof names the rejection actually observed.
func probeReadOnly(path string) (string, string, error) {
	proofs := make([]string, 0, 2)
	for _, attempt := range []struct {
		name string
		mode int
	}{{"O_WRONLY", os.O_WRONLY}, {"O_RDWR", os.O_RDWR}} {
		file, err := os.OpenFile(path, attempt.mode, 0)
		if err == nil {
			file.Close()
			return "SOURCE_IDENTITY_CHANGED", attempt.name + "=accepted", errors.New("source accepted a write open")
		}
		if !errors.Is(err, syscall.EACCES) && !errors.Is(err, syscall.EROFS) && !errors.Is(err, syscall.EPERM) {
			return "SOURCE_VALIDATION_FAILED", attempt.name + "=" + errnoName(err), errors.New("write probe failed unexpectedly")
		}
		proofs = append(proofs, attempt.name+"="+errnoName(err))
	}
	return "", strings.Join(proofs, ","), nil
}

func errnoName(err error) string {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return errno.Error()
	}
	return "unclassified"
}

func digestStream(reader io.Reader) (string, int64, error) {
	hash := sha256.New()
	size, err := io.Copy(hash, reader)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(hash.Sum(nil)), size, nil
}
