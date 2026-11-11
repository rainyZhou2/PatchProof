package memory

import (
	"context"
	"sort"
	"time"

	"github.com/patchproof/patchproof/internal/domain"
)

// PendingOutbox returns undelivered events ordered by occurrence time.
func (s *Store) PendingOutbox(ctx context.Context, limit int) ([]domain.OutboxEvent, error) {
	limit = normalizeLimit(limit, 100, 500)
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.readErrLocked(); err != nil {
		return nil, err
	}
	pending := make([]domain.OutboxEvent, 0)
	for _, event := range s.outbox {
		if event.DeliveredAt != nil {
			continue
		}
		pending = append(pending, cloneOutboxEvent(*event))
	}
	sort.SliceStable(pending, func(i, j int) bool {
		if !pending[i].OccurredAt.Equal(pending[j].OccurredAt) {
			return pending[i].OccurredAt.Before(pending[j].OccurredAt)
		}
		return pending[i].ID < pending[j].ID
	})
	if len(pending) > limit {
		pending = pending[:limit]
	}
	return pending, nil
}

// MarkOutboxDelivered marks events delivered; already delivered events are kept.
func (s *Store) MarkOutboxDelivered(ctx context.Context, ids []domain.EventID, at time.Time) error {
	if len(ids) == 0 {
		return nil
	}
	at = normalizeTime(at)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.writeErrLocked(); err != nil {
		return err
	}
	for _, id := range ids {
		event, ok := s.outbox[id]
		if !ok || event.DeliveredAt != nil {
			continue
		}
		event.DeliveredAt = timePtr(at)
	}
	return nil
}

// RecordOutboxFailure increments an event's delivery attempts and last error.
func (s *Store) RecordOutboxFailure(ctx context.Context, failure domain.OutboxDeliveryFailure) error {
	if failure.EventID == "" {
		return &domain.ValidationError{Field: "event_id", Reason: "must not be empty"}
	}
	at := normalizeTime(failure.At)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.writeErrLocked(); err != nil {
		return err
	}
	event, ok := s.outbox[failure.EventID]
	if !ok {
		return &domain.NotFoundError{Resource: "outbox event", ID: string(failure.EventID)}
	}
	event.Attempts++
	s.outboxErrors[failure.EventID] = outboxFailure{reason: failure.Reason, at: at}
	return nil
}

// AppendOutbox appends validated events without allowing duplicate IDs.
func (s *Store) AppendOutbox(ctx context.Context, events []domain.OutboxEvent) error {
	if len(events) == 0 {
		return nil
	}
	if err := validateEventList(events); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.writeErrLocked(); err != nil {
		return err
	}
	if err := s.ensureEventsAbsentLocked(events); err != nil {
		return err
	}
	s.appendEventsLocked(events)
	return nil
}
