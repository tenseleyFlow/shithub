// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"time"

	"github.com/tenseleyFlow/shithub/internal/infra/config"
)

// pprofHandler serves the runtime profiling endpoints and nothing
// else. Registering them on their own mux (rather than the package
// default one, which net/http/pprof's init() populates) keeps the
// surface explicit and lets the tests assert that a non-pprof path
// 404s.
func pprofHandler() http.Handler {
	mux := http.NewServeMux()
	// Index also serves the named profiles (heap, goroutine, allocs,
	// block, mutex, threadcreate) by trailing path segment.
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	return mux
}

// newPprofServer builds the loopback profiling server, or (nil, nil)
// when the feature is off. The loopback check is a re-check of what
// config.Validate already enforced: this listener is the one thing
// standing between an unauthenticated heap dump and the internet, so
// it does not take the caller's word for it.
//
// No WriteTimeout: `/debug/pprof/profile?seconds=30` and `trace` hold
// the response open for the duration of the collection by design.
func newPprofServer(addr string) (*http.Server, error) {
	if addr == "" {
		return nil, nil
	}
	if err := config.ValidateLoopbackAddr("web.pprof_addr", addr); err != nil {
		return nil, err
	}
	return &http.Server{
		Addr:              addr,
		Handler:           pprofHandler(),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       120 * time.Second,
	}, nil
}

// startPprof starts the profiling listener when configured and
// returns a shutdown func (a no-op when disabled). A bind failure is
// fatal rather than a warning: an operator who set the knob wants the
// profile, and silently not listening is the failure mode that wastes
// the next incident.
func startPprof(addr string, logger *slog.Logger) (func(), error) {
	noop := func() {}
	srv, err := newPprofServer(addr)
	if err != nil {
		return noop, err
	}
	if srv == nil {
		return noop, nil
	}
	ln, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		return noop, fmt.Errorf("pprof: listen %s: %w", srv.Addr, err)
	}
	logger.Info("pprof listener started (loopback only)", "addr", ln.Addr().String())
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("pprof listener stopped", "error", err)
		}
	}()
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}, nil
}
