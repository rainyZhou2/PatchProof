package domain_test

import (
	"errors"
	"testing"
	"time"

	"github.com/patchproof/patchproof/internal/domain"
)

func TestStepTransitionValidEdges(t *testing.T) {
	for from, targets := range wantStepTransitions {
		for _, to := range targets {
			t.Run(string(from)+"->"+string(to), func(t *testing.T) {
				step := domain.Step{ID: testStep1, Status: from, UpdatedAt: oldTime}
				if err := step.Transition(to, newTime); err != nil {
					t.Fatalf("Transition(%s, %s) returned error: %v", from, to, err)
				}
				if step.Status != to {
					t.Fatalf("status = %s, want %s", step.Status, to)
				}
				if !step.UpdatedAt.Equal(newTime) {
					t.Fatalf("updated at = %s, want %s", step.UpdatedAt, newTime)
				}
				if to == domain.StepRunning {
					if step.StartedAt == nil || !step.StartedAt.Equal(newTime) {
						t.Fatalf("running did not set started at: %v", step.StartedAt)
					}
				}
				if to.Terminal() && (step.FinishedAt == nil || !step.FinishedAt.Equal(newTime)) {
					t.Fatalf("terminal %s did not set finished at: %v", to, step.FinishedAt)
				}
			})
		}
	}
}

func TestStepTransitionInvalidEdges(t *testing.T) {
	for _, from := range allStepStatuses {
		for _, to := range allStepStatuses {
			if containsStepStatus(wantStepTransitions[from], to) {
				continue
			}
			t.Run(string(from)+"->"+string(to), func(t *testing.T) {
				step := domain.Step{ID: testStep1, Status: from, UpdatedAt: oldTime}
				err := step.Transition(to, newTime)
				var transitionErr *domain.StateTransitionError
				if !errors.As(err, &transitionErr) {
					t.Fatalf("Transition(%s, %s) error = %v, want *StateTransitionError", from, to, err)
				}
				if step.Status != from || !step.UpdatedAt.Equal(oldTime) {
					t.Fatalf("invalid transition mutated step: %+v", step)
				}
			})
		}
	}
}

func TestStepTransitionUnknownStatus(t *testing.T) {
	step := domain.Step{ID: testStep1, Status: domain.StepReady}
	err := step.Transition(domain.StepStatus("BOGUS"), newTime)
	var validationErr *domain.ValidationError
	if !errors.As(err, &validationErr) {
		t.Fatalf("error = %v, want *ValidationError", err)
	}
}

func TestStepTerminalStatusesAcceptNoTransitions(t *testing.T) {
	terminal := []domain.StepStatus{domain.StepSucceeded, domain.StepFailed, domain.StepCancelled, domain.StepTimedOut}
	for _, from := range terminal {
		for _, to := range allStepStatuses {
			if from.CanTransitionTo(to) {
				t.Errorf("terminal step %s accepts transition to %s", from, to)
			}
		}
	}
}

func TestStepRetryWaitCanResume(t *testing.T) {
	step := domain.Step{ID: testStep1, Status: domain.StepRetryWait}
	if err := step.Transition(domain.StepReady, newTime); err != nil {
		t.Fatalf("retry-wait -> ready: %v", err)
	}
	if step.FinishedAt != nil {
		t.Fatalf("retry-wait -> ready must not finish the step: %v", step.FinishedAt)
	}
}

func baseFenceStep() domain.Step {
	return domain.Step{
		ID:               testStep1,
		Status:           domain.StepLeased,
		Generation:       5,
		CurrentAttemptID: testAttempt1,
	}
}

func baseFenceAttempt() domain.Attempt {
	return domain.Attempt{
		ID:         testAttempt1,
		StepID:     testStep1,
		Status:     domain.AttemptRunning,
		Generation: 5,
		CreatedAt:  oldTime,
	}
}

func baseFenceRef() domain.AttemptRef {
	return domain.AttemptRef{
		StepID:     testStep1,
		AttemptID:  testAttempt1,
		Generation: 5,
	}
}

func TestValidateCommitFenceHappyPath(t *testing.T) {
	step := baseFenceStep()
	attempt := baseFenceAttempt()
	if err := domain.ValidateCommitFence(step, attempt, baseFenceRef(), oldTime.Add(time.Minute)); err != nil {
		t.Fatalf("valid commit fence rejected: %v", err)
	}

	running := step
	running.Status = domain.StepRunning
	if err := domain.ValidateCommitFence(running, attempt, baseFenceRef(), oldTime.Add(time.Minute)); err != nil {
		t.Fatalf("running step fence rejected: %v", err)
	}
}

func TestValidateCommitFenceRejections(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*domain.Step, *domain.Attempt, *domain.AttemptRef)
		at      time.Time
		wantErr bool
	}{
		{
			name: "attempt belongs to another step",
			mutate: func(_ *domain.Step, attempt *domain.Attempt, _ *domain.AttemptRef) {
				attempt.StepID = testStep2
			},
		},
		{
			name: "stale attempt generation",
			mutate: func(_ *domain.Step, attempt *domain.Attempt, _ *domain.AttemptRef) {
				attempt.Generation = 4
			},
		},
		{
			name: "attempt is not the current attempt on the step",
			mutate: func(step *domain.Step, _ *domain.Attempt, _ *domain.AttemptRef) {
				step.CurrentAttemptID = testAttempt2
			},
		},
		{
			name: "ref points at another attempt",
			mutate: func(_ *domain.Step, _ *domain.Attempt, ref *domain.AttemptRef) {
				ref.AttemptID = testAttempt2
			},
		},
		{
			name: "submitted ref generation is stale",
			mutate: func(_ *domain.Step, _ *domain.Attempt, ref *domain.AttemptRef) {
				ref.Generation = 4
			},
		},
		{
			name: "step is not leased or running",
			mutate: func(step *domain.Step, _ *domain.Attempt, _ *domain.AttemptRef) {
				step.Status = domain.StepReady
			},
		},
		{
			name: "step is blocked",
			mutate: func(step *domain.Step, _ *domain.Attempt, _ *domain.AttemptRef) {
				step.Status = domain.StepBlocked
			},
		},
		{
			name: "attempt is already terminal",
			mutate: func(_ *domain.Step, attempt *domain.Attempt, _ *domain.AttemptRef) {
				attempt.Status = domain.AttemptReported
			},
		},
		{
			name:   "commit timestamp precedes attempt creation",
			mutate: func(_ *domain.Step, _ *domain.Attempt, _ *domain.AttemptRef) {},
			at:     oldTime.Add(-2 * time.Minute),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			step := baseFenceStep()
			attempt := baseFenceAttempt()
			ref := baseFenceRef()
			tc.mutate(&step, &attempt, &ref)
			at := tc.at
			if at.IsZero() {
				at = oldTime.Add(time.Minute)
			}
			err := domain.ValidateCommitFence(step, attempt, ref, at)
			var fenceErr *domain.FencingError
			if !errors.As(err, &fenceErr) {
				t.Fatalf("error = %v, want *FencingError", err)
			}
		})
	}
}

func TestNextStepStatus(t *testing.T) {
	tests := []struct {
		name       string
		attempts   int
		maxAttempt int
		result     domain.StepResult
		wantStatus domain.StepStatus
		wantDelay  time.Duration
	}{
		{
			name: "success", attempts: 1, maxAttempt: 3,
			result:     domain.StepResult{Outcome: domain.OutcomeSucceeded, ResultDigest: digestA},
			wantStatus: domain.StepSucceeded,
		},
		{
			name: "cancelled", attempts: 1, maxAttempt: 3,
			result:     domain.StepResult{Outcome: domain.OutcomeCancelled},
			wantStatus: domain.StepCancelled,
		},
		{
			name: "retryable under limit", attempts: 1, maxAttempt: 3,
			result:     domain.StepResult{Outcome: domain.OutcomeRetryable, Retryable: true},
			wantStatus: domain.StepRetryWait, wantDelay: 5 * time.Second,
		},
		{
			name: "retryable but not flagged retryable", attempts: 1, maxAttempt: 3,
			result:     domain.StepResult{Outcome: domain.OutcomeRetryable, Retryable: false},
			wantStatus: domain.StepFailed,
		},
		{
			name: "retryable at max attempts", attempts: 3, maxAttempt: 3,
			result:     domain.StepResult{Outcome: domain.OutcomeRetryable, Retryable: true},
			wantStatus: domain.StepFailed,
		},
		{
			name: "retryable zero attempts counts as first", attempts: 0, maxAttempt: 2,
			result:     domain.StepResult{Outcome: domain.OutcomeRetryable, Retryable: true},
			wantStatus: domain.StepRetryWait, wantDelay: 5 * time.Second,
		},
		{
			name: "timed out under limit", attempts: 2, maxAttempt: 4,
			result:     domain.StepResult{Outcome: domain.OutcomeTimedOut, Retryable: true},
			wantStatus: domain.StepRetryWait, wantDelay: 10 * time.Second,
		},
		{
			name: "timed out not retryable", attempts: 2, maxAttempt: 4,
			result:     domain.StepResult{Outcome: domain.OutcomeTimedOut, Retryable: false},
			wantStatus: domain.StepTimedOut,
		},
		{
			name: "failure with retryable error class", attempts: 2, maxAttempt: 5,
			result:     domain.StepResult{Outcome: domain.OutcomeFailed, Retryable: true, ErrorClass: domain.ErrNodeLost},
			wantStatus: domain.StepRetryWait, wantDelay: 10 * time.Second,
		},
		{
			name: "failure with retryable class but not flagged", attempts: 2, maxAttempt: 5,
			result:     domain.StepResult{Outcome: domain.OutcomeFailed, Retryable: false, ErrorClass: domain.ErrNodeLost},
			wantStatus: domain.StepFailed,
		},
		{
			name: "failure with non-retryable class", attempts: 1, maxAttempt: 5,
			result:     domain.StepResult{Outcome: domain.OutcomeFailed, Retryable: true, ErrorClass: domain.ErrCompilationFailed},
			wantStatus: domain.StepFailed,
		},
		{
			name: "failure with missing class", attempts: 1, maxAttempt: 5,
			result:     domain.StepResult{Outcome: domain.OutcomeFailed, Retryable: true},
			wantStatus: domain.StepFailed,
		},
		{
			name: "failure at max attempts", attempts: 5, maxAttempt: 5,
			result:     domain.StepResult{Outcome: domain.OutcomeFailed, Retryable: true, ErrorClass: domain.ErrNodeLost},
			wantStatus: domain.StepFailed,
		},
		{
			name: "unknown outcome falls back to failure", attempts: 1, maxAttempt: 3,
			result:     domain.StepResult{Outcome: domain.StepOutcome("BOGUS"), Retryable: true},
			wantStatus: domain.StepFailed,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			step := domain.Step{ID: testStep1, AttemptCount: tc.attempts, MaxAttempts: tc.maxAttempt}
			gotStatus, gotDelay := domain.NextStepStatus(step, tc.result)
			if gotStatus != tc.wantStatus {
				t.Fatalf("status = %s, want %s", gotStatus, tc.wantStatus)
			}
			if gotDelay != tc.wantDelay {
				t.Fatalf("delay = %s, want %s", gotDelay, tc.wantDelay)
			}
		})
	}
}

func TestRetryDelayExponentialCapped(t *testing.T) {
	tests := []struct {
		attempt int
		want    time.Duration
	}{
		{-3, 5 * time.Second},
		{0, 5 * time.Second},
		{1, 5 * time.Second},
		{2, 10 * time.Second},
		{3, 20 * time.Second},
		{4, 40 * time.Second},
		{5, 80 * time.Second},
		{6, 160 * time.Second},
		{7, 300 * time.Second},
		{8, 300 * time.Second},
		{100, 300 * time.Second},
	}
	for _, tc := range tests {
		if got := domain.RetryDelay(tc.attempt); got != tc.want {
			t.Errorf("RetryDelay(%d) = %s, want %s", tc.attempt, got, tc.want)
		}
	}
}

func TestStepLeaseExpired(t *testing.T) {
	step := domain.Step{ID: testStep1}
	if step.LeaseExpired(newTime) {
		t.Fatal("step without a deadline must not be expired")
	}
	deadline := newTime
	step.LeaseDeadline = &deadline
	if step.LeaseExpired(newTime.Add(-time.Second)) {
		t.Fatal("step must not be expired before its deadline")
	}
	if !step.LeaseExpired(deadline) {
		t.Fatal("step must be expired exactly at its deadline")
	}
	if !step.LeaseExpired(deadline.Add(time.Second)) {
		t.Fatal("step must be expired after its deadline")
	}
}
