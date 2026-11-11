package nats

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/patchproof/patchproof/internal/domain"
)

func TestNormalizeConfigDefaults(t *testing.T) {
	cfg, err := normalizeConfig(Config{URLs: []string{" nats://127.0.0.1:4222 ", ""}})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if len(cfg.URLs) != 1 || cfg.URLs[0] != "nats://127.0.0.1:4222" {
		t.Fatalf("urls = %v", cfg.URLs)
	}
	if cfg.Stream != defaultStreamName {
		t.Fatalf("stream = %q", cfg.Stream)
	}
	if cfg.ConsumerPrefix != defaultConsumerPrefix {
		t.Fatalf("consumer prefix = %q", cfg.ConsumerPrefix)
	}
	if cfg.MaxAckPending != defaultMaxAckPending || cfg.BatchSize != defaultBatchSize {
		t.Fatalf("unexpected limits: ack pending=%d batch=%d", cfg.MaxAckPending, cfg.BatchSize)
	}
	if cfg.AckWait != defaultAckWait || cfg.MaxDeliver != defaultMaxDeliver {
		t.Fatalf("unexpected delivery settings: ack wait=%s max deliver=%d", cfg.AckWait, cfg.MaxDeliver)
	}
	if cfg.ConnectTimeout != defaultConnectTimeout || cfg.Replicas != 1 {
		t.Fatalf("unexpected transport settings: timeout=%s replicas=%d", cfg.ConnectTimeout, cfg.Replicas)
	}
}

func TestNormalizeConfigValidation(t *testing.T) {
	base := Config{URLs: []string{"nats://127.0.0.1:4222"}}
	if _, err := normalizeConfig(Config{}); err == nil {
		t.Fatal("expected empty URLs to fail")
	}
	cases := []struct {
		name string
		cfg  Config
	}{
		{"stream with dot", Config{URLs: base.URLs, Stream: "PATCH.PROOF"}},
		{"stream with space", Config{URLs: base.URLs, Stream: "BAD STREAM"}},
		{"consumer prefix with dot", Config{URLs: base.URLs, ConsumerPrefix: "patch.proof"}},
		{"negative batch", Config{URLs: base.URLs, BatchSize: -1}},
		{"large batch", Config{URLs: base.URLs, BatchSize: maxFetchBatch + 1}},
		{"negative ack pending", Config{URLs: base.URLs, MaxAckPending: -1}},
		{"negative ack wait", Config{URLs: base.URLs, AckWait: -time.Second}},
		{"negative max deliver", Config{URLs: base.URLs, MaxDeliver: -1}},
		{"negative timeout", Config{URLs: base.URLs, ConnectTimeout: -time.Second}},
		{"too many replicas", Config{URLs: base.URLs, Replicas: 6}},
		{"cert without key", Config{URLs: base.URLs, TLSCertFile: "cert.pem"}},
		{"key without cert", Config{URLs: base.URLs, TLSKeyFile: "key.pem"}},
	}
	for _, tc := range cases {
		if _, err := normalizeConfig(tc.cfg); err == nil {
			t.Fatalf("%s: expected error", tc.name)
		}
	}
}

func TestConsumerName(t *testing.T) {
	name, err := consumerName("patchproof", "worker")
	if err != nil {
		t.Fatalf("consumer name: %v", err)
	}
	if name != "patchproof-worker" {
		t.Fatalf("consumer name = %q", name)
	}
	for _, consumer := range []string{"", "../worker", "worker.queue", "worker queue"} {
		if _, err := consumerName("patchproof", consumer); err == nil {
			t.Fatalf("expected consumer %q to be rejected", consumer)
		}
	}
}

func TestSubjectMatchingAndValidation(t *testing.T) {
	cases := []struct {
		pattern string
		subject string
		want    bool
	}{
		{"patchproof.>", "patchproof.run.created.v1", true},
		{"patchproof.>", "patchproof", false},
		{"patchproof.*.created.v1", "patchproof.run.created.v1", true},
		{"patchproof.*.created.v1", "patchproof.run.ready.v1", false},
		{"patchproof.run.created.v1", "patchproof.run.created.v1", true},
		{"patchproof.run.created.v1", "patchproof.step.created.v1", false},
		{"patchproof.run.>", "patchproof.run", false},
		{"*", "patchproof", true},
	}
	for _, tc := range cases {
		if got := matchesSubject(tc.pattern, tc.subject); got != tc.want {
			t.Fatalf("matchesSubject(%q, %q) = %v, want %v", tc.pattern, tc.subject, got, tc.want)
		}
	}
	for _, pattern := range []string{"", "patchproof..x", "patchproof.>.x", "patchproof.foo*"} {
		if validSubjectPattern(pattern) {
			t.Fatalf("expected %q to be invalid", pattern)
		}
	}
	if !validSubjectPattern("patchproof.>") || !validSubjectPattern("patchproof.*.v1") {
		t.Fatal("expected wildcard subjects to be valid")
	}
	if _, err := normalizeFilterSubjects(nil); err == nil {
		t.Fatal("expected empty filter subjects to fail")
	}
	if _, err := normalizeFilterSubjects([]string{" "}); err == nil {
		t.Fatal("expected blank filter subject to fail")
	}
}

func TestEnvelopeConversion(t *testing.T) {
	event, err := domain.NewOutboxEvent(
		domain.NewTenantID(),
		domain.EventRunCreated,
		"aggregate-1",
		2,
		map[string]string{"run": "r1"},
		time.Now().UTC(),
	)
	if err != nil {
		t.Fatalf("new event: %v", err)
	}
	event.TraceParent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	envelope := event.Envelope()
	data, err := marshalEnvelope(envelope)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	decoded, err := unmarshalEnvelope(data)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.ID != envelope.ID || decoded.Type != envelope.Type || decoded.TenantID != envelope.TenantID {
		t.Fatalf("decoded envelope mismatch: %+v", decoded)
	}
	if decoded.TraceParent != envelope.TraceParent || decoded.Generation != envelope.Generation {
		t.Fatalf("decoded metadata mismatch: %+v", decoded)
	}
	if string(decoded.Payload) != string(envelope.Payload) {
		t.Fatalf("payload = %s, want %s", decoded.Payload, envelope.Payload)
	}

	if _, err := unmarshalEnvelope([]byte("{")); err == nil {
		t.Fatal("expected malformed JSON to fail")
	}
	if _, err := unmarshalEnvelope([]byte(`{"event_type":"patchproof.run.created.v1"}`)); err == nil {
		t.Fatal("expected missing event id to fail")
	}
	noPayload := []byte(`{"event_id":"` + string(domain.NewEventID()) + `","event_type":"patchproof.run.created.v1"}`)
	if _, err := unmarshalEnvelope(noPayload); err == nil {
		t.Fatal("expected empty payload to fail")
	}
}

func TestRetryDelay(t *testing.T) {
	cases := []struct {
		delivered uint64
		want      time.Duration
	}{
		{0, time.Second},
		{1, time.Second},
		{2, 2 * time.Second},
		{3, 4 * time.Second},
		{4, 8 * time.Second},
		{5, 16 * time.Second},
		{6, 30 * time.Second},
		{100, 30 * time.Second},
	}
	for _, tc := range cases {
		if got := retryDelay(tc.delivered); got != tc.want {
			t.Fatalf("retryDelay(%d) = %s, want %s", tc.delivered, got, tc.want)
		}
	}
}

func TestIsTransientFetchError(t *testing.T) {
	if !isTransientFetchError(context.Canceled) || !isTransientFetchError(context.DeadlineExceeded) {
		t.Fatal("expected context errors to be transient")
	}
	if isTransientFetchError(errors.New("boom")) {
		t.Fatal("generic error must not be transient")
	}
}

func TestLiveNATS(t *testing.T) {
	url := os.Getenv("PATCHPROOF_TEST_NATS_URL")
	if url == "" {
		t.Skip("set PATCHPROOF_TEST_NATS_URL to run live NATS tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	streamName := "PATCHPROOFTEST" + strings.ReplaceAll(string(domain.NewEventID()), "-", "")
	bus, err := New(ctx, Config{
		URLs:           []string{url},
		Stream:         streamName,
		ConsumerPrefix: "patchprooftest",
		MaxAckPending:  16,
		BatchSize:      4,
		AckWait:        5 * time.Second,
		MaxDeliver:     3,
		ConnectTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("new bus: %v", err)
	}
	defer bus.Close()
	if err := bus.Health(ctx); err != nil {
		t.Fatalf("health: %v", err)
	}

	consumeCtx, consumeCancel := context.WithCancel(context.Background())
	defer consumeCancel()
	received := make(chan domain.EventEnvelope, 4)
	errCh := make(chan error, 1)
	go func() {
		errCh <- bus.Consume(consumeCtx, "worker", []string{"patchproof.run.>"}, func(_ context.Context, envelope domain.EventEnvelope) error {
			received <- envelope
			return nil
		})
	}()

	event, err := domain.NewOutboxEvent(
		domain.NewTenantID(),
		domain.EventRunCreated,
		"aggregate-1",
		1,
		map[string]string{"run": "live"},
		time.Now().UTC(),
	)
	if err != nil {
		t.Fatalf("new event: %v", err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		err = bus.Publish(ctx, []domain.OutboxEvent{event})
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("publish: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	select {
	case envelope := <-received:
		if envelope.ID != event.ID {
			t.Fatalf("received event %s, want %s", envelope.ID, event.ID)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("event was not consumed")
	}
	consumeCancel()
	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, ErrClosed) {
			t.Fatalf("consume returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("consumer did not stop")
	}
}
