package logging

import (
	"context"
	"log/slog"
	"os"
	"strings"

	"github.com/patchproof/patchproof/internal/platform/config"
)

func New(cfg config.TelemetryConfig, service config.ServiceConfig) (*slog.Logger, error) {
	level := new(slog.LevelVar)
	switch strings.ToLower(cfg.LogLevel) {
	case "debug":
		level.Set(slog.LevelDebug)
	case "info", "":
		level.Set(slog.LevelInfo)
	case "warn", "warning":
		level.Set(slog.LevelWarn)
	case "error":
		level.Set(slog.LevelError)
	default:
		return nil, &configError{"telemetry.logLevel", cfg.LogLevel}
	}
	opts := &slog.HandlerOptions{Level: level}
	var handler slog.Handler
	if strings.EqualFold(cfg.LogFormat, "text") {
		handler = slog.NewTextHandler(os.Stderr, opts)
	} else {
		handler = slog.NewJSONHandler(os.Stderr, opts)
	}
	logger := slog.New(handler).With(
		slog.String("service", service.Name),
		slog.String("region", service.Region),
		slog.String("version", service.Version),
	)
	return logger, nil
}

type configError struct {
	field string
	value string
}

func (e *configError) Error() string { return "invalid " + e.field + ": " + e.value }

type contextKey struct{}

func Into(ctx context.Context, logger *slog.Logger) context.Context {
	return context.WithValue(ctx, contextKey{}, logger)
}

func From(ctx context.Context) *slog.Logger {
	if logger, ok := ctx.Value(contextKey{}).(*slog.Logger); ok {
		return logger
	}
	return slog.Default()
}

func WithRequest(ctx context.Context, attrs ...any) context.Context {
	return Into(ctx, From(ctx).With(attrs...))
}
