package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/patchproof/patchproof/internal/bootstrap"
	"github.com/patchproof/patchproof/internal/platform/config"
	"github.com/patchproof/patchproof/internal/platform/logging"
	"github.com/patchproof/patchproof/internal/platform/telemetry"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "patchproof-controller:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load(os.Getenv("PATCHPROOF_CONFIG"))
	if err != nil {
		return err
	}
	logger, err := logging.New(cfg.Telemetry, cfg.Service)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	tracing, err := telemetry.InitTracing(ctx, cfg.Telemetry, cfg.Service)
	if err != nil {
		return err
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.HTTP.ShutdownTimeout)
		defer cancel()
		_ = tracing.Shutdown(shutdownCtx)
	}()
	metrics := telemetry.NewMetrics()
	app, err := bootstrap.NewApp(ctx, cfg, logger, metrics, tracing, bootstrap.AppOptions{Role: config.RoleController, DisableHTTP: true})
	if err != nil {
		return err
	}
	return app.Run(ctx)
}
