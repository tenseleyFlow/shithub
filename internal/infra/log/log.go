// SPDX-License-Identifier: AGPL-3.0-or-later

// Package log builds the slog handler the rest of the binary uses.
//
// Output format:
//   - "text" — human-friendly key=value lines (default in dev)
//   - "json" — one JSON object per line (default in prod)
//
// Standard log fields contract (every line):
//   - time, level, msg
//   - request_id   when a request is in flight
//   - user_id      when known (post-S05)
//   - component    set by the package emitting the line
//   - error/stack  on error-level lines
//
// Redaction: field values whose key matches a known secret pattern (token,
// password, key, dsn, authorization, otpauth) are rewritten to "***" before
// they hit the underlying handler.
package log

import (
	"context"
	"io"
	"log/slog"
	"strings"
)

// Options configures the handler.
type Options struct {
	Level  string // debug | info | warn | error
	Format string // text | json
	Writer io.Writer
}

// New returns a slog.Logger configured per opts. The redacting handler
// wraps the chosen base handler so every record is sanitised before output.
func New(opts Options) *slog.Logger {
	if opts.Writer == nil {
		opts.Writer = io.Discard
	}
	level := parseLevel(opts.Level)
	handlerOpts := &slog.HandlerOptions{Level: level}

	var base slog.Handler
	switch strings.ToLower(opts.Format) {
	case "json":
		base = slog.NewJSONHandler(opts.Writer, handlerOpts)
	default:
		base = slog.NewTextHandler(opts.Writer, handlerOpts)
	}
	return slog.New(&redactHandler{base: base})
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// secretAttrKeys are case-insensitive substrings that mark an attribute key
// as a secret value to redact.
var secretAttrKeys = []string{
	"password", "pass",
	"secret",
	"key",
	"token",
	"authorization",
	"dsn",
	"otpauth",
}

// secretValueMarkers are substrings that, if found anywhere in a string
// value, signal the line is leaking a token / URL credential / secret URI
// even if the attribute key itself looks innocent.
var secretValueMarkers = []string{
	"shithub_pat_",
	"otpauth://",
	"Bearer ",
	"Basic ",
}

// redactHandler wraps another slog.Handler, scrubbing matched attributes.
type redactHandler struct {
	base  slog.Handler
	attrs []slog.Attr
	group string
}

func (h *redactHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.base.Enabled(ctx, level)
}

func (h *redactHandler) Handle(ctx context.Context, r slog.Record) error {
	scrubbed := slog.Record{
		Time:    r.Time,
		Message: r.Message,
		Level:   r.Level,
		PC:      r.PC,
	}
	r.Attrs(func(a slog.Attr) bool {
		scrubbed.AddAttrs(redactAttr(a))
		return true
	})
	return h.base.Handle(ctx, scrubbed)
}

func (h *redactHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	scrubbed := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		scrubbed[i] = redactAttr(a)
	}
	return &redactHandler{base: h.base.WithAttrs(scrubbed), attrs: append(append([]slog.Attr{}, h.attrs...), scrubbed...), group: h.group}
}

func (h *redactHandler) WithGroup(name string) slog.Handler {
	return &redactHandler{base: h.base.WithGroup(name), attrs: h.attrs, group: name}
}

func redactAttr(a slog.Attr) slog.Attr {
	if shouldRedactKey(a.Key) {
		if a.Value.Kind() == slog.KindString {
			return slog.String(a.Key, "***")
		}
	}
	if a.Value.Kind() == slog.KindString {
		if v := redactValueIfSensitive(a.Value.String()); v != a.Value.String() {
			return slog.String(a.Key, v)
		}
	}
	return a
}

func shouldRedactKey(key string) bool {
	lower := strings.ToLower(key)
	for _, needle := range secretAttrKeys {
		if strings.Contains(lower, needle) {
			return true
		}
	}
	return false
}

func redactValueIfSensitive(v string) string {
	for _, marker := range secretValueMarkers {
		if strings.Contains(v, marker) {
			return "***"
		}
	}
	return v
}
