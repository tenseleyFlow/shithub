// SPDX-License-Identifier: AGPL-3.0-or-later

package repo

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tenseleyFlow/shithub/internal/auth/audit"
	"github.com/tenseleyFlow/shithub/internal/auth/policy"
	"github.com/tenseleyFlow/shithub/internal/billing"
	"github.com/tenseleyFlow/shithub/internal/entitlements"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	secretscandb "github.com/tenseleyFlow/shithub/internal/secretscan/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
	"github.com/tenseleyFlow/shithub/internal/worker"
)

// PRO-EXT01-10c — Security tab → secret scanning sub-page.
//
// Lists findings (filterable by status), exposes an allowlist form
// to mark known false-positives, and a "Run scan now" button that
// enqueues a fresh KindSecretScanHistory job. The "Run scan now"
// button is Pro-gated; the rest of the page is always reachable so
// users can see prior findings even if they downgrade.

func (h *Handlers) repoSecretScanning(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoSettingsGeneral)
	if !ok {
		return
	}
	h.renderSecretScanningPage(w, r, row, owner.Username, "", "")
}

// repoSecretScanningRunScan handles the "Run scan now" button. Pro
// gate enforced inline so the button works the way the page renders.
func (h *Handlers) repoSecretScanningRunScan(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoSettingsGeneral)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "form parse")
		return
	}

	gate := h.repoSecretScanGate(r.Context(), row, owner.Username)
	if !gate.Allowed && h.d.BillingEnforce.UserSecretScanHistory {
		h.renderSecretScanningPage(w, r, row, owner.Username,
			"Secret scanning requires billing for this repository. Upgrade to run on-demand scans.", "")
		return
	}

	if _, err := worker.Enqueue(r.Context(), h.d.Pool, worker.KindSecretScanHistory,
		map[string]any{"repo_id": row.ID},
		worker.EnqueueOptions{}); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "secret-scan: enqueue", "repo_id", row.ID, "error", err)
		h.renderSecretScanningPage(w, r, row, owner.Username, "Could not queue the scan. Try again.", "")
		return
	}
	h.renderSecretScanningPage(w, r, row, owner.Username, "", "Scan queued — findings appear here when it completes.")
}

func (h *Handlers) repoSecretScanningAllowlistAdd(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoSettingsGeneral)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "form parse")
		return
	}
	pattern := strings.TrimSpace(r.PostFormValue("pattern"))
	path := strings.TrimSpace(r.PostFormValue("path"))
	reason := strings.TrimSpace(r.PostFormValue("reason"))
	if pattern == "" || path == "" {
		h.renderSecretScanningPage(w, r, row, owner.Username, "Pattern and path are required.", "")
		return
	}
	if len(reason) > 500 {
		h.renderSecretScanningPage(w, r, row, owner.Username, "Reason is too long (max 500 characters).", "")
		return
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	sq := secretscandb.New()
	if _, err := sq.InsertSecretScanAllowlist(r.Context(), h.d.Pool, secretscandb.InsertSecretScanAllowlistParams{
		RepoID:    row.ID,
		Pattern:   pattern,
		Path:      path,
		Reason:    reason,
		CreatedBy: pgtype.Int8{Int64: viewer.ID, Valid: viewer.ID != 0},
	}); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "secret-scan: insert allowlist", "repo_id", row.ID, "error", err)
		h.renderSecretScanningPage(w, r, row, owner.Username, "Could not add to allowlist.", "")
		return
	}
	// Sweep matching open findings to status=allowlisted so the
	// findings list reflects the new entry immediately, without
	// waiting for the next scan.
	if err := sq.ApplyAllowlistToFindings(r.Context(), h.d.Pool, secretscandb.ApplyAllowlistToFindingsParams{
		RepoID:  row.ID,
		Pattern: pattern,
		Path:    path,
	}); err != nil {
		h.d.Logger.WarnContext(r.Context(), "secret-scan: apply allowlist sweep", "repo_id", row.ID, "error", err)
	}
	h.renderSecretScanningPage(w, r, row, owner.Username, "", "Allowlist entry added.")
}

func (h *Handlers) repoSecretScanningAllowlistRemove(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoSettingsGeneral)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "form parse")
		return
	}
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		h.renderSecretScanningPage(w, r, row, owner.Username, "Invalid allowlist id.", "")
		return
	}
	if err := secretscandb.New().DeleteSecretScanAllowlist(r.Context(), h.d.Pool, secretscandb.DeleteSecretScanAllowlistParams{
		ID:     id,
		RepoID: row.ID,
	}); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "secret-scan: delete allowlist", "id", id, "repo_id", row.ID, "error", err)
		h.renderSecretScanningPage(w, r, row, owner.Username, "Could not remove allowlist entry.", "")
		return
	}
	h.renderSecretScanningPage(w, r, row, owner.Username, "", "Allowlist entry removed.")
}

func (h *Handlers) repoSecretScanningBypassApprove(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoSettingsGeneral)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "form parse")
		return
	}
	gate := h.repoSecretBypassGate(r.Context(), row, owner.Username)
	if !gate.Allowed {
		h.renderSecretScanningPage(w, r, row, owner.Username,
			"Secret push-protection bypass controls require billing for this repository.", "")
		return
	}
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		h.renderSecretScanningPage(w, r, row, owner.Username, "Invalid bypass request id.", "")
		return
	}
	note := strings.TrimSpace(r.PostFormValue("review_note"))
	if len(note) > 500 {
		h.renderSecretScanningPage(w, r, row, owner.Username, "Review note is too long (max 500 characters).", "")
		return
	}
	hours, ok := parseSecretBypassApprovalHours(r.PostFormValue("approved_for_hours"))
	if !ok {
		h.renderSecretScanningPage(w, r, row, owner.Username, "Approval duration must be between 1 and 168 hours.", "")
		return
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	approvedUntil := time.Now().UTC().Add(time.Duration(hours) * time.Hour)
	reviewed, err := secretscandb.New().ReviewSecretScanBypassRequest(r.Context(), h.d.Pool, secretscandb.ReviewSecretScanBypassRequestParams{
		ID:            id,
		RepoID:        row.ID,
		Status:        secretscandb.SecretScanBypassStatusApproved,
		ReviewedBy:    pgtype.Int8{Int64: viewer.ID, Valid: viewer.ID != 0},
		ReviewNote:    note,
		ApprovedUntil: pgtype.Timestamptz{Time: approvedUntil, Valid: true},
	})
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "secret-scan: approve bypass", "id", id, "repo_id", row.ID, "error", err)
		h.renderSecretScanningPage(w, r, row, owner.Username, "Could not approve bypass request.", "")
		return
	}
	h.recordSecretBypassReview(r, row.ID, auditActionSecretBypassApproved, reviewed)
	h.renderSecretScanningPage(w, r, row, owner.Username, "", "Bypass request approved.")
}

func (h *Handlers) repoSecretScanningBypassDeny(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoSettingsGeneral)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "form parse")
		return
	}
	gate := h.repoSecretBypassGate(r.Context(), row, owner.Username)
	if !gate.Allowed {
		h.renderSecretScanningPage(w, r, row, owner.Username,
			"Secret push-protection bypass controls require billing for this repository.", "")
		return
	}
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		h.renderSecretScanningPage(w, r, row, owner.Username, "Invalid bypass request id.", "")
		return
	}
	note := strings.TrimSpace(r.PostFormValue("review_note"))
	if len(note) > 500 {
		h.renderSecretScanningPage(w, r, row, owner.Username, "Review note is too long (max 500 characters).", "")
		return
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	reviewed, err := secretscandb.New().ReviewSecretScanBypassRequest(r.Context(), h.d.Pool, secretscandb.ReviewSecretScanBypassRequestParams{
		ID:            id,
		RepoID:        row.ID,
		Status:        secretscandb.SecretScanBypassStatusDenied,
		ReviewedBy:    pgtype.Int8{Int64: viewer.ID, Valid: viewer.ID != 0},
		ReviewNote:    note,
		ApprovedUntil: pgtype.Timestamptz{},
	})
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "secret-scan: deny bypass", "id", id, "repo_id", row.ID, "error", err)
		h.renderSecretScanningPage(w, r, row, owner.Username, "Could not deny bypass request.", "")
		return
	}
	h.recordSecretBypassReview(r, row.ID, auditActionSecretBypassDenied, reviewed)
	h.renderSecretScanningPage(w, r, row, owner.Username, "", "Bypass request denied.")
}

// renderSecretScanningPage is the shared render path. Always loads
// findings + allowlist for the repo + computes the Pro-gate state.
func (h *Handlers) renderSecretScanningPage(w http.ResponseWriter, r *http.Request, row reposdb.Repo, ownerSlug, errMsg, successMsg string) {
	statusFilter := r.URL.Query().Get("status")
	sq := secretscandb.New()
	findings, _ := sq.ListSecretScanFindingsForRepo(r.Context(), h.d.Pool, secretscandb.ListSecretScanFindingsForRepoParams{
		RepoID:       row.ID,
		StatusFilter: statusFilter,
		Limit:        100,
		Offset:       0,
	})
	allowlist, _ := sq.ListSecretScanAllowlistForRepo(r.Context(), h.d.Pool, row.ID)
	gate := h.repoSecretScanGate(r.Context(), row, ownerSlug)
	bypassGate := h.repoSecretBypassGate(r.Context(), row, ownerSlug)
	bypassRequests := []secretscandb.SecretScanBypassRequest{}
	if bypassGate.Allowed {
		bypassRequests, _ = sq.ListSecretScanBypassRequestsForRepo(r.Context(), h.d.Pool, row.ID)
	}
	_ = h.d.Render.RenderPage(w, r, "repo/security_secret_scanning", map[string]any{
		"Title":              "Secret scanning · " + row.Name,
		"CSRFToken":          middleware.CSRFTokenForRequest(r),
		"Owner":              ownerSlug,
		"Repo":               row,
		"Findings":           findings,
		"Allowlist":          allowlist,
		"StatusFilter":       statusFilter,
		"RunScanAllowed":     gate.Allowed,
		"RunScanFeatureKey":  gate.FeatureKey,
		"RunScanUpgradeHref": gate.UpgradeHref,
		"RunScanUpgradeText": gate.UpgradeText,
		"BypassAllowed":      bypassGate.Allowed,
		"BypassRequests":     bypassRequests,
		"BypassFeatureKey":   bypassGate.FeatureKey,
		"BypassUpgradeHref":  bypassGate.UpgradeHref,
		"BypassUpgradeText":  bypassGate.UpgradeText,
		"RepoActions":        h.repoActions(r, row.ID),
		"RepoCounts":         h.subnavCounts(r.Context(), row.ID, row.ForkCount),
		"CanSettings":        h.canViewSettings(middleware.CurrentUserFromContext(r.Context())),
		"ActiveSubnav":       "security",
		"Error":              errMsg,
		"Success":            successMsg,
	})
}

type repoSecretScanGate struct {
	Allowed     bool
	FeatureKey  string
	UpgradeHref string
	UpgradeText string
}

// repoSecretScanGate reports whether on-demand history scanning is
// available for this repository. Public repositories keep baseline
// secret scanning, matching GitHub's public-repo availability. Private
// personal repositories require Pro; private organization repositories
// require Team via SP26's org Secret Protection entitlement.
func (h *Handlers) repoSecretScanGate(ctx context.Context, row reposdb.Repo, ownerSlug string) repoSecretScanGate {
	gate := repoSecretScanGate{
		Allowed:     true,
		FeatureKey:  string(entitlements.FeatureSecretScanHistory),
		UpgradeHref: "/settings/billing",
		UpgradeText: "Upgrade to Pro",
	}
	if row.Visibility != reposdb.RepoVisibilityPrivate {
		return gate
	}
	if row.OwnerOrgID.Valid {
		gate.FeatureKey = string(entitlements.FeatureSecretScanning)
		gate.UpgradeHref = "/organizations/" + ownerSlug + "/settings/billing"
		gate.UpgradeText = "Upgrade to Team"
		decision, err := entitlements.CheckPrincipalFeature(ctx,
			entitlements.Deps{Pool: h.d.Pool},
			billing.PrincipalForOrg(row.OwnerOrgID.Int64),
			entitlements.FeatureSecretScanning)
		if err != nil {
			gate.Allowed = false
			return gate
		}
		gate.Allowed = decision.Allowed
		return gate
	}
	if !row.OwnerUserID.Valid {
		return gate
	}
	gate.Allowed = false
	decision, err := entitlements.CheckPrincipalFeature(ctx,
		entitlements.Deps{Pool: h.d.Pool},
		billing.PrincipalForUser(row.OwnerUserID.Int64),
		entitlements.FeatureSecretScanHistory)
	if err != nil {
		return gate
	}
	if decision.Allowed {
		gate.Allowed = true
	}
	return gate
}

// repoSecretBypassGate reports whether push-protection bypass review is
// available. Public repos and personal private repos keep the baseline
// owner-control surface; private organization repos require Team's
// secret_bypass_controls feature before the page reveals exact blocked
// paths or approval controls.
func (h *Handlers) repoSecretBypassGate(ctx context.Context, row reposdb.Repo, ownerSlug string) repoSecretScanGate {
	gate := repoSecretScanGate{
		Allowed:     true,
		FeatureKey:  string(entitlements.FeatureSecretBypassControls),
		UpgradeHref: "/settings/billing",
		UpgradeText: "Upgrade",
	}
	if row.Visibility != reposdb.RepoVisibilityPrivate || !row.OwnerOrgID.Valid {
		return gate
	}
	gate.Allowed = false
	gate.UpgradeHref = "/organizations/" + ownerSlug + "/settings/billing"
	gate.UpgradeText = "Upgrade to Team"
	decision, err := entitlements.CheckPrincipalFeature(ctx,
		entitlements.Deps{Pool: h.d.Pool},
		billing.PrincipalForOrg(row.OwnerOrgID.Int64),
		entitlements.FeatureSecretBypassControls)
	if err != nil {
		return gate
	}
	gate.Allowed = decision.Allowed
	return gate
}

func parseSecretBypassApprovalHours(raw string) (int, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 24, true
	}
	hours, err := strconv.Atoi(raw)
	if err != nil || hours < 1 || hours > 168 {
		return 0, false
	}
	return hours, true
}

type secretBypassAuditAction string

const (
	auditActionSecretBypassApproved secretBypassAuditAction = "approved"
	auditActionSecretBypassDenied   secretBypassAuditAction = "denied"
)

func (h *Handlers) recordSecretBypassReview(r *http.Request, repoID int64, action secretBypassAuditAction, row secretscandb.SecretScanBypassRequest) {
	viewer := middleware.CurrentUserFromContext(r.Context())
	auditAction := audit.ActionSecretBypassDenied
	if action == auditActionSecretBypassApproved {
		auditAction = audit.ActionSecretBypassApproved
	}
	meta := map[string]any{
		"bypass_request_id": row.ID,
		"pattern":           row.Pattern,
		"path":              row.Path,
		"line_no":           row.LineNo,
		"commit_oid":        row.CommitOid,
		"status":            row.Status,
	}
	if row.ApprovedUntil.Valid {
		meta["approved_until"] = row.ApprovedUntil.Time.UTC().Format(time.RFC3339)
	}
	if err := h.d.Audit.Record(r.Context(), h.d.Pool, viewer.ID, auditAction, audit.TargetRepo, repoID, meta); err != nil {
		h.d.Logger.WarnContext(r.Context(), "secret-scan: audit bypass review", "repo_id", repoID, "request_id", row.ID, "error", err)
	}
}
