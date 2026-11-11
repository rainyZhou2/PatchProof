package domain

import (
	"strings"
	"time"
)

type CreateRunRecord struct {
	Run           Run           `json:"run"`
	Events        []OutboxEvent `json:"events"`
	MaxActiveRuns int           `json:"max_active_runs,omitempty"`
}

type PlanTransaction struct {
	TenantID        TenantID            `json:"tenant_id"`
	RunID           RunID               `json:"run_id"`
	ExpectedVersion int64               `json:"expected_version"`
	Steps           []Step              `json:"steps"`
	Dependencies    map[StepID][]StepID `json:"dependencies"`
	Claims          []Claim             `json:"claims"`
	Events          []OutboxEvent       `json:"events"`
	At              time.Time           `json:"at"`
}

type RunFreeze struct {
	TenantID        TenantID  `json:"tenant_id"`
	RunID           RunID     `json:"run_id"`
	ExpectedVersion int64     `json:"expected_version"`
	SnapshotDigest  string    `json:"snapshot_digest"`
	BaseSnapshot    string    `json:"base_snapshot"`
	HeadSnapshot    string    `json:"head_snapshot"`
	At              time.Time `json:"at"`
}

type ShardLease struct {
	Shard     int       `json:"shard"`
	Owner     string    `json:"owner"`
	ExpiresAt time.Time `json:"expires_at"`
}

type AuditEntry struct {
	ID         string    `json:"audit_id"`
	TenantID   TenantID  `json:"tenant_id"`
	Actor      string    `json:"actor"`
	Action     string    `json:"action"`
	Resource   string    `json:"resource"`
	ResourceID string    `json:"resource_id"`
	Outcome    string    `json:"outcome"`
	Detail     string    `json:"detail,omitempty"`
	At         time.Time `json:"at"`
}

type ActionCacheEntry struct {
	TenantID      TenantID   `json:"tenant_id"`
	ActionKey     string     `json:"action_key"`
	StepKind      StepKind   `json:"step_kind"`
	Engine        string     `json:"engine"`
	EngineVersion string     `json:"engine_version"`
	Result        StepResult `json:"result"`
	CreatedAt     time.Time  `json:"created_at"`
	ExpiresAt     time.Time  `json:"expires_at"`
}

func (e ActionCacheEntry) Validate() error {
	if err := e.TenantID.Validate(); err != nil {
		return err
	}
	if !strings.HasPrefix(e.ActionKey, "sha256:") || len(e.ActionKey) != len("sha256:")+64 {
		return &ValidationError{Field: "action_key", Reason: "must be a sha256 digest"}
	}
	if !e.StepKind.Valid() {
		return &ValidationError{Field: "step_kind", Reason: "unknown step kind"}
	}
	if err := RequireNonEmpty("engine", e.Engine); err != nil {
		return err
	}
	if e.ExpiresAt.IsZero() || !e.ExpiresAt.After(e.CreatedAt) {
		return &ValidationError{Field: "expires_at", Reason: "must be after created_at"}
	}
	if err := e.Result.Validate(); err != nil {
		return err
	}
	if e.Result.Outcome != OutcomeSucceeded {
		return &ValidationError{Field: "result.outcome", Reason: "only successful results are cacheable"}
	}
	if len(e.Result.Counterexamples) != 0 {
		return &ValidationError{Field: "result.counterexamples", Reason: "counterexamples are never cached"}
	}
	for _, item := range e.Result.Evidence {
		if item.Result != ClaimTested && item.Result != ClaimProven {
			return &ValidationError{Field: "evidence.result", Reason: "only positive evidence is cacheable"}
		}
	}
	return nil
}

type OutboxDeliveryFailure struct {
	EventID EventID   `json:"event_id"`
	Reason  string    `json:"reason"`
	At      time.Time `json:"at"`
}
