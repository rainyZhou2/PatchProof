package memory

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/patchproof/patchproof/internal/domain"
)

// ListSteps returns the steps of a run in planning order. An unknown run yields
// an empty page, matching a tenant-scoped query.
func (s *Store) ListSteps(ctx context.Context, tenantID domain.TenantID, runID domain.RunID) ([]domain.Step, error) {
	if err := tenantID.Validate(); err != nil {
		return nil, err
	}
	if err := runID.Validate(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.readErrLocked(); err != nil {
		return nil, err
	}
	state, ok := s.runs[runID]
	if !ok || state.run.TenantID != tenantID {
		return []domain.Step{}, nil
	}
	return s.runStepsLocked(state), nil
}

// ListInFlightSteps returns leased/running steps across tenants for scheduler
// restart adoption.
func (s *Store) ListInFlightSteps(ctx context.Context, limit int) ([]domain.Step, error) {
	limit = normalizeLimit(limit, 500, 2000)
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.readErrLocked(); err != nil {
		return nil, err
	}
	out := make([]domain.Step, 0)
	for _, step := range s.steps {
		if step.Status == domain.StepLeased || step.Status == domain.StepRunning {
			out = append(out, *step)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.Before(out[j].UpdatedAt) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// SetAttemptExecutor persists the executor reference so a restarted scheduler
// can observe an in-flight execution instead of starting a duplicate.
func (s *Store) SetAttemptExecutor(ctx context.Context, ref domain.AttemptRef, executorRef string, at time.Time) error {
	if err := ref.Validate(); err != nil {
		return err
	}
	if err := domain.RequireNonEmpty("executor_ref", executorRef); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.writeErrLocked(); err != nil {
		return err
	}
	attempt, ok := s.attempts[ref.AttemptID]
	if !ok || attempt.StepID != ref.StepID || attempt.TenantID != ref.TenantID {
		return &domain.NotFoundError{Resource: "attempt", ID: ref.AttemptID.String()}
	}
	attempt.ExecutorRef = executorRef
	s.attempts[ref.AttemptID] = attempt
	return nil
}

// ListReadySteps returns schedulable steps ordered by priority DESC, created ASC.
func (s *Store) ListReadySteps(ctx context.Context, query domain.ReadyQuery) ([]domain.Step, error) {
	if query.TenantID != "" {
		if err := query.TenantID.Validate(); err != nil {
			return nil, err
		}
	}
	if query.ShardCount > 0 && query.Shard < 0 {
		return nil, &domain.ValidationError{Field: "shard", Reason: "must not be negative"}
	}
	kinds := make(map[domain.StepKind]struct{}, len(query.Kinds))
	for _, kind := range query.Kinds {
		if !kind.Valid() {
			return nil, &domain.ValidationError{Field: "kind", Reason: "unknown step kind " + string(kind)}
		}
		kinds[kind] = struct{}{}
	}
	now := normalizeTime(query.Now)
	limit := normalizeLimit(query.Limit, 100, 500)

	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.readErrLocked(); err != nil {
		return nil, err
	}
	out := make([]domain.Step, 0)
	for _, step := range s.steps {
		if step.Status != domain.StepReady {
			continue
		}
		if query.ShardCount > 0 && step.Shard%query.ShardCount != int(query.Shard) {
			continue
		}
		if query.TenantID != "" && step.TenantID != query.TenantID {
			continue
		}
		if len(kinds) > 0 {
			if _, ok := kinds[step.Kind]; !ok {
				continue
			}
		}
		if query.Region != "" && step.Requirements.Region != "" && !strings.EqualFold(step.Requirements.Region, query.Region) {
			continue
		}
		if step.NextAttemptAt != nil && step.NextAttemptAt.After(now) {
			continue
		}
		out = append(out, cloneStep(*step))
	}
	sortStepsReady(out)
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// PromoteDueSteps moves due RETRY_WAIT steps back to READY and emits step.ready.
func (s *Store) PromoteDueSteps(ctx context.Context, now time.Time, limit int) ([]domain.Step, error) {
	now = normalizeTime(now)
	limit = normalizeLimit(limit, 100, 500)

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.writeErrLocked(); err != nil {
		return nil, err
	}
	candidates := make([]*domain.Step, 0)
	for _, step := range s.steps {
		if step.Status != domain.StepRetryWait {
			continue
		}
		if step.NextAttemptAt == nil || step.NextAttemptAt.After(now) {
			continue
		}
		candidates = append(candidates, step)
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if !candidates[i].NextAttemptAt.Equal(*candidates[j].NextAttemptAt) {
			return candidates[i].NextAttemptAt.Before(*candidates[j].NextAttemptAt)
		}
		return candidates[i].ID < candidates[j].ID
	})
	if len(candidates) > limit {
		candidates = candidates[:limit]
	}

	events := make([]domain.OutboxEvent, 0, len(candidates))
	for _, step := range candidates {
		event, err := newStepEvent(*step, "", domain.EventStepReady, domain.StepReady, now)
		if err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	promoted := make([]domain.Step, 0, len(candidates))
	for _, step := range candidates {
		if err := step.Transition(domain.StepReady, now); err != nil {
			return nil, err
		}
		step.NextAttemptAt = nil
		promoted = append(promoted, cloneStep(*step))
	}
	s.appendEventsLocked(events)
	return promoted, nil
}

// GetStep returns a step scoped to its tenant.
func (s *Store) GetStep(ctx context.Context, tenantID domain.TenantID, stepID domain.StepID) (domain.Step, error) {
	if err := tenantID.Validate(); err != nil {
		return domain.Step{}, err
	}
	if err := stepID.Validate(); err != nil {
		return domain.Step{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.readErrLocked(); err != nil {
		return domain.Step{}, err
	}
	step, err := s.stepLocked(tenantID, stepID)
	if err != nil {
		return domain.Step{}, err
	}
	return cloneStep(*step), nil
}

// ListAttempts returns attempts in creation order.
func (s *Store) ListAttempts(ctx context.Context, tenantID domain.TenantID, stepID domain.StepID) ([]domain.Attempt, error) {
	if err := tenantID.Validate(); err != nil {
		return nil, err
	}
	if err := stepID.Validate(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.readErrLocked(); err != nil {
		return nil, err
	}
	out := make([]domain.Attempt, 0)
	for _, attemptID := range s.stepAttempts[stepID] {
		attempt, ok := s.attempts[attemptID]
		if !ok || attempt.TenantID != tenantID {
			continue
		}
		out = append(out, cloneAttempt(*attempt))
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}
