// Package biz owns registered container images, private spaces and restricted credentials.
package biz

import (
	"fmt"
	"io"
	"time"
)

type ImageScope string

const (
	TenantImages   ImageScope = "tenant"
	PlatformImages ImageScope = "platform"
)

type ImagePlatform struct{ OS, Architecture, Variant string }
type Metadata struct {
	DisplayName, Description string
	Purposes                 []string
	Accelerator              string
}
type ImageReference struct {
	Authority, Project, Repository, Reference string
	IsDigest                                  bool
}

func (r ImageReference) String() string {
	separator := ":"
	if r.IsDigest {
		separator = "@"
	}
	return r.Authority + "/" + r.Project + "/" + r.Repository + separator + r.Reference
}

type Space struct {
	ID, TenantID, InstallationID, ProjectName, RegistryAuthority, State string
	Scope                                                               ImageScope
	Reason                                                              Reason
	ProjectID                                                           int64
	Version, PullCredentialGeneration                                   int64
	CreatedAt, UpdatedAt                                                time.Time
}

type CredentialInfo struct {
	TenantID, SpaceID, Purpose, State, RobotName, Username string
	Scope                                                  ImageScope
	RobotID, Generation, Version                           int64
	ExpiresAt                                              *time.Time
	UpdatedAt                                              time.Time
}
type CredentialDelivery struct {
	Credential  CredentialInfo
	Secret      Secret `json:"-"`
	ReplayUntil time.Time
}
type Registration struct {
	ID, TenantID, SpaceID, Repository, SourceReference, Digest, ResolvedReference, MediaType string
	Scope                                                                                    ImageScope
	Metadata                                                                                 Metadata
	Platforms                                                                                []ImagePlatform
	Version                                                                                  int64
	CreatedBy, UpdatedBy                                                                     string
	CreatedAt, UpdatedAt                                                                     time.Time
	UnregisteredAt                                                                           *time.Time
}
type Filter struct {
	Search      string
	Purposes    []string
	Accelerator string
	Limit       int
}

// ManagementFilter is the offset-page filter. Unlike Filter it carries no
// limit: the page size is a separate field so the same value feeds both the
// count and the page query.
type ManagementFilter struct {
	Search      string
	Purposes    []string
	Accelerator string
}

// PageRequest is an already validated 1-based page request.
type PageRequest struct {
	Page, PageSize uint32
}

// PageKey is the descending cursor position of the legacy tenant list.
type PageKey struct {
	CreatedAt time.Time
	ImageID   string
}

// PlatformSpaceView is the read-only platform binding projection. Space is nil
// only when no platform space has ever been reserved.
type PlatformSpaceView struct {
	Space                       *Space
	ConfiguredProject           string
	ConfiguredRegistryAuthority string
	BindingStatus               string
}

type RegistrationPage struct {
	Total      int64
	Items      []Registration
	NextCursor string
}
type TenantScope struct {
	TenantID string
	Scope    ImageScope
}

type EnableSpace struct{ TenantID, Slug, IdempotencyKey string }
type IssueCredential struct {
	TenantID, IdempotencyKey string
	ExpectedVersion          int64
}
type ResetCredential = IssueCredential
type DisableCredential = IssueCredential
type RegisterImage struct {
	TenantID, ImageReference, IdempotencyKey string
	Metadata                                 Metadata
}
type ReadImage struct {
	TenantID, ImageID string
	Scope             ImageScope
}
type ListImages struct {
	TenantID string
	Scope    ImageScope
	Filter   Filter
	Cursor   string
}
type UpdateImage struct {
	TenantID, ImageID, IdempotencyKey string
	Metadata                          Metadata
	ExpectedVersion                   int64
}
type UnregisterImage struct {
	TenantID, ImageID, IdempotencyKey string
	ExpectedVersion                   int64
}

// PatchMetadata carries only requested presence: a nil field means "absent from
// the request", which is distinct from an explicitly empty value.
type PatchMetadata struct {
	DisplayName *string
	Description *string
	Purposes    *[]string
	Accelerator *string
}

// PatchImage is the management PATCH input. Mask lists the canonical snake_case
// paths that may change; TenantID is empty on the platform surface.
type PatchImage struct {
	TenantID, ImageID string
	Patch             PatchMetadata
	Mask              []string
	ExpectedVersion   int64
	IdempotencyKey    string
}

// ExpiredCandidateRecoveryInput is the local operator recovery input. It never
// carries a scope or tenant: the real scope is read from the stored space.
type ExpiredCandidateRecoveryInput struct {
	SpaceID, CommandID                                                  string
	ExpectedCommandVersion, ExpectedCredentialVersion, CandidateRobotID int64
	EvidenceSHA256                                                      string
}

// CandidateExpiryRecoveryRecord is the bounded operator evidence appended to a
// terminated candidate command.
type CandidateExpiryRecoveryRecord struct {
	Operator, EvidenceSHA256                          string
	ExpectedCommandVersion, ExpectedCredentialVersion int64
	CandidateRobotID, PreviousRobotID                 int64
	PreviousCredentialDisposition                     string
	ObservedAt                                        time.Time
}

// CandidateRecoveryResult is the recovery outcome. It never contains a secret.
type CandidateRecoveryResult struct {
	CommandID, SpaceID string
	Scope              ImageScope
	State, Phase       string
	Reason             Reason
	CommandVersion     int64
	Recovery           CandidateExpiryRecoveryRecord
}

// Secret redacts all generic formatting and encoding. Explicit transport adapters
// must deliberately convert its bytes only on authorized delivery surfaces.
type Secret []byte

func (Secret) String() string                 { return "[REDACTED]" }
func (Secret) GoString() string               { return "[REDACTED]" }
func (Secret) Format(state fmt.State, _ rune) { _, _ = io.WriteString(state, "[REDACTED]") }
func (Secret) MarshalJSON() ([]byte, error)   { return []byte(`"[REDACTED]"`), nil }
func (Secret) MarshalText() ([]byte, error)   { return []byte("[REDACTED]"), nil }

type EncryptedSecret struct {
	KeyID      string
	Ciphertext []byte
}
type SecretAAD struct {
	InstallationID, TenantID, SpaceID, Purpose, CommandID string
	Scope                                                 ImageScope
	Generation                                            int64
}
type PullMaterial struct {
	TenantID, SpaceID, RegistryAuthority, Username string
	Secret                                         Secret `json:"-"`
	Generation                                     int64
	ExpiresAt                                      *time.Time
}
type ResolveImage struct {
	TenantID, ImageID string
	Scope             ImageScope
	TargetPlatform    ImagePlatform
}
type ResolvedImage struct {
	TenantID, ImageID, Reference, Digest string
	Scope                                ImageScope
	Version                              int64
	Platform                             ImagePlatform
	// RegistryAuthority is the credential record's authoritative value. It is
	// the only source consumers may use; never re-derive it from Reference.
	RegistryAuthority string
}

// Candidate and CommandResult deliberately have no provider-body or plaintext
// secret field. Recovery stores only bounded domain facts.
type Candidate struct {
	RobotID, Generation, PreviousRobotID    int64
	CredentialVersion                       int64
	RecoveryEvidence                        string
	RobotName, Username, Ownership, Purpose string
	ExpiresAt                               *time.Time
	// ExpiryRecovery is present only on a candidate terminated by the local
	// operator recovery. Older rows decode with a nil value.
	ExpiryRecovery *CandidateExpiryRecoveryRecord
}
type CommandResult struct {
	Space        *Space          `json:",omitempty"`
	Credential   *CredentialInfo `json:",omitempty"`
	Registration *Registration   `json:",omitempty"`
}
type Command struct {
	ID, SpaceID, TenantID, Key, Kind, Actor, Fingerprint, State, Phase string
	Scope                                                              ImageScope
	Reason                                                             Reason
	Candidate                                                          Candidate
	Result                                                             CommandResult
	DeliverySecret                                                     EncryptedSecret `json:"-"`
	ReplayUntil, CompletedAt                                           *time.Time
	Version                                                            int64
	CreatedAt, UpdatedAt                                               time.Time
}
