// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/infra/config"
	notifhandlers "github.com/tenseleyFlow/shithub/internal/web/handlers/notifications"
	"github.com/tenseleyFlow/shithub/internal/web/render"
)

// buildNotifHandlers wires the S29 notification handlers. Takes the
// process-wide shared renderer (see server.go).
func buildNotifHandlers(
	cfg config.Config,
	pool *pgxpool.Pool,
	rr *render.Renderer,
	logger *slog.Logger,
) (*notifhandlers.Handlers, error) {
	if rr == nil {
		return nil, errors.New("notif: nil renderer")
	}
	return notifhandlers.New(notifhandlers.Deps{
		Logger:         logger,
		Render:         rr,
		Pool:           pool,
		UnsubscribeKey: notifUnsubscribeKey(cfg, logger),
	})
}

// notifUnsubscribeKey mirrors the worker's resolver. Kept here so
// the web binary doesn't have to import the worker bundle just for
// this helper. The dev-fallback derivation is intentional and
// logged loudly — see worker.go for the matching prod hint.
func notifUnsubscribeKey(cfg config.Config, logger *slog.Logger) []byte {
	if cfg.Notif.UnsubscribeKeyB64 != "" {
		k, err := base64.StdEncoding.DecodeString(cfg.Notif.UnsubscribeKeyB64)
		if err == nil && len(k) >= 16 {
			return k
		}
	}
	if cfg.Session.KeyB64 != "" {
		seed, err := base64.StdEncoding.DecodeString(cfg.Session.KeyB64)
		if err == nil && len(seed) > 0 {
			sum := sha256.Sum256(append([]byte("notif-unsub:"), seed...))
			if logger != nil {
				logger.Warn("notif (web): deriving unsubscribe key from session secret (dev fallback)",
					"hint", "set Notif.UnsubscribeKeyB64 in prod")
			}
			return sum[:]
		}
	}
	if logger != nil {
		logger.Warn("notif (web): no key material — using static dev key")
	}
	return []byte("shithub-dev-unsub-static-key-32B")
}
