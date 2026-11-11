package domain

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

type EventType string

const (
	EventRunCreated     EventType = "patchproof.run.created.v1"
	EventRunValidated   EventType = "patchproof.run.validated.v1"
	EventRunPlanned     EventType = "patchproof.run.planned.v1"
	EventRunQueued      EventType = "patchproof.run.queued.v1"
	EventRunStarted     EventType = "patchproof.run.started.v1"
	EventRunAggregating EventType = "patchproof.run.aggregating.v1"
	EventRunCancelling  EventType = "patchproof.run.cancelling.v1"
	EventRunCompleted   EventType = "patchproof.run.completed.v1"
	EventRunCancelled   EventType = "patchproof.run.cancelled.v1"
	EventStepReady      EventType = "patchproof.step.ready.v1"
	EventStepLeased     EventType = "patchproof.step.leased.v1"
	EventStepSucceeded  EventType = "patchproof.step.succeeded.v1"
	EventStepFailed     EventType = "patchproof.step.failed.v1"
	EventStepRetry      EventType = "patchproof.step.retry.v1"
	EventStepExpired    EventType = "patchproof.step.expired.v1"
	EventReportReady    EventType = "patchproof.report.ready.v1"
	EventClusterUpdated EventType = "patchproof.cluster.updated.v1"
)

var eventTypes = []EventType{
	EventRunCreated, EventRunValidated, EventRunPlanned, EventRunQueued, EventRunStarted,
	EventRunAggregating, EventRunCancelling, EventRunCompleted, EventRunCancelled, EventStepReady,
	EventStepLeased, EventStepSucceeded, EventStepFailed, EventStepRetry, EventStepExpired,
	EventReportReady, EventClusterUpdated,
}

func (t EventType) Valid() bool {
	for _, known := range eventTypes {
		if known == t {
			return true
		}
	}
	return false
}

func (t EventType) Subject() string { return string(t) }

func (t EventType) Version() string {
	parts := strings.Split(string(t), ".")
	if len(parts) == 0 {
		return ""
	}
	return parts[len(parts)-1]
}

func EventTypes() []EventType {
	out := make([]EventType, len(eventTypes))
	copy(out, eventTypes)
	return out
}

type OutboxEvent struct {
	ID          EventID         `json:"event_id"`
	TenantID    TenantID        `json:"tenant_id"`
	Type        EventType       `json:"event_type"`
	AggregateID string          `json:"aggregate_id"`
	RunID       RunID           `json:"run_id,omitempty"`
	Generation  int64           `json:"generation"`
	OccurredAt  time.Time       `json:"occurred_at"`
	TraceParent string          `json:"traceparent,omitempty"`
	Payload     json.RawMessage `json:"payload"`
	DeliveredAt *time.Time      `json:"delivered_at,omitempty"`
	Attempts    int             `json:"delivery_attempts"`
}

func (e OutboxEvent) Validate() error {
	if e.ID == "" {
		return &ValidationError{Field: "event_id", Reason: "must not be empty"}
	}
	if err := e.TenantID.Validate(); err != nil {
		return err
	}
	if !e.Type.Valid() {
		return &ValidationError{Field: "event_type", Reason: fmt.Sprintf("unknown event type %q", e.Type)}
	}
	if err := RequireNonEmpty("aggregate_id", e.AggregateID); err != nil {
		return err
	}
	if len(e.Payload) == 0 {
		return &ValidationError{Field: "payload", Reason: "must not be empty"}
	}
	if !json.Valid(e.Payload) {
		return &ValidationError{Field: "payload", Reason: "must be valid JSON"}
	}
	return nil
}

func NewOutboxEvent(tenantID TenantID, eventType EventType, aggregateID string, generation int64, payload any, at time.Time) (OutboxEvent, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return OutboxEvent{}, fmt.Errorf("marshal %s payload: %w", eventType, err)
	}
	event := OutboxEvent{
		ID:          NewEventID(),
		TenantID:    tenantID,
		Type:        eventType,
		AggregateID: aggregateID,
		Generation:  generation,
		OccurredAt:  at.UTC(),
		Payload:     raw,
	}
	return event, event.Validate()
}

type EventEnvelope struct {
	ID          EventID         `json:"event_id"`
	Type        EventType       `json:"event_type"`
	TenantID    TenantID        `json:"tenant_id"`
	AggregateID string          `json:"aggregate_id"`
	RunID       RunID           `json:"run_id,omitempty"`
	Generation  int64           `json:"generation"`
	OccurredAt  time.Time       `json:"occurred_at"`
	TraceParent string          `json:"traceparent,omitempty"`
	Payload     json.RawMessage `json:"payload"`
}

func (e OutboxEvent) Envelope() EventEnvelope {
	return EventEnvelope{
		ID:          e.ID,
		Type:        e.Type,
		TenantID:    e.TenantID,
		AggregateID: e.AggregateID,
		RunID:       e.RunID,
		Generation:  e.Generation,
		OccurredAt:  e.OccurredAt,
		TraceParent: e.TraceParent,
		Payload:     e.Payload,
	}
}

func (e EventEnvelope) Validate() error {
	if e.ID == "" {
		return &ValidationError{Field: "event_id", Reason: "must not be empty"}
	}
	if !e.Type.Valid() {
		return &ValidationError{Field: "event_type", Reason: "unknown event type"}
	}
	return nil
}
