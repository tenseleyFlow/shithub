// SPDX-License-Identifier: AGPL-3.0-or-later

// Package web boots the shithub HTTP server. S02 lights up the full
// middleware stack (recover, request_id, logging, real-IP, timeout,
// compress, secure headers, CSRF, session, CORS), the chi router, the
// session store, and the styled error pages. Every later sprint adds
// routes via internal/web/handlers.
package web

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"golang.org/x/crypto/chacha20poly1305"

	"github.com/tenseleyFlow/shithub/internal/auth/session"
	"github.com/tenseleyFlow/shithub/internal/infra/db"
	"github.com/tenseleyFlow/shithub/internal/web/handlers"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

// Options configures the web server.
type Options struct {
	Addr string
}

// Run boots the web server and blocks until shutdown.
//
// It listens for SIGINT/SIGTERM and gracefully drains in-flight requests on
// exit. The full middleware stack is composed here; handlers register their
// routes via internal/web/handlers.RegisterChi.
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

	sessionStore, err := buildSessionStore(logger)
	if err != nil {
		return err
	}

	// Optional DB pool (carried over from S01).
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

	r := chi.NewRouter()

	// Middleware stack — outermost first. Recover wraps the whole pipeline
	// AFTER routes register so its panic handler has a renderer ready.
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP(middleware.RealIPConfig{}))
	r.Use(middleware.AccessLog(logger))
	r.Use(middleware.SecureHeaders(middleware.DefaultSecureHeaders()))
	r.Use(middleware.Compress)
	r.Use(middleware.Timeout(30 * time.Second))
	r.Use(middleware.SessionLoader(sessionStore, logger))

	deps := handlers.Deps{
		Logger:       logger,
		TemplatesFS:  TemplatesFS(),
		StaticFS:     StaticFS(),
		LogoSVG:      string(logoBytes),
		SessionStore: sessionStore,
	}
	if pool != nil {
		deps.ReadyCheck = pool.healthcheck
	}

	_, panicHandler, notFoundHandler, err := handlers.RegisterChi(r, deps)
	if err != nil {
		return fmt.Errorf("register handlers: %w", err)
	}
	r.NotFound(notFoundHandler)

	rootHandler := middleware.Recover(logger, panicHandler)(r)

	srv := &http.Server{
		Addr:              opts.Addr,
		Handler:           rootHandler,
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

// buildSessionStore constructs the cookie session store. The key comes from
// SHITHUB_SESSION_KEY (base64 32-byte). When unset (dev), a random key is
// generated and the operator is warned — sessions don't survive restart.
func buildSessionStore(logger *slog.Logger) (session.Store, error) {
	keyB64 := os.Getenv("SHITHUB_SESSION_KEY")
	var key []byte
	if keyB64 != "" {
		decoded, err := base64.StdEncoding.DecodeString(keyB64)
		if err != nil {
			return nil, fmt.Errorf("session key: invalid base64: %w", err)
		}
		if len(decoded) != chacha20poly1305.KeySize {
			return nil, fmt.Errorf("session key: must be %d bytes, got %d",
				chacha20poly1305.KeySize, len(decoded))
		}
		key = decoded
	} else {
		generated, err := session.GenerateKey()
		if err != nil {
			return nil, fmt.Errorf("session key: generate: %w", err)
		}
		key = generated
		logger.Warn(
			"session: SHITHUB_SESSION_KEY not set; generated an ephemeral key (sessions will not survive restart)",
			"hint", "set SHITHUB_SESSION_KEY=<base64 32-byte key> in production",
		)
	}
	store, err := session.NewCookieStore(session.CookieStoreConfig{
		Key:    key,
		Secure: false, // S37 deploy enables this under TLS
	})
	if err != nil {
		return nil, fmt.Errorf("session: build store: %w", err)
	}
	return store, nil
}

// pgxpoolHandle adapts *pgxpool.Pool's lifecycle to the small interface
// /readyz needs. Defined here (not in the db package) so internal/web stays
// the boundary that owns runtime wiring.
type pgxpoolHandle struct {
	p interface {
		Close()
	}
}

func (h *pgxpoolHandle) healthcheck(ctx context.Context) error {
	type pinger interface {
		Ping(context.Context) error
	}
	if p, ok := h.p.(pinger); ok {
		return p.Ping(ctx)
	}
	return nil
}
