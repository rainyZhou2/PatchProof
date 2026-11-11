package domain

import (
	"errors"
	"fmt"
)

type ValidationError struct {
	Field  string
	Reason string
}

func (e *ValidationError) Error() string {
	if e.Field == "" {
		return "invalid request: " + e.Reason
	}
	return fmt.Sprintf("invalid %s: %s", e.Field, e.Reason)
}

type NotFoundError struct {
	Resource string
	ID       string
}

func (e *NotFoundError) Error() string {
	return fmt.Sprintf("%s %q not found", e.Resource, e.ID)
}

type ConflictError struct {
	Resource string
	ID       string
	Reason   string
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("%s %q conflict: %s", e.Resource, e.ID, e.Reason)
}

type VersionConflictError struct {
	Resource string
	Expected int64
	Actual   int64
}

func (e *VersionConflictError) Error() string {
	return fmt.Sprintf("%s version conflict: expected %d, actual %d", e.Resource, e.Expected, e.Actual)
}

type IdempotencyConflictError struct {
	RequestKey string
}

func (e *IdempotencyConflictError) Error() string {
	return fmt.Sprintf("request key %q was already used with different frozen inputs", e.RequestKey)
}

type StateTransitionError struct {
	Resource string
	From     string
	To       string
	Reason   string
}

func (e *StateTransitionError) Error() string {
	msg := fmt.Sprintf("%s cannot transition from %s to %s", e.Resource, e.From, e.To)
	if e.Reason != "" {
		return msg + ": " + e.Reason
	}
	return msg
}

type LeaseError struct {
	StepID StepID
	Reason string
}

func (e *LeaseError) Error() string {
	return fmt.Sprintf("lease for step %s rejected: %s", e.StepID, e.Reason)
}

type FencingError struct {
	StepID    StepID
	AttemptID AttemptID
	Reason    string
}

func (e *FencingError) Error() string {
	return fmt.Sprintf("attempt %s for step %s rejected by fence: %s", e.AttemptID, e.StepID, e.Reason)
}

type PolicyError struct {
	Code   string
	Reason string
}

func (e *PolicyError) Error() string {
	return fmt.Sprintf("policy %s: %s", e.Code, e.Reason)
}

var (
	ErrNotFound          = errors.New("not found")
	ErrVersionConflict   = errors.New("version conflict")
	ErrConflict          = errors.New("conflict")
	ErrIdempotency       = errors.New("idempotency key conflict")
	ErrQuotaExceeded     = errors.New("tenant quota exceeded")
	ErrShardNotOwned     = errors.New("shard not owned")
	ErrEvidenceInvalid   = errors.New("evidence invalid")
	ErrSignatureInvalid  = errors.New("signature invalid")
	ErrUnsupported       = errors.New("unsupported by engine")
	ErrRequestCancelled  = errors.New("cancelled")
	ErrAdmissionRejected = errors.New("admission rejected")
)

func IsNotFound(err error) bool {
	var nf *NotFoundError
	return errors.As(err, &nf) || errors.Is(err, ErrNotFound)
}

func IsVersionConflict(err error) bool {
	var vc *VersionConflictError
	return errors.As(err, &vc) || errors.Is(err, ErrVersionConflict)
}

func Validationf(field, format string, args ...any) error {
	return &ValidationError{Field: field, Reason: fmt.Sprintf(format, args...)}
}
