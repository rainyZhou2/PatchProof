package domain_test

import (
	"testing"

	"github.com/patchproof/patchproof/internal/domain"
)

var allStepStatuses = []domain.StepStatus{
	domain.StepBlocked, domain.StepReady, domain.StepLeased, domain.StepRunning,
	domain.StepRetryWait, domain.StepSucceeded, domain.StepFailed,
	domain.StepCancelled, domain.StepTimedOut,
}

var wantStepTransitions = map[domain.StepStatus][]domain.StepStatus{
	domain.StepBlocked:   {domain.StepReady, domain.StepCancelled},
	domain.StepReady:     {domain.StepLeased, domain.StepCancelled},
	domain.StepLeased:    {domain.StepRunning, domain.StepRetryWait, domain.StepFailed, domain.StepCancelled, domain.StepTimedOut},
	domain.StepRunning:   {domain.StepSucceeded, domain.StepFailed, domain.StepRetryWait, domain.StepCancelled, domain.StepTimedOut},
	domain.StepRetryWait: {domain.StepReady, domain.StepCancelled, domain.StepFailed},
	domain.StepSucceeded: {},
	domain.StepFailed:    {},
	domain.StepCancelled: {},
	domain.StepTimedOut:  {},
}

var allAttemptStatuses = []domain.AttemptStatus{
	domain.AttemptAssigned, domain.AttemptStarting, domain.AttemptRunning,
	domain.AttemptUploading, domain.AttemptReported, domain.AttemptLost,
	domain.AttemptExpired, domain.AttemptFailedInfra, domain.AttemptCancelled,
}

var wantAttemptTransitions = map[domain.AttemptStatus][]domain.AttemptStatus{
	domain.AttemptAssigned:    {domain.AttemptStarting, domain.AttemptLost, domain.AttemptExpired, domain.AttemptFailedInfra, domain.AttemptCancelled},
	domain.AttemptStarting:    {domain.AttemptRunning, domain.AttemptLost, domain.AttemptExpired, domain.AttemptFailedInfra, domain.AttemptCancelled},
	domain.AttemptRunning:     {domain.AttemptUploading, domain.AttemptLost, domain.AttemptExpired, domain.AttemptFailedInfra, domain.AttemptCancelled},
	domain.AttemptUploading:   {domain.AttemptReported, domain.AttemptLost, domain.AttemptExpired, domain.AttemptFailedInfra, domain.AttemptCancelled},
	domain.AttemptReported:    {},
	domain.AttemptLost:        {},
	domain.AttemptExpired:     {},
	domain.AttemptFailedInfra: {},
	domain.AttemptCancelled:   {},
}

func containsStepStatus(values []domain.StepStatus, target domain.StepStatus) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func containsAttemptStatus(values []domain.AttemptStatus, target domain.AttemptStatus) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func TestStepTransitionMatrix(t *testing.T) {
	for _, from := range allStepStatuses {
		for _, to := range allStepStatuses {
			want := containsStepStatus(wantStepTransitions[from], to)
			if got := from.CanTransitionTo(to); got != want {
				t.Errorf("step CanTransitionTo(%s, %s) = %v, want %v", from, to, got, want)
			}
		}
	}
}

func TestAttemptTransitionMatrix(t *testing.T) {
	for _, from := range allAttemptStatuses {
		for _, to := range allAttemptStatuses {
			want := containsAttemptStatus(wantAttemptTransitions[from], to)
			if got := from.CanTransitionTo(to); got != want {
				t.Errorf("attempt CanTransitionTo(%s, %s) = %v, want %v", from, to, got, want)
			}
		}
	}
}

func TestTerminalDetection(t *testing.T) {
	wantRunTerminal := map[domain.RunStatus]bool{
		domain.RunCreated: false, domain.RunValidating: false, domain.RunPlanned: false,
		domain.RunQueued: false, domain.RunRunning: false, domain.RunAggregating: false,
		domain.RunCancelling: false, domain.RunCompleted: true, domain.RunPartial: true,
		domain.RunInfraFailed: true, domain.RunCancelled: true, domain.RunRejected: true,
	}
	for _, status := range allRunStatuses {
		if got := status.Terminal(); got != wantRunTerminal[status] {
			t.Errorf("run %s Terminal() = %v, want %v", status, got, wantRunTerminal[status])
		}
		if got := status.Active(); got == wantRunTerminal[status] {
			t.Errorf("run %s Active() = %v, terminal is %v", status, got, wantRunTerminal[status])
		}
	}

	wantStepTerminal := map[domain.StepStatus]bool{
		domain.StepBlocked: false, domain.StepReady: false, domain.StepLeased: false,
		domain.StepRunning: false, domain.StepRetryWait: false, domain.StepSucceeded: true,
		domain.StepFailed: true, domain.StepCancelled: true, domain.StepTimedOut: true,
	}
	for _, status := range allStepStatuses {
		if got := status.Terminal(); got != wantStepTerminal[status] {
			t.Errorf("step %s Terminal() = %v, want %v", status, got, wantStepTerminal[status])
		}
		if got := status.Active(); got == wantStepTerminal[status] {
			t.Errorf("step %s Active() = %v, terminal is %v", status, got, wantStepTerminal[status])
		}
	}

	wantAttemptTerminal := map[domain.AttemptStatus]bool{
		domain.AttemptAssigned: false, domain.AttemptStarting: false, domain.AttemptRunning: false,
		domain.AttemptUploading: false, domain.AttemptReported: true, domain.AttemptLost: true,
		domain.AttemptExpired: true, domain.AttemptFailedInfra: true, domain.AttemptCancelled: true,
	}
	for _, status := range allAttemptStatuses {
		if got := status.Terminal(); got != wantAttemptTerminal[status] {
			t.Errorf("attempt %s Terminal() = %v, want %v", status, got, wantAttemptTerminal[status])
		}
	}
}

func TestUnknownStatusValuesInvalid(t *testing.T) {
	if domain.RunStatus("BOGUS").Valid() {
		t.Error("unknown run status is valid")
	}
	if domain.StepStatus("BOGUS").Valid() {
		t.Error("unknown step status is valid")
	}
	if domain.AttemptStatus("BOGUS").Valid() {
		t.Error("unknown attempt status is valid")
	}
	if domain.Verdict("BOGUS").Valid() {
		t.Error("unknown verdict is valid")
	}
	if domain.StepOutcome("BOGUS").Valid() {
		t.Error("unknown step outcome is valid")
	}
	if domain.ClaimResult("BOGUS").Valid() {
		t.Error("unknown claim result is valid")
	}
}

func TestIsolationTierRanking(t *testing.T) {
	ordered := []domain.IsolationTier{
		domain.IsolationTrustedLocal, domain.IsolationContainer,
		domain.IsolationGVisor, domain.IsolationVM,
	}
	for i := 1; i < len(ordered); i++ {
		if ordered[i-1].Rank() >= ordered[i].Rank() {
			t.Errorf("rank(%s)=%d should be below rank(%s)=%d", ordered[i-1], ordered[i-1].Rank(), ordered[i], ordered[i].Rank())
		}
		if !ordered[i].AtLeast(ordered[i-1]) {
			t.Errorf("%s should be at least %s", ordered[i], ordered[i-1])
		}
		if ordered[i-1].AtLeast(ordered[i]) {
			t.Errorf("%s should not be at least %s", ordered[i-1], ordered[i])
		}
	}
	if domain.IsolationTier("BOGUS").Rank() != -1 {
		t.Error("unknown isolation tier rank should be -1")
	}
	if domain.IsolationTier("BOGUS").AtLeast(domain.IsolationTrustedLocal) {
		t.Error("unknown isolation tier must not satisfy a minimum")
	}
}

func TestClaimResultSatisfaction(t *testing.T) {
	tests := []struct {
		name    string
		result  domain.ClaimResult
		minimum domain.ClaimResult
		want    bool
	}{
		{"proven satisfies tested", domain.ClaimProven, domain.ClaimTested, true},
		{"tested satisfies tested", domain.ClaimTested, domain.ClaimTested, true},
		{"proven satisfies proven", domain.ClaimProven, domain.ClaimProven, true},
		{"tested does not satisfy proven", domain.ClaimTested, domain.ClaimProven, false},
		{"unknown does not satisfy tested", domain.ClaimUnknown, domain.ClaimTested, false},
		{"not run does not satisfy tested", domain.ClaimNotRun, domain.ClaimTested, false},
		{"invalid evidence does not satisfy tested", domain.ClaimInvalidEvidence, domain.ClaimTested, false},
		{"violated does not satisfy tested", domain.ClaimViolated, domain.ClaimTested, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.result.Satisfies(tc.minimum); got != tc.want {
				t.Fatalf("%s.Satisfies(%s) = %v, want %v", tc.result, tc.minimum, got, tc.want)
			}
		})
	}
}

func TestErrorClassMetadata(t *testing.T) {
	tests := []struct {
		class       domain.ErrorClass
		category    domain.ErrorCategory
		retryable   bool
		maxAttempts int
	}{
		{domain.ErrCompilationFailed, domain.CategoryDiscovery, false, 1},
		{domain.ErrEngineCrash, domain.CategoryTool, false, 2},
		{domain.ErrNodeLost, domain.CategoryInfra, true, 5},
		{domain.ErrCASDigestMismatch, domain.CategorySecurity, false, 1},
		{domain.ErrTenantQuotaExceeded, domain.CategoryAdmission, false, 1},
		{domain.ErrLeaseExpired, domain.CategoryControl, true, 5},
		{domain.ErrImagePullFailed, domain.CategoryInfra, false, 2},
	}
	for _, tc := range tests {
		if !tc.class.Valid() {
			t.Errorf("%s should be valid", tc.class)
		}
		if got := tc.class.Category(); got != tc.category {
			t.Errorf("%s category = %s, want %s", tc.class, got, tc.category)
		}
		if got := tc.class.Retryable(); got != tc.retryable {
			t.Errorf("%s retryable = %v, want %v", tc.class, got, tc.retryable)
		}
		if got := tc.class.MaxAttempts(); got != tc.maxAttempts {
			t.Errorf("%s max attempts = %d, want %d", tc.class, got, tc.maxAttempts)
		}
	}
	if domain.ErrorClass("BOGUS").Valid() {
		t.Error("unknown error class is valid")
	}
	if got := domain.ErrorClass("BOGUS").Category(); got != domain.CategoryUnknown {
		t.Errorf("unknown error class category = %s, want UNKNOWN", got)
	}
}

func TestStepKindRequiresLanguage(t *testing.T) {
	languageKinds := []domain.StepKind{
		domain.StepKindAnalyze, domain.StepKindImpact, domain.StepKindCompile,
		domain.StepKindRegression, domain.StepKindDifferential, domain.StepKindBoundedSMT,
		domain.StepKindSecurity,
	}
	for _, kind := range languageKinds {
		if !kind.RequiresLanguage() {
			t.Errorf("%s should require a language", kind)
		}
	}
	languageFree := []domain.StepKind{
		domain.StepKindSnapshot, domain.StepKindEvidence, domain.StepKindPolicy,
		domain.StepKindReport,
	}
	for _, kind := range languageFree {
		if kind.RequiresLanguage() {
			t.Errorf("%s should not require a language", kind)
		}
	}
}

func TestNormalizeCheckName(t *testing.T) {
	if got := domain.NormalizeCheckName("  Compile-Test  "); got != "compile-test" {
		t.Fatalf("normalized = %q, want compile-test", got)
	}
}
