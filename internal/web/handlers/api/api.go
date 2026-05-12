// SPDX-License-Identifier: AGPL-3.0-or-later

// Package api owns shithub's PAT-authenticated HTTP API surface. S08
// ships exactly one route: GET /api/v1/user. Later sprints (S22 PRs,
// S30 orgs, …) extend the surface here.
//
// Routes registered here run inside the CSRF-EXEMPT group of the chi
// router. Auth is provided by the PATAuth middleware: no PAT → 401;
// PAT lacking the required scope → 403; valid scoped PAT → 200.
package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/auth/audit"
	"github.com/tenseleyFlow/shithub/internal/auth/pat"
	"github.com/tenseleyFlow/shithub/internal/auth/runnerjwt"
	"github.com/tenseleyFlow/shithub/internal/auth/sealbox"
	"github.com/tenseleyFlow/shithub/internal/auth/secretbox"
	"github.com/tenseleyFlow/shithub/internal/auth/throttle"
	"github.com/tenseleyFlow/shithub/internal/infra/storage"
	"github.com/tenseleyFlow/shithub/internal/ratelimit"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/handlers/api/apilimit"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
	"github.com/tenseleyFlow/shithub/internal/webhook"
)

// Deps is the wiring the API handlers need. Constructed by the web
// package and injected at registration time.
type Deps struct {
	Pool        *pgxpool.Pool
	Debouncer   *pat.Debouncer
	Logger      *slog.Logger
	ObjectStore storage.ObjectStore
	RepoFS      *storage.RepoFS
	RunnerJWT   *runnerjwt.Signer
	SecretBox   *secretbox.Box
	// SecretsBox holds the X25519 keypair used by the
	// `/actions/secrets/public-key` endpoint to encrypt secret values
	// in transit. Distinct from `SecretBox` (the symmetric at-rest
	// AEAD key shared with the runner). Nil disables the secrets
	// REST surface.
	SecretsBox  *sealbox.Box
	RateLimiter *ratelimit.Limiter
	// Audit records security-sensitive mutations (repo create/delete,
	// settings changes). Required for any handler that mutates server
	// state; nil disables the audit emission for that handler.
	Audit *audit.Recorder
	// Throttle is the per-actor anti-abuse limiter consulted by
	// repos.Create. Independent from RateLimiter (which is the
	// shared-counter rate-limit subsystem) — Throttle uses the older
	// per-action counter that the HTML create flow shares with us so
	// budgets are observed consistently across both surfaces.
	Throttle *throttle.Limiter
	// ShithubdPath is the absolute path of the running shithubd
	// binary, forwarded to repos.Create so its hook shims resolve
	// correctly. Empty disables hook installation (test fixtures).
	ShithubdPath string
	// BaseURL is the public scheme://host prefix used for absolute
	// pagination Link headers. Empty falls back to path-relative URLs.
	BaseURL string
	// APILimit configures the /api/v1/* rate-limit middleware. Zero
	// values inherit apilimit.Middleware's no-op fallback.
	APILimit apilimit.Config
	// WebhookSSRF is the SSRF policy applied at webhook create/update.
	// Defaults to webhook.DefaultSSRFConfig() when zero; tests override
	// to allow loopback URLs.
	WebhookSSRF webhook.SSRFConfig
}

// Handlers is the registered API handler set. Construct with New.
type Handlers struct {
	d Deps
	q *usersdb.Queries
}

// New constructs the handler set, validating Deps.
func New(d Deps) (*Handlers, error) {
	if d.Pool == nil {
		return nil, errors.New("api: nil Pool")
	}
	if d.Debouncer == nil {
		d.Debouncer = pat.NewDebouncer(0)
	}
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	return &Handlers{d: d, q: usersdb.New()}, nil
}

// apiMaxBodyBytes caps the request body for any /api/v1 handler. The
// largest documented payload today is a check-run create with a 64 KiB
// summary + 64 KiB text — comfortably below this. Tightening per-route
// is fine; widening should happen at the route group, not here.
//
// The auth-side cap (signup/login/reset) is a separate, lower limit
// (`MaxBodySize(8 KiB)`) wired in `auth_wiring.go`. This cap defends
// the same surface against a misbehaving PAT-bearing client shipping
// a 50 MB JSON blob to weaponize the parser.
const apiMaxBodyBytes = 256 * 1024

// runnerAPIMaxBodyBytes must fit a 512 KiB raw log chunk after base64
// expansion plus JSON framing.
const runnerAPIMaxBodyBytes = 768 * 1024

// Mount registers /api/v1/* on r. Caller is responsible for putting r
// in a CSRF-exempt group.
//
// Outer middleware on every /api/v1/* request: apilimit stamps the
// X-RateLimit-* headers and refuses over-budget callers with a JSON
// 429. Inner groups attach body caps, PAT auth, and scope decorators
// according to the surface they expose.
func (h *Handlers) Mount(r chi.Router) {
	apiLimitMW := apilimit.Middleware(h.d.RateLimiter, apilimit.Config{
		AuthedPerHour: h.d.APILimit.AuthedPerHour,
		AnonPerHour:   h.d.APILimit.AnonPerHour,
		Logger:        h.d.Logger,
	})
	r.Group(func(r chi.Router) {
		r.Use(middleware.MaxBodySize(runnerAPIMaxBodyBytes))
		h.mountRunners(r)
	})
	r.Group(func(r chi.Router) {
		r.Use(middleware.MaxBodySize(apiMaxBodyBytes))
		r.Use(middleware.PATAuthMiddleware(middleware.PATConfig{
			Pool:      h.d.Pool,
			Debouncer: h.d.Debouncer,
		}))
		r.Use(apiLimitMW)
		// /meta is capability discovery — no scope required, anon ok.
		h.mountMeta(r)
		r.Group(func(r chi.Router) {
			r.Use(middleware.RequireScope(pat.ScopeUserRead))
			r.Get("/api/v1/user", h.userMe)
		})
		// S24 check-runs / check-suites — RequireScope is per-route
		// inside the helper since reads need repo:read but writes need
		// repo:write.
		h.mountChecks(r)
		// S41g Actions lifecycle controls.
		h.mountActionsLifecycle(r)
		// S26 stars: PUT/DELETE need user:write, GET needs user:read.
		h.mountStars(r)
		// S50 §1 — user emails (read-only over REST).
		h.mountUserEmails(r)
		// S50 §1 — user SSH keys CRUD.
		h.mountUserKeys(r)
		// S50 §2 — repos REST core (list/single/create/patch/delete).
		h.mountRepos(r)
		// S50 §3 — issues + comments + lock.
		h.mountIssues(r)
		// S50 §3 — repo labels CRUD.
		h.mountLabels(r)
		// S50 §3 follow-up — milestones CRUD.
		h.mountMilestones(r)
		// S50 §3 follow-up — assignees suggestion.
		h.mountAssignees(r)
		// S50 §4 — pull requests core (list/get/create/patch/merge).
		h.mountPulls(r)
		// S50 §4b — pull-request reviews + inline comments + requested
		// reviewers.
		h.mountPullReviews(r)
		// S50 §5 — search (repositories, issues, code).
		h.mountSearch(r)
		// S50 §7 — orgs.
		h.mountOrgs(r)
		// S50 §8 — webhooks.
		h.mountHooks(r)
		// S50 §9 — branches + tags (read-only).
		h.mountBranches(r)
		// S50 §10 — repo collaborators CRUD.
		h.mountCollaborators(r)
		// S50 §11 — commits (read-only).
		h.mountCommits(r)
		// S50 §12 — repo contents (file/dir at ref).
		h.mountContents(r)
		// S50 §13 — repo forks (list + create).
		h.mountForks(r)
		// S50 §14 — notifications inbox.
		h.mountNotifications(r)
		// S50 §15 — watching / subscriptions.
		h.mountWatching(r)
		// S50 §16 — events / activity feed.
		h.mountEvents(r)
		// S50 §17 — followers / following.
		h.mountFollowers(r)
		// S50 §18 — actions workflow runs / jobs.
		h.mountActionsRuns(r)
		// S50 §19 — stargazers + per-user starred lists.
		h.mountStargazers(r)
		// S50 §20 — issue events / timeline (read-only).
		h.mountIssueEvents(r)
		// S50 §13 — actions workflows list/get + workflow_dispatch.
		h.mountActionsWorkflows(r)
		// S50 §13 — workflow enable/disable + run delete + artifacts + job logs.
		h.mountActionsLifecycleREST(r)
	})
}

// userResponse is the public shape of a user record. Mirrors GitHub's
// /user response in spirit; we'll grow it organically as features land.
type userResponse struct {
	ID        int64  `json:"id"`
	Username  string `json:"username"`
	Name      string `json:"name,omitempty"`
	Verified  bool   `json:"email_verified"`
	CreatedAt string `json:"created_at"`
}

func (h *Handlers) userMe(w http.ResponseWriter, r *http.Request) {
	auth := middleware.PATAuthFromContext(r.Context())
	if auth.UserID == 0 {
		writeAPIError(w, http.StatusUnauthorized, "unauthenticated")
		return
	}
	user, err := h.q.GetUserByID(r.Context(), h.d.Pool, auth.UserID)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "user not found")
		return
	}
	writeJSON(w, http.StatusOK, userResponse{
		ID:        user.ID,
		Username:  user.Username,
		Name:      user.DisplayName,
		Verified:  user.EmailVerified,
		CreatedAt: user.CreatedAt.Time.Format("2006-01-02T15:04:05Z07:00"),
	})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeAPIError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
