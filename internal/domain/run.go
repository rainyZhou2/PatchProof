package domain

import (
	"fmt"
	"strings"
	"time"
)

type Run struct {
	ID              RunID      `json:"run_id"`
	TenantID        TenantID   `json:"tenant_id"`
	ProjectID       ProjectID  `json:"project_id"`
	RequestKey      string     `json:"request_key"`
	Fingerprint     string     `json:"fingerprint"`
	BaseCommit      string     `json:"base_commit"`
	HeadCommit      string     `json:"head_commit"`
	PatchDigest     string     `json:"patch_digest"`
	SnapshotDigest  string     `json:"snapshot_digest,omitempty"`
	BaseSnapshot    string     `json:"base_snapshot_digest,omitempty"`
	HeadSnapshot    string     `json:"head_snapshot_digest,omitempty"`
	PolicyDigest    string     `json:"policy_digest"`
	PolicyRef       string     `json:"policy_ref"`
	RequestedChecks []string   `json:"requested_checks"`
	Budget          Budget     `json:"budget"`
	Status          RunStatus  `json:"status"`
	Verdict         Verdict    `json:"verdict,omitempty"`
	StateVersion    int64      `json:"state_version"`
	FailureClass    ErrorClass `json:"failure_class,omitempty"`
	FailureMessage  string     `json:"failure_message,omitempty"`
	Region          string     `json:"region"`
	CreatedBy       string     `json:"created_by"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
	PlannedAt       *time.Time `json:"planned_at,omitempty"`
	StartedAt       *time.Time `json:"started_at,omitempty"`
	FinishedAt      *time.Time `json:"finished_at,omitempty"`
}

type RunRequest struct {
	TenantID        TenantID  `json:"tenant_id"`
	ProjectID       ProjectID `json:"project_id"`
	RequestKey      string    `json:"request_key"`
	Fingerprint     string    `json:"fingerprint"`
	BaseCommit      string    `json:"base_commit"`
	HeadCommit      string    `json:"head_commit"`
	PatchDigest     string    `json:"patch_digest,omitempty"`
	PolicyRef       string    `json:"policy_ref"`
	PolicyDigest    string    `json:"policy_digest,omitempty"`
	RequestedChecks []string  `json:"requested_checks"`
	Budget          Budget    `json:"budget"`
	Region          string    `json:"region,omitempty"`
	CreatedBy       string    `json:"created_by"`
}

func (r RunRequest) Validate() error {
	if err := r.TenantID.Validate(); err != nil {
		return err
	}
	if r.ProjectID.String() == "" {
		return &ValidationError{Field: "project_id", Reason: "must not be empty"}
	}
	if err := RequireNonEmpty("request_key", r.RequestKey); err != nil {
		return err
	}
	if err := RequireMaxLen("request_key", r.RequestKey, 200); err != nil {
		return err
	}
	if err := RequireNonEmpty("fingerprint", r.Fingerprint); err != nil {
		return err
	}
	for _, field := range []struct{ name, value string }{
		{"base_commit", r.BaseCommit}, {"head_commit", r.HeadCommit},
	} {
		if len(field.value) < 7 || len(field.value) > 64 {
			return &ValidationError{Field: field.name, Reason: "must be a 7-64 character commit hash"}
		}
		for _, c := range field.value {
			if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
				return &ValidationError{Field: field.name, Reason: "must be hexadecimal"}
			}
		}
	}
	if strings.EqualFold(r.BaseCommit, r.HeadCommit) {
		return &ValidationError{Field: "head_commit", Reason: "must differ from base_commit"}
	}
	if r.PolicyRef == "" && r.PolicyDigest == "" {
		return &ValidationError{Field: "policy_ref", Reason: "either policy_ref or policy_digest is required"}
	}
	if err := r.Budget.Validate(); err != nil {
		return err
	}
	return nil
}

func (r RunRequest) NormalizeChecks() []string {
	seen := make(map[string]struct{}, len(r.RequestedChecks))
	out := make([]string, 0, len(r.RequestedChecks))
	for _, check := range r.RequestedChecks {
		normalized := NormalizeCheckName(check)
		if normalized == "" {
			continue
		}
		if _, ok := seen[normalized]; ok {
			continue
		}
		seen[normalized] = struct{}{}
		out = append(out, normalized)
	}
	return out
}

func (r Run) Validate() error {
	if err := r.ID.Validate(); err != nil {
		return err
	}
	if err := r.TenantID.Validate(); err != nil {
		return err
	}
	if !r.Status.Valid() {
		return &ValidationError{Field: "status", Reason: "unknown run status"}
	}
	if !r.Budget.IsZero() {
		if err := r.Budget.Validate(); err != nil {
			return err
		}
	}
	return nil
}

func (r *Run) Transition(next RunStatus, at time.Time) error {
	if !next.Valid() {
		return &ValidationError{Field: "run.status", Reason: fmt.Sprintf("unknown status %q", next)}
	}
	if !r.Status.CanTransitionTo(next) {
		return &StateTransitionError{Resource: "run " + r.ID.String(), From: string(r.Status), To: string(next)}
	}
	at = at.UTC()
	r.Status = next
	r.StateVersion++
	r.UpdatedAt = at
	switch next {
	case RunPlanned:
		r.PlannedAt = timePtr(at)
	case RunRunning:
		if r.StartedAt == nil {
			r.StartedAt = timePtr(at)
		}
	case RunCompleted, RunPartial, RunInfraFailed, RunCancelled, RunRejected:
		r.FinishedAt = timePtr(at)
	}
	return nil
}

func (r *Run) SetVerdict(v Verdict) error {
	if !v.Valid() {
		return &ValidationError{Field: "verdict", Reason: "unknown verdict"}
	}
	if !r.Status.Terminal() && r.Status != RunAggregating {
		return &StateTransitionError{Resource: "run " + r.ID.String(), From: string(r.Status), To: "verdict", Reason: "verdict is only final after aggregation"}
	}
	r.Verdict = v
	return nil
}

func (r *Run) FreezeInputs(snapshotDigest, baseSnapshot, headSnapshot, policyDigest string, at time.Time) error {
	if r.Status != RunCreated && r.Status != RunValidating {
		return &StateTransitionError{Resource: "run " + r.ID.String(), From: string(r.Status), To: "frozen", Reason: "inputs can only be frozen before planning"}
	}
	if snapshotDigest == "" || policyDigest == "" {
		return &ValidationError{Field: "snapshot_digest", Reason: "snapshot and policy digests are required"}
	}
	r.SnapshotDigest = snapshotDigest
	r.BaseSnapshot = baseSnapshot
	r.HeadSnapshot = headSnapshot
	r.PolicyDigest = policyDigest
	r.UpdatedAt = at.UTC()
	return nil
}

func (r Run) InputsFrozen() bool {
	return r.Status != RunCreated && r.Status != RunValidating && r.Status != RunRejected
}

type RunUpdate struct {
	TenantID        TenantID
	RunID           RunID
	ExpectedVersion int64
	From            RunStatus
	To              RunStatus
	Verdict         Verdict
	FailureClass    ErrorClass
	FailureMessage  string
	At              time.Time
	Event           *OutboxEvent
}

func (u RunUpdate) Validate() error {
	if err := u.TenantID.Validate(); err != nil {
		return err
	}
	if err := u.RunID.Validate(); err != nil {
		return err
	}
	if !u.From.Valid() || !u.To.Valid() {
		return &ValidationError{Field: "run.status", Reason: "statuses must be valid"}
	}
	if !u.From.CanTransitionTo(u.To) {
		return &StateTransitionError{Resource: "run " + u.RunID.String(), From: string(u.From), To: string(u.To)}
	}
	if u.Verdict != "" && !u.Verdict.Valid() {
		return &ValidationError{Field: "verdict", Reason: "unknown verdict"}
	}
	return nil
}

type RunFilter struct {
	TenantID      TenantID
	ProjectID     ProjectID
	Statuses      []RunStatus
	CreatedAfter  *time.Time
	CreatedBefore *time.Time
	Limit         int
	Cursor        string
}

type RunPage struct {
	Runs       []Run  `json:"runs"`
	NextCursor string `json:"next_cursor,omitempty"`
}

func timePtr(t time.Time) *time.Time { return &t }

func (f RunFilter) Normalize(maxLimit int) RunFilter {
	if f.Limit <= 0 {
		f.Limit = 50
	}
	if f.Limit > maxLimit {
		f.Limit = maxLimit
	}
	return f
}
