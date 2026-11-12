package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/go-kratos/kratos/v3/config"
	"github.com/go-kratos/kratos/v3/config/file"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/image"
	conf "github.com/zhangzhe-ctrl/ani-resource-service/internal/conf/v1"
)

var imageAdminAction, imageAdminInput, imageAdminOperator, imageAdminSecretOutput string

func init() {
	flag.StringVar(&imageAdminAction, "image-admin", "", "run one controlled Image operator action")
	flag.StringVar(&imageAdminInput, "image-admin-input", "", "private JSON input file; empty reads stdin")
	flag.StringVar(&imageAdminOperator, "image-operator-config", "", "private operator identity and output directory JSON file")
	flag.StringVar(&imageAdminSecretOutput, "image-secret-output", "", "new private credential output file inside the operator output directory")
}
func validImageAdminAction(action string) bool {
	switch action {
	case "init-platform", "issue-platform-publisher", "register-platform", "update-platform", "unregister-platform", "inspect-space", "recover-project", "purge-expired-secrets", "terminate-expired-candidate":
		return true
	}
	return false
}
func validateImageAdminModes() error {
	if imageAdminAction == "" {
		if imageAdminInput != "" || imageAdminOperator != "" || imageAdminSecretOutput != "" {
			return fmt.Errorf("Image operator flags require image-admin")
		}
		return nil
	}
	if !validImageAdminAction(imageAdminAction) {
		return fmt.Errorf("unknown Image operator action")
	}
	if flagImageMigrate || flagMigrate || baseConnectivityAction != "" {
		return fmt.Errorf("Image operator, migrations and base connectivity modes are exclusive")
	}
	if imageAdminOperator == "" {
		return fmt.Errorf("Image operator identity file required")
	}
	if (imageAdminAction == "issue-platform-publisher") != (imageAdminSecretOutput != "") {
		return fmt.Errorf("only publisher delivery requires an explicit new private output file")
	}
	return nil
}

type imageOperatorConfig struct {
	Subject               string `json:"subject"`
	Actor                 string `json:"actor"`
	InstallationID        string `json:"installation_id"`
	SecretOutputDirectory string `json:"secret_output_directory"`
}

func decodeImageAdmin(body []byte, out any) error {
	invalid := func() error { return fmt.Errorf("invalid Image operator JSON input") }
	if len(body) == 0 || len(body) > 65536 {
		return invalid()
	}
	// Operator inputs have a flat schema. Reject duplicate top-level keys before
	// ordinary strict decoding, including duplicated identity or output paths.
	scan := json.NewDecoder(bytes.NewReader(body))
	tok, err := scan.Token()
	if err != nil || tok != json.Delim('{') {
		return invalid()
	}
	seen := map[string]bool{}
	for scan.More() {
		tok, err = scan.Token()
		if err != nil {
			return invalid()
		}
		key, ok := tok.(string)
		if !ok || seen[key] {
			return invalid()
		}
		seen[key] = true
		var value json.RawMessage
		if scan.Decode(&value) != nil {
			return invalid()
		}
	}
	if tok, err = scan.Token(); err != nil || tok != json.Delim('}') {
		return invalid()
	}
	if scan.Decode(new(any)) != io.EOF {
		return invalid()
	}
	d := json.NewDecoder(bytes.NewReader(body))
	d.DisallowUnknownFields()
	if d.Decode(out) != nil || d.Decode(new(any)) != io.EOF {
		return invalid()
	}
	return nil
}

type imageAdminKey struct {
	IdempotencyKey string `json:"idempotency_key"`
}
type imageAdminIssue struct {
	IdempotencyKey  string `json:"idempotency_key"`
	ExpectedVersion int64  `json:"expected_version"`
	Rotate          bool   `json:"rotate"`
}
type imageAdminRegistration struct {
	IdempotencyKey string   `json:"idempotency_key"`
	ImageReference string   `json:"image_reference"`
	DisplayName    string   `json:"display_name"`
	Description    string   `json:"description"`
	Purposes       []string `json:"purposes"`
	Accelerator    string   `json:"accelerator"`
}

func (v imageAdminRegistration) metadata() biz.Metadata {
	return biz.Metadata{DisplayName: v.DisplayName, Description: v.Description, Purposes: v.Purposes, Accelerator: v.Accelerator}
}

type imageAdminUpdate struct {
	IdempotencyKey  string   `json:"idempotency_key"`
	ImageID         string   `json:"image_id"`
	ExpectedVersion int64    `json:"expected_version"`
	DisplayName     string   `json:"display_name"`
	Description     string   `json:"description"`
	Purposes        []string `json:"purposes"`
	Accelerator     string   `json:"accelerator"`
}
type imageAdminUnregister struct {
	IdempotencyKey  string `json:"idempotency_key"`
	ImageID         string `json:"image_id"`
	ExpectedVersion int64  `json:"expected_version"`
}
type imageAdminSpace struct {
	SpaceID string `json:"space_id"`
}
type imageAdminRecovery struct {
	SpaceID        string `json:"space_id"`
	ProjectID      int64  `json:"project_id"`
	EvidenceSHA256 string `json:"evidence_sha256"`
}

// imageAdminTerminateCandidate mirrors ExpiredCandidateRecoveryInput. It has no
// scope, tenant or force field: the stored space decides the scope.
type imageAdminTerminateCandidate struct {
	SpaceID                   string `json:"space_id"`
	CommandID                 string `json:"command_id"`
	ExpectedCommandVersion    int64  `json:"expected_command_version"`
	ExpectedCredentialVersion int64  `json:"expected_credential_version"`
	CandidateRobotID          int64  `json:"candidate_robot_id"`
	EvidenceSHA256            string `json:"evidence_sha256"`
}

// Parse before connecting to any dependency. No free-form SQL/HTTP/scope fields.
func imageAdminRequest(action string, body []byte) (any, error) {
	var request any
	switch action {
	case "init-platform":
		request = &imageAdminKey{}
	case "issue-platform-publisher":
		request = &imageAdminIssue{}
	case "register-platform":
		request = &imageAdminRegistration{}
	case "update-platform":
		request = &imageAdminUpdate{}
	case "unregister-platform":
		request = &imageAdminUnregister{}
	case "inspect-space":
		request = &imageAdminSpace{}
	case "recover-project":
		request = &imageAdminRecovery{}
	case "terminate-expired-candidate":
		request = &imageAdminTerminateCandidate{}
	case "purge-expired-secrets":
		request = &struct{}{}
	default:
		return nil, fmt.Errorf("unknown Image operator action")
	}
	if err := decodeImageAdmin(body, request); err != nil {
		return nil, err
	}
	return request, nil
}

// A private directory is an operator capability. O_EXCL and an anchored root
// prevent overwriting an existing file/symlink or selecting an arbitrary path.
func reserveImageSecretOutput(directory, path string) (*os.File, *os.Root, error) {
	fail := func() (*os.File, *os.Root, error) {
		return nil, nil, fmt.Errorf("Image secret output must be a new file in the private operator directory")
	}
	if !filepath.IsAbs(directory) || filepath.Clean(directory) != directory || !filepath.IsAbs(path) || filepath.Dir(path) != directory || filepath.Clean(path) != path || !regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,95}$`).MatchString(filepath.Base(path)) {
		return fail()
	}
	before, err := os.Lstat(directory)
	if err != nil || !before.IsDir() || before.Mode().Perm()&0077 != 0 {
		return fail()
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return fail()
	}
	after, err := root.Stat(".")
	if err != nil || !os.SameFile(before, after) {
		root.Close()
		return fail()
	}
	f, err := root.OpenFile(filepath.Base(path), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		root.Close()
		return fail()
	}
	return f, root, nil
}
func runImageAdmin() error {
	if err := validateImageAdminModes(); err != nil {
		return err
	}
	raw, err := readImagePrivateFile(imageAdminOperator)
	if err != nil {
		return err
	}
	var operator imageOperatorConfig
	err = decodeImageAdmin(raw, &operator)
	clear(raw)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	caller := biz.Caller{Kind: biz.PlatformCaller, Subject: operator.Subject, Actor: operator.Actor}
	ctx = biz.WithCaller(ctx, caller)
	if _, err = biz.RequirePlatform(ctx); err != nil {
		return err
	}
	if _, err = biz.ParseTenant(operator.InstallationID); err != nil {
		return err
	}
	if imageAdminInput != "" {
		raw, err = readImagePrivateFile(imageAdminInput)
	} else {
		raw, err = io.ReadAll(io.LimitReader(os.Stdin, 65537))
	}
	if err != nil {
		return fmt.Errorf("Image operator input unavailable")
	}
	request, err := imageAdminRequest(imageAdminAction, raw)
	clear(raw)
	if err != nil {
		return err
	}
	var output *os.File
	if imageAdminSecretOutput != "" {
		var root *os.Root
		output, root, err = reserveImageSecretOutput(operator.SecretOutputDirectory, imageAdminSecretOutput)
		if err != nil {
			return err
		}
		defer root.Close()
		defer output.Close()
		// Remove only the still-empty inode created by this invocation on failure.
		defer func() {
			stat, e := output.Stat()
			if e != nil || stat.Size() != 0 {
				return
			}
			current, e := root.Stat(filepath.Base(imageAdminSecretOutput))
			if e == nil && os.SameFile(stat, current) {
				_ = root.Remove(filepath.Base(imageAdminSecretOutput))
			}
		}()
	}
	c := config.New(config.WithSource(file.NewSource(flagconf)))
	defer c.Close()
	if c.Load() != nil {
		return fmt.Errorf("Image operator runtime configuration unavailable")
	}
	var bc conf.Bootstrap
	if c.Scan(&bc) != nil || bc.Image == nil || !bc.Image.Enabled || bc.Image.InstallationId != operator.InstallationID {
		return fmt.Errorf("Image operator installation/configuration mismatch")
	}
	components, err := openImage(ctx, bc.Image)
	if err != nil {
		return err
	}
	defer components.Close()
	platform, err := biz.NewPlatform(components.repository, components.lifecycle)
	if err != nil {
		return err
	}
	result, err := executeImageAdmin(ctx, platform, request, output)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(struct {
		Action  string `json:"action"`
		Subject string `json:"subject"`
		Actor   string `json:"actor"`
		Result  any    `json:"result"`
	}{imageAdminAction, operator.Subject, operator.Actor, result})
}
func executeImageAdmin(ctx context.Context, p *biz.Platform, request any, output io.Writer) (any, error) {
	switch v := request.(type) {
	case *imageAdminKey:
		return p.InitializePlatform(ctx, biz.InitPlatform{IdempotencyKey: v.IdempotencyKey})
	case *imageAdminIssue:
		if output == nil {
			return nil, fmt.Errorf("private credential output required")
		}
		delivery, err := p.IssuePlatformPublisher(ctx, biz.IssuePlatformCredential{IdempotencyKey: v.IdempotencyKey, ExpectedVersion: v.ExpectedVersion, Rotate: v.Rotate})
		if err != nil {
			return nil, err
		}
		defer clear(delivery.Secret)
		// Explicit credential serialization is confined to the reserved 0600 file.
		err = json.NewEncoder(output).Encode(struct {
			Credential  biz.CredentialInfo `json:"credential"`
			Secret      string             `json:"secret"`
			ReplayUntil time.Time          `json:"replay_until"`
		}{delivery.Credential, string(delivery.Secret), delivery.ReplayUntil})
		if err != nil {
			return nil, fmt.Errorf("Image credential output write failed; replay the same request to a new file")
		}
		if f, ok := output.(*os.File); ok && f.Sync() != nil {
			return nil, fmt.Errorf("Image credential output sync failed; replay the same request to a new file")
		}
		return delivery.Credential, nil
	case *imageAdminRegistration:
		return p.RegisterPlatformImage(ctx, biz.RegisterImage{ImageReference: v.ImageReference, IdempotencyKey: v.IdempotencyKey, Metadata: v.metadata()})
	case *imageAdminUpdate:
		return p.UpdatePlatformImage(ctx, biz.UpdateImage{ImageID: v.ImageID, ExpectedVersion: v.ExpectedVersion, IdempotencyKey: v.IdempotencyKey, Metadata: biz.Metadata{DisplayName: v.DisplayName, Description: v.Description, Purposes: v.Purposes, Accelerator: v.Accelerator}})
	case *imageAdminUnregister:
		return p.UnregisterPlatformImage(ctx, biz.UnregisterImage{ImageID: v.ImageID, ExpectedVersion: v.ExpectedVersion, IdempotencyKey: v.IdempotencyKey})
	case *imageAdminSpace:
		return p.InspectSpace(ctx, v.SpaceID)
	case *imageAdminRecovery:
		return p.RecoverProjectBinding(ctx, v.SpaceID, v.ProjectID, v.EvidenceSHA256)
	case *imageAdminTerminateCandidate:
		// Platform scope must go through the Platform wrapper so the engine holds
		// the platform repository; the tenant scope dispatches inside the engine.
		return p.TerminateExpiredCandidate(ctx, biz.ExpiredCandidateRecoveryInput{SpaceID: v.SpaceID, CommandID: v.CommandID, ExpectedCommandVersion: v.ExpectedCommandVersion, ExpectedCredentialVersion: v.ExpectedCredentialVersion, CandidateRobotID: v.CandidateRobotID, EvidenceSHA256: v.EvidenceSHA256})
	case *struct{}:
		n, err := p.PurgeExpiredDeliverySecrets(ctx)
		return struct {
			Purged int64 `json:"purged"`
		}{n}, err
	default:
		return nil, fmt.Errorf("unknown Image operator request")
	}
}
