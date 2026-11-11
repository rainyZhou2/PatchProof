package memory

import (
	"context"
	"sort"

	"github.com/patchproof/patchproof/internal/domain"
)

// AppendEvidence appends validated, immutable evidence records.
func (s *Store) AppendEvidence(ctx context.Context, evidence []domain.Evidence) error {
	if len(evidence) == 0 {
		return nil
	}
	for i := range evidence {
		if err := evidence[i].Validate(); err != nil {
			return err
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.writeErrLocked(); err != nil {
		return err
	}
	seen := make(map[domain.EvidenceID]struct{}, len(evidence))
	for i := range evidence {
		item := evidence[i]
		state, ok := s.runs[item.RunID]
		if !ok || state.run.TenantID != item.TenantID {
			return &domain.NotFoundError{Resource: "run", ID: item.RunID.String()}
		}
		if _, duplicate := seen[item.ID]; duplicate {
			return &domain.ConflictError{Resource: "evidence", ID: string(item.ID), Reason: "duplicate evidence id in batch"}
		}
		if _, duplicate := s.evidenceIDs[item.ID]; duplicate {
			return &domain.ConflictError{Resource: "evidence", ID: string(item.ID), Reason: "already exists"}
		}
		seen[item.ID] = struct{}{}
	}
	for i := range evidence {
		item := cloneEvidence(evidence[i])
		s.evidence[item.RunID] = append(s.evidence[item.RunID], item)
		s.evidenceIDs[item.ID] = struct{}{}
	}
	return nil
}

// ListEvidence returns evidence ordered by created_at and evidence id.
func (s *Store) ListEvidence(ctx context.Context, tenantID domain.TenantID, runID domain.RunID) ([]domain.Evidence, error) {
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
		return []domain.Evidence{}, nil
	}
	out := make([]domain.Evidence, 0, len(s.evidence[runID]))
	for _, item := range s.evidence[runID] {
		out = append(out, cloneEvidence(item))
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// ListCounterexamples returns a run's counterexamples ordered by created_at.
func (s *Store) ListCounterexamples(ctx context.Context, tenantID domain.TenantID, runID domain.RunID) ([]domain.Counterexample, error) {
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
		return []domain.Counterexample{}, nil
	}
	out := make([]domain.Counterexample, 0, len(s.counterexamples[runID]))
	for _, item := range s.counterexamples[runID] {
		out = append(out, cloneCounterexample(item))
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].CaseID < out[j].CaseID
	})
	return out, nil
}

// PutReport upserts the report for a run.
func (s *Store) PutReport(ctx context.Context, report domain.Report) error {
	if err := report.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.writeErrLocked(); err != nil {
		return err
	}
	state, ok := s.runs[report.RunID]
	if !ok || state.run.TenantID != report.TenantID {
		return &domain.NotFoundError{Resource: "run", ID: report.RunID.String()}
	}
	s.reports[report.RunID] = cloneReport(report)
	return nil
}

// GetReport returns the report for a run.
func (s *Store) GetReport(ctx context.Context, tenantID domain.TenantID, runID domain.RunID) (domain.Report, error) {
	if err := tenantID.Validate(); err != nil {
		return domain.Report{}, err
	}
	if err := runID.Validate(); err != nil {
		return domain.Report{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.readErrLocked(); err != nil {
		return domain.Report{}, err
	}
	state, ok := s.runs[runID]
	if !ok || state.run.TenantID != tenantID {
		return domain.Report{}, &domain.NotFoundError{Resource: "report", ID: runID.String()}
	}
	report, ok := s.reports[runID]
	if !ok {
		return domain.Report{}, &domain.NotFoundError{Resource: "report", ID: runID.String()}
	}
	return cloneReport(report), nil
}
