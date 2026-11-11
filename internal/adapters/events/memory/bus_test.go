package memory

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/patchproof/patchproof/internal/domain"
)

func testEvent(t *testing.T, eventType domain.EventType, payload any) domain.OutboxEvent {
	t.Helper()
	event, err := domain.NewOutboxEvent(domain.NewTenantID(), eventType, "aggregate-1", 1, payload, time.Now())
	if err != nil {
		t.Fatalf("new event: %v", err)
	}
	return event
}

func waitForSubscribers(t *testing.T, bus *Bus, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if bus.Subscribers() == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("subscriber count = %d, want %d", bus.Subscribers(), want)
}

func stopConsumer(t *testing.T, cancel context.CancelFunc, errCh <-chan error) {
	t.Helper()
	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("consume returned %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("consumer did not stop")
	}
}

func TestPublishConsume(t *testing.T) {
	bus := New()
	received := make(chan domain.EventEnvelope, 1)
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- bus.Consume(ctx, "test", []string{"patchproof.>"}, func(_ context.Context, envelope domain.EventEnvelope) error {
			received <- envelope
			return nil
		})
	}()
	waitForSubscribers(t, bus, 1)

	event := testEvent(t, domain.EventRunCreated, map[string]string{"run": "r1"})
	if err := bus.Publish(context.Background(), []domain.OutboxEvent{event}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	select {
	case envelope := <-received:
		if envelope.ID != event.ID || envelope.Type != event.Type || envelope.TenantID != event.TenantID {
			t.Fatalf("unexpected envelope: %+v", envelope)
		}
		var payload map[string]string
		if err := json.Unmarshal(envelope.Payload, &payload); err != nil {
			t.Fatalf("decode payload: %v", err)
		}
		if payload["run"] != "r1" {
			t.Fatalf("payload = %+v", payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("event was not delivered")
	}
	stopConsumer(t, cancel, errCh)
}

func TestPublishWildcardFilters(t *testing.T) {
	bus := New()
	received := make(chan domain.EventEnvelope, 4)
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- bus.Consume(ctx, "step", []string{"patchproof.step.>"}, func(_ context.Context, envelope domain.EventEnvelope) error {
			received <- envelope
			return nil
		})
	}()
	waitForSubscribers(t, bus, 1)

	if err := bus.Publish(context.Background(), []domain.OutboxEvent{testEvent(t, domain.EventRunCreated, map[string]int{"n": 1})}); err != nil {
		t.Fatalf("publish run: %v", err)
	}
	select {
	case envelope := <-received:
		t.Fatalf("unexpected delivery for %s", envelope.Type)
	case <-time.After(50 * time.Millisecond):
	}

	stepEvent := testEvent(t, domain.EventStepReady, map[string]int{"n": 2})
	if err := bus.Publish(context.Background(), []domain.OutboxEvent{stepEvent}); err != nil {
		t.Fatalf("publish step: %v", err)
	}
	select {
	case envelope := <-received:
		if envelope.Type != domain.EventStepReady {
			t.Fatalf("delivered %s, want step ready", envelope.Type)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("step event was not delivered")
	}
	stopConsumer(t, cancel, errCh)
}

func TestSubjectMatching(t *testing.T) {
	cases := []struct {
		pattern string
		subject string
		want    bool
	}{
		{"patchproof.>", "patchproof.run.created.v1", true},
		{"patchproof.>", "patchproof", false},
		{"patchproof.*.created.v1", "patchproof.run.created.v1", true},
		{"patchproof.*.created.v1", "patchproof.step.created.v1", true},
		{"patchproof.*.created.v1", "patchproof.run.ready.v1", false},
		{"patchproof.run.created.v1", "patchproof.run.created.v1", true},
		{"patchproof.run.created.v1", "patchproof.run.created.v2", false},
		{"*", "patchproof", true},
		{"patchproof.run.>", "patchproof.run", false},
	}
	for _, tc := range cases {
		if got := matchesSubject(tc.pattern, tc.subject); got != tc.want {
			t.Fatalf("matchesSubject(%q, %q) = %v, want %v", tc.pattern, tc.subject, got, tc.want)
		}
	}
	for _, pattern := range []string{"", "patchproof..x", "patchproof.>.x", "patchproof.foo*", "patchproof.>"} {
		valid := validSubjectPattern(pattern)
		if pattern == "patchproof.>" && !valid {
			t.Fatalf("expected %q to be valid", pattern)
		}
		if pattern != "patchproof.>" && valid {
			t.Fatalf("expected %q to be invalid", pattern)
		}
	}
}

func TestHandlerErrorThenSuccess(t *testing.T) {
	bus := New()
	var attempts atomic.Int32
	success := make(chan struct{})
	var once sync.Once
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- bus.Consume(ctx, "retry", []string{"patchproof.>"}, func(_ context.Context, _ domain.EventEnvelope) error {
			if attempts.Add(1) < 3 {
				return errors.New("transient failure")
			}
			once.Do(func() { close(success) })
			return nil
		})
	}()
	waitForSubscribers(t, bus, 1)

	if err := bus.Publish(context.Background(), []domain.OutboxEvent{testEvent(t, domain.EventRunCreated, map[string]int{"n": 1})}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	select {
	case <-success:
	case <-time.After(2 * time.Second):
		t.Fatalf("handler did not succeed, attempts = %d", attempts.Load())
	}
	if attempts.Load() < 3 {
		t.Fatalf("attempts = %d, want at least 3", attempts.Load())
	}
	stopConsumer(t, cancel, errCh)
}

func TestHandlerFailureHonoursCancellation(t *testing.T) {
	bus := New()
	var attempts atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- bus.Consume(ctx, "failing", []string{"patchproof.>"}, func(context.Context, domain.EventEnvelope) error {
			attempts.Add(1)
			return errors.New("always failing")
		})
	}()
	waitForSubscribers(t, bus, 1)
	if err := bus.Publish(context.Background(), []domain.OutboxEvent{testEvent(t, domain.EventRunCreated, map[string]int{"n": 1})}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for attempts.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	stopConsumer(t, cancel, errCh)
}

func TestMultipleConsumers(t *testing.T) {
	bus := New()
	const consumers = 3
	var received atomic.Int32
	done := make(chan struct{}, consumers)
	ctx, cancel := context.WithCancel(context.Background())
	errs := make(chan error, consumers)
	for i := 0; i < consumers; i++ {
		go func() {
			errs <- bus.Consume(ctx, "fanout", []string{"patchproof.>"}, func(context.Context, domain.EventEnvelope) error {
				received.Add(1)
				done <- struct{}{}
				return nil
			})
		}()
	}
	waitForSubscribers(t, bus, consumers)
	if err := bus.Publish(context.Background(), []domain.OutboxEvent{testEvent(t, domain.EventRunCreated, map[string]int{"n": 1})}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	for i := 0; i < consumers; i++ {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("only %d consumers received the event", received.Load())
		}
	}
	cancel()
	for i := 0; i < consumers; i++ {
		select {
		case err := <-errs:
			if err != nil {
				t.Fatalf("consume returned %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("consumer did not stop")
		}
	}
}

func TestPublishCopiesEvents(t *testing.T) {
	bus := New()
	received := make(chan domain.EventEnvelope, 1)
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- bus.Consume(ctx, "copy", []string{"patchproof.>"}, func(_ context.Context, envelope domain.EventEnvelope) error {
			received <- envelope
			return nil
		})
	}()
	waitForSubscribers(t, bus, 1)

	event := domain.OutboxEvent{
		ID:          domain.NewEventID(),
		TenantID:    domain.NewTenantID(),
		Type:        domain.EventRunCreated,
		AggregateID: "aggregate-1",
		Generation:  1,
		OccurredAt:  time.Now().UTC(),
		Payload:     json.RawMessage(`{"value":"original"}`),
	}
	if err := event.Validate(); err != nil {
		t.Fatalf("event: %v", err)
	}
	if err := bus.Publish(context.Background(), []domain.OutboxEvent{event}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	event.Payload[0] = 'X'
	select {
	case envelope := <-received:
		if string(envelope.Payload) != `{"value":"original"}` {
			t.Fatalf("payload = %q, want original", envelope.Payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("event was not delivered")
	}
	stopConsumer(t, cancel, errCh)
}

func TestCloseUnblocksConsume(t *testing.T) {
	bus := New()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		errCh <- bus.Consume(ctx, "close", []string{"patchproof.>"}, func(context.Context, domain.EventEnvelope) error {
			return nil
		})
	}()
	waitForSubscribers(t, bus, 1)
	if err := bus.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	select {
	case err := <-errCh:
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("consume error = %v, want ErrClosed", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("consume did not return after close")
	}
	if err := bus.Publish(context.Background(), []domain.OutboxEvent{testEvent(t, domain.EventRunCreated, map[string]int{"n": 1})}); !errors.Is(err, ErrClosed) {
		t.Fatalf("publish after close error = %v, want ErrClosed", err)
	}
	if err := bus.Health(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("health after close error = %v, want ErrClosed", err)
	}
	if err := bus.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
}

func TestInvalidOperations(t *testing.T) {
	bus := New()
	if err := bus.Consume(context.Background(), "test", []string{"patchproof.>"}, nil); err == nil {
		t.Fatal("expected nil handler to fail")
	}
	if err := bus.Consume(context.Background(), "", []string{"patchproof.>"}, func(context.Context, domain.EventEnvelope) error { return nil }); err == nil {
		t.Fatal("expected empty consumer to fail")
	}
	if err := bus.Consume(context.Background(), "test", nil, func(context.Context, domain.EventEnvelope) error { return nil }); err == nil {
		t.Fatal("expected empty subject list to fail")
	}
	if err := bus.Consume(context.Background(), "test", []string{"patchproof..bad"}, func(context.Context, domain.EventEnvelope) error { return nil }); err == nil {
		t.Fatal("expected invalid subject to fail")
	}
	invalid := domain.OutboxEvent{Type: domain.EventRunCreated}
	if err := bus.Publish(context.Background(), []domain.OutboxEvent{invalid}); err == nil {
		t.Fatal("expected invalid event to fail")
	}
	if err := bus.Health(context.Background()); err != nil {
		t.Fatalf("health: %v", err)
	}
}
