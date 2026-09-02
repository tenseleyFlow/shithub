// SPDX-License-Identifier: AGPL-3.0-or-later

// Package web boots the shithub HTTP server. The full middleware stack
// (recover, request_id, logging, real-IP, timeout, compress, secure
// headers, CSRF, session, CORS, metrics, tracing), the chi router, the
// session store, the styled error pages, and the observability sinks
// (logging, metrics, tracing, error reporting) are composed here.
package web

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/chacha20poly1305"

	"github.com/tenseleyFlow/shithub/internal/auth/runnerjwt"
	"github.com/tenseleyFlow/shithub/internal/auth/secretbox"
	"github.com/tenseleyFlow/shithub/internal/auth/session"
	"github.com/tenseleyFlow/shithub/internal/cache/pagecache"
	"github.com/tenseleyFlow/shithub/internal/infra/config"
	"github.com/tenseleyFlow/shithub/internal/infra/db"
	"github.com/tenseleyFlow/shithub/internal/infra/errrep"
	infralog "github.com/tenseleyFlow/shithub/internal/infra/log"
	"github.com/tenseleyFlow/shithub/internal/infra/metrics"
	"github.com/tenseleyFlow/shithub/internal/infra/tracing"
	"github.com/tenseleyFlow/shithub/internal/ratelimit"
	"github.com/tenseleyFlow/shithub/internal/version"
	"github.com/tenseleyFlow/shithub/internal/web/handlers"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
	"github.com/tenseleyFlow/shithub/internal/web/render"
)

// Options configures the web server. Addr overrides config when non-empty
// (preserves the existing --addr CLI flag behavior).
type Options struct {
	Addr string
}

// Run boots the web server and blocks until shutdown.
func Run(ctx context.Context, opts Options) error {
	cfg, err := config.Load(nil)
	if err != nil {
		return err
	}
	if opts.Addr != "" {
		cfg.Web.Addr = opts.Addr
	}

	logger := infralog.New(infralog.Options{
		Level:  cfg.Log.Level,
		Format: cfg.Log.Format,
		Writer: os.Stderr,
	})

	// Error reporting (no-op when DSN empty).
	flushErrRep, err := errrep.Init(errrep.Config{
		DSN:         cfg.ErrorReporting.DSN,
		Environment: cfg.ErrorReporting.Environment,
		Release:     cfg.ErrorReporting.Release,
	})
	if err != nil {
		return fmt.Errorf("errrep: %w", err)
	}
	defer func() { _ = flushErrRep(context.Background()) }()
	if cfg.ErrorReporting.DSN != "" {
		// Wrap the slog handler so error-level records are reported.
		// We rebuild the logger so every component that pulls it from
		// here gets the wrapped chain.
		logger = slog.New(&errrep.SlogHandler{Inner: logger.Handler()})
	}

	// Tracing (no-op when disabled).
	flushTracing, err := tracing.Init(ctx, tracing.Config{
		Enabled:     cfg.Tracing.Enabled,
		Endpoint:    cfg.Tracing.Endpoint,
		SampleRate:  cfg.Tracing.SampleRate,
		ServiceName: cfg.Tracing.ServiceName,
	})
	if err != nil {
		return fmt.Errorf("tracing: %w", err)
	}
	defer func() { _ = flushTracing(context.Background()) }()

	// pprof on its own loopback listener (web.pprof_addr; empty =
	// disabled). Deliberately not mounted on the main router — see
	// internal/web/pprof.go and runbooks/observability.md.
	stopPprof, err := startPprof(cfg.Web.PprofAddr, logger)
	if err != nil {
		return err
	}
	defer stopPprof()

	logoBytes, err := LogoSVG()
	if err != nil {
		return fmt.Errorf("load logo: %w", err)
	}

	sessionStore, err := buildSessionStore(cfg.Session, logger)
	if err != nil {
		return err
	}

	// Optional DB pool (carried over from S01); now driven by config.
	var pool *pgxpool.Pool
	if cfg.DB.URL != "" {
		//nolint:gosec // G115: max_conns is operator-configured with small numeric values (typ. 10–100).
		p, err := db.Open(ctx, db.Config{
			URL:            cfg.DB.URL,
			MaxConns:       int32(cfg.DB.MaxConns),
			MinConns:       int32(cfg.DB.MinConns),
			ConnectTimeout: cfg.DB.ConnectTimeout,
		})
		if err != nil {
			logger.Warn("db: open failed; /readyz will report unhealthy", "error", err)
		} else {
			pool = p
			defer p.Close()
			metrics.ObserveDBPool(ctx, pool, 10*time.Second)
			metrics.ObserveActions(ctx, pool, 15*time.Second)
			metrics.ObserveBilling(ctx, pool, 15*time.Second)
		}
	}

	// Resolved at boot so a malformed CIDR is a startup failure, not a
	// silently-disabled proxy trust list. Validate() already parsed
	// these once; this is the copy the middleware keeps.
	trustedProxies, err := cfg.Web.TrustedProxyNets()
	if err != nil {
		return err
	}
	if len(trustedProxies) == 0 {
		logger.Warn("web: no trusted proxies configured; X-Forwarded-For is ignored",
			"hint", "set SHITHUB_WEB__TRUSTED_PROXIES when a reverse proxy fronts shithub, or every client shares one rate-limit bucket")
	}

	r := chi.NewRouter()

	// Middleware stack — outermost first.
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP(middleware.RealIPConfig{TrustedProxies: trustedProxies}))
	r.Use(middleware.AccessLog(logger))
	r.Use(middleware.Metrics)
	if cfg.Tracing.Enabled {
		r.Use(tracing.Middleware)
	}
	r.Use(middleware.SecureHeaders(middleware.DefaultSecureHeaders()))
	// Compress + Timeout are NOT global: the smart-HTTP git routes need
	// to stream uncompressed pack data for many minutes. RegisterChi
	// applies them inside the CSRF-exempt and CSRF-protected groups but
	// skips the git group.
	r.Use(middleware.SessionLoader(sessionStore, logger))
	if pool != nil {
		r.Use(middleware.OptionalUser(usernameLookup(pool)))
	}
	r.Use(middleware.PolicyCache())
	r.Use(middleware.EntitlementsCache())

	// One renderer per process. render.New parses every page template
	// with all partials cloned in, which costs ~83 MB of live heap; the
	// pre-2026-09 wiring built one per handler set and carried ~660 MB
	// of static heap, which is what OOM-killed shithubd on the 4 GB
	// box. Every handler builder below takes this instance — do not
	// call render.New again in the web wiring. See
	// docs/internal/caching.md, "Renderer invariant".
	sharedRenderer, err := render.New(TemplatesFS(), render.Options{
		Octicons: render.BuiltinOcticons(),
	})
	if err != nil {
		return fmt.Errorf("renderer: %w", err)
	}

	deps := handlers.Deps{
		Logger:       logger,
		TemplatesFS:  TemplatesFS(),
		Renderer:     sharedRenderer,
		StaticFS:     StaticFS(),
		LogoSVG:      string(logoBytes),
		SessionStore: sessionStore,
		Pool:         pool,
		BaseURL:      cfg.Auth.BaseURL,
		CookieSecure: cfg.Session.Secure,
	}
	if pool != nil {
		deps.ReadyCheck = func(ctx context.Context) error { return pool.Ping(ctx) }
	}
	if cfg.Metrics.Enabled {
		deps.MetricsHandler = metrics.Handler(cfg.Metrics.BasicAuthUser, cfg.Metrics.BasicAuthPass)
	}

	if pool != nil {
		objectStore, err := buildObjectStore(cfg.Storage.S3, logger)
		if err != nil {
			return fmt.Errorf("object store: %w", err)
		}

		auth, err := buildAuthHandlers(cfg, pool, sessionStore, objectStore, logger, sharedRenderer)
		if err != nil {
			return fmt.Errorf("auth handlers: %w", err)
		}
		deps.AuthMounter = auth.Mount
		deps.DeviceCodeAPIMounter = auth.MountDeviceCodeAPI

		var (
			runnerJWT  *runnerjwt.Signer
			actionsBox *secretbox.Box
		)
		if cfg.Auth.TOTPKeyB64 != "" {
			runnerJWT, err = runnerjwt.NewFromTOTPKeyB64(cfg.Auth.TOTPKeyB64)
			if err != nil {
				return fmt.Errorf("runner jwt: %w", err)
			}
			actionsBox, err = secretbox.FromBase64(cfg.Auth.TOTPKeyB64)
			if err != nil {
				return fmt.Errorf("actions secretbox: %w", err)
			}
		} else {
			logger.Warn("actions runner API disabled: auth.totp_key_b64 is not configured",
				"hint", "set SHITHUB_TOTP_KEY=$(openssl rand -base64 32) to enable runner job JWTs")
		}
		// Hoist the limiter so the HTML middleware (F02) and the
		// /api/v1 surface share one Limiter wrapper. The state itself
		// lives in Postgres and is keyed by scope, so the two
		// surfaces are independent budget-wise even though they share
		// the same Go object.
		rateLimiter := ratelimit.New(pool)
		api, err := buildAPIHandlers(cfg, pool, objectStore, runnerJWT, actionsBox, rateLimiter, logger)
		if err != nil {
			return fmt.Errorf("api handlers: %w", err)
		}
		deps.APIMounter = api.Mount
		deps.HTMLRateLimit = middleware.HTMLRateLimit(rateLimiter, middleware.HTMLRateLimitConfig{
			AnonBurst:    cfg.RateLimit.HTML.AnonBurst,
			AnonRefill:   cfg.RateLimit.HTML.AnonRefill,
			AuthedBurst:  cfg.RateLimit.HTML.AuthedBurst,
			AuthedRefill: cfg.RateLimit.HTML.AuthedRefill,
			Logger:       logger,
		})

		profile, err := buildProfileHandlers(cfg, pool, objectStore, sharedRenderer, logger)
		if err != nil {
			return fmt.Errorf("profile handlers: %w", err)
		}
		deps.AvatarMounter = profile.MountAvatars
		deps.ProfileMounter = profile.MountProfile
		deps.OrgRepositoriesMounter = profile.MountOrgRepositories

		repoH, err := buildRepoHandlers(cfg, pool, objectStore, sharedRenderer, logger)
		if err != nil {
			return fmt.Errorf("repo handlers: %w", err)
		}
		// F01 PR-4: subscribe the in-process commits LRU to the
		// pagecache invalidation channel. The worker's push:process
		// job publishes when a push lands; the listener calls
		// InvalidateBranch on the cache held by repoH. Owned by the
		// server lifecycle ctx so a clean shutdown stops the
		// listener too.
		if cache := repoH.CommitsPageCache(); cache != nil {
			go pagecache.Listen(ctx, pool, func(repoID int64, oid string) {
				cache.InvalidateBranch(repoID, oid)
			}, logger)
		}
		// /new is wrapped in RequireUser — it requires a logged-in caller.
		deps.RepoNewMounter = func(r chi.Router) {
			r.Group(func(r chi.Router) {
				r.Use(middleware.RequireUser)
				repoH.MountNew(r)
			})
		}
		deps.RepoHomeMounter = repoH.MountRepoHome
		deps.RepoActionsStreamMounter = repoH.MountRepoActionsStreams
		deps.RepoCodeMounter = repoH.MountCode
		deps.RepoHistoryMounter = repoH.MountHistory
		deps.RepoRefsMounter = repoH.MountRefs
		deps.RepoSettingsBranchesMounter = func(r chi.Router) {
			r.Group(func(r chi.Router) {
				r.Use(middleware.RequireUser)
				repoH.MountSettingsBranches(r)
			})
		}
		deps.RepoActionsAPIMounter = func(r chi.Router) {
			r.Group(func(r chi.Router) {
				r.Use(middleware.RequireUser)
				repoH.MountRepoActionsAPI(r)
			})
		}
		deps.RepoSettingsGeneralMounter = func(r chi.Router) {
			r.Group(func(r chi.Router) {
				r.Use(middleware.RequireUser)
				repoH.MountSettingsGeneral(r)
			})
		}
		deps.RepoSettingsActionsMounter = func(r chi.Router) {
			r.Group(func(r chi.Router) {
				r.Use(middleware.RequireUser)
				repoH.MountSettingsActions(r)
			})
		}
		deps.RepoWebhooksMounter = func(r chi.Router) {
			r.Group(func(r chi.Router) {
				r.Use(middleware.RequireUser)
				repoH.MountWebhooks(r)
			})
		}
		// Issues GETs are public (subject to policy.Can), POSTs require
		// auth. The handler enforces auth + policy per request, so we
		// register the whole surface in the public group; an unauth
		// POST hits the policy gate and 404s out of the existence-leak
		// path. Browser flows still need RequireUser to redirect-to-login,
		// so the POST routes get wrapped through the same group with
		// RequireUser inserted only for state-mutating verbs.
		deps.RepoIssuesMounter = repoH.MountIssues
		deps.RepoPullsMounter = repoH.MountPulls
		deps.RepoSocialMounter = repoH.MountSocial
		deps.RepoForkMounter = repoH.MountFork

		// Search gets its own Limiter wired around /search +
		// /search/quick (audit 2026-05-10 H4). Independent instance
		// from auth's RateLimiter; both share DB-backed counter
		// state, segregated by Policy.Scope.
		searchH, err := buildSearchHandlers(pool, sharedRenderer, logger, ratelimit.New(pool), cfg.Billing.Enforce)
		if err != nil {
			return fmt.Errorf("search handlers: %w", err)
		}
		deps.SearchMounter = searchH.Mount

		notifH, err := buildNotifHandlers(cfg, pool, sharedRenderer, logger)
		if err != nil {
			return fmt.Errorf("notif handlers: %w", err)
		}
		deps.NotifInboxMounter = func(r chi.Router) {
			r.Group(func(r chi.Router) {
				r.Use(middleware.RequireUser)
				notifH.MountAuthed(r)
			})
		}
		deps.NotifPublicMounter = notifH.MountPublic

		// S30 — orgs.
		orgH, err := buildOrgHandlers(cfg, pool, objectStore, sharedRenderer, logger)
		if err != nil {
			return fmt.Errorf("org handlers: %w", err)
		}
		if cfg.Billing.Enabled {
			deps.BillingWebhookMounter = orgH.MountBillingWebhook
		}
		deps.OrgCreateMounter = func(r chi.Router) {
			r.Group(func(r chi.Router) {
				r.Use(middleware.RequireUser)
				orgH.MountCreate(r)
			})
		}
		// /{org}/people: GETs are public (org existence is non-secret;
		// member lists for private orgs are deferred). Mutations are
		// owner-checked inside the handler, but RequireUser wraps the
		// POST routes so unauth submits redirect to /login.
		deps.OrgRoutesMounter = func(r chi.Router) {
			r.Group(func(r chi.Router) {
				orgH.MountOrgRoutes(r)
			})
		}
		deps.OrgInvitationsMounter = func(r chi.Router) {
			r.Group(func(r chi.Router) {
				r.Use(middleware.RequireUser)
				orgH.MountInvitations(r)
			})
		}

		// Lifecycle danger-zone routes — also auth-required.
		deps.RepoLifecycleMounter = func(r chi.Router) {
			r.Group(func(r chi.Router) {
				r.Use(middleware.RequireUser)
				repoH.MountLifecycle(r)
			})
		}

		// S34 — site admin. Gated by RequireUser + RequireSiteAdmin
		// (404 not 403 for non-admins). Shares the process renderer.
		//
		// Email sender is the same one auth uses; the admin "Reset
		// password" action sends through it (SR2 C3). Version is the
		// build-time-stamped value so /admin/system reports reality
		// instead of the literal "dev" (SR2 L6).
		adminSender, err := pickEmailSender(cfg)
		if err != nil {
			return fmt.Errorf("admin handlers: pick email sender: %w", err)
		}
		adminH, err := buildAdminHandlers(cfg, pool, sharedRenderer, logger, version.Version, adminSender)
		if err != nil {
			return fmt.Errorf("admin handlers: %w", err)
		}
		deps.AdminMounter = func(r chi.Router) {
			r.Group(func(r chi.Router) {
				r.Use(middleware.RequireUser)
				r.Use(middleware.RequireSiteAdmin(nil)) // nil ⇒ http.NotFound
				adminH.Mount(r)
			})
		}

		gitHTTPH, err := buildGitHTTPHandlers(cfg, pool, runnerJWT, logger)
		if err != nil {
			return fmt.Errorf("git-http handlers: %w", err)
		}
		deps.GitHTTPMounter = gitHTTPH.MountSmartHTTP
	} else {
		logger.Warn("auth: no DB pool — signup/login routes not mounted")
	}

	_, panicHandler, notFoundHandler, err := handlers.RegisterChi(r, deps)
	if err != nil {
		return fmt.Errorf("register handlers: %w", err)
	}
	r.NotFound(notFoundHandler)
	// H19/H20: 405 needs an Allow header (RFC 9110 §15.5.6) and the
	// API must answer OPTIONS preflight with CORS headers. Close over
	// the mux so the handler can probe which methods are registered
	// for the request path.
	r.MethodNotAllowed(http.HandlerFunc(methodNotAllowedHandlerFor(r, buildCORSOriginPolicy(cfg))))

	rootHandler := middleware.Recover(logger, panicHandler)(r)

	srv := &http.Server{
		Addr:              cfg.Web.Addr,
		Handler:           rootHandler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       cfg.Web.ReadTimeout,
		WriteTimeout:      cfg.Web.WriteTimeout,
		IdleTimeout:       120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info(
			"shithub web server starting",
			"addr", srv.Addr,
			"env", cfg.Env,
			"db", pool != nil,
			"metrics", cfg.Metrics.Enabled,
			"tracing", cfg.Tracing.Enabled,
			"errrep", cfg.ErrorReporting.DSN != "",
		)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	select {
	case err, ok := <-errCh:
		if !ok {
			return nil
		}
		return err
	case sig := <-sigCh:
		logger.Info("shutdown signal received", "signal", sig.String())
	case <-ctx.Done():
		logger.Info("context canceled, shutting down")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.Web.ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	return nil
}

// buildSessionStore constructs the cookie session store from the config's
// session block. SHITHUB_SESSION_KEY (env) overrides cfg.KeyB64 when set.
func buildSessionStore(cfg config.SessionConfig, logger *slog.Logger) (session.Store, error) {
	keyB64 := os.Getenv("SHITHUB_SESSION_KEY")
	if keyB64 == "" {
		keyB64 = cfg.KeyB64
	}
	var key []byte
	if keyB64 != "" {
		decoded, err := base64.StdEncoding.DecodeString(keyB64)
		if err != nil {
			return nil, fmt.Errorf("session key: invalid base64: %w", err)
		}
		if len(decoded) != chacha20poly1305.KeySize {
			return nil, fmt.Errorf("session key: must be %d bytes, got %d",
				chacha20poly1305.KeySize, len(decoded))
		}
		key = decoded
	} else {
		generated, err := session.GenerateKey()
		if err != nil {
			return nil, fmt.Errorf("session key: generate: %w", err)
		}
		key = generated
		logger.Warn(
			"session: no key configured; generated an ephemeral key (sessions will not survive restart)",
			"hint", "set SHITHUB_SESSION_KEY=<base64 32-byte> or session.key_b64 in production",
		)
	}
	store, err := session.NewCookieStore(session.CookieStoreConfig{
		Key:    key,
		MaxAge: cfg.MaxAge,
		Secure: cfg.Secure,
	})
	if err != nil {
		return nil, fmt.Errorf("session: build store: %w", err)
	}
	return store, nil
}

// methodNotAllowedHandler is the global 405 handler attached to the
// root chi router. G13 (F2-11 / F2-17): chi's default 405 sends a bare
// response with no body; the API's `{"error":...}` envelope contract
// requires a message. Branch on path prefix so /api/v1/* requests get
// the JSON shape callers expect; non-API paths keep the default
// behavior (HTML pages have their own 405 rendering through
// middleware).
//
// Method and path are user-controlled but flow through json.Encoder,
// which escapes quotes / control chars so a crafted request can't
// break the envelope or inject HTML — the response is also tagged
// application/json with Cache-Control: no-store.
func methodNotAllowedHandler(w http.ResponseWriter, req *http.Request) {
	if strings.HasPrefix(req.URL.Path, "/api/v1/") {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusMethodNotAllowed)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error": "method " + req.Method + " not allowed on " + req.URL.Path,
		})
		return
	}
	w.WriteHeader(http.StatusMethodNotAllowed)
}

// buildCORSOriginPolicy projects the server config onto the CORS
// allow-list policy used by the OPTIONS preflight handler. `sameHost`
// is the BaseURL origin (scheme://host[:port]); `allowed` carries the
// explicit cross-origin entries from `web.cors_allowed_origins`.
func buildCORSOriginPolicy(cfg config.Config) corsOriginPolicy {
	policy := corsOriginPolicy{
		allowed: make(map[string]struct{}, len(cfg.Web.CORSAllowedOrigins)),
	}
	if u, err := url.Parse(cfg.Auth.BaseURL); err == nil && u.Scheme != "" && u.Host != "" {
		policy.sameHost = u.Scheme + "://" + u.Host
	}
	for _, o := range cfg.Web.CORSAllowedOrigins {
		policy.allowed[strings.TrimSpace(o)] = struct{}{}
	}
	return policy
}

// corsOriginPolicy captures the operator-configured cross-origin
// surface. `sameHost` is the public base URL of this shithub instance
// (always allowed); `allowed` is the explicit cross-origin allow-list
// (anything not in either slot is rejected). Constructed once at boot
// from config and closed over by methodNotAllowedHandlerFor.
type corsOriginPolicy struct {
	sameHost string
	allowed  map[string]struct{}
}

// allow reports whether `origin` is permitted to mount cross-origin
// requests. Same-host always allowed; localhost dev origins always
// allowed (so a local CRA/Vite dev server can hit the API); everything
// else has to be in the allow-list. Empty origin (non-browser request)
// is allowed — the auth check carries the security weight there.
func (p corsOriginPolicy) allow(origin string) bool {
	if origin == "" {
		return true
	}
	if p.sameHost != "" && origin == p.sameHost {
		return true
	}
	if strings.HasPrefix(origin, "http://localhost:") || strings.HasPrefix(origin, "http://127.0.0.1:") {
		return true
	}
	_, ok := p.allowed[origin]
	return ok
}

// methodNotAllowedHandlerFor wraps methodNotAllowedHandler so it can
// emit the RFC 9110 §15.5.6 `Allow:` header (H19) and answer browser
// CORS preflight via OPTIONS (H20). The chi mux is closed over for
// route discovery: we probe each common verb against the request
// path with mx.Match — every match is a method the route supports.
//
// I11 (audit-I33): the CORS branch now checks `policy` instead of
// echoing any Origin. Unknown origins fall through to the 405 path
// without setting any ACAO header; the browser refuses the request
// just as it would for a server that didn't speak CORS at all.
func methodNotAllowedHandlerFor(mx *chi.Mux, policy corsOriginPolicy) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		allowed := discoverAllowedMethods(mx, req.URL.Path)
		if len(allowed) > 0 {
			w.Header().Set("Allow", strings.Join(allowed, ", "))
		}
		if req.Method == http.MethodOptions && strings.HasPrefix(req.URL.Path, "/api/v1/") {
			origin := req.Header.Get("Origin")
			if !policy.allow(origin) {
				// I11 (audit-I33): unknown origin — emit 204 with no
				// ACAO header. The browser sees the missing header
				// and refuses the cross-origin request. Pre-fix any
				// origin (including `null` and `*`) got reflected.
				w.Header().Set("Vary", "Origin")
				w.WriteHeader(http.StatusNoContent)
				return
			}
			if origin == "" {
				origin = "*"
			}
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Methods", strings.Join(allowed, ", "))
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Request-Id")
			w.Header().Set("Access-Control-Max-Age", "600")
			w.Header().Set("Vary", "Origin")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		methodNotAllowedHandler(w, req)
	}
}

// discoverAllowedMethods returns the set of methods registered on
// path, in canonical-ordered form. Always includes OPTIONS when at
// least one other method matches (since we handle preflight for the
// API surface).
func discoverAllowedMethods(mx *chi.Mux, path string) []string {
	verbs := []string{
		http.MethodGet, http.MethodHead, http.MethodPost,
		http.MethodPut, http.MethodPatch, http.MethodDelete,
	}
	var allowed []string
	for _, v := range verbs {
		rctx := chi.NewRouteContext()
		if mx.Match(rctx, v, path) {
			allowed = append(allowed, v)
		}
	}
	if len(allowed) > 0 && strings.HasPrefix(path, "/api/v1/") {
		allowed = append(allowed, http.MethodOptions)
	}
	return allowed
}
