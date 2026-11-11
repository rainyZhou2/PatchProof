package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/patchproof/patchproof/internal/domain"
)

type stepEventPayload struct {
	StepID     string `json:"step_id"`
	RunID      string `json:"run_id"`
	AttemptID  string `json:"attempt_id,omitempty"`
	Generation int64  `json:"generation"`
	Status     string `json:"status,omitempty"`
}

func stepEvent(eventType domain.EventType, step domain.Step, attemptID domain.AttemptID, at time.Time) (domain.OutboxEvent, error) {
	payload := stepEventPayload{
		StepID:     step.ID.String(),
		RunID:      step.RunID.String(),
		AttemptID:  attemptID.String(),
		Generation: step.Generation,
		Status:     string(step.Status),
	}
	event, err := domain.NewOutboxEvent(step.TenantID, eventType, step.ID.String(), step.Generation, payload, at)
	if err != nil {
		return domain.OutboxEvent{}, err
	}
	event.RunID = step.RunID
	return event, nil
}

func insertOutbox(ctx context.Context, q querier, events []domain.OutboxEvent) error {
	seen := make(map[domain.EventID]struct{}, len(events))
	for _, event := range events {
		if err := event.Validate(); err != nil {
			return err
		}
		if _, duplicate := seen[event.ID]; duplicate {
			return &domain.ConflictError{Resource: "outbox_event", ID: string(event.ID), Reason: "duplicate event id in batch"}
		}
		seen[event.ID] = struct{}{}
		if _, err := uuid.Parse(string(event.ID)); err != nil {
			return &domain.ValidationError{Field: "event_id", Reason: "must be a UUID"}
		}
		if event.RunID != "" {
			if _, err := uuid.Parse(event.RunID.String()); err != nil {
				return &domain.ValidationError{Field: "run_id", Reason: "must be a UUID"}
			}
		}
		var exists bool
		if err := q.QueryRow(ctx,
			"SELECT EXISTS (SELECT 1 FROM outbox_events WHERE event_id = $1::uuid)",
			string(event.ID)).Scan(&exists); err != nil {
			return mapError(err, "outbox_event", string(event.ID))
		}
		if exists {
			return &domain.ConflictError{Resource: "outbox_event", ID: string(event.ID), Reason: "already exists"}
		}
		occurredAt := normalizeTime(event.OccurredAt)
		var deliveredAt any
		if event.DeliveredAt != nil {
			deliveredAt = event.DeliveredAt.UTC()
		}
		if _, err := q.Exec(ctx, `INSERT INTO outbox_events (
			event_id, tenant_id, event_type, aggregate_id, run_id, generation, traceparent,
			payload, created_at, delivered_at, delivery_attempts
		) VALUES ($1::uuid, $2::uuid, $3, $4, NULLIF($5, '')::uuid, $6, $7, $8, $9, $10, $11)`,
			string(event.ID), event.TenantID.String(), string(event.Type), event.AggregateID,
			event.RunID.String(), event.Generation, event.TraceParent, []byte(event.Payload),
			occurredAt, deliveredAt, event.Attempts,
		); err != nil {
			return mapError(err, "outbox_event", string(event.ID))
		}
	}
	return nil
}

// AppendOutbox inserts outbox events.
func (s *Store) AppendOutbox(ctx context.Context, events []domain.OutboxEvent) error {
	if len(events) == 0 {
		return nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin append outbox: %w", err)
	}
	if err := insertOutbox(ctx, tx, events); err != nil {
		_ = tx.Rollback(ctx)
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit append outbox: %w", err)
	}
	return nil
}

// PendingOutbox returns undelivered events oldest first.
func (s *Store) PendingOutbox(ctx context.Context, limit int) ([]domain.OutboxEvent, error) {
	limit = clampLimit(limit, 100, 500)
	rows, err := s.pool.Query(ctx, "SELECT "+outboxColumns+` FROM outbox_events
		WHERE delivered_at IS NULL AND dead_lettered_at IS NULL
		ORDER BY created_at ASC, event_id ASC
		LIMIT $1`, limit)
	if err != nil {
		return nil, mapError(err, "outbox_event", "")
	}
	defer rows.Close()
	events := make([]domain.OutboxEvent, 0, limit)
	for rows.Next() {
		event, err := scanOutboxEvent(rows)
		if err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

// MarkOutboxDeadLettered stops retrying events permanently.
func (s *Store) MarkOutboxDeadLettered(ctx context.Context, ids []domain.EventID, at time.Time) error {
	if len(ids) == 0 {
		return nil
	}
	parsed := make([]uuid.UUID, 0, len(ids))
	for _, id := range ids {
		value, err := uuid.Parse(string(id))
		if err != nil {
			return &domain.ValidationError{Field: "event_id", Reason: "must be a UUID"}
		}
		parsed = append(parsed, value)
	}
	if at.IsZero() {
		at = time.Now().UTC()
	}
	if _, err := s.pool.Exec(ctx, `UPDATE outbox_events SET dead_lettered_at = $2
		WHERE event_id = ANY($1::uuid[]) AND delivered_at IS NULL AND dead_lettered_at IS NULL`,
		parsed, at.UTC()); err != nil {
		return mapError(err, "outbox_event", "")
	}
	return nil
}

// MarkOutboxDelivered marks events as delivered at the supplied time.
func (s *Store) MarkOutboxDelivered(ctx context.Context, ids []domain.EventID, at time.Time) error {
	if len(ids) == 0 {
		return nil
	}
	parsed := make([]uuid.UUID, 0, len(ids))
	for _, id := range ids {
		value, err := uuid.Parse(string(id))
		if err != nil {
			return &domain.ValidationError{Field: "event_id", Reason: "must be a UUID"}
		}
		parsed = append(parsed, value)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE outbox_events SET delivered_at = $2
		WHERE event_id = ANY($1::uuid[]) AND delivered_at IS NULL`, parsed, normalizeTime(at)); err != nil {
		return mapError(err, "outbox_event", "")
	}
	return nil
}

// RecordOutboxFailure records a failed delivery attempt for an event.
func (s *Store) RecordOutboxFailure(ctx context.Context, failure domain.OutboxDeliveryFailure) error {
	if failure.EventID == "" {
		return &domain.ValidationError{Field: "event_id", Reason: "must not be empty"}
	}
	if _, err := uuid.Parse(string(failure.EventID)); err != nil {
		return &domain.ValidationError{Field: "event_id", Reason: "must be a UUID"}
	}
	if err := domain.RequireNonEmpty("reason", failure.Reason); err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx, `UPDATE outbox_events
		SET delivery_attempts = delivery_attempts + 1, last_error = $2
		WHERE event_id = $1::uuid`, string(failure.EventID), failure.Reason)
	if err != nil {
		return mapError(err, "outbox_event", string(failure.EventID))
	}
	if tag.RowsAffected() == 0 {
		return notFound("outbox_event", string(failure.EventID))
	}
	return nil
}
