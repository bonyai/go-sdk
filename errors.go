package bonya

import (
	"errors"
	"net/http"
	"time"
)

// Error kinds. Every error the SDK returns for a failed call is an *Error
// whose kind matches exactly one of these with errors.Is:
//
//	if errors.Is(err, bonya.ErrNoCapacity) { ... }
//
// Callers never need HTTP statuses: the kind, the stable Code and RetryAfter
// carry everything a program should branch on.
var (
	// ErrInvalidRequest: the request or its arguments were refused as invalid.
	ErrInvalidRequest = errors.New("invalid request")
	// ErrUnauthenticated: the credential is missing, invalid or expired.
	ErrUnauthenticated = errors.New("not authenticated")
	// ErrPermissionDenied: the credential is valid but not allowed to do this.
	ErrPermissionDenied = errors.New("permission denied")
	// ErrNotFound: the resource does not exist or is not visible to the caller.
	ErrNotFound = errors.New("not found")
	// ErrConflict: the resource is in a state that does not allow the call.
	ErrConflict = errors.New("conflict")
	// ErrSandboxPaused: the sandbox was paused on request; edge traffic does
	// not wake it. Wake it explicitly first.
	ErrSandboxPaused = errors.New("sandbox paused")
	// ErrSnapshotInUse: a sandbox created from the snapshot still exists.
	ErrSnapshotInUse = errors.New("snapshot in use")
	// ErrLimitReached: an organization limit (for example snapshot count) was reached.
	ErrLimitReached = errors.New("limit reached")
	// ErrRateLimited: too many requests; retry after RetryAfter.
	ErrRateLimited = errors.New("rate limited")
	// ErrTooLarge: the request body or file is larger than allowed.
	ErrTooLarge = errors.New("too large")
	// ErrTimeout: the command ran past its timeout and was stopped. Running
	// it again would time out the same way; the Message says whether the
	// stop was confirmed.
	ErrTimeout = errors.New("timed out")
	// ErrNoCapacity: no capacity to start or wake the sandbox now; retry
	// after RetryAfter.
	ErrNoCapacity = errors.New("no capacity")
	// ErrUnavailable: the service could not be reached or answered that a
	// dependency is unavailable. Nothing was done.
	ErrUnavailable = errors.New("service unavailable")
	// ErrOutcomeUnknown: the connection failed after a non-repeatable
	// request was sent, so it may or may not have taken effect. The SDK never
	// resends such a request; check the resource before trying again.
	ErrOutcomeUnknown = errors.New("outcome unknown")
	// ErrInternal: the service failed unexpectedly, or answered with an error
	// this SDK version does not know.
	ErrInternal = errors.New("internal error")
)

// Error is the error type of every failed SDK call.
type Error struct {
	// Kind is one of the Err* values above.
	Kind error
	// Code is the service's stable machine-readable code (for example
	// "lifecycle_conflict"); empty for errors raised by the SDK itself.
	Code string
	// Message is a human-readable explanation.
	Message string
	// SandboxID is the sandbox the call acted on, when there is one.
	SandboxID string
	// RetryAfter is how long to wait before retrying, when the service said so.
	RetryAfter time.Duration

	cause error
}

func (e *Error) Error() string {
	msg := "bonya: " + e.Kind.Error()
	if e.Message != "" {
		msg += ": " + e.Message
	}
	if e.SandboxID != "" {
		msg += " (sandbox " + e.SandboxID + ")"
	}
	return msg
}

// Is matches the error's kind.
func (e *Error) Is(target error) bool { return target == e.Kind }

// Unwrap returns the underlying cause (for example a network error), if any.
func (e *Error) Unwrap() error { return e.cause }

// codeKinds maps the service's problem codes to error kinds. Codes the SDK
// handles itself (capability_expired, unknown_identity) still map here so a
// failed recovery surfaces sensibly.
var codeKinds = map[string]error{
	"malformed_request":      ErrInvalidRequest,
	"validation_failed":      ErrInvalidRequest,
	"invalid_credential":     ErrUnauthenticated,
	"capability_expired":     ErrUnauthenticated,
	"forbidden":              ErrPermissionDenied,
	"unknown_identity":       ErrPermissionDenied,
	"not_found":              ErrNotFound,
	"lifecycle_conflict":     ErrConflict,
	"final_owner":            ErrConflict,
	"sandbox_paused":         ErrSandboxPaused,
	"snapshot_in_use":        ErrSnapshotInUse,
	"snapshot_limit_reached": ErrLimitReached,
	"payload_too_large":      ErrTooLarge,
	"exec_timeout":           ErrTimeout,
	"rate_limited":           ErrRateLimited,
	"capacity_unavailable":   ErrNoCapacity,
	"no_capacity":            ErrNoCapacity,
	"upstream_unavailable":   ErrUnavailable,
	"internal":               ErrInternal,
}

// kindForStatus classifies a response whose code is unknown to this SDK
// version (or that had no problem body, for example from a proxy), so a
// newer service never surfaces as a raw status.
func kindForStatus(status int) error {
	switch {
	case status == http.StatusBadRequest:
		return ErrInvalidRequest
	case status == http.StatusUnauthorized:
		return ErrUnauthenticated
	case status == http.StatusForbidden:
		return ErrPermissionDenied
	case status == http.StatusNotFound:
		return ErrNotFound
	case status == http.StatusConflict:
		return ErrConflict
	case status == http.StatusRequestEntityTooLarge:
		return ErrTooLarge
	case status == http.StatusTooManyRequests:
		return ErrRateLimited
	case status == http.StatusBadGateway, status == http.StatusServiceUnavailable, status == http.StatusGatewayTimeout:
		return ErrUnavailable
	case status >= 400 && status < 500:
		return ErrInvalidRequest
	default:
		return ErrInternal
	}
}

func invalidArgument(msg string) *Error {
	return &Error{Kind: ErrInvalidRequest, Message: msg}
}
