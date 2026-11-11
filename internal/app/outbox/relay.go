package outbox

import (
	"context"
	"log/slog"
	"time"

	"github.com/patchproof/patchproof/internal/domain"
	"github.com/patchproof/patchproof/internal/platform/telemetry"
	"github.com/patchproof/patchproof/internal/ports"
)

type Config struct {
	Interval    time.Duration
	Batch       int
	MaxAttempts int
	Clock       ports.Clock
}

type Relay struct {
	store   ports.MetadataStore
	bus     ports.EventBus
	metrics *telemetry.Metrics
	logger  *slog.Logger
	config  Config
}

func New(store ports.MetadataStore, bus ports.EventBus, config Config, metrics *telemetry.Metrics, logger *slog.Logger) (*Relay, error) {
	if store == nil || bus == nil {
		return nil, errNilDependency
	}
	if config.Interval <= 0 {
		config.Interval = time.Second
	}
	if config.Batch <= 0 {
		config.Batch = 128
	}
	if config.MaxAttempts <= 0 {
		config.MaxAttempts = 10
	}
	if config.Clock == nil {
		config.Clock = systemClock{}
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Relay{store: store, bus: bus, metrics: metrics, logger: logger, config: config}, nil
}

func (r *Relay) Run(ctx context.Context) error {
	ticker := time.NewTicker(r.config.Interval)
	defer ticker.Stop()
	for {
		if err := r.drain(ctx); err != nil && ctx.Err() == nil {
			r.logger.Warn("outbox drain failed", slog.String("error", err.Error()))
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func (r *Relay) drain(ctx context.Context) error {
	events, err := r.store.PendingOutbox(ctx, r.config.Batch)
	if err != nil {
		return err
	}
	if len(events) == 0 {
		return nil
	}
	if err := r.bus.Publish(ctx, events); err != nil {
		for _, event := range events {
			now := r.config.Clock.Now()
			if recordErr := r.store.RecordOutboxFailure(ctx, domain.OutboxDeliveryFailure{
				EventID: event.ID, Reason: err.Error(), At: now,
			}); recordErr != nil {
				r.logger.Warn("record outbox failure", slog.String("error", recordErr.Error()))
			}
			if event.Attempts+1 >= r.config.MaxAttempts {
				if deadErr := r.store.MarkOutboxDeadLettered(ctx, []domain.EventID{event.ID}, now); deadErr != nil {
					r.logger.Warn("dead-letter outbox event", slog.String("event_id", string(event.ID)), slog.String("error", deadErr.Error()))
				}
			}
			if r.metrics != nil {
				r.metrics.OutboxFailures.WithLabelValues(string(event.Type)).Inc()
			}
		}
		return err
	}
	ids := make([]domain.EventID, 0, len(events))
	for _, event := range events {
		ids = append(ids, event.ID)
		if r.metrics != nil {
			r.metrics.OutboxPublished.WithLabelValues(string(event.Type)).Inc()
		}
	}
	return r.store.MarkOutboxDelivered(ctx, ids, r.config.Clock.Now())
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now().UTC() }

var errNilDependency = &dependencyError{}

type dependencyError struct{}

func (e *dependencyError) Error() string {
	return "outbox relay requires a metadata store and an event bus"
}
