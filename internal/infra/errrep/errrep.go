// SPDX-License-Identifier: AGPL-3.0-or-later

// Package errrep wires the Sentry-protocol-compatible error reporter.
// When DSN is empty, every entry point becomes a no-op so callers don't
// need to branch. The wire format works against either Sentry SaaS or a
// self-hosted GlitchTip instance (the lean per S03 sprint spec).
package errrep

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"time"

	"github.com/getsentry/sentry-go"
)

// Config controls SDK initialization.
type Config struct {
	DSN         string
	Environment string
	Release     string
}

// Init initializes the SDK. Returns a flush function the caller calls on
// shutdown to drain any queued events. When DSN is empty Init is a no-op
// and the returned flush is a no-op too.
func Init(cfg Config) (func(context.Context) error, error) {
	if cfg.DSN == "" {
		return func(context.Context) error { return nil }, nil
	}
	err := sentry.Init(sentry.ClientOptions{
		Dsn:              cfg.DSN,
		Environment:      cfg.Environment,
		Release:          cfg.Release,
		AttachStacktrace: true,
		EnableTracing:    false,
	})
	if err != nil {
		return nil, fmt.Errorf("errrep: sentry init: %w", err)
	}
	return func(_ context.Context) error {
		if !sentry.Flush(3 * time.Second) {
			return errors.New("errrep: flush did not complete")
		}
		return nil
	}, nil
}

// CaptureException reports an error. Safe to call when the SDK is not
// configured (it's a no-op).
func CaptureException(err error) {
	if err == nil {
		return
	}
	sentry.CaptureException(err)
}

// CapturePanic reports a recovered panic value. requestID is used to
// correlate with logs and traces.
func CapturePanic(recovered any, requestID string) {
	if recovered == nil {
		return
	}
	hub := sentry.CurrentHub().Clone()
	hub.WithScope(func(scope *sentry.Scope) {
		if requestID != "" {
			scope.SetTag("request_id", requestID)
		}
		scope.SetContext("shithub", sentry.Context{
			"stack": string(debug.Stack()),
		})
		hub.RecoverWithContext(context.Background(), recovered)
	})
}

// SlogHandler wraps an underlying slog.Handler so that records at error
// level are also reported to Sentry. Other levels pass through unchanged.
type SlogHandler struct {
	Inner slog.Handler
}

func (h *SlogHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.Inner.Enabled(ctx, level)
}

func (h *SlogHandler) Handle(ctx context.Context, r slog.Record) error {
	if r.Level >= slog.LevelError {
		hub := sentry.CurrentHub().Clone()
		hub.WithScope(func(scope *sentry.Scope) {
			extras := sentry.Context{}
			r.Attrs(func(a slog.Attr) bool {
				switch a.Key {
				case "request_id", "user_id", "component", "route":
					scope.SetTag(a.Key, a.Value.String())
				default:
					extras[a.Key] = a.Value.Any()
				}
				return true
			})
			if len(extras) > 0 {
				scope.SetContext("shithub", extras)
			}
			hub.CaptureMessage(r.Message)
		})
	}
	return h.Inner.Handle(ctx, r)
}

func (h *SlogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &SlogHandler{Inner: h.Inner.WithAttrs(attrs)}
}

func (h *SlogHandler) WithGroup(name string) slog.Handler {
	return &SlogHandler{Inner: h.Inner.WithGroup(name)}
}
