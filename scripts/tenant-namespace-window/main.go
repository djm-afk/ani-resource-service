// Command tenant-namespace-window is a one-time, exact-manifest DDL tool.
// It never migrates, backfills, deploys, changes privileges or mutates Kubernetes.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	data "github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network"
)

type pod struct{ Name, UID, IP string }
type deployment struct{ Namespace, Name, UID, Container string }
type manifest struct {
	Schema                                    string
	Window                                    data.NamespaceUIDWindowInput
	Deployment                                deployment
	OldPods                                   []pod
	NewImage, NewImageDigest, NewSourceCommit string
}
type envelope struct {
	Manifest     manifest
	Plan         data.NamespaceUIDWindowPlan
	Applied      bool
	Verification json.RawMessage
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func readFile(path string, private bool) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 65536 || private && info.Mode().Perm() != 0600 {
		return nil, fmt.Errorf("bounded regular file required; owner DSN must be mode0600")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read input file failed")
	}
	return raw, nil
}
func decode(raw []byte, out any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return fmt.Errorf("invalid manifest or original freeze plan")
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("trailing JSON data")
	}
	return nil
}
func digest(value any) string {
	raw, _ := json.Marshal(value)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
func run() error {
	manifestFile := flag.String("manifest-file", "", "reviewed exact two-Namespace window manifest")
	dsnFile := flag.String("dsn-file", "", "existing maintenance-owner DSN file (0600)")
	stage := flag.String("stage", "freeze", "freeze, require-uid, release or inspect")
	frozenFile := flag.String("freeze-plan-file", "", "original freeze plan JSON, mandatory after freeze")
	apply := flag.Bool("apply", false, "apply only the exact freshly reviewed plan")
	reviewed := flag.String("reviewed-sha256", "", "SHA256 emitted by this stage's read-only plan")
	flag.Parse()
	if *manifestFile == "" || *dsnFile == "" {
		return fmt.Errorf("manifest-file and dsn-file required")
	}
	raw, err := readFile(*manifestFile, false)
	if err != nil {
		return err
	}
	var m manifest
	if err = decode(raw, &m); err != nil {
		return err
	}
	if m.Schema != "ani.resource.namespace-uid-window.v1" || len(m.Window.Rows) != 2 || m.Window.ClusterID != "f39bf6e2-f3ed-4191-ada3-caeebf86c220" {
		return fmt.Errorf("only the reviewed test-cluster two-row manifest is accepted")
	}
	if m.Deployment.Namespace != "ani-system" || m.Deployment.UID == "" || m.Deployment.Container == "" || len(m.OldPods) == 0 || len(m.OldPods) > 32 {
		return fmt.Errorf("exact original Resource deployment/container/pods required")
	}
	dns := regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$`)
	if !dns.MatchString(m.Deployment.Name) || !dns.MatchString(m.Deployment.Container) {
		return fmt.Errorf("invalid explicit deployment identity")
	}
	m.Window.Stage = *stage
	m.Window.WriterAddresses = []string{}
	for _, p := range m.OldPods {
		if !dns.MatchString(p.Name) || p.UID == "" || p.IP == "" {
			return fmt.Errorf("invalid original pod identity")
		}
		m.Window.WriterAddresses = append(m.Window.WriterAddresses, p.IP)
	}
	if *stage != "freeze" && *stage != "inspect" {
		original, err := readFile(*frozenFile, false)
		if err != nil {
			return fmt.Errorf("original freeze plan file required")
		}
		var frozen envelope
		if err = decode(original, &frozen); err != nil {
			return err
		}
		originalProof := digest(struct {
			Deployment            deployment
			OldPods               []pod
			Image, Digest, Commit string
		}{frozen.Manifest.Deployment, frozen.Manifest.OldPods, frozen.Manifest.NewImage, frozen.Manifest.NewImageDigest, frozen.Manifest.NewSourceCommit})
		if originalProof != frozen.Plan.Input.WriterProofSHA256 {
			return fmt.Errorf("original writer manifest is not bound to freeze plan")
		}
		originalSHA := frozen.Plan.SHA256
		frozen.Plan.SHA256 = ""
		if originalSHA == "" || digest(frozen.Plan) != originalSHA || frozen.Plan.Input.Stage != "freeze" || frozen.Plan.Input.WindowID != m.Window.WindowID || frozen.Plan.Input.ClusterID != m.Window.ClusterID || frozen.Plan.Input.Database != m.Window.Database || frozen.Plan.Input.RuntimeRole != m.Window.RuntimeRole || digest(frozen.Plan.Input.Rows) != digest(m.Window.Rows) || digest(frozen.Manifest.OldPods) != digest(m.OldPods) || frozen.Manifest.Deployment != m.Deployment {
			return fmt.Errorf("original freeze manifest/hash/identities differ")
		}
		m.Window.OldBackends = frozen.Plan.Before.OldBackends
	}
	if *stage == "release" {
		if !regexp.MustCompile(`^sha256:[a-f0-9]{64}$`).MatchString(m.NewImageDigest) || !regexp.MustCompile(`^[a-f0-9]{40}$`).MatchString(m.NewSourceCommit) || m.NewImage == "" {
			return fmt.Errorf("release requires reviewed new Res image reference/digest/source commit")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	var verification json.RawMessage
	verify := func(ctx context.Context) error {
		call, stop := context.WithTimeout(ctx, 12*time.Second)
		defer stop()
		request, _ := json.Marshal(m)
		command := "python3 -c '" + strings.ReplaceAll(clusterReadScript, "'", "'\\''") + "'"
		cmd := exec.CommandContext(call, "ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=5", "-o", "ServerAliveInterval=5", "-o", "ServerAliveCountMax=1", "kubercloud@100.64.128.12", command)
		cmd.Stdin = bytes.NewReader(request)
		var output bytes.Buffer
		cmd.Stdout = &output
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("approved SSH full-identity/Namespace/writer verification failed")
		}
		if output.Len() > 65536 || !json.Valid(output.Bytes()) {
			return fmt.Errorf("invalid bounded cluster verification")
		}
		verification = bytes.Clone(output.Bytes())
		return nil
	}
	if err = verify(ctx); err != nil {
		return err
	}
	// Bind the exact reviewed manifest, not transient resourceVersions, to each
	// stage plan. Live verification is repeated while the table lock is held.
	m.Window.WriterProofSHA256 = digest(struct {
		Deployment            deployment
		OldPods               []pod
		Image, Digest, Commit string
	}{m.Deployment, m.OldPods, m.NewImage, m.NewImageDigest, m.NewSourceCommit})
	dsn, err := readFile(*dsnFile, true)
	if err != nil {
		return err
	}
	config, err := pgxpool.ParseConfig(strings.TrimSpace(string(dsn)))
	if err != nil {
		return fmt.Errorf("invalid maintenance owner DSN")
	}
	config.MaxConns = 1
	owner, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return fmt.Errorf("maintenance owner database unavailable")
	}
	defer owner.Close()
	plan, err := data.PlanNamespaceUIDWindow(ctx, owner, m.Window)
	if err != nil {
		return err
	}
	result := envelope{Manifest: m, Plan: plan, Verification: verification}
	if *apply {
		plan, err = data.ApplyNamespaceUIDWindow(ctx, owner, m.Window, *reviewed, verify)
		if err != nil {
			return err
		}
		result.Plan = plan
		result.Applied = true
		result.Verification = verification
	}
	return json.NewEncoder(os.Stdout).Encode(result)
}
