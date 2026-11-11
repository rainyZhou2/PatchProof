package memory

import (
	"context"
	"encoding/base64"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/patchproof/patchproof/internal/domain"
)

// CreateRun inserts a run, deduplicating by tenant, project and request key.
func (s *Store) CreateRun(ctx context.Context, record domain.CreateRunRecord) (domain.Run, error) {
	run := record.Run
	if err := validateRunForCreate(run); err != nil {
		return domain.Run{}, err
	}
	if err := validateEventList(record.Events); err != nil {
		return domain.Run{}, err
	}
	if run.CreatedAt.IsZero() {
		run.CreatedAt = time.Now().UTC()
	} else {
		run.CreatedAt = run.CreatedAt.UTC()
	}
	if run.UpdatedAt.IsZero() {
		run.UpdatedAt = run.CreatedAt
	} else {
		run.UpdatedAt = run.UpdatedAt.UTC()
	}
	run = cloneRun(run)

	key := runKey{tenantID: run.TenantID, projectID: run.ProjectID, request: run.RequestKey}

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.writeErrLocked(); err != nil {
		return domain.Run{}, err
	}
	if existingID, ok := s.runKeys[key]; ok {
		existing := s.runs[existingID]
		if existing == nil {
			return domain.Run{}, wrapInternal("idempotency index points at missing run %s", existingID)
		}
		if existing.run.Fingerprint != run.Fingerprint {
			return domain.Run{}, &domain.IdempotencyConflictError{RequestKey: run.RequestKey}
		}
		return cloneRun(existing.run), nil
	}
	if _, exists := s.runs[run.ID]; exists {
		return domain.Run{}, &domain.ConflictError{Resource: "run", ID: run.ID.String(), Reason: "already exists"}
	}
	if err := s.ensureEventsAbsentLocked(record.Events); err != nil {
		return domain.Run{}, err
	}

	s.runs[run.ID] = &runState{run: run}
	s.runKeys[key] = run.ID
	s.appendEventsLocked(record.Events)
	return cloneRun(run), nil
}

func validateRunForCreate(run domain.Run) error {
	if err := run.Validate(); err != nil {
		return err
	}
	if err := domain.RequireNonEmpty("request_key", run.RequestKey); err != nil {
		return err
	}
	if err := domain.RequireMaxLen("request_key", run.RequestKey, 200); err != nil {
		return err
	}
	if run.StateVersion < 0 {
		return &domain.ValidationError{Field: "state_version", Reason: "must not be negative"}
	}
	return nil
}

// GetRun returns a run scoped to its tenant.
func (s *Store) GetRun(ctx context.Context, tenantID domain.TenantID, runID domain.RunID) (domain.Run, error) {
	if err := tenantID.Validate(); err != nil {
		return domain.Run{}, err
	}
	if err := runID.Validate(); err != nil {
		return domain.Run{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.readErrLocked(); err != nil {
		return domain.Run{}, err
	}
	state, err := s.runStateLocked(tenantID, runID)
	if err != nil {
		return domain.Run{}, err
	}
	return cloneRun(state.run), nil
}

// ListRuns returns a filtered page ordered by created_at DESC, run_id DESC.
func (s *Store) ListRuns(ctx context.Context, filter domain.RunFilter) (domain.RunPage, error) {
	if filter.TenantID != "" {
		if err := filter.TenantID.Validate(); err != nil {
			return domain.RunPage{}, err
		}
	}
	statuses := make(map[domain.RunStatus]struct{}, len(filter.Statuses))
	for _, status := range filter.Statuses {
		if !status.Valid() {
			return domain.RunPage{}, &domain.ValidationError{Field: "run.status", Reason: fmt.Sprintf("unknown status %q", status)}
		}
		statuses[status] = struct{}{}
	}
	limit := normalizeLimit(filter.Limit, 50, 500)

	var cursorAt time.Time
	var cursorID domain.RunID
	if filter.Cursor != "" {
		var err error
		cursorAt, cursorID, err = decodeRunCursor(filter.Cursor)
		if err != nil {
			return domain.RunPage{}, err
		}
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.readErrLocked(); err != nil {
		return domain.RunPage{}, err
	}

	matched := make([]domain.Run, 0)
	for _, state := range s.runs {
		run := state.run
		if filter.TenantID != "" && run.TenantID != filter.TenantID {
			continue
		}
		if filter.ProjectID != "" && run.ProjectID != filter.ProjectID {
			continue
		}
		if len(statuses) > 0 {
			if _, ok := statuses[run.Status]; !ok {
				continue
			}
		}
		if filter.CreatedAfter != nil && run.CreatedAt.Before(*filter.CreatedAfter) {
			continue
		}
		if filter.CreatedBefore != nil && !run.CreatedAt.Before(*filter.CreatedBefore) {
			continue
		}
		if !cursorAt.IsZero() {
			if run.CreatedAt.After(cursorAt) {
				continue
			}
			if run.CreatedAt.Equal(cursorAt) && run.ID >= cursorID {
				continue
			}
		}
		matched = append(matched, cloneRun(run))
	}
	sort.SliceStable(matched, func(i, j int) bool {
		if !matched[i].CreatedAt.Equal(matched[j].CreatedAt) {
			return matched[i].CreatedAt.After(matched[j].CreatedAt)
		}
		return matched[i].ID > matched[j].ID
	})

	page := domain.RunPage{Runs: make([]domain.Run, 0, limit)}
	for i := 0; i < len(matched) && i < limit; i++ {
		page.Runs = append(page.Runs, matched[i])
	}
	if len(matched) > limit && limit > 0 {
		last := page.Runs[len(page.Runs)-1]
		page.NextCursor = encodeRunCursor(last.CreatedAt, last.ID)
	}
	return page, nil
}

func encodeRunCursor(at time.Time, runID domain.RunID) string {
	raw := at.UTC().Format(time.RFC3339Nano) + "|" + runID.String()
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeRunCursor(cursor string) (time.Time, domain.RunID, error) {
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		raw, err = base64.URLEncoding.DecodeString(cursor)
	}
	if err != nil {
		return time.Time{}, "", &domain.ValidationError{Field: "cursor", Reason: "is not valid base64url"}
	}
	parts := strings.SplitN(string(raw), "|", 2)
	if len(parts) != 2 {
		return time.Time{}, "", &domain.ValidationError{Field: "cursor", Reason: "has an invalid format"}
	}
	at, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil {
		return time.Time{}, "", &domain.ValidationError{Field: "cursor", Reason: "has an invalid timestamp"}
	}
	runID, err := domain.ParseRunID(parts[1])
	if err != nil {
		return time.Time{}, "", &domain.ValidationError{Field: "cursor", Reason: "has an invalid run id"}
	}
	return at.UTC(), runID, nil
}

// UpdateRun applies an optimistic, from-status-checked run transition.
func (s *Store) UpdateRun(ctx context.Context, update domain.RunUpdate) (domain.Run, error) {
	if err := update.Validate(); err != nil {
		return domain.Run{}, err
	}
	if update.FailureClass != "" && !update.FailureClass.Valid() {
		return domain.Run{}, &domain.ValidationError{Field: "failure_class", Reason: "unknown error class"}
	}
	if update.Event != nil {
		if err := validateEventList([]domain.OutboxEvent{*update.Event}); err != nil {
			return domain.Run{}, err
		}
	}
	at := normalizeTime(update.At)

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.writeErrLocked(); err != nil {
		return domain.Run{}, err
	}
	state, err := s.runStateLocked(update.TenantID, update.RunID)
	if err != nil {
		return domain.Run{}, err
	}
	if state.run.StateVersion != update.ExpectedVersion {
		return domain.Run{}, &domain.VersionConflictError{
			Resource: "run " + update.RunID.String(),
			Expected: update.ExpectedVersion,
			Actual:   state.run.StateVersion,
		}
	}
	if state.run.Status != update.From {
		return domain.Run{}, &domain.StateTransitionError{
			Resource: "run " + update.RunID.String(),
			From:     string(state.run.Status),
			To:       string(update.To),
			Reason:   "expected from-status " + string(update.From),
		}
	}

	updated := cloneRun(state.run)
	if err := updated.Transition(update.To, at); err != nil {
		return domain.Run{}, err
	}
	if update.Verdict != "" {
		if err := updated.SetVerdict(update.Verdict); err != nil {
			return domain.Run{}, err
		}
	}
	if update.FailureClass != "" {
		updated.FailureClass = update.FailureClass
	}
	if update.FailureMessage != "" {
		updated.FailureMessage = update.FailureMessage
	}
	var events []domain.OutboxEvent
	if update.Event != nil {
		if err := s.ensureEventsAbsentLocked([]domain.OutboxEvent{*update.Event}); err != nil {
			return domain.Run{}, err
		}
		events = append(events, *update.Event)
	}

	state.run = updated
	s.appendEventsLocked(events)
	return cloneRun(updated), nil
}

// FreezeRunInputs pins snapshot and policy inputs before planning.
func (s *Store) FreezeRunInputs(ctx context.Context, freeze domain.RunFreeze) (domain.Run, error) {
	if err := freeze.TenantID.Validate(); err != nil {
		return domain.Run{}, err
	}
	if err := freeze.RunID.Validate(); err != nil {
		return domain.Run{}, err
	}
	at := normalizeTime(freeze.At)

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.writeErrLocked(); err != nil {
		return domain.Run{}, err
	}
	state, err := s.runStateLocked(freeze.TenantID, freeze.RunID)
	if err != nil {
		return domain.Run{}, err
	}
	if state.run.StateVersion != freeze.ExpectedVersion {
		return domain.Run{}, &domain.VersionConflictError{
			Resource: "run " + freeze.RunID.String(),
			Expected: freeze.ExpectedVersion,
			Actual:   state.run.StateVersion,
		}
	}
	if state.run.Status != domain.RunCreated && state.run.Status != domain.RunValidating {
		return domain.Run{}, &domain.StateTransitionError{
			Resource: "run " + freeze.RunID.String(),
			From:     string(state.run.Status),
			To:       "frozen",
			Reason:   "inputs can only be frozen before planning",
		}
	}

	updated := cloneRun(state.run)
	if err := updated.FreezeInputs(freeze.SnapshotDigest, freeze.BaseSnapshot, freeze.HeadSnapshot, updated.PolicyDigest, at); err != nil {
		return domain.Run{}, err
	}
	updated.StateVersion++
	updated.UpdatedAt = at
	state.run = updated
	return cloneRun(updated), nil
}

// PlanRun atomically installs a validated DAG and marks the run planned.
func (s *Store) PlanRun(ctx context.Context, tx domain.PlanTransaction) (domain.Run, error) {
	if err := tx.TenantID.Validate(); err != nil {
		return domain.Run{}, err
	}
	if err := tx.RunID.Validate(); err != nil {
		return domain.Run{}, err
	}
	at := normalizeTime(tx.At)

	dag := domain.DAG{
		RunID:        tx.RunID,
		Steps:        tx.Steps,
		Dependencies: tx.Dependencies,
		Claims:       tx.Claims,
	}
	if err := dag.Validate(); err != nil {
		return domain.Run{}, err
	}
	for i := range tx.Steps {
		if tx.Steps[i].TenantID != tx.TenantID {
			return domain.Run{}, &domain.ValidationError{Field: "dag.steps", Reason: fmt.Sprintf("step %s belongs to another tenant", tx.Steps[i].ID)}
		}
	}
	if err := validateEventList(tx.Events); err != nil {
		return domain.Run{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.writeErrLocked(); err != nil {
		return domain.Run{}, err
	}
	state, err := s.runStateLocked(tx.TenantID, tx.RunID)
	if err != nil {
		return domain.Run{}, err
	}
	if state.run.StateVersion != tx.ExpectedVersion {
		return domain.Run{}, &domain.VersionConflictError{
			Resource: "run " + tx.RunID.String(),
			Expected: tx.ExpectedVersion,
			Actual:   state.run.StateVersion,
		}
	}
	if state.run.Status != domain.RunValidating && state.run.Status != domain.RunPlanned {
		return domain.Run{}, &domain.StateTransitionError{
			Resource: "run " + tx.RunID.String(),
			From:     string(state.run.Status),
			To:       string(domain.RunPlanned),
		}
	}
	if err := s.ensureEventsAbsentLocked(tx.Events); err != nil {
		return domain.Run{}, err
	}

	replacing := make(map[domain.StepID]struct{}, len(state.stepIDs))
	if state.run.Status == domain.RunPlanned {
		for _, id := range state.stepIDs {
			replacing[id] = struct{}{}
		}
	}
	for i := range tx.Steps {
		if _, ok := replacing[tx.Steps[i].ID]; ok {
			continue
		}
		if _, exists := s.steps[tx.Steps[i].ID]; exists {
			return domain.Run{}, &domain.ConflictError{Resource: "step", ID: tx.Steps[i].ID.String(), Reason: "already exists"}
		}
	}

	newSteps := make([]domain.Step, len(tx.Steps))
	for i := range tx.Steps {
		step := cloneStep(tx.Steps[i])
		if step.CreatedAt.IsZero() {
			step.CreatedAt = at
		} else {
			step.CreatedAt = step.CreatedAt.UTC()
		}
		if step.UpdatedAt.IsZero() {
			step.UpdatedAt = step.CreatedAt
		} else {
			step.UpdatedAt = step.UpdatedAt.UTC()
		}
		newSteps[i] = step
	}
	newClaims := cloneClaims(tx.Claims)
	for i := range newClaims {
		if newClaims[i].CreatedAt.IsZero() {
			newClaims[i].CreatedAt = at
		} else {
			newClaims[i].CreatedAt = newClaims[i].CreatedAt.UTC()
		}
	}
	newStepIDs := make([]domain.StepID, len(newSteps))
	for i := range newSteps {
		newStepIDs[i] = newSteps[i].ID
	}
	newDeps := make(map[domain.StepID][]domain.StepID, len(tx.Dependencies))
	for id, deps := range tx.Dependencies {
		newDeps[id] = append([]domain.StepID(nil), deps...)
	}

	updated := cloneRun(state.run)
	if updated.Status == domain.RunValidating {
		if err := updated.Transition(domain.RunPlanned, at); err != nil {
			return domain.Run{}, err
		}
	} else {
		updated.PlannedAt = timePtr(at)
		updated.StateVersion++
		updated.UpdatedAt = at
	}

	for _, id := range state.stepIDs {
		delete(s.steps, id)
		if attemptIDs, ok := s.stepAttempts[id]; ok {
			for _, attemptID := range attemptIDs {
				delete(s.attempts, attemptID)
			}
			delete(s.stepAttempts, id)
		}
		delete(s.metrics, id)
	}
	state.stepIDs = newStepIDs
	state.deps = newDeps
	state.claims = newClaims
	for i := range newSteps {
		step := newSteps[i]
		s.steps[step.ID] = &step
	}
	state.run = updated
	s.appendEventsLocked(tx.Events)
	return cloneRun(updated), nil
}

// GetDAG returns the planned steps, dependency map and claims for a run.
func (s *Store) GetDAG(ctx context.Context, tenantID domain.TenantID, runID domain.RunID) (domain.DAG, error) {
	if err := tenantID.Validate(); err != nil {
		return domain.DAG{}, err
	}
	if err := runID.Validate(); err != nil {
		return domain.DAG{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.readErrLocked(); err != nil {
		return domain.DAG{}, err
	}
	state, err := s.runStateLocked(tenantID, runID)
	if err != nil {
		return domain.DAG{}, err
	}
	dag := domain.DAG{
		RunID:  runID,
		Steps:  s.runStepsLocked(state),
		Claims: cloneClaims(state.claims),
	}
	if state.deps != nil {
		dag.Dependencies = make(map[domain.StepID][]domain.StepID, len(state.deps))
		for id, deps := range state.deps {
			dag.Dependencies[id] = append([]domain.StepID(nil), deps...)
		}
	}
	return dag, nil
}

func timePtr(at time.Time) *time.Time {
	return &at
}
