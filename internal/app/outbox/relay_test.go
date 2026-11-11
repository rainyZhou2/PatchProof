package outbox

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/patchproof/patchproof/internal/adapters/events/memory"
	memorystore "github.com/patchproof/patchproof/internal/adapters/storage/memory"
	"github.com/patchproof/patchproof/internal/domain"
	"github.com/patchproof/patchproof/internal/ports"
)

type errorBus struct {
	published int
}

func (b *errorBus) Publish(context.Context, []domain.OutboxEvent) error {
	b.published++
	return errors.New("bus unavailable")
}
func (b *errorBus) Consume(context.Context, string, []string, func(context.Context, domain.EventEnvelope) error) error {
	return nil
}
func (b *errorBus) Health(context.Context) error { return nil }
func (b *errorBus) Close() error                 { return nil }

var _ ports.EventBus = (*errorBus)(nil)

func testEvent(t *testing.T, tenant domain.TenantID) domain.OutboxEvent {
	t.Helper()
	event, err := domain.NewOutboxEvent(tenant, domain.EventRunCreated, "22222222-2222-4222-8222-222222222222", 0, map[string]string{"run_id": "22222222-2222-4222-8222-222222222222"}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	return event
}

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestRelayPublishesAndMarksDelivered(t *testing.T) {
	store := memorystore.New()
	bus := memory.New()
	tenant := domain.TenantID("11111111-1111-4111-8111-111111111111")
	event := testEvent(t, tenant)
	if err := store.AppendOutbox(context.Background(), []domain.OutboxEvent{event}); err != nil {
		t.Fatal(err)
	}
	relay, err := New(store, bus, Config{Interval: time.Hour, Batch: 10}, nil, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	if err := relay.drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	pending, err := store.PendingOutbox(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("expected no pending events, got %d", len(pending))
	}
}

func TestRelayRecordsFailure(t *testing.T) {
	store := memorystore.New()
	bus := &errorBus{}
	tenant := domain.TenantID("11111111-1111-4111-8111-111111111111")
	event := testEvent(t, tenant)
	if err := store.AppendOutbox(context.Background(), []domain.OutboxEvent{event}); err != nil {
		t.Fatal(err)
	}
	relay, err := New(store, bus, Config{Interval: time.Hour, Batch: 10}, nil, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	if err := relay.drain(context.Background()); err == nil {
		t.Fatal("expected a publish error")
	}
	pending, err := store.PendingOutbox(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].Attempts == 0 {
		t.Fatalf("expected one pending event with a recorded failure, got %#v", pending)
	}
	if bus.published != 1 {
		t.Fatalf("expected one publish attempt, got %d", bus.published)
	}
}
