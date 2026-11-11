package memory

import (
	"context"
	"sort"
	"time"

	"github.com/patchproof/patchproof/internal/domain"
)

// CommitAttempt fences, records and applies a worker result for one attempt.
func (s *Store) CommitAttempt(ctx context.Context, commit domain.AttemptCommit) (domain.CommitOutcome, error) {
	if err := commit.Validate(); err != nil {
		return domain.CommitOutcome{}, err
	}
	at := normalizeTime(commit.At)

	evidence := make([]domain.Evidence, len(commit.Result.Evidence))
	seenEvidence := make(map[domain.EvidenceID]struct{}, len(commit.Result.Evidence))
	for i := range commit.Result.Evidence {
		item := cloneEvidence(commit.Result.Evidence[i])
		if item.TenantID != commit.Ref.TenantID || item.RunID != commit.Ref.RunID {
			return domain.CommitOutcome{}, &domain.ValidationError{Field: "evidence", Reason: "evidence must belong to the committing run and tenant"}
		}
		if item.StepID == "" {
			item.StepID = commit.Ref.StepID
		} else if item.StepID != commit.Ref.StepID {
			return domain.CommitOutcome{}, &domain.ValidationError{Field: "evidence.step_id", Reason: "evidence belongs to another step"}
		}
		if _, duplicate := seenEvidence[item.ID]; duplicate {
			return domain.CommitOutcome{}, &domain.ValidationError{Field: "evidence.evidence_id", Reason: "duplicate evidence id in result"}
		}
		seenEvidence[item.ID] = struct{}{}
		evidence[i] = item
	}
	counterexamples := make([]domain.Counterexample, len(commit.Result.Counterexamples))
	for i := range commit.Result.Counterexamples {
		item := cloneCounterexample(commit.Result.Counterexamples[i])
		if item.TenantID != commit.Ref.TenantID || item.RunID != commit.Ref.RunID {
			return domain.CommitOutcome{}, &domain.ValidationError{Field: "counterexamples", Reason: "counterexamples must belong to the committing run and tenant"}
		}
		counterexamples[i] = item
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.writeErrLocked(); err != nil {
		return domain.CommitOutcome{}, err
	}
	state, step, attempt, err := s.loadFencedAttemptLocked(commit.Ref, at)
	if err != nil {
		return domain.CommitOutcome{}, err
	}
	for _, item := range evidence {
		if _, exists := s.evidenceIDs[item.ID]; exists {
			return domain.CommitOutcome{}, &domain.ConflictError{Resource: "evidence", ID: string(item.ID), Reason: "already exists"}
		}
	}

	next, delay := domain.NextStepStatus(*step, commit.Result)
	eventType := domain.EventStepFailed
	switch next {
	case domain.StepSucceeded:
		eventType = domain.EventStepSucceeded
	case domain.StepRetryWait:
		eventType = domain.EventStepRetry
	}
	event, err := newStepEvent(*step, commit.Ref.AttemptID, eventType, next, at)
	if err != nil {
		return domain.CommitOutcome{}, err
	}

	attemptStatus := domain.AttemptFailedInfra
	switch commit.Result.Outcome {
	case domain.OutcomeSucceeded:
		attemptStatus = domain.AttemptReported
	case domain.OutcomeCancelled:
		attemptStatus = domain.AttemptCancelled
	}
	attempt.Status = attemptStatus
	attempt.ResultDigest = commit.Result.ResultDigest
	attempt.ErrorClass = commit.Result.ErrorClass
	attempt.ErrorMessage = commit.Result.Message
	attempt.FinishedAt = timePtr(at)
	if commit.Result.WorkerID != "" {
		attempt.WorkerID = commit.Result.WorkerID
	}
	if commit.Result.ExecutorRef != "" {
		attempt.ExecutorRef = commit.Result.ExecutorRef
	}
	applyStepOutcome(step, next, commit.Result, delay, at)

	for _, item := range evidence {
		stored := cloneEvidence(item)
		s.evidence[item.RunID] = append(s.evidence[item.RunID], stored)
		s.evidenceIDs[item.ID] = struct{}{}
	}
	for _, item := range counterexamples {
		s.counterexamples[item.RunID] = append(s.counterexamples[item.RunID], cloneCounterexample(item))
	}
	s.metrics[step.ID] = append(s.metrics[step.ID], metricRecord{
		tenantID: step.TenantID,
		stepID:   step.ID,
		kind:     step.Kind,
		at:       at,
		metrics:  cloneMetrics(commit.Result.Metrics),
	})

	promoted, promotedEvents, err := s.preparePromotedLocked(state, at)
	if err != nil {
		return domain.CommitOutcome{}, err
	}
	s.appendEventsLocked(append([]domain.OutboxEvent{event}, promotedEvents...))
	allTerminal := s.allStepsTerminalLocked(state)
	return domain.CommitOutcome{
		Step:        cloneStep(*step),
		Attempt:     cloneAttempt(*attempt),
		Promoted:    promoted,
		AllTerminal: allTerminal,
		RunStatus:   state.run.Status,
	}, nil
}

// FailAttempt fences and records a worker failure without result evidence.
func (s *Store) FailAttempt(ctx context.Context, failure domain.AttemptFailure) (domain.CommitOutcome, error) {
	if err := failure.Validate(); err != nil {
		return domain.CommitOutcome{}, err
	}
	at := normalizeTime(failure.At)

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.writeErrLocked(); err != nil {
		return domain.CommitOutcome{}, err
	}
	state, step, attempt, err := s.loadFencedAttemptLocked(failure.Ref, at)
	if err != nil {
		return domain.CommitOutcome{}, err
	}

	next := domain.StepFailed
	var delay time.Duration
	if failure.ErrorClass == domain.ErrCancelled {
		next = domain.StepCancelled
	} else {
		next, delay = domain.NextStepStatus(*step, domain.StepResult{
			Outcome:    domain.OutcomeFailed,
			ErrorClass: failure.ErrorClass,
			Message:    failure.Message,
			Retryable:  failure.Retryable,
			FinishedAt: at,
		})
	}
	eventType := domain.EventStepFailed
	if next == domain.StepRetryWait {
		eventType = domain.EventStepRetry
	}
	event, err := newStepEvent(*step, failure.Ref.AttemptID, eventType, next, at)
	if err != nil {
		return domain.CommitOutcome{}, err
	}

	attemptStatus := domain.AttemptFailedInfra
	if failure.ErrorClass == domain.ErrCancelled {
		attemptStatus = domain.AttemptCancelled
	}
	attempt.Status = attemptStatus
	attempt.ErrorClass = failure.ErrorClass
	attempt.ErrorMessage = failure.Message
	attempt.FinishedAt = timePtr(at)
	applyStepFailure(step, next, failure.ErrorClass, failure.Message, delay, at)

	s.appendEventsLocked([]domain.OutboxEvent{event})
	return domain.CommitOutcome{
		Step:        cloneStep(*step),
		Attempt:     cloneAttempt(*attempt),
		AllTerminal: s.allStepsTerminalLocked(state),
		RunStatus:   state.run.Status,
	}, nil
}

func (s *Store) loadFencedAttemptLocked(ref domain.AttemptRef, at time.Time) (*runState, *domain.Step, *domain.Attempt, error) {
	state, err := s.runStateLocked(ref.TenantID, ref.RunID)
	if err != nil {
		return nil, nil, nil, err
	}
	step, err := s.stepLocked(ref.TenantID, ref.StepID)
	if err != nil {
		return nil, nil, nil, err
	}
	attempt, err := s.attemptLocked(ref.AttemptID)
	if err != nil {
		return nil, nil, nil, err
	}
	if state.run.Status.Terminal() || state.run.Status == domain.RunCancelling {
		return nil, nil, nil, &domain.FencingError{StepID: step.ID, AttemptID: attempt.ID, Reason: "run is not active"}
	}
	if step.RunID != ref.RunID || attempt.StepID != step.ID || attempt.RunID != ref.RunID || attempt.TenantID != ref.TenantID {
		return nil, nil, nil, &domain.FencingError{StepID: step.ID, AttemptID: attempt.ID, Reason: "attempt belongs to another step"}
	}
	if err := domain.ValidateCommitFence(*step, *attempt, ref, at); err != nil {
		return nil, nil, nil, err
	}
	return state, step, attempt, nil
}

func applyStepOutcome(step *domain.Step, next domain.StepStatus, result domain.StepResult, delay time.Duration, at time.Time) {
	step.UpdatedAt = at
	step.LeaseOwner = ""
	step.LeaseDeadline = nil
	step.CurrentAttemptID = ""
	step.NextAttemptAt = nil
	if result.ResultDigest != "" {
		step.ResultDigest = result.ResultDigest
	}
	if result.OutputManifest != "" {
		step.OutputManifest = result.OutputManifest
	}
	switch next {
	case domain.StepSucceeded:
		step.Status = domain.StepSucceeded
		step.FailureClass = ""
		step.FailureMessage = ""
		step.FinishedAt = timePtr(at)
	case domain.StepCancelled:
		step.Status = domain.StepCancelled
		step.FailureClass = result.ErrorClass
		step.FailureMessage = result.Message
		step.FinishedAt = timePtr(at)
	case domain.StepRetryWait:
		step.Status = domain.StepRetryWait
		step.FailureClass = result.ErrorClass
		step.FailureMessage = result.Message
		step.FinishedAt = nil
		step.NextAttemptAt = timePtr(at.Add(delay))
	default:
		step.Status = next
		step.FailureClass = result.ErrorClass
		step.FailureMessage = result.Message
		step.FinishedAt = timePtr(at)
	}
}

func applyStepFailure(step *domain.Step, next domain.StepStatus, class domain.ErrorClass, message string, delay time.Duration, at time.Time) {
	step.UpdatedAt = at
	step.LeaseOwner = ""
	step.LeaseDeadline = nil
	step.CurrentAttemptID = ""
	step.NextAttemptAt = nil
	step.Status = next
	step.FailureClass = class
	step.FailureMessage = message
	if next == domain.StepRetryWait {
		step.FinishedAt = nil
		step.NextAttemptAt = timePtr(at.Add(delay))
		return
	}
	step.FinishedAt = timePtr(at)
}

func (s *Store) preparePromotedLocked(state *runState, at time.Time) ([]domain.Step, []domain.OutboxEvent, error) {
	ids := s.promotableLocked(state)
	promoted := make([]domain.Step, 0, len(ids))
	events := make([]domain.OutboxEvent, 0, len(ids))
	for _, id := range ids {
		step, ok := s.steps[id]
		if !ok {
			return nil, nil, wrapInternal("blocked step %s is missing", id)
		}
		event, err := newStepEvent(*step, "", domain.EventStepReady, domain.StepReady, at)
		if err != nil {
			return nil, nil, err
		}
		events = append(events, event)
	}
	for _, id := range ids {
		step := s.steps[id]
		if err := step.Transition(domain.StepReady, at); err != nil {
			return nil, nil, err
		}
		step.NextAttemptAt = nil
		promoted = append(promoted, cloneStep(*step))
	}
	return promoted, events, nil
}

func (s *Store) promotableLocked(state *runState) []domain.StepID {
	out := make([]domain.StepID, 0)
	for _, id := range state.stepIDs {
		step, ok := s.steps[id]
		if !ok || step.Status != domain.StepBlocked {
			continue
		}
		ready := true
		for _, dependency := range state.deps[id] {
			dependencyStep, ok := s.steps[dependency]
			if !ok || dependencyStep.Status != domain.StepSucceeded {
				ready = false
				break
			}
		}
		if ready {
			out = append(out, id)
		}
	}
	return out
}

func (s *Store) allStepsTerminalLocked(state *runState) bool {
	if len(state.stepIDs) == 0 {
		return false
	}
	for _, id := range state.stepIDs {
		step, ok := s.steps[id]
		if !ok || !step.Status.Terminal() {
			return false
		}
	}
	return true
}

// ExpireLeases expires due leases, retrying or finally failing their steps.
func (s *Store) ExpireLeases(ctx context.Context, now time.Time, limit int) ([]domain.LeaseExpiry, error) {
	now = normalizeTime(now)
	limit = normalizeLimit(limit, 100, 500)

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.writeErrLocked(); err != nil {
		return nil, err
	}
	candidates := make([]*domain.Step, 0)
	for _, step := range s.steps {
		if step.Status != domain.StepLeased && step.Status != domain.StepRunning {
			continue
		}
		if step.LeaseDeadline == nil || now.Before(*step.LeaseDeadline) {
			continue
		}
		candidates = append(candidates, step)
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if !candidates[i].LeaseDeadline.Equal(*candidates[j].LeaseDeadline) {
			return candidates[i].LeaseDeadline.Before(*candidates[j].LeaseDeadline)
		}
		return candidates[i].ID < candidates[j].ID
	})
	if len(candidates) > limit {
		candidates = candidates[:limit]
	}
	states := make(map[domain.RunID]*runState, len(candidates))
	for _, step := range candidates {
		state, ok := s.runs[step.RunID]
		if !ok {
			return nil, wrapInternal("leased step %s has no run", step.ID)
		}
		states[step.RunID] = state
	}

	expiries := make([]domain.LeaseExpiry, 0, len(candidates))
	events := make([]domain.OutboxEvent, 0, len(candidates))
	for _, step := range candidates {
		state := states[step.RunID]
		attemptID := step.CurrentAttemptID
		generation := step.Generation
		attempt := s.attempts[attemptID]
		cancelling := state.run.Status.Terminal() || state.run.Status == domain.RunCancelling

		final := true
		eventType := domain.EventStepExpired
		nextStatus := domain.StepFailed
		if cancelling {
			nextStatus = domain.StepCancelled
		} else if step.AttemptCount < step.MaxAttempts {
			final = false
			eventType = domain.EventStepRetry
			nextStatus = domain.StepRetryWait
		}
		event, err := newStepEvent(*step, attemptID, eventType, nextStatus, now)
		if err != nil {
			return nil, err
		}
		events = append(events, event)

		if attempt != nil && !attempt.Status.Terminal() {
			switch {
			case cancelling:
				attempt.Status = domain.AttemptCancelled
				attempt.ErrorClass = domain.ErrCancelled
				attempt.ErrorMessage = "run cancelled"
			default:
				attempt.Status = domain.AttemptExpired
				attempt.ErrorClass = domain.ErrLeaseExpired
				attempt.ErrorMessage = "lease expired"
			}
			attempt.FinishedAt = timePtr(now)
		}

		step.UpdatedAt = now
		step.LeaseOwner = ""
		step.LeaseDeadline = nil
		step.CurrentAttemptID = ""
		step.NextAttemptAt = nil
		switch {
		case cancelling:
			step.Status = domain.StepCancelled
			step.FailureClass = domain.ErrCancelled
			step.FailureMessage = "run cancelled"
			step.FinishedAt = timePtr(now)
		case final:
			step.Status = domain.StepFailed
			step.FailureClass = domain.ErrLeaseExpired
			step.FailureMessage = "lease expired"
			step.FinishedAt = timePtr(now)
		default:
			step.Status = domain.StepRetryWait
			step.FailureClass = domain.ErrLeaseExpired
			step.FailureMessage = "lease expired"
			step.FinishedAt = nil
			step.NextAttemptAt = timePtr(now.Add(domain.RetryDelay(step.AttemptCount)))
		}
		expiries = append(expiries, domain.LeaseExpiry{
			TenantID:   step.TenantID,
			RunID:      step.RunID,
			StepID:     step.ID,
			AttemptID:  attemptID,
			Generation: generation,
			ExpiredAt:  now,
			Final:      final,
		})
	}
	s.appendEventsLocked(events)
	return expiries, nil
}

// CancelRunSteps cancels every non-terminal step and attempt of a run.
func (s *Store) CancelRunSteps(ctx context.Context, tenantID domain.TenantID, runID domain.RunID, at time.Time) (int, error) {
	if err := tenantID.Validate(); err != nil {
		return 0, err
	}
	if err := runID.Validate(); err != nil {
		return 0, err
	}
	at = normalizeTime(at)

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.writeErrLocked(); err != nil {
		return 0, err
	}
	state, err := s.runStateLocked(tenantID, runID)
	if err != nil {
		return 0, err
	}
	events := make([]domain.OutboxEvent, 0)
	cancelled := 0
	for _, id := range state.stepIDs {
		step, ok := s.steps[id]
		if !ok || step.Status.Terminal() {
			continue
		}
		event, err := newStepEvent(*step, step.CurrentAttemptID, domain.EventStepFailed, domain.StepCancelled, at)
		if err != nil {
			return 0, err
		}
		events = append(events, event)
		step.Status = domain.StepCancelled
		step.FailureClass = domain.ErrCancelled
		step.FailureMessage = "run cancelled"
		step.FinishedAt = timePtr(at)
		step.UpdatedAt = at
		step.LeaseOwner = ""
		step.LeaseDeadline = nil
		step.CurrentAttemptID = ""
		step.NextAttemptAt = nil
		cancelled++
		for _, attemptID := range s.stepAttempts[id] {
			attempt, ok := s.attempts[attemptID]
			if !ok || attempt.Status.Terminal() {
				continue
			}
			attempt.Status = domain.AttemptCancelled
			attempt.ErrorClass = domain.ErrCancelled
			attempt.ErrorMessage = "run cancelled"
			attempt.FinishedAt = timePtr(at)
		}
	}
	s.appendEventsLocked(events)
	return cancelled, nil
}
