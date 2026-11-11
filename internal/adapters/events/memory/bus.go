// Package memory provides an in-process event bus for tests and local
// single-process operation. Events are delivered to every matching consumer;
// handler failures are retried with bounded backoff.
package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/patchproof/patchproof/internal/domain"
	"github.com/patchproof/patchproof/internal/ports"
)

const (
	defaultQueueSize  = 256
	initialRetryDelay = 10 * time.Millisecond
	maxRetryDelay     = time.Second
)

// ErrClosed reports an operation on a closed bus.
var ErrClosed = errors.New("event bus is closed")

// Bus is a concurrency-safe in-process event bus.
type Bus struct {
	mu        sync.Mutex
	closed    bool
	closedCh  chan struct{}
	nextID    uint64
	subs      map[uint64]*subscription
	queueSize int
}

type subscription struct {
	id       uint64
	subjects []string
	handler  func(context.Context, domain.EventEnvelope) error
	queue    chan domain.EventEnvelope
	stop     chan struct{}
	stopOnce sync.Once
	done     chan struct{}
}

var _ ports.EventBus = (*Bus)(nil)

// New creates an empty in-process event bus.
func New() *Bus {
	return &Bus{
		closedCh:  make(chan struct{}),
		subs:      make(map[uint64]*subscription),
		queueSize: defaultQueueSize,
	}
}

// Publish delivers copies of each event to every consumer whose subject filter
// matches. It returns when all copies are queued or the context ends.
func (b *Bus) Publish(ctx context.Context, events []domain.OutboxEvent) error {
	if len(events) == 0 {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	envelopes := make([]domain.EventEnvelope, 0, len(events))
	for index := range events {
		if err := events[index].Validate(); err != nil {
			return fmt.Errorf("memory bus publish: event %d: %w", index, err)
		}
		envelopes = append(envelopes, copyEnvelope(events[index].Envelope()))
	}

	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return ErrClosed
	}
	subscribers := make([]*subscription, 0, len(b.subs))
	for _, sub := range b.subs {
		subscribers = append(subscribers, sub)
	}
	b.mu.Unlock()

	for _, envelope := range envelopes {
		subject := envelope.Type.Subject()
		for _, sub := range subscribers {
			if !matchesAny(subject, sub.subjects) {
				continue
			}
			select {
			case sub.queue <- copyEnvelope(envelope):
			case <-ctx.Done():
				return ctx.Err()
			case <-b.closedCh:
				return ErrClosed
			case <-sub.stop:
			}
		}
	}
	return nil
}

// Consume registers a consumer and blocks until ctx is done or the bus is
// closed. Each event is retried until the handler succeeds or shutdown starts.
func (b *Bus) Consume(ctx context.Context, consumer string, subjects []string, handler func(context.Context, domain.EventEnvelope) error) error {
	if handler == nil {
		return errors.New("memory bus consume: handler must not be nil")
	}
	if strings.TrimSpace(consumer) == "" {
		return errors.New("memory bus consume: consumer must not be empty")
	}
	patterns, err := normalizeSubjects(subjects)
	if err != nil {
		return fmt.Errorf("memory bus consume: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	sub, err := b.subscribe(ctx, patterns, handler)
	if err != nil {
		return err
	}
	defer b.unsubscribe(sub)
	select {
	case <-ctx.Done():
		return nil
	case <-b.closedCh:
		return ErrClosed
	}
}

// Health reports whether the bus accepts operations.
func (b *Bus) Health(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return ErrClosed
	}
	return nil
}

// Close stops all consumers and rejects further operations. It is idempotent.
func (b *Bus) Close() error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil
	}
	b.closed = true
	close(b.closedCh)
	subscribers := make([]*subscription, 0, len(b.subs))
	for id, sub := range b.subs {
		delete(b.subs, id)
		subscribers = append(subscribers, sub)
	}
	b.mu.Unlock()
	for _, sub := range subscribers {
		sub.signalStop()
	}
	return nil
}

// Subscribers returns the number of registered consumers.
func (b *Bus) Subscribers() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subs)
}

func (b *Bus) subscribe(ctx context.Context, patterns []string, handler func(context.Context, domain.EventEnvelope) error) (*subscription, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil, ErrClosed
	}
	id := b.nextID
	b.nextID++
	sub := &subscription{
		id:       id,
		subjects: patterns,
		handler:  handler,
		queue:    make(chan domain.EventEnvelope, b.queueSize),
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
	b.subs[id] = sub
	go sub.run(ctx, b)
	return sub, nil
}

func (b *Bus) unsubscribe(sub *subscription) {
	b.mu.Lock()
	delete(b.subs, sub.id)
	b.mu.Unlock()
	sub.signalStop()
	<-sub.done
}

func (s *subscription) run(ctx context.Context, bus *Bus) {
	defer close(s.done)
	for {
		select {
		case <-ctx.Done():
			return
		case <-bus.closedCh:
			return
		case <-s.stop:
			return
		case envelope := <-s.queue:
			if !s.process(ctx, bus, envelope) {
				return
			}
		}
	}
}

func (s *subscription) process(ctx context.Context, bus *Bus, envelope domain.EventEnvelope) bool {
	delay := initialRetryDelay
	for {
		if err := s.handler(ctx, envelope); err == nil {
			return true
		}
		if ctx.Err() != nil {
			return false
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return false
		case <-bus.closedCh:
			timer.Stop()
			return false
		case <-s.stop:
			timer.Stop()
			return false
		case <-timer.C:
		}
		delay *= 2
		if delay > maxRetryDelay {
			delay = maxRetryDelay
		}
	}
}

func (s *subscription) signalStop() {
	s.stopOnce.Do(func() {
		close(s.stop)
	})
}

func copyEnvelope(envelope domain.EventEnvelope) domain.EventEnvelope {
	envelope.Payload = append(json.RawMessage(nil), envelope.Payload...)
	return envelope
}
