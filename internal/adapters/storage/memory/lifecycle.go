package memory

import "context"

// Ping reports whether the store is open.
func (s *Store) Ping(ctx context.Context) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return errStoreClosed
	}
	return nil
}

// Close releases all in-memory state. It is safe to call more than once.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	s.tenants = nil
	s.projects = nil
	s.policies = nil
	s.runs = nil
	s.runKeys = nil
	s.steps = nil
	s.attempts = nil
	s.stepAttempts = nil
	s.evidence = nil
	s.evidenceIDs = nil
	s.counterexamples = nil
	s.reports = nil
	s.metrics = nil
	s.outbox = nil
	s.outboxErrors = nil
	s.clusters = nil
	s.shardLeases = nil
	s.audit = nil
	s.actionCache = nil
	return nil
}
