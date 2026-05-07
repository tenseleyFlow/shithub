// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import "net/http"

// healthz returns 200 if the process is alive. No dependency checks.
func healthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte("ok\n"))
}

// readyz returns 200 when the server is ready to take traffic. S00 has no
// dependencies to check; S01 wires Postgres into here, S04 wires storage.
func readyz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte("ready\n"))
}
