package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
)

type Group struct {
	ctx    context.Context
	cancel context.CancelFunc
	logger *slog.Logger
	wg     sync.WaitGroup
	mu     sync.Mutex
	err    error
	names  []string
}

func New(ctx context.Context, logger *slog.Logger) *Group {
	ctx, cancel := context.WithCancel(ctx)
	return &Group{ctx: ctx, cancel: cancel, logger: logger}
}

func (g *Group) Go(name string, run func(context.Context) error) {
	g.mu.Lock()
	g.names = append(g.names, name)
	g.mu.Unlock()
	g.wg.Add(1)
	go func() {
		defer g.wg.Done()
		defer func() {
			if recovered := recover(); recovered != nil {
				err := fmt.Errorf("component %s panicked: %v", name, recovered)
				g.logger.Error("component panic", slog.String("component", name), slog.Any("panic", recovered), slog.String("stack", string(debug.Stack())))
				g.fail(err)
			}
		}()
		g.logger.Info("component starting", slog.String("component", name))
		if err := run(g.ctx); err != nil && !errors.Is(err, context.Canceled) {
			g.fail(fmt.Errorf("component %s: %w", name, err))
			return
		}
		g.logger.Info("component stopped", slog.String("component", name))
	}()
}

func (g *Group) fail(err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.err == nil {
		g.err = err
	}
	g.cancel()
}

func (g *Group) Wait() error {
	g.wg.Wait()
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.err
}

func (g *Group) Stop() {
	g.cancel()
}

func (g *Group) Context() context.Context { return g.ctx }
