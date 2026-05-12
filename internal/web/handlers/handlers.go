// SPDX-License-Identifier: AGPL-3.0-or-later

// Package handlers registers HTTP handlers on the web server's mux.
//
// S02 ships the full chi-routed surface plus error pages. Each future
// sprint adds its own routes via this package.
package handlers

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/auth/session"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
	"github.com/tenseleyFlow/shithub/internal/web/render"
)

// Deps holds the dependencies the handlers need. The web package owns the
// embedded filesystems and constructs Deps; this package stays decoupled
// from the embed.FS instances so it remains testable.
type Deps struct {
	Logger       *slog.Logger
	TemplatesFS  fs.FS
	StaticFS     fs.FS
	LogoSVG      string
	SessionStore session.Store
	Pool         *pgxpool.Pool
	// BaseURL is the public scheme+host for canonical crawler URLs
	// (for example https://shithub.sh). Empty falls back to the request
	// host, which keeps tests and local dev working.
	BaseURL string
	// CookieSecure is the Secure flag for session-related cookies
	// (currently the CSRF cookie). Mirrors session.Config.Secure
	// from the loaded config so the CSRF cookie matches the
	// session cookie in TLS deployments. Defaults to false when
	// unset, which is correct for tests and dev (SR2 H6).
	CookieSecure bool
	// ReadyCheck is optionally invoked by /readyz. Returning a non-nil
	// error makes /readyz report 503. If nil, /readyz always reports ready.
	ReadyCheck func(context.Context) error
	// MetricsHandler, when non-nil, is mounted at /metrics. Caller is
	// responsible for any access control (e.g. HTTP Basic auth wrapping).
	MetricsHandler http.Handler
	// AuthMounter, when non-nil, is invoked inside the CSRF-protected
	// route group with the chi.Router so the auth handlers can register
	// signup/login/logout/reset/verify routes.
	AuthMounter func(chi.Router)
	// APIMounter, when non-nil, is invoked inside the CSRF-EXEMPT route
	// group so the API surface (PAT-authenticated, no browser-form
	// posts) can register its routes.
	APIMounter func(chi.Router)
	// AvatarMounter, when non-nil, registers /avatars/{username} on the
	// CSRF-exempt group (avatar GETs are safe and benefit from caching).
	AvatarMounter func(chi.Router)
	// RepoNewMounter, when non-nil, registers /new on the CSRF-protected
	// group. The handler enforces auth itself.
	RepoNewMounter func(chi.Router)
	// RepoHomeMounter, when non-nil, registers /{owner}/{repo} on the
	// CSRF-protected group. Two-segment match doesn't collide with the
	// /{username} catch-all.
	RepoHomeMounter func(chi.Router)
	// RepoLifecycleMounter, when non-nil, registers the danger-zone
	// routes (rename, transfer, archive, visibility, delete, restore,
	// transfer accept/decline/cancel, inbox). All routes are auth-
	// required; the handler enforces policy.Can per route.
	RepoLifecycleMounter func(chi.Router)
	// RepoCodeMounter, when non-nil, registers /tree/* /blob/* /raw/*
	// /find/* under the repo two-segment prefix. Public for read; the
	// handler runs the policy gate per request.
	RepoCodeMounter func(chi.Router)
	// RepoHistoryMounter registers /commits/{ref}, /commit/{sha},
	// /blame/{ref}/{path...}, /commits/{ref}.atom (S18).
	RepoHistoryMounter func(chi.Router)
	// RepoRefsMounter registers /branches, /tags, /compare/* (S20).
	RepoRefsMounter func(chi.Router)
	// RepoSettingsBranchesMounter registers /settings/branches +
	// /settings/default-branch (S20). Auth-required.
	RepoSettingsBranchesMounter func(chi.Router)
	// RepoActionsAPIMounter registers POST/state-changing routes
	// under /{owner}/{repo}/actions/ — currently the
	// workflow_dispatch endpoint (S41b). Auth-required + per-handler
	// repo-write check.
	RepoActionsAPIMounter func(chi.Router)
	// RepoSettingsGeneralMounter registers the General/Access tabs and
	// the deferred-tab placeholders (webhooks, keys, notifications,
	// tags) under /{owner}/{repo}/settings/* (S32). Auth-required.
	RepoSettingsGeneralMounter func(chi.Router)
	// RepoSettingsActionsMounter registers Actions secrets + variables
	// settings under /{owner}/{repo}/settings/* (S41c). Auth-required.
	RepoSettingsActionsMounter func(chi.Router)
	// RepoWebhooksMounter registers the per-repo webhook CRUD +
	// delivery views under /{owner}/{repo}/settings/webhooks/* (S33).
	// Auth-required.
	RepoWebhooksMounter func(chi.Router)
	// RepoIssuesMounter registers /{owner}/{repo}/issues, /labels, and
	// /milestones routes (S21). Reads are public (per-repo policy gate);
	// writes are auth-required.
	RepoIssuesMounter func(chi.Router)
	// RepoPullsMounter registers /{owner}/{repo}/pulls* routes (S22).
	// Same auth shape as issues — reads public, writes auth-required.
	RepoPullsMounter func(chi.Router)
	// RepoSocialMounter registers /{owner}/{repo}/{star,unstar,watch,
	// stargazers,watchers} (S26). Stargazer/watcher GETs are public
	// (subject to repo visibility); the action POSTs require auth.
	RepoSocialMounter func(chi.Router)
	// RepoForkMounter registers /{owner}/{repo}/{fork,sync,forks}
	// (S27). The forks list GET is public; fork + sync POSTs are
	// auth-required.
	RepoForkMounter func(chi.Router)
	// SearchMounter registers /search and /search/quick (S28).
	// Both are public — visibility scoping is done inside the
	// search package via policy.VisibilityPredicate.
	SearchMounter func(chi.Router)
	// NotifInboxMounter registers the per-viewer notification inbox
	// + thread-subscribe + mark-read routes (S29). RequireUser is
	// applied inside the wiring layer because every route in the
	// set is per-recipient.
	NotifInboxMounter func(chi.Router)
	// NotifPublicMounter registers the unauthenticated one-click
	// unsubscribe endpoint (S29). HMAC-signed URL = no session
	// needed, so the route lives in the public group alongside
	// /healthz / /static.
	NotifPublicMounter func(chi.Router)
	// OrgCreateMounter registers /organizations/new + POST
	// /organizations (S30). Wrapped in RequireUser at the wiring
	// layer.
	OrgCreateMounter func(chi.Router)
	// OrgRoutesMounter registers /{org}/people + invite + member
	// management. Reads (people page) are public; mutations are
	// owner-gated inside the handler. Must register BEFORE the
	// /{username} catch-all so the `people` segment matches.
	OrgRoutesMounter func(chi.Router)
	// OrgRepositoriesMounter registers /orgs/{org}/repositories. The
	// GitHub-style /orgs prefix avoids stealing /{user}/repositories
	// from a real user-owned repo named "repositories".
	OrgRepositoriesMounter func(chi.Router)
	// OrgInvitationsMounter registers /invitations/{token} +
	// accept/decline. RequireUser at the wiring layer.
	OrgInvitationsMounter func(chi.Router)
	// AdminMounter, when non-nil, registers /admin/* routes (S34).
	// The mounter wraps the handler chain in RequireUser +
	// RequireSiteAdmin so non-admins receive 404, not 403.
	AdminMounter func(chi.Router)
	// GitHTTPMounter, when non-nil, registers the smart-HTTP git routes
	// (`*.git/info/refs`, `git-upload-pack`, `git-receive-pack`). MUST
	// land in a route group that bypasses CSRF, response compression,
	// and the global request timeout — git generates its own pack
	// format, uses HTTP Basic, and clones can run for many minutes.
	GitHTTPMounter func(chi.Router)
	// ProfileMounter, when non-nil, registers the /{username} catch-all
	// route. MUST run last in its group — chi matches in registration
	// order, and {username} swallows everything else.
	ProfileMounter func(chi.Router)
}

// panicHandler implements middleware.PanicHandler. The recover middleware
// invokes it when a downstream handler panics; we render the styled 500
// page through the registered renderer.
type panicHandler struct {
	render *render.Renderer
}

func (h *panicHandler) HandlePanic(w http.ResponseWriter, r *http.Request, _ string, _ any) {
	h.render.HTTPError(w, r, http.StatusInternalServerError, "")
}

// RegisterChi wires every S02 route into r. Returns the chi.Router (for
// further wiring), a panic handler that the caller installs in the
// recover middleware, and a NotFound handler for the catch-all.
func RegisterChi(r *chi.Mux, deps Deps) (*chi.Mux, middleware.PanicHandler, http.HandlerFunc, error) {
	if deps.Logger == nil {
		return nil, nil, nil, fmt.Errorf("handlers.RegisterChi: nil Logger")
	}
	if deps.TemplatesFS == nil {
		return nil, nil, nil, fmt.Errorf("handlers.RegisterChi: nil TemplatesFS")
	}
	if deps.StaticFS == nil {
		return nil, nil, nil, fmt.Errorf("handlers.RegisterChi: nil StaticFS")
	}

	rr, err := render.New(deps.TemplatesFS, render.Options{
		Octicons: render.BuiltinOcticons(),
	})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("renderer: %w", err)
	}

	csrf := middleware.CSRF(middleware.CSRFConfig{
		// SR2 H6: session-cookie Secure flag mirrors here so TLS
		// deployments don't accept the CSRF cookie over plaintext.
		Secure: deps.CookieSecure,
		FailureHandler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rr.HTTPError(w, r, http.StatusForbidden, "csrf")
		}),
	})

	// /metrics MUST NOT pass through Compress: Prometheus scrapers
	// (Alloy 1.16, vmagent, …) advertise Accept-Encoding: gzip but
	// mis-handle Content-Encoding: gzip on the response, parsing the
	// raw 0x1f magic byte as text and failing the scrape (up=0).
	// Mount it on the bare router so only the global middleware
	// (request_id, access_log, metrics, secure_headers) applies.
	if deps.MetricsHandler != nil {
		r.Handle("/metrics", deps.MetricsHandler)
	}

	// Static and health endpoints are CSRF-exempt; everything else passes
	// through the CSRF wrapper for state-changing methods.
	r.Group(func(r chi.Router) {
		r.Use(middleware.Compress)
		r.Use(middleware.Timeout(30 * time.Second))
		r.Handle("/static/*", http.StripPrefix("/static/", staticFileServer(deps.StaticFS)))
		crawlers := crawlerHandler{baseURL: deps.BaseURL}
		r.Get("/robots.txt", crawlers.serveRobots)
		r.Get("/sitemap.xml", crawlers.serveSitemap)
		// S17: Chroma highlight CSS is generated at runtime from the
		// theme; serve under /static/css/chroma.css so the layout can
		// link it without a build step.
		r.Get("/static/css/chroma.css", chromaCSSHandler())
		// HEAD honored alongside GET so strict probes (HEAD-only health
		// checks, some Kubernetes-style livenessProbes) get 200 not 405
		// (SR2 L8).
		r.Get("/healthz", healthz)
		r.Head("/healthz", healthz)
		r.Handle("/readyz", readinessHandler(deps.ReadyCheck, deps.Logger))
		if deps.APIMounter != nil {
			deps.APIMounter(r)
		}
		if deps.AvatarMounter != nil {
			deps.AvatarMounter(r)
		}
		// One-click unsubscribe lands in the public group (no CSRF,
		// no session) — RFC 8058 mailers click it from arbitrary
		// agents.
		if deps.NotifPublicMounter != nil {
			deps.NotifPublicMounter(r)
		}
	})

	// Smart-HTTP git routes get their own group: NO CSRF (HTTP Basic
	// flow, no browser form posts), NO response compression (git emits
	// its own pack format), and NO global request timeout (long clones
	// run for minutes). The global SecureHeaders / RealIP / RequestID
	// stack still applies; everything else is per-group.
	if deps.GitHTTPMounter != nil {
		r.Group(func(r chi.Router) {
			deps.GitHTTPMounter(r)
		})
	}

	// Application routes — CSRF protected. Compress + Timeout live in
	// this group (and the static one above) rather than globally so the
	// git-HTTP group can opt out.
	r.Group(func(r chi.Router) {
		r.Use(middleware.Compress)
		r.Use(middleware.Timeout(30 * time.Second))
		r.Use(csrf)
		marketing := marketingHandler{render: rr, baseURL: deps.BaseURL, logger: deps.Logger}
		r.Get("/", helloHandler{render: rr, logoSVG: deps.LogoSVG, baseURL: deps.BaseURL, logger: deps.Logger}.ServeHTTP)
		r.Get("/about", marketing.serveAbout)
		r.Get("/explore", exploreHandler{render: rr, logger: deps.Logger, pool: deps.Pool}.ServeExplore)
		r.Get("/trending", exploreHandler{render: rr, logger: deps.Logger, pool: deps.Pool}.ServeTrending)
		globalNavH := globalNavHandler{render: rr, logger: deps.Logger, pool: deps.Pool}
		r.Group(func(r chi.Router) {
			r.Use(middleware.RequireUser)
			r.Get("/issues", globalNavH.RedirectIssues)
			r.Get("/issues/new", globalNavH.ServeNewIssue)
			r.Get("/issues/{view}", globalNavH.ServeIssues)
			r.Get("/pulls", globalNavH.ServePulls)
			r.Get("/repos", globalNavH.ServeRepos)
		})
		// /internal/panic is a dev affordance: GET it to trigger the
		// panic-recovery path so an operator can confirm the styled 500
		// page renders. S35 will gate this behind a dev flag.
		r.Get("/internal/panic", panicTrigger)
		if deps.AuthMounter != nil {
			deps.AuthMounter(r)
		}
		if deps.RepoNewMounter != nil {
			deps.RepoNewMounter(r)
		}
		// Code-tab + history routes register BEFORE RepoHome's two-segment
		// route so /{owner}/{repo}/tree/* and /commit/* don't get
		// swallowed.
		if deps.RepoCodeMounter != nil {
			deps.RepoCodeMounter(r)
		}
		if deps.RepoHistoryMounter != nil {
			deps.RepoHistoryMounter(r)
		}
		if deps.RepoRefsMounter != nil {
			deps.RepoRefsMounter(r)
		}
		if deps.RepoSettingsBranchesMounter != nil {
			deps.RepoSettingsBranchesMounter(r)
		}
		if deps.RepoActionsAPIMounter != nil {
			deps.RepoActionsAPIMounter(r)
		}
		if deps.RepoSettingsGeneralMounter != nil {
			deps.RepoSettingsGeneralMounter(r)
		}
		if deps.RepoSettingsActionsMounter != nil {
			deps.RepoSettingsActionsMounter(r)
		}
		// Webhooks (S33) — register BEFORE the general mounter so the
		// /settings/webhooks GET resolves to the new CRUD list, not
		// any stale placeholder.
		if deps.RepoWebhooksMounter != nil {
			deps.RepoWebhooksMounter(r)
		}
		if deps.RepoIssuesMounter != nil {
			deps.RepoIssuesMounter(r)
		}
		if deps.RepoPullsMounter != nil {
			deps.RepoPullsMounter(r)
		}
		if deps.RepoSocialMounter != nil {
			deps.RepoSocialMounter(r)
		}
		if deps.RepoForkMounter != nil {
			deps.RepoForkMounter(r)
		}
		if deps.SearchMounter != nil {
			deps.SearchMounter(r)
		}
		if deps.NotifInboxMounter != nil {
			deps.NotifInboxMounter(r)
		}
		if deps.OrgCreateMounter != nil {
			deps.OrgCreateMounter(r)
		}
		if deps.OrgInvitationsMounter != nil {
			deps.OrgInvitationsMounter(r)
		}
		// /{org}/people MUST register before /{username} catch-all
		// so the explicit `people` segment matches first.
		if deps.OrgRoutesMounter != nil {
			deps.OrgRoutesMounter(r)
		}
		if deps.OrgRepositoriesMounter != nil {
			deps.OrgRepositoriesMounter(r)
		}
		if deps.RepoHomeMounter != nil {
			deps.RepoHomeMounter(r)
		}
		// Lifecycle danger-zone + transfers + restore. Order: after
		// RepoHome so explicit settings paths are matched first, before
		// Profile's /{username} catch-all.
		if deps.AdminMounter != nil {
			deps.AdminMounter(r)
		}
		if deps.RepoLifecycleMounter != nil {
			deps.RepoLifecycleMounter(r)
		}
		// Profile is registered LAST so /{username} doesn't shadow any
		// static top-level route.
		if deps.ProfileMounter != nil {
			deps.ProfileMounter(r)
		}
	})

	notFound := func(w http.ResponseWriter, r *http.Request) {
		rr.HTTPError(w, r, http.StatusNotFound, r.URL.Path)
	}

	return r, &panicHandler{render: rr}, notFound, nil
}

// Register is preserved for the existing test suite that exercises the
// surface without bringing up the full server. Internally it wraps
// RegisterChi and mounts the chi router on mux.
func Register(mux *http.ServeMux, deps Deps) error {
	r := chi.NewRouter()
	_, _, notFound, err := RegisterChi(r, deps)
	if err != nil {
		return err
	}
	r.NotFound(notFound)
	mux.Handle("/", r)
	return nil
}

// panicTrigger panics on demand to exercise the recover middleware.
func panicTrigger(_ http.ResponseWriter, _ *http.Request) {
	panic("S02 panic trigger: this is intentional")
}
