// SPDX-License-Identifier: AGPL-3.0-or-later

package middleware

import (
	"compress/gzip"
	"io"
	"net/http"
	"strings"
)

// Compress returns middleware that gzip-encodes responses for clients that
// advertise gzip support, bypassing already-compressed responses (images,
// pre-compressed git pack data, etc.). The middleware is intentionally
// modest — chi/Negroni-grade compression suites are overkill here.
//
// Routes that stream (git protocol in S12) must skip this middleware via a
// dedicated route group, since wrapping ResponseWriter breaks Flusher
// guarantees the streaming pack relies on.
func Compress(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !clientAcceptsGzip(r) {
			next.ServeHTTP(w, r)
			return
		}
		gw := &gzipResponseWriter{ResponseWriter: w, req: r}
		defer func() { _ = gw.Close() }()
		next.ServeHTTP(gw, r)
	})
}

func clientAcceptsGzip(r *http.Request) bool {
	enc := r.Header.Get("Accept-Encoding")
	if enc == "" {
		return false
	}
	for _, part := range strings.Split(enc, ",") {
		token := strings.TrimSpace(part)
		if strings.HasPrefix(token, "gzip") {
			return true
		}
	}
	return false
}

type gzipResponseWriter struct {
	http.ResponseWriter
	req     *http.Request
	gz      *gzip.Writer
	wrote   bool
	skipped bool
}

func (g *gzipResponseWriter) WriteHeader(code int) {
	if !g.wrote {
		g.wrote = true
		ct := g.Header().Get("Content-Type")
		if !shouldCompress(ct) {
			g.skipped = true
			g.ResponseWriter.WriteHeader(code)
			return
		}
		g.Header().Set("Content-Encoding", "gzip")
		g.Header().Set("Vary", "Accept-Encoding")
		g.Header().Del("Content-Length")
		g.gz = gzip.NewWriter(g.ResponseWriter)
	}
	g.ResponseWriter.WriteHeader(code)
}

func (g *gzipResponseWriter) Write(b []byte) (int, error) {
	if !g.wrote {
		g.WriteHeader(http.StatusOK)
	}
	if g.skipped || g.gz == nil {
		return g.ResponseWriter.Write(b)
	}
	return g.gz.Write(b)
}

func (g *gzipResponseWriter) Close() error {
	if g.gz != nil {
		return g.gz.Close()
	}
	return nil
}

func (g *gzipResponseWriter) Flush() {
	if g.gz != nil {
		_ = g.gz.Flush()
	}
	if f, ok := g.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// shouldCompress returns true for content types that benefit from gzip.
// Already-compressed binary types (images, fonts, git pack) skip.
func shouldCompress(ct string) bool {
	if ct == "" {
		return true // unknown — let gzip handle it
	}
	ct = strings.ToLower(ct)
	switch {
	case strings.HasPrefix(ct, "text/"):
		return true
	case strings.Contains(ct, "json"):
		return true
	case strings.Contains(ct, "javascript"):
		return true
	case strings.Contains(ct, "xml"):
		return true
	case strings.Contains(ct, "svg"):
		return true
	}
	return false
}

// io.WriteCloser interface assertion — Go won't compile if the contract
// drifts.
var _ io.WriteCloser = (*gzipResponseWriter)(nil)
