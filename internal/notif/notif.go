// SPDX-License-Identifier: AGPL-3.0-or-later

// Package notif owns S29's notification fan-out: consume from
// `domain_events`, compute the recipient set per the routing
// matrix, materialize inbox rows + email-side dispatch.
//
// The recipient computation is the only interesting code here. The
// routing matrix is data (a Go map keyed on event-kind +
// recipient-relation), the auto-subscription rules are a tiny set
// of "if absent, insert", and the storm dampener is a per-recipient,
// per-thread, time-window count.
//
// Visibility re-check at fan-out is the security boundary: a repo
// can flip from public to private between event-emit and fan-out;
// a stale-read public event must not produce a notification for a
// recipient who can no longer see the underlying resource.
package notif

import (
	"errors"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/auth/email"
	"github.com/tenseleyFlow/shithub/internal/notifications"
)

// Deps wires the package against the rest of the runtime.
//
// EmailSender is optional: when nil, notifications still land in
// the inbox but no email goes out. Useful in tests + during local
// dev when the operator hasn't configured SMTP. EmailFrom is the
// default From address; per-template overrides are post-MVP.
//
// SiteName is the human-readable site name used in subject lines /
// list-unsubscribe footers ("shithub" by default; operators can
// re-brand for self-hosted instances).
//
// BaseURL is the public-facing scheme+host (e.g.
// "https://shithub.example") used to build canonical links in
// email bodies + the unsubscribe URL.
//
// UnsubscribeKey is the HMAC-SHA256 key for the one-click
// list-unsubscribe URL. Rotating the key invalidates outstanding
// unsubscribe links — operators are expected to keep it stable.
type Deps struct {
	Pool           *pgxpool.Pool
	Logger         *slog.Logger
	EmailSender    email.Sender
	EmailFrom      string
	SiteName       string
	BaseURL        string
	UnsubscribeKey []byte
	// RuleEngine evaluates per-recipient notification routing rules
	// (PRO-EXT01-16a). Nil disables rule evaluation entirely — the
	// fanout still upserts inbox rows + sends emails normally.
	RuleEngine *notifications.Evaluator
}

// Errors surfaced by the orchestrator.
var (
	ErrNotFound       = errors.New("notif: notification not found")
	ErrUnauthorized   = errors.New("notif: not your notification")
	ErrUnsubscribeBad = errors.New("notif: invalid unsubscribe token")
)

// FanoutBatch is the per-tick cap on how many domain_events the
// fan-out worker drains in one call. Bounded so a single tick can't
// monopolize the worker pool when a backlog accumulates (e.g. after
// a deploy that takes the worker offline for a moment).
const FanoutBatch = 200

// StormDampener defaults — one email per recipient per thread per
// 10 minutes (the spec's day-1 lean). Per-recipient absolute cap
// is 100 emails per hour.
const (
	StormPerThreadCap  = 1
	StormPerThreadMins = 10
	StormAbsoluteCap   = 100
	StormAbsoluteMins  = 60
)
