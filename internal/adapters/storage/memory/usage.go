package memory

import (
	"context"
	"math"
	"time"

	"github.com/patchproof/patchproof/internal/domain"
)

// CountActiveSteps returns the number of capacity-consuming steps for a tenant.
func (s *Store) CountActiveSteps(ctx context.Context, tenantID domain.TenantID) (int, error) {
	if err := tenantID.Validate(); err != nil {
		return 0, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.readErrLocked(); err != nil {
		return 0, err
	}
	count := 0
	for _, step := range s.steps {
		if step.TenantID != tenantID {
			continue
		}
		// Only leased/running work consumes executor capacity. READY and
		// RETRY_WAIT are queued states; counting them would let a tenant's own
		// backlog consume its concurrency quota and deadlock scheduling.
		switch step.Status {
		case domain.StepLeased, domain.StepRunning:
			count++
		}
	}
	return count, nil
}

// TenantUsage reports active run/step counts and metric sums since a timestamp.
func (s *Store) TenantUsage(ctx context.Context, tenantID domain.TenantID, since time.Time) (domain.TenantUsage, error) {
	if err := tenantID.Validate(); err != nil {
		return domain.TenantUsage{}, err
	}
	since = since.UTC()

	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.readErrLocked(); err != nil {
		return domain.TenantUsage{}, err
	}
	usage := domain.TenantUsage{TenantID: tenantID}
	for _, state := range s.runs {
		if state.run.TenantID == tenantID && state.run.Status.Active() {
			usage.ActiveRuns++
		}
	}
	for _, step := range s.steps {
		if step.TenantID != tenantID {
			continue
		}
		if step.Status.Active() {
			usage.ActiveSteps++
		}
	}
	for _, records := range s.metrics {
		for _, record := range records {
			if record.tenantID != tenantID {
				continue
			}
			if !since.IsZero() && record.at.Before(since) {
				continue
			}
			usage.DailyCPUSeconds += math.Max(record.metrics.CPUSeconds, float64(record.metrics.ExecutionMS)/1000)
			if record.kind == domain.StepKindDifferential {
				usage.DailyFuzzSeconds += math.Max(record.metrics.CPUSeconds, float64(record.metrics.ExecutionMS)/1000)
			}
			usage.CASWrittenBytes += record.metrics.CASWrittenBytes
		}
	}
	return usage, nil
}
