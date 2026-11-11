package domain

import (
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"
)

var stepKeyPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9:._-]{0,127}$`)

type Step struct {
	ID                  StepID               `json:"step_id"`
	RunID               RunID                `json:"run_id"`
	TenantID            TenantID             `json:"tenant_id"`
	Key                 string               `json:"key"`
	Kind                StepKind             `json:"kind"`
	Status              StepStatus           `json:"status"`
	Generation          int64                `json:"generation"`
	AttemptCount        int                  `json:"attempt_count"`
	MaxAttempts         int                  `json:"max_attempts"`
	CurrentAttemptID    AttemptID            `json:"current_attempt_id,omitempty"`
	LeaseOwner          string               `json:"lease_owner,omitempty"`
	LeaseDeadline       *time.Time           `json:"lease_deadline,omitempty"`
	TargetCluster       ClusterID            `json:"target_cluster,omitempty"`
	Requirements        ResourceRequirements `json:"requirements"`
	InputManifestDigest string               `json:"input_manifest_digest"`
	ResultDigest        string               `json:"result_digest,omitempty"`
	OutputManifest      string               `json:"output_manifest,omitempty"`
	FailureClass        ErrorClass           `json:"failure_class,omitempty"`
	FailureMessage      string               `json:"failure_message,omitempty"`
	NextAttemptAt       *time.Time           `json:"next_attempt_at,omitempty"`
	Priority            int                  `json:"priority"`
	Shard               int                  `json:"shard"`
	Options             map[string]string    `json:"options,omitempty"`
	CreatedAt           time.Time            `json:"created_at"`
	UpdatedAt           time.Time            `json:"updated_at"`
	StartedAt           *time.Time           `json:"started_at,omitempty"`
	FinishedAt          *time.Time           `json:"finished_at,omitempty"`
}

func (s Step) Validate() error {
	if err := s.ID.Validate(); err != nil {
		return err
	}
	if err := s.RunID.Validate(); err != nil {
		return err
	}
	if err := s.TenantID.Validate(); err != nil {
		return err
	}
	if !stepKeyPattern.MatchString(s.Key) {
		return &ValidationError{Field: "step.key", Reason: "must be a lowercase identifier of at most 128 characters"}
	}
	if !s.Kind.Valid() {
		return &ValidationError{Field: "step.kind", Reason: fmt.Sprintf("unknown step kind %q", s.Kind)}
	}
	if !s.Status.Valid() {
		return &ValidationError{Field: "step.status", Reason: "unknown step status"}
	}
	if err := s.Requirements.Validate(); err != nil {
		return err
	}
	if s.MaxAttempts <= 0 || s.MaxAttempts > 20 {
		return &ValidationError{Field: "step.max_attempts", Reason: "must be between 1 and 20"}
	}
	if s.Generation < 0 {
		return &ValidationError{Field: "step.generation", Reason: "must not be negative"}
	}
	if s.Shard < 0 {
		return &ValidationError{Field: "step.shard", Reason: "must not be negative"}
	}
	if len(s.Options) > 64 {
		return &ValidationError{Field: "step.options", Reason: "at most 64 options are allowed"}
	}
	for key, value := range s.Options {
		if key == "" || len(key) > 128 {
			return &ValidationError{Field: "step.options", Reason: "option keys must be 1-128 characters"}
		}
		if len(value) > 8192 {
			return &ValidationError{Field: "step.options", Reason: "option values must be at most 8192 characters"}
		}
		if strings.ContainsRune(key, 0) || strings.ContainsRune(value, 0) {
			return &ValidationError{Field: "step.options", Reason: "option keys and values must not contain NUL"}
		}
	}
	return nil
}

func (s *Step) Transition(next StepStatus, at time.Time) error {
	if !next.Valid() {
		return &ValidationError{Field: "step.status", Reason: fmt.Sprintf("unknown status %q", next)}
	}
	if !s.Status.CanTransitionTo(next) {
		return &StateTransitionError{Resource: "step " + s.ID.String(), From: string(s.Status), To: string(next)}
	}
	at = at.UTC()
	s.Status = next
	s.UpdatedAt = at
	switch next {
	case StepRunning:
		if s.StartedAt == nil {
			s.StartedAt = timePtr(at)
		}
	case StepSucceeded, StepFailed, StepCancelled, StepTimedOut:
		s.FinishedAt = timePtr(at)
	}
	return nil
}

type Attempt struct {
	ID           AttemptID     `json:"attempt_id"`
	StepID       StepID        `json:"step_id"`
	RunID        RunID         `json:"run_id"`
	TenantID     TenantID      `json:"tenant_id"`
	Generation   int64         `json:"generation"`
	Status       AttemptStatus `json:"status"`
	ClusterID    ClusterID     `json:"cluster_id,omitempty"`
	WorkerID     string        `json:"worker_id,omitempty"`
	ExecutorRef  string        `json:"executor_ref,omitempty"`
	ResultDigest string        `json:"result_digest,omitempty"`
	ErrorClass   ErrorClass    `json:"error_class,omitempty"`
	ErrorMessage string        `json:"error_message,omitempty"`
	CreatedAt    time.Time     `json:"created_at"`
	StartedAt    *time.Time    `json:"started_at,omitempty"`
	FinishedAt   *time.Time    `json:"finished_at,omitempty"`
}

func (a Attempt) Validate() error {
	if err := a.ID.Validate(); err != nil {
		return err
	}
	if err := a.StepID.Validate(); err != nil {
		return err
	}
	if err := a.RunID.Validate(); err != nil {
		return err
	}
	if err := a.TenantID.Validate(); err != nil {
		return err
	}
	if !a.Status.Valid() {
		return &ValidationError{Field: "attempt.status", Reason: "unknown attempt status"}
	}
	if a.Generation <= 0 {
		return &ValidationError{Field: "attempt.generation", Reason: "must be positive"}
	}
	return nil
}

type AttemptRef struct {
	TenantID   TenantID  `json:"tenant_id"`
	RunID      RunID     `json:"run_id"`
	StepID     StepID    `json:"step_id"`
	AttemptID  AttemptID `json:"attempt_id"`
	Generation int64     `json:"generation"`
}

func (r AttemptRef) Validate() error {
	if err := r.TenantID.Validate(); err != nil {
		return err
	}
	if err := r.RunID.Validate(); err != nil {
		return err
	}
	if err := r.StepID.Validate(); err != nil {
		return err
	}
	if err := r.AttemptID.Validate(); err != nil {
		return err
	}
	if r.Generation <= 0 {
		return &ValidationError{Field: "generation", Reason: "must be positive"}
	}
	return nil
}

type Lease struct {
	TenantID            TenantID             `json:"tenant_id"`
	RunID               RunID                `json:"run_id"`
	StepID              StepID               `json:"step_id"`
	AttemptID           AttemptID            `json:"attempt_id"`
	Generation          int64                `json:"generation"`
	Owner               string               `json:"owner"`
	ExpiresAt           time.Time            `json:"expires_at"`
	Kind                StepKind             `json:"kind"`
	Requirements        ResourceRequirements `json:"requirements"`
	InputManifestDigest string               `json:"input_manifest_digest"`
	Priority            int                  `json:"priority"`
}

func (l Lease) Ref() AttemptRef {
	return AttemptRef{
		TenantID:   l.TenantID,
		RunID:      l.RunID,
		StepID:     l.StepID,
		AttemptID:  l.AttemptID,
		Generation: l.Generation,
	}
}

type LeaseRequest struct {
	TenantID           TenantID      `json:"tenant_id"`
	RunID              RunID         `json:"run_id"`
	StepID             StepID        `json:"step_id"`
	AttemptID          AttemptID     `json:"attempt_id"`
	ExpectedGeneration int64         `json:"expected_generation"`
	Owner              string        `json:"owner"`
	TTL                time.Duration `json:"ttl"`
	ClusterID          ClusterID     `json:"cluster_id"`
	Now                time.Time     `json:"now"`
	Priority           int           `json:"priority"`
}

func (r LeaseRequest) Validate() error {
	if err := r.TenantID.Validate(); err != nil {
		return err
	}
	if err := r.RunID.Validate(); err != nil {
		return err
	}
	if err := r.StepID.Validate(); err != nil {
		return err
	}
	if err := r.AttemptID.Validate(); err != nil {
		return err
	}
	if r.ExpectedGeneration < 0 {
		return &ValidationError{Field: "expected_generation", Reason: "must not be negative"}
	}
	if err := RequireNonEmpty("lease_owner", r.Owner); err != nil {
		return err
	}
	if r.TTL < 10*time.Second || r.TTL > time.Hour {
		return &ValidationError{Field: "lease_ttl", Reason: "must be between 10s and 1h"}
	}
	return nil
}

type LeaseRenewal struct {
	Ref       AttemptRef    `json:"ref"`
	Owner     string        `json:"owner"`
	Extension time.Duration `json:"extension"`
	Now       time.Time     `json:"now"`
}

type LeaseExpiry struct {
	TenantID   TenantID  `json:"tenant_id"`
	RunID      RunID     `json:"run_id"`
	StepID     StepID    `json:"step_id"`
	AttemptID  AttemptID `json:"attempt_id"`
	Generation int64     `json:"generation"`
	ExpiredAt  time.Time `json:"expired_at"`
	Final      bool      `json:"final"`
}

type StepMetrics struct {
	CPUSeconds              float64 `json:"cpu_seconds"`
	MemoryGiBSeconds        float64 `json:"memory_gib_seconds"`
	EphemeralDiskGiBSeconds float64 `json:"ephemeral_disk_gib_seconds"`
	EgressBytes             int64   `json:"egress_bytes"`
	CASWrittenBytes         int64   `json:"cas_written_bytes"`
	QueueWaitMS             int64   `json:"queue_wait_ms"`
	ExecutionMS             int64   `json:"execution_ms"`
	UploadMS                int64   `json:"upload_ms"`
}

func (m StepMetrics) Validate() error {
	if m.CPUSeconds < 0 || m.MemoryGiBSeconds < 0 || m.EphemeralDiskGiBSeconds < 0 ||
		m.EgressBytes < 0 || m.CASWrittenBytes < 0 || m.QueueWaitMS < 0 ||
		m.ExecutionMS < 0 || m.UploadMS < 0 {
		return &ValidationError{Field: "metrics", Reason: "values must not be negative"}
	}
	return nil
}

type StepResult struct {
	Outcome         StepOutcome       `json:"outcome"`
	ErrorClass      ErrorClass        `json:"error_class,omitempty"`
	Message         string            `json:"message,omitempty"`
	Retryable       bool              `json:"retryable"`
	ResultDigest    string            `json:"result_digest"`
	OutputManifest  string            `json:"output_manifest,omitempty"`
	Artifacts       []CASRef          `json:"artifacts,omitempty"`
	Evidence        []Evidence        `json:"evidence,omitempty"`
	Counterexamples []Counterexample  `json:"counterexamples,omitempty"`
	Metrics         StepMetrics       `json:"metrics"`
	ExecutorRef     string            `json:"executor_ref,omitempty"`
	WorkerID        string            `json:"worker_id,omitempty"`
	Outputs         map[string]string `json:"outputs,omitempty"`
	FinishedAt      time.Time         `json:"finished_at"`
}

func (r StepResult) Validate() error {
	if !r.Outcome.Valid() {
		return &ValidationError{Field: "result.outcome", Reason: "unknown step outcome"}
	}
	if r.ErrorClass != "" && !r.ErrorClass.Valid() {
		return &ValidationError{Field: "result.error_class", Reason: "unknown error class"}
	}
	if err := r.Metrics.Validate(); err != nil {
		return err
	}
	if r.Outcome == OutcomeSucceeded && r.ResultDigest == "" {
		return &ValidationError{Field: "result.result_digest", Reason: "succeeded results require a digest"}
	}
	for i := range r.Evidence {
		if err := r.Evidence[i].Validate(); err != nil {
			return err
		}
	}
	for i := range r.Counterexamples {
		if err := r.Counterexamples[i].Validate(); err != nil {
			return err
		}
	}
	return nil
}

type AttemptCommit struct {
	Ref    AttemptRef `json:"ref"`
	Result StepResult `json:"result"`
	At     time.Time  `json:"at"`
}

func (c AttemptCommit) Validate() error {
	if err := c.Ref.Validate(); err != nil {
		return err
	}
	return c.Result.Validate()
}

type CommitOutcome struct {
	Step        Step      `json:"step"`
	Attempt     Attempt   `json:"attempt"`
	Promoted    []Step    `json:"promoted,omitempty"`
	AllTerminal bool      `json:"all_terminal"`
	RunStatus   RunStatus `json:"run_status"`
}

type AttemptFailure struct {
	Ref        AttemptRef `json:"ref"`
	ErrorClass ErrorClass `json:"error_class"`
	Message    string     `json:"message"`
	Retryable  bool       `json:"retryable"`
	At         time.Time  `json:"at"`
}

func (f AttemptFailure) Validate() error {
	if err := f.Ref.Validate(); err != nil {
		return err
	}
	if !f.ErrorClass.Valid() {
		return &ValidationError{Field: "error_class", Reason: "unknown error class"}
	}
	return nil
}

type ReadyQuery struct {
	Limit      int
	Shard      ShardID
	ShardCount int
	TenantID   TenantID
	Region     string
	Now        time.Time
	Kinds      []StepKind
}

type TenantUsage struct {
	TenantID         TenantID `json:"tenant_id"`
	ActiveRuns       int      `json:"active_runs"`
	ActiveSteps      int      `json:"active_steps"`
	DailyCPUSeconds  float64  `json:"daily_cpu_seconds"`
	DailyFuzzSeconds float64  `json:"daily_fuzz_seconds"`
}

func (s Step) LeaseExpired(at time.Time) bool {
	return s.LeaseDeadline != nil && !at.Before(*s.LeaseDeadline)
}

func ValidateCommitFence(step Step, attempt Attempt, ref AttemptRef, at time.Time) error {
	if attempt.StepID != step.ID {
		return &FencingError{StepID: step.ID, AttemptID: attempt.ID, Reason: "attempt belongs to another step"}
	}
	if attempt.Generation != step.Generation {
		return &FencingError{StepID: step.ID, AttemptID: attempt.ID, Reason: fmt.Sprintf("generation %d is stale, step is at %d", attempt.Generation, step.Generation)}
	}
	if step.CurrentAttemptID != attempt.ID || ref.AttemptID != attempt.ID {
		return &FencingError{StepID: step.ID, AttemptID: attempt.ID, Reason: "attempt is not the current attempt"}
	}
	if ref.Generation != step.Generation {
		return &FencingError{StepID: step.ID, AttemptID: attempt.ID, Reason: "submitted generation does not match the current lease"}
	}
	if step.Status != StepLeased && step.Status != StepRunning {
		return &FencingError{StepID: step.ID, AttemptID: attempt.ID, Reason: "step is not leased or running"}
	}
	if attempt.Status.Terminal() {
		return &FencingError{StepID: step.ID, AttemptID: attempt.ID, Reason: "attempt is already terminal"}
	}
	if at.Before(attempt.CreatedAt.Add(-time.Minute)) {
		return &FencingError{StepID: step.ID, AttemptID: attempt.ID, Reason: "commit timestamp precedes attempt creation"}
	}
	return nil
}

func NextStepStatus(step Step, result StepResult) (StepStatus, time.Duration) {
	if result.Outcome == OutcomeSucceeded {
		return StepSucceeded, 0
	}
	if result.Outcome == OutcomeCancelled {
		return StepCancelled, 0
	}
	attempts := step.AttemptCount
	if attempts < 1 {
		attempts = 1
	}
	canRetry := result.Retryable && attempts < step.MaxAttempts
	switch result.Outcome {
	case OutcomeRetryable:
		if canRetry {
			return StepRetryWait, RetryDelay(attempts)
		}
		return StepFailed, 0
	case OutcomeTimedOut:
		if canRetry {
			return StepRetryWait, RetryDelay(attempts)
		}
		return StepTimedOut, 0
	default:
		if canRetry && result.ErrorClass != "" && result.ErrorClass.Retryable() {
			return StepRetryWait, RetryDelay(attempts)
		}
		return StepFailed, 0
	}
}

func RetryDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	base := 5 * time.Second
	if attempt > 8 {
		attempt = 8
	}
	delay := time.Duration(math.Pow(2, float64(attempt-1))) * base
	if delay > 5*time.Minute {
		delay = 5 * time.Minute
	}
	return delay
}
