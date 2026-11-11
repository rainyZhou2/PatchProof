package domain

import "strings"

type RunStatus string

const (
	RunCreated     RunStatus = "CREATED"
	RunValidating  RunStatus = "VALIDATING"
	RunPlanned     RunStatus = "PLANNED"
	RunQueued      RunStatus = "QUEUED"
	RunRunning     RunStatus = "RUNNING"
	RunAggregating RunStatus = "AGGREGATING"
	RunCancelling  RunStatus = "CANCELLING"
	RunCompleted   RunStatus = "COMPLETED"
	RunPartial     RunStatus = "PARTIAL"
	RunInfraFailed RunStatus = "INFRA_FAILED"
	RunCancelled   RunStatus = "CANCELLED"
	RunRejected    RunStatus = "REJECTED"
)

var runTransitions = map[RunStatus][]RunStatus{
	RunCreated:     {RunValidating, RunRejected, RunCancelling, RunCancelled},
	RunValidating:  {RunPlanned, RunRejected, RunInfraFailed, RunCancelling, RunCancelled},
	RunPlanned:     {RunQueued, RunCancelling, RunCancelled},
	RunQueued:      {RunRunning, RunCancelling, RunCancelled, RunInfraFailed},
	RunRunning:     {RunAggregating, RunCancelling, RunPartial, RunInfraFailed, RunCancelled},
	RunAggregating: {RunCompleted, RunPartial, RunInfraFailed, RunCancelled},
	RunCancelling:  {RunCancelled, RunPartial, RunInfraFailed},
}

func (s RunStatus) Valid() bool {
	switch s {
	case RunCreated, RunValidating, RunPlanned, RunQueued, RunRunning, RunAggregating,
		RunCancelling, RunCompleted, RunPartial, RunInfraFailed, RunCancelled, RunRejected:
		return true
	}
	return false
}

func (s RunStatus) Terminal() bool {
	switch s {
	case RunCompleted, RunPartial, RunInfraFailed, RunCancelled, RunRejected:
		return true
	}
	return false
}

func (s RunStatus) Active() bool { return s.Valid() && !s.Terminal() }

func (s RunStatus) CanTransitionTo(next RunStatus) bool {
	for _, allowed := range runTransitions[s] {
		if allowed == next {
			return true
		}
	}
	return false
}

type Verdict string

const (
	VerdictNone           Verdict = ""
	VerdictCounterexample Verdict = "COUNTEREXAMPLE_FOUND"
	VerdictVerified       Verdict = "VERIFIED_WITHIN_SCOPE"
	VerdictInconclusive   Verdict = "INCONCLUSIVE"
	VerdictPolicyBlocked  Verdict = "POLICY_BLOCKED"
	VerdictInfraFailed    Verdict = "INFRA_FAILED"
	VerdictCancelled      Verdict = "CANCELLED"
)

func (v Verdict) Valid() bool {
	switch v {
	case VerdictCounterexample, VerdictVerified, VerdictInconclusive, VerdictPolicyBlocked,
		VerdictInfraFailed, VerdictCancelled:
		return true
	}
	return false
}

type StepStatus string

const (
	StepBlocked   StepStatus = "BLOCKED"
	StepReady     StepStatus = "READY"
	StepLeased    StepStatus = "LEASED"
	StepRunning   StepStatus = "RUNNING"
	StepRetryWait StepStatus = "RETRY_WAIT"
	StepSucceeded StepStatus = "SUCCEEDED"
	StepFailed    StepStatus = "FAILED"
	StepCancelled StepStatus = "CANCELLED"
	StepTimedOut  StepStatus = "TIMED_OUT"
)

var stepTransitions = map[StepStatus][]StepStatus{
	StepBlocked:   {StepReady, StepCancelled},
	StepReady:     {StepLeased, StepCancelled},
	StepLeased:    {StepRunning, StepRetryWait, StepFailed, StepCancelled, StepTimedOut},
	StepRunning:   {StepSucceeded, StepFailed, StepRetryWait, StepCancelled, StepTimedOut},
	StepRetryWait: {StepReady, StepCancelled, StepFailed},
	StepSucceeded: {},
	StepFailed:    {},
	StepCancelled: {},
	StepTimedOut:  {},
}

func (s StepStatus) Valid() bool {
	switch s {
	case StepBlocked, StepReady, StepLeased, StepRunning, StepRetryWait, StepSucceeded,
		StepFailed, StepCancelled, StepTimedOut:
		return true
	}
	return false
}

func (s StepStatus) Terminal() bool {
	switch s {
	case StepSucceeded, StepFailed, StepCancelled, StepTimedOut:
		return true
	}
	return false
}

func (s StepStatus) Active() bool { return s.Valid() && !s.Terminal() }

func (s StepStatus) CanTransitionTo(next StepStatus) bool {
	for _, allowed := range stepTransitions[s] {
		if allowed == next {
			return true
		}
	}
	return false
}

type AttemptStatus string

const (
	AttemptAssigned    AttemptStatus = "ASSIGNED"
	AttemptStarting    AttemptStatus = "STARTING"
	AttemptRunning     AttemptStatus = "RUNNING"
	AttemptUploading   AttemptStatus = "UPLOADING"
	AttemptReported    AttemptStatus = "REPORTED"
	AttemptLost        AttemptStatus = "LOST"
	AttemptExpired     AttemptStatus = "EXPIRED"
	AttemptFailedInfra AttemptStatus = "FAILED_INFRA"
	AttemptCancelled   AttemptStatus = "CANCELLED"
)

var attemptTransitions = map[AttemptStatus][]AttemptStatus{
	AttemptAssigned:    {AttemptStarting, AttemptLost, AttemptExpired, AttemptFailedInfra, AttemptCancelled},
	AttemptStarting:    {AttemptRunning, AttemptLost, AttemptExpired, AttemptFailedInfra, AttemptCancelled},
	AttemptRunning:     {AttemptUploading, AttemptLost, AttemptExpired, AttemptFailedInfra, AttemptCancelled},
	AttemptUploading:   {AttemptReported, AttemptLost, AttemptExpired, AttemptFailedInfra, AttemptCancelled},
	AttemptReported:    {},
	AttemptLost:        {},
	AttemptExpired:     {},
	AttemptFailedInfra: {},
	AttemptCancelled:   {},
}

func (s AttemptStatus) Valid() bool {
	switch s {
	case AttemptAssigned, AttemptStarting, AttemptRunning, AttemptUploading, AttemptReported,
		AttemptLost, AttemptExpired, AttemptFailedInfra, AttemptCancelled:
		return true
	}
	return false
}

func (s AttemptStatus) Terminal() bool {
	switch s {
	case AttemptReported, AttemptLost, AttemptExpired, AttemptFailedInfra, AttemptCancelled:
		return true
	}
	return false
}

func (s AttemptStatus) CanTransitionTo(next AttemptStatus) bool {
	for _, allowed := range attemptTransitions[s] {
		if allowed == next {
			return true
		}
	}
	return false
}

type StepOutcome string

const (
	OutcomeSucceeded StepOutcome = "SUCCEEDED"
	OutcomeFailed    StepOutcome = "FAILED"
	OutcomeRetryable StepOutcome = "RETRYABLE"
	OutcomeCancelled StepOutcome = "CANCELLED"
	OutcomeTimedOut  StepOutcome = "TIMED_OUT"
)

func (o StepOutcome) Valid() bool {
	switch o {
	case OutcomeSucceeded, OutcomeFailed, OutcomeRetryable, OutcomeCancelled, OutcomeTimedOut:
		return true
	}
	return false
}

type ClaimResult string

const (
	ClaimViolated        ClaimResult = "VIOLATED"
	ClaimProven          ClaimResult = "PROVEN_WITHIN_MODEL"
	ClaimTested          ClaimResult = "TESTED_WITHIN_SCOPE"
	ClaimUnknown         ClaimResult = "UNKNOWN"
	ClaimNotRun          ClaimResult = "NOT_RUN"
	ClaimInvalidEvidence ClaimResult = "INVALID_EVIDENCE"
)

func (r ClaimResult) Valid() bool {
	switch r {
	case ClaimViolated, ClaimProven, ClaimTested, ClaimUnknown, ClaimNotRun, ClaimInvalidEvidence:
		return true
	}
	return false
}

func (r ClaimResult) Satisfies(minimum ClaimResult) bool {
	rank := map[ClaimResult]int{
		ClaimInvalidEvidence: 0,
		ClaimViolated:        0,
		ClaimNotRun:          1,
		ClaimUnknown:         1,
		ClaimTested:          2,
		ClaimProven:          3,
	}
	return rank[r] >= rank[minimum]
}

type ClaimKind string

const (
	ClaimKindRegression ClaimKind = "REGRESSION"
	ClaimKindFix        ClaimKind = "FIX"
	ClaimKindCompile    ClaimKind = "COMPILE"
	ClaimKindTest       ClaimKind = "TEST"
	ClaimKindSecurity   ClaimKind = "SECURITY"
	ClaimKindProvenance ClaimKind = "PROVENANCE"
)

func (k ClaimKind) Valid() bool {
	switch k {
	case ClaimKindRegression, ClaimKindFix, ClaimKindCompile, ClaimKindTest, ClaimKindSecurity,
		ClaimKindProvenance:
		return true
	}
	return false
}

type EvidenceKind string

const (
	EvidenceCompile      EvidenceKind = "COMPILE"
	EvidenceStatic       EvidenceKind = "STATIC_ANALYSIS"
	EvidenceDifferential EvidenceKind = "DIFFERENTIAL_TEST"
	EvidenceRegression   EvidenceKind = "REGRESSION_TEST"
	EvidenceFuzz         EvidenceKind = "FUZZ"
	EvidenceSMT          EvidenceKind = "SMT"
	EvidencePolicy       EvidenceKind = "POLICY"
	EvidenceCoverage     EvidenceKind = "COVERAGE"
	EvidenceEngineCrash  EvidenceKind = "ENGINE_CRASH"
	EvidenceProvenance   EvidenceKind = "PROVENANCE"
)

func (k EvidenceKind) Valid() bool {
	switch k {
	case EvidenceCompile, EvidenceStatic, EvidenceDifferential, EvidenceRegression, EvidenceFuzz,
		EvidenceSMT, EvidencePolicy, EvidenceCoverage, EvidenceEngineCrash, EvidenceProvenance:
		return true
	}
	return false
}

type ErrorClass string

const (
	ErrCompilationFailed   ErrorClass = "COMPILATION_FAILED"
	ErrTestAssertionFailed ErrorClass = "TEST_ASSERTION_FAILED"
	ErrCounterexampleFound ErrorClass = "COUNTEREXAMPLE_FOUND"
	ErrEngineUnsupported   ErrorClass = "ENGINE_UNSUPPORTED"
	ErrSolverTimeout       ErrorClass = "SOLVER_TIMEOUT"
	ErrEngineCrash         ErrorClass = "ENGINE_CRASH"
	ErrNodeLost            ErrorClass = "NODE_LOST"
	ErrPodEvicted          ErrorClass = "POD_EVICTED"
	ErrImagePullFailed     ErrorClass = "IMAGE_PULL_FAILED"
	ErrCASUnavailable      ErrorClass = "CAS_5XX"
	ErrCASDigestMismatch   ErrorClass = "CAS_DIGEST_MISMATCH"
	ErrPolicyTampered      ErrorClass = "POLICY_TAMPERED"
	ErrLeaseExpired        ErrorClass = "LEASE_EXPIRED"
	ErrTenantQuotaExceeded ErrorClass = "TENANT_QUOTA_EXCEEDED"
	ErrValidationFailed    ErrorClass = "VALIDATION_FAILED"
	ErrDependencyFailed    ErrorClass = "DEPENDENCY_FAILED"
	ErrExecutorError       ErrorClass = "EXECUTOR_ERROR"
	ErrWorkerLost          ErrorClass = "WORKER_LOST"
	ErrUploadFailed        ErrorClass = "UPLOAD_FAILED"
	ErrSandboxUnavailable  ErrorClass = "SANDBOX_UNAVAILABLE"
	ErrTimeout             ErrorClass = "TIMEOUT"
	ErrCancelled           ErrorClass = "CANCELLED"
	ErrPolicyBlocked       ErrorClass = "POLICY_BLOCKED"
	ErrAuthzDenied         ErrorClass = "AUTHZ_DENIED"
)

type ErrorCategory string

const (
	CategoryDiscovery ErrorCategory = "DISCOVERY"
	CategoryUnknown   ErrorCategory = "UNKNOWN"
	CategoryTool      ErrorCategory = "TOOL"
	CategoryInfra     ErrorCategory = "INFRA"
	CategorySecurity  ErrorCategory = "SECURITY"
	CategoryAdmission ErrorCategory = "ADMISSION"
	CategoryControl   ErrorCategory = "CONTROL"
)

func (c ErrorClass) Category() ErrorCategory {
	switch c {
	case ErrCompilationFailed, ErrTestAssertionFailed, ErrCounterexampleFound:
		return CategoryDiscovery
	case ErrEngineUnsupported, ErrSolverTimeout:
		return CategoryUnknown
	case ErrEngineCrash:
		return CategoryTool
	case ErrNodeLost, ErrPodEvicted, ErrImagePullFailed, ErrCASUnavailable, ErrWorkerLost,
		ErrUploadFailed, ErrExecutorError, ErrSandboxUnavailable, ErrTimeout:
		return CategoryInfra
	case ErrCASDigestMismatch, ErrPolicyTampered, ErrAuthzDenied:
		return CategorySecurity
	case ErrTenantQuotaExceeded:
		return CategoryAdmission
	case ErrLeaseExpired, ErrDependencyFailed:
		return CategoryControl
	}
	return CategoryUnknown
}

func (c ErrorClass) Retryable() bool {
	switch c {
	case ErrNodeLost, ErrPodEvicted, ErrCASUnavailable, ErrWorkerLost, ErrUploadFailed,
		ErrExecutorError, ErrLeaseExpired, ErrTimeout:
		return true
	}
	return false
}

func (c ErrorClass) MaxAttempts() int {
	switch c {
	case ErrEngineCrash, ErrImagePullFailed:
		return 2
	case ErrNodeLost, ErrPodEvicted, ErrWorkerLost, ErrUploadFailed, ErrExecutorError, ErrCASUnavailable, ErrLeaseExpired, ErrTimeout:
		return 5
	}
	return 1
}

func (c ErrorClass) Valid() bool {
	switch c {
	case ErrCompilationFailed, ErrTestAssertionFailed, ErrCounterexampleFound, ErrEngineUnsupported,
		ErrSolverTimeout, ErrEngineCrash, ErrNodeLost, ErrPodEvicted, ErrImagePullFailed,
		ErrCASUnavailable, ErrCASDigestMismatch, ErrPolicyTampered, ErrLeaseExpired,
		ErrTenantQuotaExceeded, ErrValidationFailed, ErrDependencyFailed, ErrExecutorError,
		ErrWorkerLost, ErrUploadFailed, ErrSandboxUnavailable, ErrTimeout, ErrCancelled,
		ErrPolicyBlocked, ErrAuthzDenied:
		return true
	}
	return false
}

type IsolationTier string

const (
	IsolationTrustedLocal IsolationTier = "trusted-local"
	IsolationContainer    IsolationTier = "isolated-container"
	IsolationGVisor       IsolationTier = "untrusted-gvisor"
	IsolationVM           IsolationTier = "untrusted-vm"
)

func (t IsolationTier) Valid() bool {
	switch t {
	case IsolationTrustedLocal, IsolationContainer, IsolationGVisor, IsolationVM:
		return true
	}
	return false
}

func (t IsolationTier) Rank() int {
	switch t {
	case IsolationTrustedLocal:
		return 0
	case IsolationContainer:
		return 1
	case IsolationGVisor:
		return 2
	case IsolationVM:
		return 3
	}
	return -1
}

func (t IsolationTier) AtLeast(minimum IsolationTier) bool {
	return t.Rank() >= minimum.Rank() && t.Valid()
}

type NetworkMode string

const (
	NetworkNone       NetworkMode = "none"
	NetworkRestricted NetworkMode = "restricted"
)

func (n NetworkMode) Valid() bool { return n == NetworkNone || n == NetworkRestricted }

type ArtifactTransport string

const (
	TransportBrokeredOffline ArtifactTransport = "brokered-offline-io"
	TransportDirect          ArtifactTransport = "direct"
)

func (t ArtifactTransport) Valid() bool {
	return t == TransportBrokeredOffline || t == TransportDirect
}

type SecurityTier string

const (
	SecurityTierStandard  SecurityTier = "standard"
	SecurityTierElevated  SecurityTier = "elevated"
	SecurityTierRegulated SecurityTier = "regulated"
)

func (t SecurityTier) Valid() bool {
	return t == SecurityTierStandard || t == SecurityTierElevated || t == SecurityTierRegulated
}

type TenantStatus string

const (
	TenantActive    TenantStatus = "ACTIVE"
	TenantMigrating TenantStatus = "MIGRATING"
	TenantSuspended TenantStatus = "SUSPENDED"
	TenantDeleting  TenantStatus = "DELETING"
)

func (s TenantStatus) Valid() bool {
	switch s {
	case TenantActive, TenantMigrating, TenantSuspended, TenantDeleting:
		return true
	}
	return false
}

type AttestationStatus string

const (
	AttestationPending AttestationStatus = "PENDING_ATTESTATION"
	AttestationSigned  AttestationStatus = "SIGNED"
	AttestationFailed  AttestationStatus = "FAILED"
)

func (s AttestationStatus) Valid() bool {
	return s == AttestationPending || s == AttestationSigned || s == AttestationFailed
}

type StepKind string

const (
	StepKindSnapshot     StepKind = "snapshot"
	StepKindAnalyze      StepKind = "analyze"
	StepKindImpact       StepKind = "impact-analysis"
	StepKindCompile      StepKind = "compile"
	StepKindRegression   StepKind = "regression-test"
	StepKindDifferential StepKind = "differential-fuzz"
	StepKindBoundedSMT   StepKind = "bounded-smt"
	StepKindSecurity     StepKind = "security-scan"
	StepKindEvidence     StepKind = "evidence-normalization"
	StepKindPolicy       StepKind = "policy-evaluation"
	StepKindReport       StepKind = "report"
)

func (k StepKind) Valid() bool {
	switch k {
	case StepKindSnapshot, StepKindAnalyze, StepKindImpact, StepKindCompile, StepKindRegression,
		StepKindDifferential, StepKindBoundedSMT, StepKindSecurity, StepKindEvidence,
		StepKindPolicy, StepKindReport:
		return true
	}
	return false
}

func (k StepKind) String() string { return string(k) }

func (k StepKind) RequiresLanguage() bool {
	switch k {
	case StepKindAnalyze, StepKindImpact, StepKindCompile, StepKindRegression, StepKindDifferential,
		StepKindBoundedSMT, StepKindSecurity:
		return true
	}
	return false
}

type ChangeKind string

const (
	ChangeAdded    ChangeKind = "ADDED"
	ChangeModified ChangeKind = "MODIFIED"
	ChangeDeleted  ChangeKind = "DELETED"
	ChangeRenamed  ChangeKind = "RENAMED"
)

type Resolution string

const (
	ResolutionExact   Resolution = "EXACT"
	ResolutionApprox  Resolution = "APPROX"
	ResolutionUnknown Resolution = "UNKNOWN"
)

func (r Resolution) Valid() bool {
	return r == ResolutionExact || r == ResolutionApprox || r == ResolutionUnknown
}

func NormalizeCheckName(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}
