// Package memory provides an in-memory implementation of ports.MetadataStore
// for local mode, unit tests and API integration tests.
package memory

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/patchproof/patchproof/internal/domain"
	"github.com/patchproof/patchproof/internal/ports"
)

var errStoreClosed = errors.New("memory metadata store is closed")

// Store is a concurrency-safe in-memory metadata store. A single RWMutex
// protects all state and every returned value is a deep copy.
type Store struct {
	mu     sync.RWMutex
	closed bool

	tenants  map[domain.TenantID]domain.Tenant
	projects map[projectKey]domain.Project
	policies map[policyKey]domain.Policy

	runs    map[domain.RunID]*runState
	runKeys map[runKey]domain.RunID

	steps        map[domain.StepID]*domain.Step
	attempts     map[domain.AttemptID]*domain.Attempt
	stepAttempts map[domain.StepID][]domain.AttemptID

	evidence        map[domain.RunID][]domain.Evidence
	evidenceIDs     map[domain.EvidenceID]struct{}
	counterexamples map[domain.RunID][]domain.Counterexample
	reports         map[domain.RunID]domain.Report
	metrics         map[domain.StepID][]metricRecord

	outbox       map[domain.EventID]*domain.OutboxEvent
	outboxErrors map[domain.EventID]outboxFailure

	clusters    map[domain.ClusterID]domain.Cluster
	shardLeases map[int]domain.ShardLease
	audit       []domain.AuditEntry
	actionCache map[actionCacheKey]domain.ActionCacheEntry
}

type runKey struct {
	tenantID  domain.TenantID
	projectID domain.ProjectID
	request   string
}

type projectKey struct {
	tenantID  domain.TenantID
	projectID domain.ProjectID
}

type policyKey struct {
	tenantID domain.TenantID
	digest   string
}

type runState struct {
	run     domain.Run
	stepIDs []domain.StepID
	deps    map[domain.StepID][]domain.StepID
	claims  []domain.Claim
}

type metricRecord struct {
	tenantID domain.TenantID
	stepID   domain.StepID
	kind     domain.StepKind
	at       time.Time
	metrics  domain.StepMetrics
}

type outboxFailure struct {
	reason string
	at     time.Time
}

var _ ports.MetadataStore = (*Store)(nil)

// New returns an empty, ready-to-use in-memory metadata store.
func New() *Store {
	s := &Store{}
	s.initLocked()
	return s
}

func (s *Store) initLocked() {
	if s.tenants == nil {
		s.tenants = make(map[domain.TenantID]domain.Tenant)
	}
	if s.projects == nil {
		s.projects = make(map[projectKey]domain.Project)
	}
	if s.policies == nil {
		s.policies = make(map[policyKey]domain.Policy)
	}
	if s.runs == nil {
		s.runs = make(map[domain.RunID]*runState)
	}
	if s.runKeys == nil {
		s.runKeys = make(map[runKey]domain.RunID)
	}
	if s.steps == nil {
		s.steps = make(map[domain.StepID]*domain.Step)
	}
	if s.attempts == nil {
		s.attempts = make(map[domain.AttemptID]*domain.Attempt)
	}
	if s.stepAttempts == nil {
		s.stepAttempts = make(map[domain.StepID][]domain.AttemptID)
	}
	if s.evidence == nil {
		s.evidence = make(map[domain.RunID][]domain.Evidence)
	}
	if s.evidenceIDs == nil {
		s.evidenceIDs = make(map[domain.EvidenceID]struct{})
	}
	if s.counterexamples == nil {
		s.counterexamples = make(map[domain.RunID][]domain.Counterexample)
	}
	if s.reports == nil {
		s.reports = make(map[domain.RunID]domain.Report)
	}
	if s.metrics == nil {
		s.metrics = make(map[domain.StepID][]metricRecord)
	}
	if s.outbox == nil {
		s.outbox = make(map[domain.EventID]*domain.OutboxEvent)
	}
	if s.outboxErrors == nil {
		s.outboxErrors = make(map[domain.EventID]outboxFailure)
	}
	if s.clusters == nil {
		s.clusters = make(map[domain.ClusterID]domain.Cluster)
	}
	if s.shardLeases == nil {
		s.shardLeases = make(map[int]domain.ShardLease)
	}
	if s.actionCache == nil {
		s.actionCache = make(map[actionCacheKey]domain.ActionCacheEntry)
	}
}

// readErrLocked reports whether the store can still serve reads.
func (s *Store) readErrLocked() error {
	if s.closed {
		return &domain.NotFoundError{Resource: "memory metadata store", ID: "closed"}
	}
	return nil
}

// writeErrLocked reports whether the store can still accept writes and lazily
// initializes maps so a zero-value Store is also safe to use.
func (s *Store) writeErrLocked() error {
	if s.closed {
		return errStoreClosed
	}
	s.initLocked()
	return nil
}

func (s *Store) runStateLocked(tenantID domain.TenantID, runID domain.RunID) (*runState, error) {
	state, ok := s.runs[runID]
	if !ok || state.run.TenantID != tenantID {
		return nil, &domain.NotFoundError{Resource: "run", ID: runID.String()}
	}
	return state, nil
}

func (s *Store) stepLocked(tenantID domain.TenantID, stepID domain.StepID) (*domain.Step, error) {
	step, ok := s.steps[stepID]
	if !ok || step.TenantID != tenantID {
		return nil, &domain.NotFoundError{Resource: "step", ID: stepID.String()}
	}
	return step, nil
}

func (s *Store) attemptLocked(attemptID domain.AttemptID) (*domain.Attempt, error) {
	attempt, ok := s.attempts[attemptID]
	if !ok {
		return nil, &domain.NotFoundError{Resource: "attempt", ID: attemptID.String()}
	}
	return attempt, nil
}

func (s *Store) runStepsLocked(state *runState) []domain.Step {
	out := make([]domain.Step, 0, len(state.stepIDs))
	for _, id := range state.stepIDs {
		if step, ok := s.steps[id]; ok {
			out = append(out, cloneStep(*step))
		}
	}
	return out
}

func normalizeTime(at time.Time) time.Time {
	if at.IsZero() {
		return time.Now().UTC()
	}
	return at.UTC()
}

func normalizeLimit(limit, fallback, max int) int {
	if limit <= 0 {
		return fallback
	}
	if limit > max {
		return max
	}
	return limit
}

func cloneStrings(values []string) []string {
	if values == nil {
		return nil
	}
	return append([]string(nil), values...)
}

func cloneTime(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	value := *t
	return &value
}

func sortStepsReady(steps []domain.Step) {
	sort.SliceStable(steps, func(i, j int) bool {
		if steps[i].Priority != steps[j].Priority {
			return steps[i].Priority > steps[j].Priority
		}
		if !steps[i].CreatedAt.Equal(steps[j].CreatedAt) {
			return steps[i].CreatedAt.Before(steps[j].CreatedAt)
		}
		return steps[i].ID < steps[j].ID
	})
}

func wrapInternal(format string, args ...any) error {
	return fmt.Errorf("memory metadata store: "+format, args...)
}
