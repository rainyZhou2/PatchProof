package memory

import (
	"fmt"
	"time"

	"github.com/patchproof/patchproof/internal/domain"
)

type stepEventPayload struct {
	RunID      string `json:"run_id"`
	StepID     string `json:"step_id"`
	AttemptID  string `json:"attempt_id,omitempty"`
	Generation int64  `json:"generation"`
	Status     string `json:"status"`
}

// validateEventList validates a caller-supplied batch and rejects duplicate IDs
// inside the batch. It does not consult store state.
func validateEventList(events []domain.OutboxEvent) error {
	seen := make(map[domain.EventID]struct{}, len(events))
	for i := range events {
		event := events[i]
		if err := event.Validate(); err != nil {
			return fmt.Errorf("outbox event %d: %w", i, err)
		}
		if _, duplicate := seen[event.ID]; duplicate {
			return &domain.ConflictError{Resource: "outbox event", ID: string(event.ID), Reason: "duplicate event id in batch"}
		}
		seen[event.ID] = struct{}{}
	}
	return nil
}

// ensureEventsAbsentLocked rejects batches whose IDs already exist.
func (s *Store) ensureEventsAbsentLocked(events []domain.OutboxEvent) error {
	for i := range events {
		if _, exists := s.outbox[events[i].ID]; exists {
			return &domain.ConflictError{Resource: "outbox event", ID: string(events[i].ID), Reason: "already exists"}
		}
	}
	return nil
}

func (s *Store) appendEventsLocked(events []domain.OutboxEvent) {
	for i := range events {
		event := cloneOutboxEvent(events[i])
		s.outbox[event.ID] = &event
	}
}

// newStepEvent builds a deterministic step lifecycle event. The payload only
// contains stable identifiers, the generation and the resulting status.
func newStepEvent(step domain.Step, attemptID domain.AttemptID, eventType domain.EventType, status domain.StepStatus, at time.Time) (domain.OutboxEvent, error) {
	payload := stepEventPayload{
		RunID:      step.RunID.String(),
		StepID:     step.ID.String(),
		AttemptID:  attemptID.String(),
		Generation: step.Generation,
		Status:     string(status),
	}
	event, err := domain.NewOutboxEvent(step.TenantID, eventType, step.ID.String(), step.Generation, payload, at)
	if err != nil {
		return domain.OutboxEvent{}, wrapInternal("build %s event for step %s: %w", eventType, step.ID, err)
	}
	event.RunID = step.RunID
	return event, nil
}
