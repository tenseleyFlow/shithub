// SPDX-License-Identifier: AGPL-3.0-or-later

package auth

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tenseleyFlow/shithub/internal/auth/audit"
	"github.com/tenseleyFlow/shithub/internal/auth/email"
	"github.com/tenseleyFlow/shithub/internal/auth/pat"
	"github.com/tenseleyFlow/shithub/internal/auth/policy"
	"github.com/tenseleyFlow/shithub/internal/billing"
	"github.com/tenseleyFlow/shithub/internal/entitlements"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

// recentAuthWindow is how long after a successful TOTP challenge a user
// can mint a PAT without re-confirming. Matches the spec's 10-minute
// "recent-auth" pattern.
const recentAuthWindow = 10 * time.Minute

// tokensList renders /settings/tokens.
func (h *Handlers) tokensList(w http.ResponseWriter, r *http.Request) {
	h.renderTokensList(w, r, "", "", nil, "")
}

// tokenRepoChoice is a compact projection of the user's owned repos for
// rendering the binding dropdown on /settings/tokens. Org repos are not
// included — initial 11b scope only supports binding to user-owned
// repos, which keeps the entitlement reasoning straightforward.
type tokenRepoChoice struct {
	ID   int64
	Name string
}

func (h *Handlers) renderTokensList(
	w http.ResponseWriter, r *http.Request,
	createError, createName string, createScopes []string, justCreatedRaw string,
) {
	user := middleware.CurrentUserFromContext(r.Context())
	rows, err := h.q.ListUserTokens(r.Context(), h.d.Pool, user.ID)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "tokens: list", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	fineGrainedAllowed, _, _ := h.userFineGrainedPATsAllowed(r.Context(), user.ID)
	repoChoices := h.userTokenRepoChoices(r.Context(), user.ID)
	h.renderPage(w, r, "settings/tokens", map[string]any{
		"Title":              "Personal access tokens",
		"CSRFToken":          middleware.CSRFTokenForRequest(r),
		"SettingsActive":     "tokens",
		"Tokens":             rows,
		"AllScopes":          pat.AllScopes,
		"CreateError":        createError,
		"CreateName":         createName,
		"CreateScopes":       createScopes,
		"JustCreatedRaw":     justCreatedRaw,
		"RecentAuthOK":       h.recentAuthOK(r),
		"FineGrainedAllowed": fineGrainedAllowed,
		"FineGrainedKey":     string(entitlements.FeatureFineGrainedPATs),
		"RepoChoices":        repoChoices,
	})
}

// userTokenRepoChoices fetches the user's owned repos (omitting deleted)
// for the binding dropdown. Returns an empty slice on any error rather
// than failing the page — the dropdown is supplementary; the rest of the
// page must still render.
func (h *Handlers) userTokenRepoChoices(ctx context.Context, userID int64) []tokenRepoChoice {
	rows, err := reposdb.New().ListReposForOwnerUser(ctx, h.d.Pool,
		pgtype.Int8{Int64: userID, Valid: true})
	if err != nil {
		h.d.Logger.WarnContext(ctx, "tokens: list repos for binding", "error", err)
		return nil
	}
	out := make([]tokenRepoChoice, 0, len(rows))
	for _, repo := range rows {
		out = append(out, tokenRepoChoice{ID: repo.ID, Name: repo.Name})
	}
	return out
}

// userFineGrainedPATsAllowed wraps the entitlement check + logs the
// would-deny. Same shape as the other Pro gates in this package.
func (h *Handlers) userFineGrainedPATsAllowed(ctx context.Context, userID int64) (bool, entitlements.Decision, error) {
	decision, err := entitlements.CheckPrincipalFeature(ctx,
		entitlements.Deps{Pool: h.d.Pool},
		billing.PrincipalForUser(userID),
		entitlements.FeatureFineGrainedPATs)
	if err != nil {
		return false, entitlements.Decision{}, err
	}
	if !decision.Allowed {
		h.d.Logger.InfoContext(ctx, "entitlements.report_only_deny",
			"principal", billing.PrincipalForUser(userID).String(),
			"principal_kind", string(billing.SubjectKindUser),
			"principal_id", userID,
			"feature", string(entitlements.FeatureFineGrainedPATs),
			"reason", string(decision.Reason),
			"required_plan", string(decision.RequiredPlan),
			"mode", "report_only")
	}
	return decision.Allowed, decision, nil
}

// recentAuthOK reports whether the session has a Recent2FAAt within the
// configured window. Users without 2FA enrolled are treated as recent
// (the gate only matters when 2FA is in play).
func (h *Handlers) recentAuthOK(r *http.Request) bool {
	user := middleware.CurrentUserFromContext(r.Context())
	totpRow, err := h.q.GetUserTOTP(r.Context(), h.d.Pool, user.ID)
	if err != nil || !totpRow.ConfirmedAt.Valid {
		return true
	}
	s := middleware.SessionFromContext(r.Context())
	if s.Recent2FAAt == 0 {
		return false
	}
	return time.Since(time.Unix(s.Recent2FAAt, 0)) <= recentAuthWindow
}

// tokensCreate handles POST /settings/tokens.
func (h *Handlers) tokensCreate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "form parse")
		return
	}
	user := middleware.CurrentUserFromContext(r.Context())

	name := strings.TrimSpace(r.PostFormValue("name"))
	scopes := pat.NormalizeScopes(r.PostForm["scopes"])
	expiresIn := r.PostFormValue("expires_in")
	ipAllowlistRaw := r.PostFormValue("ip_allowlist")
	repoBindingRaw := strings.TrimSpace(r.PostFormValue("repo_id"))

	render := func(msg string) {
		h.renderTokensList(w, r, msg, name, scopes, "")
	}

	if !h.recentAuthOK(r) {
		render("Confirm 2FA recently before creating a token: sign in again with your authenticator code.")
		return
	}
	if name == "" || len(name) > 80 {
		render("Token name must be 1–80 characters.")
		return
	}
	if len(scopes) == 0 {
		render("Pick at least one scope.")
		return
	}

	count, err := h.q.CountActiveUserTokens(r.Context(), h.d.Pool, user.ID)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "tokens: count", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	if count >= int64(pat.MaxTokensPerUser) {
		render("You've reached the per-user token cap. Revoke an unused token first.")
		return
	}

	raw, hash, prefix, err := pat.Mint()
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "tokens: mint", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}

	expiresAt := pgtype.Timestamptz{}
	switch expiresIn {
	case "30":
		expiresAt = pgtype.Timestamptz{Time: time.Now().AddDate(0, 0, 30), Valid: true}
	case "90":
		expiresAt = pgtype.Timestamptz{Time: time.Now().AddDate(0, 0, 90), Valid: true}
	case "365":
		expiresAt = pgtype.Timestamptz{Time: time.Now().AddDate(1, 0, 0), Valid: true}
	}

	// PRO-EXT01-11a: optional IP allowlist. Parse first so a malformed
	// entry hard-errors before we mint the token (would otherwise leave
	// a fresh token with the user's allowlist silently dropped).
	ipAllowlist, invalidIPs := pat.ParseAllowlist(ipAllowlistRaw)
	if len(invalidIPs) > 0 {
		render("Invalid IP / CIDR entries: " + strings.Join(invalidIPs, ", "))
		return
	}
	if len(ipAllowlist) > 0 {
		allowed, decision, derr := h.userFineGrainedPATsAllowed(r.Context(), user.ID)
		if derr != nil {
			h.d.Logger.ErrorContext(r.Context(), "tokens: entitlement check", "error", derr)
			h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
			return
		}
		if !allowed && h.d.BillingEnforce.UserFineGrainedPATs {
			banner := decision.PrincipalUpgradeBanner("PAT IP allowlist", billing.PrincipalForUser(user.ID), "")
			render(banner.Message)
			return
		}
		// Report-only: log already emitted inside the helper; honor the
		// request so the user's intent is recorded.
	}

	// PRO-EXT01-11b: optional repo binding. Same Pro-gate as the IP
	// allowlist — they're both fine-grained PAT controls. Verify
	// ownership before persisting so a user can't bind a token to a
	// repo they don't own (the FK alone won't enforce ownership).
	repoBinding := pgtype.Int8{}
	if repoBindingRaw != "" {
		rid, perr := strconv.ParseInt(repoBindingRaw, 10, 64)
		if perr != nil || rid <= 0 {
			render("Repo binding must be a valid repo ID.")
			return
		}
		repo, gerr := reposdb.New().GetRepoByID(r.Context(), h.d.Pool, rid)
		if gerr != nil || !policy.NewRepoRefFromRepo(repo).IsOwnedByUser(user.ID) {
			// "Not yours" and "doesn't exist" collapse to the same
			// message — we don't want to leak existence of private
			// repos the user can't see.
			render("Pick a repo you own.")
			return
		}
		allowed, decision, derr := h.userFineGrainedPATsAllowed(r.Context(), user.ID)
		if derr != nil {
			h.d.Logger.ErrorContext(r.Context(), "tokens: entitlement check", "error", derr)
			h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
			return
		}
		if !allowed && h.d.BillingEnforce.UserFineGrainedPATs {
			banner := decision.PrincipalUpgradeBanner("PAT repo binding", billing.PrincipalForUser(user.ID), "")
			render(banner.Message)
			return
		}
		repoBinding = pgtype.Int8{Int64: rid, Valid: true}
	}

	row, err := h.q.InsertUserToken(r.Context(), h.d.Pool, usersdb.InsertUserTokenParams{
		UserID:      user.ID,
		Name:        name,
		TokenHash:   hash,
		TokenPrefix: prefix,
		Scopes:      scopes,
		ExpiresAt:   expiresAt,
		IpAllowlist: ipAllowlist,
		RepoID:      repoBinding,
	})
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "tokens: insert", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}

	if err := h.d.Audit.Record(r.Context(), h.d.Pool, user.ID,
		audit.ActionPATCreated, audit.TargetUser, user.ID, map[string]any{
			"token_id": row.ID,
			"prefix":   prefix,
			"scopes":   scopes,
		}); err != nil {
		h.d.Logger.WarnContext(r.Context(), "tokens: audit create", "error", err)
	}

	h.notifyTokenCreated(r, user.ID, name, prefix)

	// Render the list page with the raw token displayed once. It MUST NOT
	// land in the session, the URL, or any log line.
	h.renderTokensList(w, r, "", "", nil, raw)
}

// tokensRevoke handles POST /settings/tokens/{id}/revoke.
func (h *Handlers) tokensRevoke(w http.ResponseWriter, r *http.Request) {
	idStr := chi.URLParam(r, "id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "token not found")
		return
	}
	user := middleware.CurrentUserFromContext(r.Context())

	rows, err := h.q.RevokeUserToken(r.Context(), h.d.Pool, usersdb.RevokeUserTokenParams{
		ID: id, UserID: user.ID,
	})
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "tokens: revoke", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	if rows == 0 {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "token not found")
		return
	}

	if err := h.d.Audit.Record(r.Context(), h.d.Pool, user.ID,
		audit.ActionPATRevoked, audit.TargetUser, user.ID, map[string]any{
			"token_id": id,
		}); err != nil {
		h.d.Logger.WarnContext(r.Context(), "tokens: audit revoke", "error", err)
	}

	http.Redirect(w, r, "/settings/tokens", http.StatusSeeOther)
}

func (h *Handlers) notifyTokenCreated(r *http.Request, userID int64, name, prefix string) {
	user, err := h.q.GetUserByID(r.Context(), h.d.Pool, userID)
	if err != nil || !user.PrimaryEmailID.Valid {
		return
	}
	em, err := h.q.GetUserEmailByID(r.Context(), h.d.Pool, user.PrimaryEmailID.Int64)
	if err != nil {
		return
	}
	msg, err := email.TokenCreatedMessage(h.d.Branding, string(em.Email), user.Username,
		name, prefix, clientIP(r))
	if err != nil {
		h.d.Logger.WarnContext(r.Context(), "tokens: build email", "error", err)
		return
	}
	if err := h.d.Email.Send(r.Context(), msg); err != nil {
		h.d.Logger.WarnContext(r.Context(), "tokens: send email", "error", err)
	}
}
