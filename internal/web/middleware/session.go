// SPDX-License-Identifier: AGPL-3.0-or-later

package middleware

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/tenseleyFlow/shithub/internal/auth/session"
)

var (
	sessionKey      = ctxKey{name: "session"}
	sessionStoreKey = ctxKey{name: "session_store"}
)

// SessionLoader returns middleware that loads the session into request
// context before downstream handlers run. Save happens via the store —
// handlers that mutate the session call store.Save explicitly.
func SessionLoader(store session.Store, logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			s, err := store.Load(r)
			if err != nil {
				if logger != nil {
					logger.WarnContext(r.Context(), "session load failed", slog.Any("error", err))
				}
				s = &session.Session{}
			}
			ctx := context.WithValue(r.Context(), sessionKey, s)
			ctx = context.WithValue(ctx, sessionStoreKey, store)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// SessionFromContext returns the loaded session, or an empty Session if
// the loader middleware didn't run (which would itself be a wiring bug).
func SessionFromContext(ctx context.Context) *session.Session {
	if s, ok := ctx.Value(sessionKey).(*session.Session); ok && s != nil {
		return s
	}
	return &session.Session{}
}

// SessionStoreFromContext returns the session store associated with the
// request. Handlers that need to Save / Clear pull it out via this helper.
func SessionStoreFromContext(ctx context.Context) session.Store {
	if s, ok := ctx.Value(sessionStoreKey).(session.Store); ok {
		return s
	}
	return nil
}
