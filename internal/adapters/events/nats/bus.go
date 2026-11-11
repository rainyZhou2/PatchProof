// Package nats provides a NATS JetStream event bus for PatchProof domain
// events. Events are published with server-side deduplication identifiers and
// consumed through durable pull consumers.
package nats

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/patchproof/patchproof/internal/domain"
	"github.com/patchproof/patchproof/internal/ports"
)

const (
	defaultStreamName     = "PATCHPROOF"
	defaultConsumerPrefix = "patchproof"
	defaultMaxAckPending  = 512
	defaultBatchSize      = 64
	defaultAckWait        = 30 * time.Second
	defaultMaxDeliver     = 5
	defaultConnectTimeout = 10 * time.Second
	streamMaxAge          = 30 * 24 * time.Hour
	maxFetchBatch         = 10000
	minRetryDelay         = time.Second
	maxRetryDelay         = 30 * time.Second
	eventSubjectRoot      = "patchproof.>"
	traceParentHeader     = "traceparent"
)

// ErrClosed reports an operation on a closed bus.
var ErrClosed = errors.New("event bus is closed")

// Config configures the NATS JetStream event bus.
type Config struct {
	// URLs lists NATS server URLs tried in order.
	URLs []string
	// Stream is the JetStream stream name; PATCHPROOF when empty.
	Stream string
	// ConsumerPrefix prefixes durable consumer names; patchproof when empty.
	ConsumerPrefix string
	// MaxAckPending bounds unacknowledged messages per consumer.
	MaxAckPending int
	// BatchSize is the publish and fetch batch size.
	BatchSize int
	// AckWait is the acknowledgement timeout for consumed messages.
	AckWait time.Duration
	// MaxDeliver limits redeliveries per message.
	MaxDeliver int
	// CredentialsFile is an optional NATS user credentials file.
	CredentialsFile string
	// TLSCertFile and TLSKeyFile configure mutual TLS when set together.
	TLSCertFile string
	TLSKeyFile  string
	// TLSCAFile is an optional root CA bundle.
	TLSCAFile string
	// ConnectTimeout bounds connection and stream setup.
	ConnectTimeout time.Duration
	// Replicas is the stream replica count, from 1 to 5.
	Replicas int
}

// Bus is a NATS JetStream-backed event bus.
type Bus struct {
	nc             *nats.Conn
	js             jetstream.JetStream
	stream         string
	consumerPrefix string
	maxAckPending  int
	batchSize      int
	ackWait        time.Duration
	maxDeliver     int
	closed         atomic.Bool
	closedCh       chan struct{}
	malformed      atomic.Int64
}

var _ ports.EventBus = (*Bus)(nil)

// New connects to NATS and creates or updates the configured stream.
func New(ctx context.Context, cfg Config) (*Bus, error) {
	normalized, err := normalizeConfig(cfg)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	options := []nats.Option{
		nats.Name("patchproof-events"),
		nats.Timeout(normalized.ConnectTimeout),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(2 * time.Second),
	}
	if normalized.CredentialsFile != "" {
		options = append(options, nats.UserCredentials(normalized.CredentialsFile))
	}
	if normalized.TLSCertFile != "" {
		options = append(options, nats.ClientCert(normalized.TLSCertFile, normalized.TLSKeyFile))
	}
	if normalized.TLSCAFile != "" {
		options = append(options, nats.RootCAs(normalized.TLSCAFile))
	}

	connection, err := nats.Connect(strings.Join(normalized.URLs, ","), options...)
	if err != nil {
		return nil, fmt.Errorf("nats events: connect: %w", err)
	}
	if err := ctx.Err(); err != nil {
		connection.Close()
		return nil, err
	}
	js, err := jetstream.New(connection)
	if err != nil {
		connection.Close()
		return nil, fmt.Errorf("nats events: create jetstream client: %w", err)
	}

	streamConfig := jetstream.StreamConfig{
		Name:        normalized.Stream,
		Description: "PatchProof domain events",
		Subjects:    []string{eventSubjectRoot},
		Retention:   jetstream.LimitsPolicy,
		Storage:     jetstream.FileStorage,
		Discard:     jetstream.DiscardOld,
		MaxAge:      streamMaxAge,
		MaxMsgs:     -1,
		MaxBytes:    -1,
		Replicas:    normalized.Replicas,
		Duplicates:  2 * time.Minute,
	}
	createCtx, cancel := context.WithTimeout(ctx, normalized.ConnectTimeout)
	defer cancel()
	if _, err := js.CreateOrUpdateStream(createCtx, streamConfig); err != nil {
		connection.Close()
		return nil, fmt.Errorf("nats events: ensure stream %q: %w", normalized.Stream, err)
	}

	return &Bus{
		nc:             connection,
		js:             js,
		stream:         normalized.Stream,
		consumerPrefix: normalized.ConsumerPrefix,
		maxAckPending:  normalized.MaxAckPending,
		batchSize:      normalized.BatchSize,
		ackWait:        normalized.AckWait,
		maxDeliver:     normalized.MaxDeliver,
		closedCh:       make(chan struct{}),
	}, nil
}

// Publish sends events to their type subjects in bounded batches. Per-event
// failures are collected and returned together.
func (b *Bus) Publish(ctx context.Context, events []domain.OutboxEvent) error {
	if len(events) == 0 {
		return nil
	}
	if b.closed.Load() {
		return ErrClosed
	}
	var errs []error
	for start := 0; start < len(events); start += b.batchSize {
		end := start + b.batchSize
		if end > len(events) {
			end = len(events)
		}
		if err := b.publishBatch(ctx, events[start:end]); err != nil {
			errs = append(errs, fmt.Errorf("publish events %d-%d: %w", start, end, err))
		}
		if err := ctx.Err(); err != nil {
			errs = append(errs, err)
			break
		}
	}
	return errors.Join(errs...)
}

// Consume creates a durable pull consumer and blocks until ctx is done, the bus
// is closed, or a systemic failure occurs. Malformed messages are terminated
// and counted rather than returned as errors.
func (b *Bus) Consume(ctx context.Context, consumer string, subjects []string, handler func(context.Context, domain.EventEnvelope) error) error {
	if handler == nil {
		return errors.New("nats events consume: handler must not be nil")
	}
	name, err := consumerName(b.consumerPrefix, consumer)
	if err != nil {
		return fmt.Errorf("nats events consume: %w", err)
	}
	patterns, err := normalizeFilterSubjects(subjects)
	if err != nil {
		return fmt.Errorf("nats events consume: %w", err)
	}
	if b.closed.Load() {
		return ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	consumeCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-b.closedCh:
			cancel()
		case <-consumeCtx.Done():
		}
	}()

	consumerConfig := jetstream.ConsumerConfig{
		Name:           name,
		Durable:        name,
		DeliverPolicy:  jetstream.DeliverAllPolicy,
		AckPolicy:      jetstream.AckExplicitPolicy,
		AckWait:        b.ackWait,
		MaxDeliver:     b.maxDeliver,
		MaxAckPending:  b.maxAckPending,
		FilterSubjects: patterns,
		ReplayPolicy:   jetstream.ReplayInstantPolicy,
	}
	streamConsumer, err := b.js.CreateOrUpdateConsumer(consumeCtx, b.stream, consumerConfig)
	if err != nil {
		if consumeCtx.Err() != nil {
			return b.consumeResult()
		}
		return fmt.Errorf("nats events consume: ensure consumer %q: %w", name, err)
	}

	for {
		if consumeCtx.Err() != nil {
			return b.consumeResult()
		}
		batch, err := streamConsumer.Fetch(b.batchSize, jetstream.FetchContext(consumeCtx))
		if err != nil {
			if consumeCtx.Err() != nil {
				return b.consumeResult()
			}
			if isTransientFetchError(err) {
				continue
			}
			return fmt.Errorf("nats events consume: fetch for %q: %w", name, err)
		}
		failure := b.handleBatch(consumeCtx, batch, patterns, handler)
		if batchErr := batch.Error(); batchErr != nil && failure == nil {
			if consumeCtx.Err() != nil {
				return b.consumeResult()
			}
			if !isTransientFetchError(batchErr) {
				failure = fmt.Errorf("nats events consume: fetch batch for %q: %w", name, batchErr)
			}
		}
		if failure != nil {
			return failure
		}
		if consumeCtx.Err() != nil {
			return b.consumeResult()
		}
	}
}

// Health reports whether the connection and stream are available.
func (b *Bus) Health(ctx context.Context) error {
	if b.closed.Load() {
		return ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !b.nc.IsConnected() {
		return fmt.Errorf("nats events: connection status %s", b.nc.Status())
	}
	if _, err := b.js.Stream(ctx, b.stream); err != nil {
		return fmt.Errorf("nats events: stream %q: %w", b.stream, err)
	}
	return nil
}

// Close drains the connection and closes it. It is idempotent.
func (b *Bus) Close() error {
	if !b.closed.CompareAndSwap(false, true) {
		return nil
	}
	close(b.closedCh)
	var errs []error
	if err := b.nc.Drain(); err != nil && !errors.Is(err, nats.ErrConnectionClosed) {
		errs = append(errs, fmt.Errorf("nats events: drain connection: %w", err))
	}
	b.nc.Close()
	return errors.Join(errs...)
}

// MalformedMessages returns the number of messages terminated because their
// payload was not a valid event envelope.
func (b *Bus) MalformedMessages() int64 {
	return b.malformed.Load()
}

func (b *Bus) publishBatch(ctx context.Context, events []domain.OutboxEvent) error {
	var errs []error
	for _, event := range events {
		if err := ctx.Err(); err != nil {
			errs = append(errs, err)
			break
		}
		if err := b.publishOne(ctx, event); err != nil {
			errs = append(errs, fmt.Errorf("event %s: %w", event.ID, err))
		}
	}
	return errors.Join(errs...)
}

func (b *Bus) publishOne(ctx context.Context, event domain.OutboxEvent) error {
	if err := event.Validate(); err != nil {
		return err
	}
	if b.closed.Load() {
		return ErrClosed
	}
	data, err := marshalEnvelope(event.Envelope())
	if err != nil {
		return err
	}
	header := nats.Header{}
	header.Set(jetstream.MsgIDHeader, string(event.ID))
	if event.TraceParent != "" {
		header.Set(traceParentHeader, event.TraceParent)
	}
	message := &nats.Msg{
		Subject: event.Type.Subject(),
		Data:    data,
		Header:  header,
	}
	if _, err := b.js.PublishMsg(ctx, message); err != nil {
		return fmt.Errorf("publish to %s: %w", message.Subject, err)
	}
	return nil
}

func (b *Bus) handleBatch(ctx context.Context, batch jetstream.MessageBatch, patterns []string, handler func(context.Context, domain.EventEnvelope) error) error {
	var failure error
	for message := range batch.Messages() {
		if ctx.Err() != nil {
			continue
		}
		if failure != nil {
			continue
		}
		if err := b.handleMessage(ctx, message, patterns, handler); err != nil {
			failure = err
		}
	}
	return failure
}

func (b *Bus) handleMessage(ctx context.Context, message jetstream.Msg, patterns []string, handler func(context.Context, domain.EventEnvelope) error) error {
	if !matchesAny(message.Subject(), patterns) {
		return terminate(message, "subject does not match consumer filters")
	}
	envelope, err := unmarshalEnvelope(message.Data())
	if err != nil {
		b.malformed.Add(1)
		return terminate(message, "invalid event envelope")
	}
	if err := handler(ctx, envelope); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		if err := message.NakWithDelay(retryDelay(deliveryCount(message))); err != nil {
			return fmt.Errorf("nats events: nak event %s: %w", envelope.ID, err)
		}
		return nil
	}
	if err := message.Ack(); err != nil {
		return fmt.Errorf("nats events: ack event %s: %w", envelope.ID, err)
	}
	return nil
}

func (b *Bus) consumeResult() error {
	if b.closed.Load() {
		return ErrClosed
	}
	return nil
}

func terminate(message jetstream.Msg, reason string) error {
	if err := message.TermWithReason(reason); err != nil {
		return fmt.Errorf("nats events: terminate message: %w", err)
	}
	return nil
}

func deliveryCount(message jetstream.Msg) uint64 {
	metadata, err := message.Metadata()
	if err != nil || metadata.NumDelivered == 0 {
		return 1
	}
	return metadata.NumDelivered
}

func retryDelay(delivered uint64) time.Duration {
	if delivered < 1 {
		delivered = 1
	}
	delay := minRetryDelay
	for attempt := uint64(1); attempt < delivered && delay < maxRetryDelay; attempt++ {
		delay *= 2
	}
	if delay > maxRetryDelay {
		delay = maxRetryDelay
	}
	return delay
}

func isTransientFetchError(err error) bool {
	return errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, nats.ErrTimeout) ||
		errors.Is(err, jetstream.ErrNoMessages)
}

func normalizeConfig(cfg Config) (Config, error) {
	urls := make([]string, 0, len(cfg.URLs))
	for _, raw := range cfg.URLs {
		if url := strings.TrimSpace(raw); url != "" {
			urls = append(urls, url)
		}
	}
	if len(urls) == 0 {
		return Config{}, errors.New("nats events: at least one URL is required")
	}
	stream := strings.TrimSpace(cfg.Stream)
	if stream == "" {
		stream = defaultStreamName
	}
	if err := validateName("stream", stream); err != nil {
		return Config{}, fmt.Errorf("nats events: %w", err)
	}
	prefix := strings.TrimSpace(cfg.ConsumerPrefix)
	if prefix == "" {
		prefix = defaultConsumerPrefix
	}
	if err := validateName("consumer prefix", prefix); err != nil {
		return Config{}, fmt.Errorf("nats events: %w", err)
	}
	batchSize := cfg.BatchSize
	switch {
	case batchSize == 0:
		batchSize = defaultBatchSize
	case batchSize < 0:
		return Config{}, errors.New("nats events: batch size must not be negative")
	case batchSize > maxFetchBatch:
		return Config{}, fmt.Errorf("nats events: batch size must be at most %d", maxFetchBatch)
	}
	maxAckPending := cfg.MaxAckPending
	switch {
	case maxAckPending == 0:
		maxAckPending = defaultMaxAckPending
	case maxAckPending < 0:
		return Config{}, errors.New("nats events: max ack pending must not be negative")
	}
	ackWait := cfg.AckWait
	switch {
	case ackWait == 0:
		ackWait = defaultAckWait
	case ackWait < 0:
		return Config{}, errors.New("nats events: ack wait must not be negative")
	}
	maxDeliver := cfg.MaxDeliver
	switch {
	case maxDeliver == 0:
		maxDeliver = defaultMaxDeliver
	case maxDeliver < 0:
		return Config{}, errors.New("nats events: max deliver must not be negative")
	}
	connectTimeout := cfg.ConnectTimeout
	switch {
	case connectTimeout == 0:
		connectTimeout = defaultConnectTimeout
	case connectTimeout < 0:
		return Config{}, errors.New("nats events: connect timeout must not be negative")
	}
	replicas := cfg.Replicas
	switch {
	case replicas == 0:
		replicas = 1
	case replicas < 0:
		return Config{}, errors.New("nats events: replicas must not be negative")
	case replicas > 5:
		return Config{}, errors.New("nats events: replicas must be at most 5")
	}
	if (cfg.TLSCertFile == "") != (cfg.TLSKeyFile == "") {
		return Config{}, errors.New("nats events: TLS certificate and key must be set together")
	}
	return Config{
		URLs:            urls,
		Stream:          stream,
		ConsumerPrefix:  prefix,
		MaxAckPending:   maxAckPending,
		BatchSize:       batchSize,
		AckWait:         ackWait,
		MaxDeliver:      maxDeliver,
		CredentialsFile: cfg.CredentialsFile,
		TLSCertFile:     cfg.TLSCertFile,
		TLSKeyFile:      cfg.TLSKeyFile,
		TLSCAFile:       cfg.TLSCAFile,
		ConnectTimeout:  connectTimeout,
		Replicas:        replicas,
	}, nil
}
