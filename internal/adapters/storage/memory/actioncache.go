package memory

import (
	"context"
	"time"

	"github.com/patchproof/patchproof/internal/domain"
)

type actionCacheKey struct {
	tenantID domain.TenantID
	key      string
}

func (s *Store) GetActionCache(ctx context.Context, tenantID domain.TenantID, actionKey string, now time.Time) (domain.ActionCacheEntry, error) {
	if err := ctx.Err(); err != nil {
		return domain.ActionCacheEntry{}, err
	}
	if err := tenantID.Validate(); err != nil {
		return domain.ActionCacheEntry{}, err
	}
	if actionKey == "" {
		return domain.ActionCacheEntry{}, &domain.ValidationError{Field: "action_key", Reason: "must not be empty"}
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.writeErrLocked(); err != nil {
		return domain.ActionCacheEntry{}, err
	}
	entry, ok := s.actionCache[actionCacheKey{tenantID: tenantID, key: actionKey}]
	if !ok {
		return domain.ActionCacheEntry{}, &domain.NotFoundError{Resource: "action_cache", ID: actionKey}
	}
	if !entry.ExpiresAt.After(now.UTC()) {
		delete(s.actionCache, actionCacheKey{tenantID: tenantID, key: actionKey})
		return domain.ActionCacheEntry{}, &domain.NotFoundError{Resource: "action_cache", ID: actionKey}
	}
	return cloneActionCacheEntry(entry), nil
}

func (s *Store) PutActionCache(ctx context.Context, entry domain.ActionCacheEntry) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := entry.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.writeErrLocked(); err != nil {
		return err
	}
	s.actionCache[actionCacheKey{tenantID: entry.TenantID, key: entry.ActionKey}] = cloneActionCacheEntry(entry)
	return nil
}

func (s *Store) PurgeActionCache(ctx context.Context, before time.Time, limit int) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if limit <= 0 {
		limit = 500
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.writeErrLocked(); err != nil {
		return 0, err
	}
	removed := 0
	for key, entry := range s.actionCache {
		if removed >= limit {
			break
		}
		if !entry.ExpiresAt.After(before.UTC()) {
			delete(s.actionCache, key)
			removed++
		}
	}
	return removed, nil
}

func cloneActionCacheEntry(entry domain.ActionCacheEntry) domain.ActionCacheEntry {
	cloned := entry
	cloned.Result = cloneStepResult(entry.Result)
	return cloned
}

func cloneStepResult(result domain.StepResult) domain.StepResult {
	cloned := result
	cloned.Artifacts = cloneCASRefs(result.Artifacts)
	cloned.Outputs = cloneStringMap(result.Outputs)
	cloned.Evidence = make([]domain.Evidence, 0, len(result.Evidence))
	for _, item := range result.Evidence {
		cloned.Evidence = append(cloned.Evidence, cloneEvidence(item))
	}
	cloned.Counterexamples = make([]domain.Counterexample, 0, len(result.Counterexamples))
	for _, item := range result.Counterexamples {
		cloned.Counterexamples = append(cloned.Counterexamples, cloneCounterexample(item))
	}
	return cloned
}
