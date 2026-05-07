// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"io/fs"
	"net/http"
)

// staticFileServer serves files from the embedded static FS with sane
// caching headers. S02 will replace this with a content-hashed asset
// pipeline; S00 keeps the surface minimal.
func staticFileServer(staticFS fs.FS) http.Handler {
	fileServer := http.FileServer(http.FS(staticFS))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Conservative caching: short TTL + must-revalidate. Long-cache
		// behavior moves to fingerprinted URLs in S02.
		w.Header().Set("Cache-Control", "public, max-age=300, must-revalidate")
		fileServer.ServeHTTP(w, r)
	})
}
