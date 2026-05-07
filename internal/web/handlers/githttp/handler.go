// SPDX-License-Identifier: AGPL-3.0-or-later

package githttp

import (
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tenseleyFlow/shithub/internal/git/protocol"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

// MountSmartHTTP registers the smart-HTTP routes on r. Caller is
// responsible for placing this group ahead of any conflicting routes
// (specifically, register before the /{owner}/{repo} two-segment route
// from S11 — `.git` URLs would otherwise be eaten by it).
//
// Caller is also responsible for stripping CSRF, response-compression,
// and request-timeout middleware from this group; see web/server.go.
func (h *Handlers) MountSmartHTTP(r chi.Router) {
	r.Get("/{owner}/{repo}.git/info/refs", h.infoRefs)
	r.Post("/{owner}/{repo}.git/git-upload-pack", h.uploadPack)
	r.Post("/{owner}/{repo}.git/git-receive-pack", h.receivePack)
}

// infoRefs handles GET /{owner}/{repo}.git/info/refs?service=git-... .
//
// Per the smart-HTTP protocol the response body has shape:
//
//	001e# service=git-<svc>\n
//	0000
//	<advertise-refs output from git>
//
// We stream the trailing advertise-refs body straight from the
// subprocess so a huge ref set doesn't buffer in memory.
func (h *Handlers) infoRefs(w http.ResponseWriter, r *http.Request) {
	svc, ok := serviceFromQuery(r.URL.Query().Get("service"))
	if !ok {
		http.Error(w, "service query parameter required", http.StatusBadRequest)
		return
	}

	row, allow := h.authorizeForService(w, r, svc)
	if !allow {
		return
	}

	gitDir, err := h.d.RepoFS.RepoPath(chi.URLParam(r, "owner"), row.Name)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "githttp: path", "error", err)
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/x-"+string(svc)+"-advertisement")
	setNoCacheHeaders(w)

	if err := protocol.WriteServiceAdvertisement(w, string(svc)); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "githttp: pkt", "error", err)
		return
	}
	cmd := protocol.Cmd(r.Context(), svc, gitDir, true, nil)
	cmd.Stdout = w
	stderr := protocol.DrainStderr(cmd)
	if err := cmd.Run(); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "githttp: info/refs",
			"error", err, "service", svc, "stderr", string(stderr()))
	}
}

// uploadPack handles POST /{owner}/{repo}.git/git-upload-pack.
// Streams: req.Body → git-upload-pack stdin; git-upload-pack stdout → w.
func (h *Handlers) uploadPack(w http.ResponseWriter, r *http.Request) {
	h.runPack(w, r, protocol.UploadPack)
}

// receivePack handles POST /{owner}/{repo}.git/git-receive-pack.
// Same streaming shape as uploadPack but with the SHITHUB_* env vars
// set so the post-receive hook (S14) can identify the actor.
func (h *Handlers) receivePack(w http.ResponseWriter, r *http.Request) {
	h.runPack(w, r, protocol.ReceivePack)
}

// runPack is the shared body for both POST endpoints.
func (h *Handlers) runPack(w http.ResponseWriter, r *http.Request, svc protocol.Service) {
	row, allow := h.authorizeForService(w, r, svc)
	if !allow {
		return
	}

	gitDir, err := h.d.RepoFS.RepoPath(chi.URLParam(r, "owner"), row.Name)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "githttp: path", "error", err)
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	// Hard cap on the request body. A 2 GiB+1 push reads up to the cap
	// from req.Body and then errors; we surface the read error from
	// the subprocess as a 413 IF we haven't started writing yet.
	body := http.MaxBytesReader(w, r.Body, h.d.MaxPushBytes)
	defer func() { _ = body.Close() }()

	w.Header().Set("Content-Type", "application/x-"+string(svc)+"-result")
	setNoCacheHeaders(w)

	var env []string
	if svc == protocol.ReceivePack {
		env = h.hookEnv(r, row)
	}
	cmd := protocol.Cmd(r.Context(), svc, gitDir, false, env)
	cmd.Stdin = body
	cmd.Stdout = w
	stderr := protocol.DrainStderr(cmd)
	if err := cmd.Run(); err != nil {
		// At this point we may have already written headers + bytes;
		// surfacing 413/500 cleanly is best-effort.
		h.d.Logger.ErrorContext(r.Context(), "githttp: pack",
			"error", err, "service", svc, "stderr", string(stderr()))
	}
}

// authorizeForService resolves the repo + checks visibility/permission.
// Returns the repo row + allow=true on success. On any failure writes
// the appropriate response (404, 401, 403, 410) and returns allow=false.
func (h *Handlers) authorizeForService(w http.ResponseWriter, r *http.Request, svc protocol.Service) (reposdb.Repo, bool) {
	ownerName := chi.URLParam(r, "owner")
	repoName := strings.TrimSuffix(chi.URLParam(r, "repo"), ".git")
	// chi already strips .git for the URL pattern but we defensively
	// trim again — the route is /{owner}/{repo}.git/info/refs, where
	// chi captures `{repo}` WITHOUT the `.git` suffix.

	owner, err := h.uq.GetUserByUsername(r.Context(), h.d.Pool, ownerName)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return reposdb.Repo{}, false
	}
	row, err := h.rq.GetRepoByOwnerUserAndName(r.Context(), h.d.Pool, reposdb.GetRepoByOwnerUserAndNameParams{
		OwnerUserID: pgtype.Int8{Int64: owner.ID, Valid: true},
		Name:        repoName,
	})
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return reposdb.Repo{}, false
	}

	auth, authErr := h.resolveBasicAuth(r.Context(), r.Header.Get("Authorization"))
	requireAuth := svc == protocol.ReceivePack || row.Visibility == reposdb.RepoVisibilityPrivate
	if authErr != nil || (requireAuth && auth.Anonymous) {
		writeChallenge(w)
		return reposdb.Repo{}, false
	}

	// Permission check — inline; S15 replaces this with the policy package.
	// V1: only the owner can read a private repo or write to any repo.
	if !auth.Anonymous && auth.UserID != owner.ID {
		http.Error(w, "forbidden", http.StatusForbidden)
		return reposdb.Repo{}, false
	}
	if svc == protocol.ReceivePack {
		if row.IsArchived {
			writeGitErrorMessage(w, http.StatusForbidden,
				"repository is archived; pushes are disabled")
			return reposdb.Repo{}, false
		}
		if row.DeletedAt.Valid {
			http.Error(w, "gone", http.StatusGone)
			return reposdb.Repo{}, false
		}
	}
	return row, true
}

// hookEnv assembles the SHITHUB_* env vars to thread through git-
// receive-pack into S14's hooks.
func (h *Handlers) hookEnv(r *http.Request, row reposdb.Repo) []string {
	auth, _ := h.resolveBasicAuth(r.Context(), r.Header.Get("Authorization"))
	owner, err := h.uq.GetUserByID(r.Context(), h.d.Pool, ownerIDFromRow(row))
	ownerName := ""
	if err == nil {
		ownerName = owner.Username
	}
	return []string{
		"SHITHUB_USER_ID=" + strconv.FormatInt(auth.UserID, 10),
		"SHITHUB_USERNAME=" + auth.Username,
		"SHITHUB_REPO_ID=" + strconv.FormatInt(row.ID, 10),
		"SHITHUB_REPO_FULL_NAME=" + ownerName + "/" + row.Name,
		"SHITHUB_PROTOCOL=http",
		"SHITHUB_REMOTE_IP=" + clientIP(r),
		"SHITHUB_REQUEST_ID=" + middleware.RequestIDFromContext(r.Context()),
		// PATH must be inherited so the subprocess can find git's helpers.
		"PATH=" + os.Getenv("PATH"),
	}
}

// ownerIDFromRow extracts the user-owner ID; orgs come in S31. Until
// then we trust the XOR check in the migration.
func ownerIDFromRow(row reposdb.Repo) int64 {
	if row.OwnerUserID.Valid {
		return row.OwnerUserID.Int64
	}
	return 0
}

// serviceFromQuery maps the ?service=... value to our typed enum.
func serviceFromQuery(s string) (protocol.Service, bool) {
	switch s {
	case string(protocol.UploadPack):
		return protocol.UploadPack, true
	case string(protocol.ReceivePack):
		return protocol.ReceivePack, true
	}
	return "", false
}

// writeChallenge emits a 401 with the canonical Basic challenge.
func writeChallenge(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Basic realm="shithub"`)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte("authentication required\n"))
}

// writeGitErrorMessage writes a friendly message in the body. For
// non-streamed responses (we haven't started writing the pack stream
// yet) this surfaces in `git push`'s stderr verbatim.
func writeGitErrorMessage(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, msg+"\n")
}

// setNoCacheHeaders matches what the canonical git http-backend emits
// — git endpoints are uncacheable both because the stream is dynamic
// and because intermediaries shouldn't second-guess us.
func setNoCacheHeaders(w http.ResponseWriter) {
	w.Header().Set("Expires", "Fri, 01 Jan 1980 00:00:00 GMT")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Cache-Control", "no-cache, max-age=0, must-revalidate")
}

// clientIP returns the request's source address. RealIP middleware in
// the global stack populates X-Real-IP-style headers; we read those
// first, then fall back to RemoteAddr's host part.
func clientIP(r *http.Request) string {
	if ip := middleware.RealIPFromContext(r.Context(), r); ip != "" {
		return ip
	}
	if i := strings.LastIndex(r.RemoteAddr, ":"); i > 0 {
		return r.RemoteAddr[:i]
	}
	return r.RemoteAddr
}
