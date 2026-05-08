// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"net/http"
	"sync"

	"github.com/tenseleyFlow/shithub/internal/repos/highlight"
)

// chromaCSSHandler serves the runtime-generated Chroma stylesheet at
// /static/css/chroma.css. The CSS is theme-derived (Chroma's `github`
// style) and immutable for the process lifetime, so we cache it once.
//
// Long-cache headers are safe because changing the theme would also
// require a binary upgrade — the operator's restart invalidates the
// upstream cache.
func chromaCSSHandler() http.HandlerFunc {
	var (
		once sync.Once
		body []byte
	)
	return func(w http.ResponseWriter, _ *http.Request) {
		once.Do(func() { body = []byte(highlight.CSS()) })
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=86400, must-revalidate")
		_, _ = w.Write(body)
	}
}
