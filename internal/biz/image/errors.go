package biz

import "errors"

type Reason string

const (
	InvalidArgument           Reason = "INVALID_ARGUMENT"
	InvalidReference          Reason = "INVALID_REFERENCE"
	InvalidCursor             Reason = "INVALID_CURSOR"
	TrustedCallerRequired     Reason = "TRUSTED_CALLER_REQUIRED"
	PermissionDenied          Reason = "PERMISSION_DENIED"
	ImageProjectDenied        Reason = "IMAGE_PROJECT_DENIED"
	ImageNotFound             Reason = "IMAGE_NOT_FOUND"
	SpaceNotFound             Reason = "SPACE_NOT_FOUND"
	ImageAlreadyRegistered    Reason = "IMAGE_ALREADY_REGISTERED"
	SpaceNameConflict         Reason = "SPACE_NAME_CONFLICT"
	SpaceNameImmutable        Reason = "SPACE_NAME_IMMUTABLE"
	IdempotencyConflict       Reason = "IDEMPOTENCY_CONFLICT"
	VersionConflict           Reason = "VERSION_CONFLICT"
	SpaceNotReady             Reason = "SPACE_NOT_READY"
	CredentialAlreadyActive   Reason = "CREDENTIAL_ALREADY_ACTIVE"
	CredentialNotIssued       Reason = "CREDENTIAL_NOT_ISSUED"
	CredentialDeliveryExpired Reason = "CREDENTIAL_DELIVERY_EXPIRED"
	UnsupportedArtifact       Reason = "UNSUPPORTED_ARTIFACT"
	PlatformMismatch          Reason = "PLATFORM_MISMATCH"
	SpaceOwnershipUnconfirmed Reason = "SPACE_OWNERSHIP_UNCONFIRMED"
	DependencyUnavailable     Reason = "DEPENDENCY_UNAVAILABLE"
	RequestInProgress         Reason = "REQUEST_IN_PROGRESS"
	DeadlineExceeded          Reason = "DEADLINE_EXCEEDED"
	InternalError             Reason = "INTERNAL_ERROR"

	// ImageSpaceBindingMismatch reports a stored tenant space whose installation,
	// registry authority or project no longer matches the deployment binding. It
	// only blocks new registrations; local metadata and unregister stay available.
	ImageSpaceBindingMismatch Reason = "IMAGE_SPACE_BINDING_MISMATCH"
	// CandidateRecoveryCompleted reports a command whose expired candidate was
	// already terminated by the local operator recovery entry.
	CandidateRecoveryCompleted Reason = "CANDIDATE_RECOVERY_COMPLETED"
	// CandidateExpiredRecovered is the terminal command reason written by the
	// local operator recovery.
	CandidateExpiredRecovered Reason = "CANDIDATE_EXPIRED_RECOVERED"
)

type domainError struct {
	reason  Reason
	message string
}

func (e *domainError) Error() string { return string(e.reason) + ": " + e.message }

// Fail accepts a safe domain message, never a raw upstream body or SQL error.
func Fail(reason Reason, message string) error { return &domainError{reason: reason, message: message} }
func ReasonOf(err error) Reason {
	if err == nil {
		return ""
	}
	var e *domainError
	if errors.As(err, &e) {
		return e.reason
	}
	return InternalError
}

// ProjectCreationRejected is evidence from the registry adapter that this
// particular create request was rejected before any project could be created.
// A generic reason (including PermissionDenied) is not sufficient evidence.
func ProjectCreationRejected(err error) error { return &projectCreationRejected{err} }

type projectCreationRejected struct{ error }

func (e *projectCreationRejected) Unwrap() error { return e.error }

func isProjectCreationRejected(err error) bool {
	var rejected *projectCreationRejected
	return errors.As(err, &rejected)
}
