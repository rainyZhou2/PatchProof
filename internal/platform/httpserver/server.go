package httpserver

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/patchproof/patchproof/internal/platform/config"
)

func New(addr string, handler http.Handler, cfg config.HTTPConfig, logger *slog.Logger) (*http.Server, error) {
	if addr == "" {
		return nil, fmt.Errorf("http server: listen address is required")
	}
	server := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.IdleTimeout,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}
	if cfg.TLSCertFile != "" || cfg.TLSKeyFile != "" {
		certificate, err := tls.LoadX509KeyPair(cfg.TLSCertFile, cfg.TLSKeyFile)
		if err != nil {
			return nil, fmt.Errorf("load TLS certificate: %w", err)
		}
		server.TLSConfig = &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12}
	}
	return server, nil
}

func Serve(ctx context.Context, server *http.Server, logger *slog.Logger) error {
	listener, err := net.Listen("tcp", server.Addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", server.Addr, err)
	}
	logger.Info("http server listening", slog.String("addr", listener.Addr().String()))
	errCh := make(chan error, 1)
	go func() {
		if server.TLSConfig != nil {
			errCh <- server.ServeTLS(listener, "", "")
			return
		}
		errCh <- server.Serve(listener)
	}()
	select {
	case <-ctx.Done():
		return nil
	case err := <-errCh:
		if err == http.ErrServerClosed {
			return nil
		}
		return err
	}
}

func Shutdown(server *http.Server, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return server.Shutdown(ctx)
}
