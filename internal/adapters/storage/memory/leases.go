package memory

import (
	"context"
	"time"

	"github.com/patchproof/patchproof/internal/domain"
)

// AcquireLease atomically grants one generation of a READY step to one owner.
func (s *Store) AcquireLease(ctx context.Context, request domain.LeaseRequest) (domain.Lease, error) {
	if err := request.Validate(); err != nil {
		return domain.Lease{}, err
	}
	now := normalizeTime(request.Now)

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.writeErrLocked(); err != nil {
		return domain.Lease{}, err
	}
	state, err := s.runStateLocked(request.TenantID, request.RunID)
	if err != nil {
		return domain.Lease{}, err
	}
	if state.run.Status.Terminal() || state.run.Status == domain.RunCancelling {
		return domain.Lease{}, &domain.LeaseError{StepID: request.StepID, Reason: "run is not active"}
	}
	step, err := s.stepLocked(request.TenantID, request.StepID)
	if err != nil {
		return domain.Lease{}, err
	}
	if step.RunID != request.RunID {
		return domain.Lease{}, &domain.LeaseError{StepID: request.StepID, Reason: "step does not belong to the run"}
	}
	if step.Status != domain.StepReady {
		return domain.Lease{}, &domain.LeaseError{StepID: request.StepID, Reason: "step is not READY"}
	}
	if step.Generation != request.ExpectedGeneration {
		return domain.Lease{}, &domain.LeaseError{StepID: request.StepID, Reason: "generation does not match the current lease"}
	}
	if step.NextAttemptAt != nil && step.NextAttemptAt.After(now) {
		return domain.Lease{}, &domain.LeaseError{StepID: request.StepID, Reason: "step is not due yet"}
	}
	if _, exists := s.attempts[request.AttemptID]; exists {
		return domain.Lease{}, &domain.LeaseError{StepID: request.StepID, Reason: "attempt id already exists"}
	}
	if request.MaxParallelSteps > 0 {
		active := 0
		for _, id := range state.stepIDs {
			candidate, ok := s.steps[id]
			if !ok {
				continue
			}
			if candidate.Status == domain.StepLeased || candidate.Status == domain.StepRunning {
				active++
			}
		}
		if active >= request.MaxParallelSteps {
			return domain.Lease{}, &domain.LeaseError{StepID: request.StepID, Reason: "run parallel step budget is exhausted"}
		}
	}
	if request.MaxCPUSeconds > 0 {
		used := 0.0
		for _, id := range state.stepIDs {
			for _, record := range s.metrics[id] {
				used += record.metrics.CPUSeconds
			}
		}
		if used >= request.MaxCPUSeconds {
			return domain.Lease{}, &domain.LeaseError{StepID: request.StepID, Reason: "run CPU budget is exhausted"}
		}
	}

	newGeneration := step.Generation + 1
	deadline := now.Add(request.TTL)
	candidate := *step
	candidate.Generation = newGeneration
	candidate.Status = domain.StepLeased
	event, err := newStepEvent(candidate, request.AttemptID, domain.EventStepLeased, domain.StepLeased, now)
	if err != nil {
		return domain.Lease{}, err
	}

	if err := step.Transition(domain.StepLeased, now); err != nil {
		return domain.Lease{}, err
	}
	step.Generation = newGeneration
	step.CurrentAttemptID = request.AttemptID
	step.LeaseOwner = request.Owner
	step.LeaseDeadline = timePtr(deadline)
	step.NextAttemptAt = nil
	step.AttemptCount++
	if request.ClusterID != "" {
		step.TargetCluster = request.ClusterID
	}

	attempt := &domain.Attempt{
		ID:         request.AttemptID,
		StepID:     step.ID,
		RunID:      step.RunID,
		TenantID:   step.TenantID,
		Generation: newGeneration,
		Status:     domain.AttemptAssigned,
		ClusterID:  request.ClusterID,
		CreatedAt:  now,
	}
	s.attempts[attempt.ID] = attempt
	s.stepAttempts[step.ID] = append(s.stepAttempts[step.ID], attempt.ID)

	lease := domain.Lease{
		TenantID:            step.TenantID,
		RunID:               step.RunID,
		StepID:              step.ID,
		AttemptID:           attempt.ID,
		Generation:          newGeneration,
		Owner:               request.Owner,
		ExpiresAt:           deadline,
		Kind:                step.Kind,
		Requirements:        cloneRequirements(step.Requirements),
		InputManifestDigest: step.InputManifestDigest,
		Priority:            step.Priority,
	}
	s.appendEventsLocked([]domain.OutboxEvent{event})
	return cloneLease(lease), nil
}

// RenewLease extends a live lease for the current owner and generation.
func (s *Store) RenewLease(ctx context.Context, renewal domain.LeaseRenewal) error {
	if err := renewal.Ref.Validate(); err != nil {
		return err
	}
	if len(renewal.Owner) > 256 {
		return &domain.ValidationError{Field: "lease_owner", Reason: "must be at most 256 characters"}
	}
	if renewal.Extension <= 0 {
		return &domain.ValidationError{Field: "lease_extension", Reason: "must be positive"}
	}
	now := normalizeTime(renewal.Now)

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.writeErrLocked(); err != nil {
		return err
	}
	state, err := s.runStateLocked(renewal.Ref.TenantID, renewal.Ref.RunID)
	if err != nil {
		return err
	}
	if state.run.Status.Terminal() || state.run.Status == domain.RunCancelling {
		return &domain.LeaseError{StepID: renewal.Ref.StepID, Reason: "run is not active"}
	}
	step, err := s.stepLocked(renewal.Ref.TenantID, renewal.Ref.StepID)
	if err != nil {
		return err
	}
	attempt, err := s.attemptLocked(renewal.Ref.AttemptID)
	if err != nil {
		return err
	}
	if step.RunID != renewal.Ref.RunID || attempt.StepID != step.ID {
		return &domain.LeaseError{StepID: step.ID, Reason: "attempt does not belong to the step"}
	}
	if step.Generation != renewal.Ref.Generation || attempt.Generation != step.Generation ||
		step.CurrentAttemptID != attempt.ID {
		return &domain.LeaseError{StepID: step.ID, Reason: "generation is stale"}
	}
	if step.Status != domain.StepLeased && step.Status != domain.StepRunning {
		return &domain.LeaseError{StepID: step.ID, Reason: "step is not leased or running"}
	}
	if renewal.Owner != "" && step.LeaseOwner != renewal.Owner {
		return &domain.LeaseError{StepID: step.ID, Reason: "lease is owned by another worker"}
	}
	if step.LeaseDeadline == nil || !now.Before(*step.LeaseDeadline) {
		return &domain.LeaseError{StepID: step.ID, Reason: "lease has expired"}
	}
	deadline := now.Add(renewal.Extension)
	if deadline.Before(*step.LeaseDeadline) {
		deadline = *step.LeaseDeadline
	}
	step.LeaseDeadline = timePtr(deadline)
	step.UpdatedAt = now
	return nil
}

// StartAttempt marks an assigned attempt and its step as running.
func (s *Store) StartAttempt(ctx context.Context, ref domain.AttemptRef, at time.Time) error {
	if err := ref.Validate(); err != nil {
		return err
	}
	at = normalizeTime(at)

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.writeErrLocked(); err != nil {
		return err
	}
	state, err := s.runStateLocked(ref.TenantID, ref.RunID)
	if err != nil {
		return err
	}
	step, err := s.stepLocked(ref.TenantID, ref.StepID)
	if err != nil {
		return err
	}
	attempt, err := s.attemptLocked(ref.AttemptID)
	if err != nil {
		return err
	}
	if state.run.Status.Terminal() || state.run.Status == domain.RunCancelling {
		return &domain.FencingError{StepID: step.ID, AttemptID: attempt.ID, Reason: "run is not active"}
	}
	if attempt.StepID != step.ID || step.RunID != ref.RunID {
		return &domain.FencingError{StepID: step.ID, AttemptID: attempt.ID, Reason: "attempt belongs to another step"}
	}
	if step.Generation != ref.Generation || attempt.Generation != step.Generation ||
		step.CurrentAttemptID != attempt.ID {
		return &domain.FencingError{StepID: step.ID, AttemptID: attempt.ID, Reason: "generation is stale"}
	}
	if step.Status == domain.StepRunning && attempt.Status == domain.AttemptRunning {
		return nil
	}
	if step.Status != domain.StepLeased {
		return &domain.FencingError{StepID: step.ID, AttemptID: attempt.ID, Reason: "step is not leased"}
	}
	if attempt.Status != domain.AttemptAssigned && attempt.Status != domain.AttemptStarting {
		return &domain.FencingError{StepID: step.ID, AttemptID: attempt.ID, Reason: "attempt is not assignable"}
	}
	if err := step.Transition(domain.StepRunning, at); err != nil {
		return err
	}
	attempt.Status = domain.AttemptRunning
	if attempt.StartedAt == nil {
		attempt.StartedAt = timePtr(at)
	}
	return nil
}
