// SPDX-License-Identifier: AGPL-3.0-or-later

// Package web boots the shithub HTTP server. S00 stands up only the bare
// shell — the hello page, static assets, and /healthz. S02 (web shell)
// fleshes out the middleware stack, sessions, error pages, and Primer-themed
// base templates. Every later sprint adds routes via internal/web/handlers.
package web

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/tenseleyFlow/shithub/internal/infra/db"
	"github.com/tenseleyFlow/shithub/internal/web/handlers"
)

// Options configures the web server.
type Options struct {
	Addr string
}

// Run boots the web server and blocks until shutdown.
//
// It listens for SIGINT/SIGTERM and gracefully drains in-flight requests on
// exit. The S00 surface is intentionally minimal; later sprints add the full
// middleware stack, session store, and rendering pipeline.
func Run(ctx context.Context, opts Options) error {
	if opts.Addr == "" {
		opts.Addr = ":8080"
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	logoBytes, err := LogoSVG()
	if err != nil {
		return fmt.Errorf("load logo: %w", err)
	}

	// DB pool is optional in S01: the server boots without one (the hello
	// page works), but /readyz reports 503 if a DB is configured but
	// unreachable. S02+ will make a pool effectively required.
	var pool *pgxpoolHandle
	if cfg := db.Defaults().Resolve(); cfg.URL != "" {
		p, err := db.Open(ctx, cfg)
		if err != nil {
			logger.Warn("db: open failed; /readyz will report unhealthy", "error", err)
		} else {
			pool = &pgxpoolHandle{p: p}
			defer p.Close()
		}
	}

	mux := http.NewServeMux()
	deps := handlers.Deps{
		Logger:      logger,
		TemplatesFS: TemplatesFS(),
		StaticFS:    StaticFS(),
		LogoSVG:     string(logoBytes),
	}
	if pool != nil {
		deps.ReadyCheck = pool.healthcheck
	}
	if err := handlers.Register(mux, deps); err != nil {
		return fmt.Errorf("register handlers: %w", err)
	}

	srv := &http.Server{
		Addr:              opts.Addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("shithub web server starting", "addr", opts.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	select {
	case err, ok := <-errCh:
		if !ok {
			return nil
		}
		return err
	case sig := <-sigCh:
		logger.Info("shutdown signal received", "signal", sig.String())
	case <-ctx.Done():
		logger.Info("context canceled, shutting down")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	return nil
}

// pgxpoolHandle is an internal wrapper that converts the pool into the
// callback-shape the handlers package expects, without exposing pgx types
// to internal/web/handlers. It also lets us pass a nil pool through cleanly.
type pgxpoolHandle struct {
	p interface {
		Close()
	}
}

func (h *pgxpoolHandle) healthcheck(ctx context.Context) error {
	// Re-open the type via the db package's typed helper.
	type pinger interface {
		Ping(context.Context) error
	}
	if p, ok := h.p.(pinger); ok {
		return p.Ping(ctx)
	}
	return nil
}
