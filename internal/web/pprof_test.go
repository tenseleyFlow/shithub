// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPprofHandlerServesProfiles(t *testing.T) {
	h := pprofHandler()
	for _, path := range []string{
		"/debug/pprof/",
		"/debug/pprof/heap?debug=1",
		"/debug/pprof/goroutine?debug=1",
		"/debug/pprof/cmdline",
	} {
		t.Run(path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("GET %s = %d, want 200", path, rec.Code)
			}
			if rec.Body.Len() == 0 {
				t.Fatalf("GET %s returned an empty body", path)
			}
		})
	}
}

// The listener must serve pprof and nothing else — no static assets,
// no app routes, no accidental catch-all.
func TestPprofHandlerServesNothingElse(t *testing.T) {
	h := pprofHandler()
	for _, path := range []string{"/", "/healthz", "/metrics", "/debug/", "/settings"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, rec.Code)
		}
	}
}

func TestNewPprofServerRejectsNonLoopback(t *testing.T) {
	tests := []struct {
		name string
		addr string
	}{
		{"wildcard bind", ":6060"},
		{"all interfaces v4", "0.0.0.0:6060"},
		{"all interfaces v6", "[::]:6060"},
		{"public address", "24.199.108.81:6060"},
		{"private lan address", "10.50.0.2:6060"},
		{"hostname is not resolved", "localhost:6060"},
		{"no port", "127.0.0.1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, err := newPprofServer(tt.addr)
			if err == nil {
				t.Fatalf("newPprofServer(%q) accepted a non-loopback address", tt.addr)
			}
			if srv != nil {
				t.Fatalf("newPprofServer(%q) returned a server alongside the error", tt.addr)
			}
		})
	}
}

func TestNewPprofServerAcceptsLoopback(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:6060", "127.0.0.1:0", "[::1]:6060", "127.1.2.3:6060"} {
		srv, err := newPprofServer(addr)
		if err != nil {
			t.Fatalf("newPprofServer(%q): %v", addr, err)
		}
		if srv == nil {
			t.Fatalf("newPprofServer(%q) returned no server", addr)
		}
	}
}

func TestNewPprofServerDisabledWhenEmpty(t *testing.T) {
	srv, err := newPprofServer("")
	if err != nil {
		t.Fatalf("newPprofServer(\"\"): %v", err)
	}
	if srv != nil {
		t.Fatal("empty web.pprof_addr should disable the listener")
	}
}

// End-to-end over a real loopback socket: this is the thing the
// runbook tells an operator to curl.
func TestStartPprofServesOverLoopback(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	addr := freeLoopbackAddr(t)
	stop, err := startPprof(addr, logger)
	if err != nil {
		t.Fatalf("startPprof(%q): %v", addr, err)
	}
	defer stop()

	resp, err := http.Get("http://" + addr + "/debug/pprof/heap?debug=1")
	if err != nil {
		t.Fatalf("GET heap: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET heap = %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "heap profile") {
		t.Fatalf("heap response does not look like a profile: %.120q", body)
	}
}

func TestStartPprofDisabledIsANoop(t *testing.T) {
	stop, err := startPprof("", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("startPprof(\"\"): %v", err)
	}
	stop()
}

func TestStartPprofRefusesNonLoopback(t *testing.T) {
	_, err := startPprof("0.0.0.0:6060", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err == nil {
		t.Fatal("startPprof accepted a wildcard bind")
	}
}

// freeLoopbackAddr returns a loopback host:port that was free a
// moment ago. Racy in principle, fine in a test process.
func freeLoopbackAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("close reservation: %v", err)
	}
	return addr
}
