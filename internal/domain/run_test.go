package domain_test

import (
	"errors"
	"testing"
	"time"

	"github.com/patchproof/patchproof/internal/domain"
)

var allRunStatuses = []domain.RunStatus{
	domain.RunCreated, domain.RunValidating, domain.RunPlanned, domain.RunQueued,
	domain.RunRunning, domain.RunAggregating, domain.RunCancelling, domain.RunCompleted,
	domain.RunPartial, domain.RunInfraFailed, domain.RunCancelled, domain.RunRejected,
}

// wantRunTransitions is the independently written reference matrix. Keeping it
// separate from the production table means a missing or extra edge is caught.
var wantRunTransitions = map[domain.RunStatus][]domain.RunStatus{
	domain.RunCreated:     {domain.RunValidating, domain.RunRejected, domain.RunCancelling, domain.RunCancelled},
	domain.RunValidating:  {domain.RunPlanned, domain.RunRejected, domain.RunInfraFailed, domain.RunCancelling, domain.RunCancelled},
	domain.RunPlanned:     {domain.RunQueued, domain.RunCancelling, domain.RunCancelled},
	domain.RunQueued:      {domain.RunRunning, domain.RunCancelling, domain.RunCancelled, domain.RunInfraFailed},
	domain.RunRunning:     {domain.RunAggregating, domain.RunCancelling, domain.RunPartial, domain.RunInfraFailed, domain.RunCancelled},
	domain.RunAggregating: {domain.RunCompleted, domain.RunPartial, domain.RunInfraFailed, domain.RunCancelled},
	domain.RunCancelling:  {domain.RunCancelled, domain.RunPartial, domain.RunInfraFailed},
}

func containsStatus(values []domain.RunStatus, target domain.RunStatus) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func TestRunTransitionMatrix(t *testing.T) {
	for _, from := range allRunStatuses {
		for _, to := range allRunStatuses {
			want := containsStatus(wantRunTransitions[from], to)
			if got := from.CanTransitionTo(to); got != want {
				t.Errorf("CanTransitionTo(%s, %s) = %v, want %v", from, to, got, want)
			}
		}
	}
}

func TestRunTransitionValidEdges(t *testing.T) {
	for from, targets := range wantRunTransitions {
		for _, to := range targets {
			t.Run(string(from)+"->"+string(to), func(t *testing.T) {
				run := domain.Run{ID: testRunID, TenantID: testTenantID, Status: from, StateVersion: 7, UpdatedAt: oldTime}
				if err := run.Transition(to, newTime); err != nil {
					t.Fatalf("Transition(%s, %s) returned error: %v", from, to, err)
				}
				if run.Status != to {
					t.Fatalf("status = %s, want %s", run.Status, to)
				}
				if run.StateVersion != 8 {
					t.Fatalf("state version = %d, want 8", run.StateVersion)
				}
				if !run.UpdatedAt.Equal(newTime) {
					t.Fatalf("updated at = %s, want %s", run.UpdatedAt, newTime)
				}
				switch to {
				case domain.RunPlanned:
					if run.PlannedAt == nil || !run.PlannedAt.Equal(newTime) {
						t.Fatalf("planned at = %v, want %s", run.PlannedAt, newTime)
					}
				case domain.RunRunning:
					if run.StartedAt == nil || !run.StartedAt.Equal(newTime) {
						t.Fatalf("started at = %v, want %s", run.StartedAt, newTime)
					}
				}
				if to.Terminal() && (run.FinishedAt == nil || !run.FinishedAt.Equal(newTime)) {
					t.Fatalf("terminal status %s did not set finished at: %v", to, run.FinishedAt)
				}
			})
		}
	}
}

func TestRunTransitionInvalidEdges(t *testing.T) {
	for _, from := range allRunStatuses {
		for _, to := range allRunStatuses {
			if containsStatus(wantRunTransitions[from], to) {
				continue
			}
			t.Run(string(from)+"->"+string(to), func(t *testing.T) {
				run := domain.Run{ID: testRunID, TenantID: testTenantID, Status: from, StateVersion: 3, UpdatedAt: oldTime}
				err := run.Transition(to, newTime)
				var transitionErr *domain.StateTransitionError
				if !errors.As(err, &transitionErr) {
					t.Fatalf("Transition(%s, %s) error = %v, want *StateTransitionError", from, to, err)
				}
				if run.Status != from || run.StateVersion != 3 || !run.UpdatedAt.Equal(oldTime) {
					t.Fatalf("invalid transition mutated run: %+v", run)
				}
			})
		}
	}
}

func TestRunTransitionUnknownStatus(t *testing.T) {
	run := domain.Run{ID: testRunID, TenantID: testTenantID, Status: domain.RunCreated}
	err := run.Transition(domain.RunStatus("BOGUS"), newTime)
	var validationErr *domain.ValidationError
	if !errors.As(err, &validationErr) {
		t.Fatalf("error = %v, want *ValidationError", err)
	}
	if run.Status != domain.RunCreated {
		t.Fatalf("run mutated by unknown status: %s", run.Status)
	}
}

func TestRunTerminalStatusesAcceptNoTransitions(t *testing.T) {
	terminal := []domain.RunStatus{
		domain.RunCompleted, domain.RunPartial, domain.RunInfraFailed,
		domain.RunCancelled, domain.RunRejected,
	}
	for _, from := range terminal {
		if !from.Terminal() {
			t.Fatalf("%s should be terminal", from)
		}
		for _, to := range allRunStatuses {
			if from.CanTransitionTo(to) {
				t.Errorf("terminal %s accepts transition to %s", from, to)
			}
			run := domain.Run{ID: testRunID, TenantID: testTenantID, Status: from}
			err := run.Transition(to, newTime)
			var transitionErr *domain.StateTransitionError
			if !errors.As(err, &transitionErr) {
				t.Errorf("Transition(%s, %s) error = %v, want *StateTransitionError", from, to, err)
			}
		}
	}
}

func TestRunStartedAtOnlySetOnce(t *testing.T) {
	run := domain.Run{ID: testRunID, TenantID: testTenantID, Status: domain.RunQueued}
	first := newTime
	if err := run.Transition(domain.RunRunning, first); err != nil {
		t.Fatalf("queue->running: %v", err)
	}
	if err := run.Transition(domain.RunAggregating, first.Add(time.Minute)); err != nil {
		t.Fatalf("running->aggregating: %v", err)
	}
	if run.StartedAt == nil || !run.StartedAt.Equal(first) {
		t.Fatalf("started at was overwritten: %v", run.StartedAt)
	}
}

func TestRunSetVerdict(t *testing.T) {
	validStatuses := []domain.RunStatus{
		domain.RunAggregating, domain.RunCompleted, domain.RunPartial,
		domain.RunInfraFailed, domain.RunCancelled, domain.RunRejected,
	}
	for _, status := range validStatuses {
		run := domain.Run{ID: testRunID, TenantID: testTenantID, Status: status}
		if err := run.SetVerdict(domain.VerdictVerified); err != nil {
			t.Errorf("SetVerdict in %s returned error: %v", status, err)
			continue
		}
		if run.Verdict != domain.VerdictVerified {
			t.Errorf("verdict = %s, want VERIFIED_WITHIN_SCOPE", run.Verdict)
		}
	}

	invalidStatuses := []domain.RunStatus{
		domain.RunCreated, domain.RunValidating, domain.RunPlanned,
		domain.RunQueued, domain.RunRunning, domain.RunCancelling,
	}
	for _, status := range invalidStatuses {
		run := domain.Run{ID: testRunID, TenantID: testTenantID, Status: status}
		err := run.SetVerdict(domain.VerdictInconclusive)
		var transitionErr *domain.StateTransitionError
		if !errors.As(err, &transitionErr) {
			t.Errorf("SetVerdict in %s error = %v, want *StateTransitionError", status, err)
		}
		if run.Verdict != domain.VerdictNone {
			t.Errorf("SetVerdict in %s mutated verdict to %s", status, run.Verdict)
		}
	}

	run := domain.Run{ID: testRunID, TenantID: testTenantID, Status: domain.RunCompleted}
	var validationErr *domain.ValidationError
	if err := run.SetVerdict(domain.Verdict("BOGUS")); !errors.As(err, &validationErr) {
		t.Fatalf("invalid verdict error = %v, want *ValidationError", err)
	}
	if err := run.SetVerdict(domain.VerdictNone); !errors.As(err, &validationErr) {
		t.Fatalf("empty verdict error = %v, want *ValidationError", err)
	}
}
