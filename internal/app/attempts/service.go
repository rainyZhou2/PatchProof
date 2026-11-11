package attempts

import (
	"context"
	"fmt"
	"time"

	"github.com/patchproof/patchproof/internal/actioncache"
	"github.com/patchproof/patchproof/internal/domain"
	"github.com/patchproof/patchproof/internal/ports"
)

type Service struct {
	store        ports.MetadataStore
	clock        ports.Clock
	ttl          time.Duration
	cacheEnabled bool
	cacheTTL     time.Duration
	cacheEpoch   string
	workerImage  string
}

func New(store ports.MetadataStore, clock ports.Clock, ttl time.Duration) (*Service, error) {
	if store == nil {
		return nil, fmt.Errorf("attempt service: metadata store is required")
	}
	if clock == nil {
		clock = systemClock{}
	}
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	return &Service{store: store, clock: clock, ttl: ttl}, nil
}

// ConfigureActionCache enables result reuse for cacheable steps completed
// through the worker callback API.
func (s *Service) ConfigureActionCache(enabled bool, ttl time.Duration, epoch, workerImage string) {
	s.cacheEnabled = enabled
	s.cacheTTL = ttl
	s.cacheEpoch = epoch
	s.workerImage = workerImage
}

func (s *Service) Start(ctx context.Context, ref domain.AttemptRef) error {
	err := s.store.StartAttempt(ctx, ref, s.clock.Now())
	if err == nil {
		return nil
	}
	attempts, listErr := s.store.ListAttempts(ctx, ref.TenantID, ref.StepID)
	if listErr == nil {
		for _, attempt := range attempts {
			if attempt.ID == ref.AttemptID && (attempt.Status == domain.AttemptRunning || attempt.Status.Terminal()) {
				return nil
			}
		}
	}
	return err
}

func (s *Service) Heartbeat(ctx context.Context, ref domain.AttemptRef) error {
	return s.store.RenewLease(ctx, domain.LeaseRenewal{Ref: ref, Extension: s.ttl, Now: s.clock.Now()})
}

func (s *Service) Complete(ctx context.Context, ref domain.AttemptRef, result domain.StepResult) (domain.CommitOutcome, error) {
	if result.Outcome == "" {
		return domain.CommitOutcome{}, &domain.ValidationError{Field: "result.outcome", Reason: "must not be empty"}
	}
	for _, evidence := range result.Evidence {
		if evidence.RunID != "" && evidence.RunID != ref.RunID {
			return domain.CommitOutcome{}, &domain.ValidationError{Field: "evidence.run_id", Reason: "does not match the attempt"}
		}
		if evidence.TenantID != "" && evidence.TenantID != ref.TenantID {
			return domain.CommitOutcome{}, &domain.ValidationError{Field: "evidence.tenant_id", Reason: "does not match the attempt"}
		}
		if evidence.StepID != "" && evidence.StepID != ref.StepID {
			return domain.CommitOutcome{}, &domain.ValidationError{Field: "evidence.step_id", Reason: "does not match the attempt"}
		}
	}
	for _, counterexample := range result.Counterexamples {
		if counterexample.RunID != "" && counterexample.RunID != ref.RunID {
			return domain.CommitOutcome{}, &domain.ValidationError{Field: "counterexample.run_id", Reason: "does not match the attempt"}
		}
		if counterexample.TenantID != "" && counterexample.TenantID != ref.TenantID {
			return domain.CommitOutcome{}, &domain.ValidationError{Field: "counterexample.tenant_id", Reason: "does not match the attempt"}
		}
	}
	outcome, err := s.store.CommitAttempt(ctx, domain.AttemptCommit{Ref: ref, Result: result, At: s.clock.Now()})
	if err != nil {
		return domain.CommitOutcome{}, err
	}
	s.storeActionCache(ctx, outcome.Step, result)
	return outcome, nil
}

func (s *Service) storeActionCache(ctx context.Context, step domain.Step, result domain.StepResult) {
	if !s.cacheEnabled || !actioncache.Cacheable(step) || !actioncache.Storable(result) {
		return
	}
	run, err := s.store.GetRun(ctx, step.TenantID, step.RunID)
	if err != nil {
		return
	}
	key, err := actioncache.KeyForStep(step, run, s.cacheEpoch, s.workerImage)
	if err != nil {
		return
	}
	engineVersion := s.cacheEpoch
	for _, item := range result.Evidence {
		if item.Engine.Version != "" {
			engineVersion = item.Engine.Version
			break
		}
	}
	entry, err := actioncache.NewEntry(step.TenantID, key, step, engineVersion, result, s.cacheTTL, s.clock.Now())
	if err != nil {
		return
	}
	_ = s.store.PutActionCache(ctx, entry)
}

func (s *Service) Fail(ctx context.Context, ref domain.AttemptRef, class domain.ErrorClass, message string, retryable bool) (domain.CommitOutcome, error) {
	if class == "" {
		class = domain.ErrExecutorError
	}
	if !class.Valid() {
		return domain.CommitOutcome{}, &domain.ValidationError{Field: "error_class", Reason: "unknown error class"}
	}
	return s.store.FailAttempt(ctx, domain.AttemptFailure{
		Ref: ref, ErrorClass: class, Message: message, Retryable: retryable, At: s.clock.Now(),
	})
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now().UTC() }
