// SPDX-License-Identifier: AGPL-3.0-or-later

package githttp

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tenseleyFlow/shithub/internal/auth/policy"
	"github.com/tenseleyFlow/shithub/internal/git/protocol"
	"github.com/tenseleyFlow/shithub/internal/orgs"
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

	// Owner can be a user OR an org; orgs.Resolve hits the principals
	// table to dispatch on kind. Mirrors the same lookup the HTML repo
	// handler uses (web/handlers/repo/repo.go::lookupRepoForViewer).
	row, err := h.lookupRepo(r.Context(), ownerName, repoName)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return reposdb.Repo{}, false
	}

	auth, authErr := h.resolveBasicAuth(r.Context(), r.Header.Get("Authorization"))
	repoRef := policy.NewRepoRefFromRepo(row)
	requireAuth := svc == protocol.ReceivePack || repoRef.IsPrivate()
	if authErr != nil || (requireAuth && auth.Anonymous) {
		writeChallenge(w)
		return reposdb.Repo{}, false
	}

	// Build the policy actor and ask Can(). Owner identity, collab role,
	// archived/deleted gates all live in the policy package now.
	var actor policy.Actor
	if auth.Anonymous {
		actor = policy.AnonymousActor()
	} else {
		actor = policy.UserActor(auth.UserID, auth.Username, false, false)
	}
	action := policy.ActionRepoRead
	if svc == protocol.ReceivePack {
		action = policy.ActionRepoWrite
	}
	decision := policy.Can(r.Context(), policy.Deps{Pool: h.d.Pool}, actor, action, repoRef)
	if !decision.Allow {
		switch decision.Code {
		case policy.DenyRepoDeleted:
			http.Error(w, "gone", http.StatusGone)
		case policy.DenyArchived:
			// User sees this directly in their git client.
			writeGitErrorMessage(w, http.StatusForbidden,
				"repository is archived; pushes are disabled")
		case policy.DenyVisibility:
			http.Error(w, "not found", http.StatusNotFound)
		default:
			http.Error(w, "forbidden", http.StatusForbidden)
		}
		return reposdb.Repo{}, false
	}
	return row, true
}

// hookEnv assembles the SHITHUB_* env vars to thread through git-
// receive-pack into S14's hooks.
func (h *Handlers) hookEnv(r *http.Request, row reposdb.Repo) []string {
	auth, _ := h.resolveBasicAuth(r.Context(), r.Header.Get("Authorization"))
	ownerName := h.ownerName(r.Context(), row)
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

// lookupRepo resolves a repo by owner-slug + name, dispatching on
// whether the owner-slug names a user or an org. Returns the same
// row shape regardless of owner kind; pgx.ErrNoRows-equivalent on
// any failure (so the caller writes a 404 without leaking which
// half failed — slug missing vs. repo missing).
func (h *Handlers) lookupRepo(ctx context.Context, ownerName, repoName string) (reposdb.Repo, error) {
	principal, err := orgs.Resolve(ctx, h.d.Pool, ownerName)
	if err != nil {
		return reposdb.Repo{}, err
	}
	switch principal.Kind {
	case orgs.PrincipalUser:
		return h.rq.GetRepoByOwnerUserAndName(ctx, h.d.Pool, reposdb.GetRepoByOwnerUserAndNameParams{
			OwnerUserID: pgtype.Int8{Int64: principal.ID, Valid: true},
			Name:        repoName,
		})
	case orgs.PrincipalOrg:
		return h.rq.GetRepoByOwnerOrgAndName(ctx, h.d.Pool, reposdb.GetRepoByOwnerOrgAndNameParams{
			OwnerOrgID: pgtype.Int8{Int64: principal.ID, Valid: true},
			Name:       repoName,
		})
	default:
		return reposdb.Repo{}, errOwnerKindUnknown
	}
}

// ownerName returns the owner's display slug (username for users,
// slug for orgs). Used to compose SHITHUB_REPO_FULL_NAME for hooks.
// Returns "" on any lookup failure — callers tolerate the empty
// string rather than failing the push because of a metadata gap.
func (h *Handlers) ownerName(ctx context.Context, row reposdb.Repo) string {
	switch {
	case row.OwnerUserID.Valid:
		u, err := h.uq.GetUserByID(ctx, h.d.Pool, row.OwnerUserID.Int64)
		if err != nil {
			return ""
		}
		return u.Username
	case row.OwnerOrgID.Valid:
		o, err := h.oq.GetOrgByID(ctx, h.d.Pool, row.OwnerOrgID.Int64)
		if err != nil {
			return ""
		}
		return o.Slug
	}
	return ""
}

var errOwnerKindUnknown = errors.New("githttp: owner principal kind not user/org")

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
