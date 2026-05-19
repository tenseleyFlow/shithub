// SPDX-License-Identifier: AGPL-3.0-or-later

package repo

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tenseleyFlow/shithub/internal/auth/policy"
	"github.com/tenseleyFlow/shithub/internal/billing"
	"github.com/tenseleyFlow/shithub/internal/entitlements"
	"github.com/tenseleyFlow/shithub/internal/repos/codescan"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

func (h *Handlers) repoCodeScanning(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoRead)
	if !ok {
		return
	}
	h.renderCodeScanningPage(w, r, row, owner.Username, "", "")
}

func (h *Handlers) repoCodeScanningUpload(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoWrite)
	if !ok {
		return
	}
	gate := h.repoCodeScanGate(r.Context(), row, owner.Username)
	if !gate.Allowed {
		h.renderCodeScanningPage(w, r, row, owner.Username,
			"Code scanning SARIF uploads require Team for private organization repositories.", "")
		return
	}

	body, form, err := readSARIFUpload(w, r)
	if err != nil {
		h.renderCodeScanningPage(w, r, row, owner.Username, err.Error(), "")
		return
	}
	upload, err := codescan.ParseSARIF(body)
	if err != nil {
		h.renderCodeScanningPage(w, r, row, owner.Username, codeScanUploadError(err), "")
		return
	}

	commitSHA := firstNonBlank(form.value("commit_sha"), r.URL.Query().Get("commit_sha"), "unknown")
	refName := firstNonBlank(form.value("ref_name"), r.URL.Query().Get("ref_name"), row.DefaultBranch)
	category := firstNonBlank(form.value("category"), r.URL.Query().Get("category"), upload.Category)
	if len(commitSHA) > 128 {
		commitSHA = commitSHA[:128]
	}
	if len(refName) > 255 {
		refName = refName[:255]
	}
	if len(category) > 160 {
		category = category[:160]
	}

	viewer := middleware.CurrentUserFromContext(r.Context())
	tx, err := h.d.Pool.Begin(r.Context())
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "code-scanning: begin tx", "repo_id", row.ID, "error", err)
		h.renderCodeScanningPage(w, r, row, owner.Username, "Could not store the SARIF upload.", "")
		return
	}
	defer func() { _ = tx.Rollback(r.Context()) }()

	if _, err := h.rq.CreateCodeScanningUpload(r.Context(), tx, reposdb.CreateCodeScanningUploadParams{
		RepoID:         row.ID,
		ToolName:       upload.ToolName,
		ToolGuid:       upload.ToolGUID,
		Category:       category,
		CommitSha:      commitSHA,
		RefName:        refName,
		AlertCount:     int32(len(upload.Alerts)),
		RawSarifSha256: codescan.Digest(body),
		UploadedBy:     pgtype.Int8{Int64: viewer.ID, Valid: viewer.ID != 0},
	}); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "code-scanning: create upload", "repo_id", row.ID, "error", err)
		h.renderCodeScanningPage(w, r, row, owner.Username, "Could not store the SARIF upload.", "")
		return
	}

	for _, alert := range upload.Alerts {
		if _, err := h.rq.UpsertCodeScanningAlert(r.Context(), tx, reposdb.UpsertCodeScanningAlertParams{
			RepoID:      row.ID,
			ToolName:    alert.ToolName,
			ToolGuid:    alert.ToolGUID,
			RuleID:      alert.RuleID,
			RuleName:    alert.RuleName,
			Severity:    alert.Severity,
			Message:     alert.Message,
			Path:        alert.Path,
			StartLine:   alert.StartLine,
			EndLine:     alert.EndLine,
			StartColumn: alert.StartColumn,
			EndColumn:   alert.EndColumn,
			Fingerprint: alert.Fingerprint,
			CommitSha:   commitSHA,
			RefName:     refName,
		}); err != nil {
			h.d.Logger.ErrorContext(r.Context(), "code-scanning: upsert alert", "repo_id", row.ID, "rule_id", alert.RuleID, "error", err)
			h.renderCodeScanningPage(w, r, row, owner.Username, "Could not store one or more SARIF alerts.", "")
			return
		}
	}
	if err := tx.Commit(r.Context()); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "code-scanning: commit upload", "repo_id", row.ID, "error", err)
		h.renderCodeScanningPage(w, r, row, owner.Username, "Could not store the SARIF upload.", "")
		return
	}

	h.renderCodeScanningPage(w, r, row, owner.Username, "",
		"SARIF upload stored. Code scanning alerts were updated.")
}

func (h *Handlers) renderCodeScanningPage(w http.ResponseWriter, r *http.Request, row reposdb.Repo, ownerSlug, errMsg, successMsg string) {
	statusFilter := normalizeCodeScanStatus(r.URL.Query().Get("status"))
	alerts, _ := h.rq.ListCodeScanningAlertsForRepo(r.Context(), h.d.Pool, reposdb.ListCodeScanningAlertsForRepoParams{
		RepoID:       row.ID,
		Limit:        100,
		Offset:       0,
		StatusFilter: statusFilter,
	})
	summary, _ := h.rq.CodeScanningSummaryForRepo(r.Context(), h.d.Pool, row.ID)
	campaigns, _ := h.rq.ListCodeSecurityCampaignsForRepo(r.Context(), h.d.Pool, row.ID)
	gate := h.repoCodeScanGate(r.Context(), row, ownerSlug)

	data := h.repoHeaderData(r, row, ownerSlug, "security")
	data["Title"] = "Code scanning · " + row.Name
	data["Alerts"] = alerts
	data["Campaigns"] = campaigns
	data["Summary"] = summary
	data["StatusFilter"] = statusFilter
	data["UploadAllowed"] = gate.Allowed
	data["UploadFeatureKey"] = gate.FeatureKey
	data["UploadUpgradeHref"] = gate.UpgradeHref
	data["UploadUpgradeText"] = gate.UpgradeText
	data["Error"] = errMsg
	data["Success"] = successMsg
	if err := h.d.Render.RenderPage(w, r, "repo/security_code_scanning", data); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "code-scanning page render", "repo_id", row.ID, "error", err)
	}
}

type repoCodeScanGate struct {
	Allowed     bool
	FeatureKey  string
	UpgradeHref string
	UpgradeText string
}

func (h *Handlers) repoCodeScanGate(ctx context.Context, row reposdb.Repo, ownerSlug string) repoCodeScanGate {
	gate := repoCodeScanGate{
		Allowed:     true,
		FeatureKey:  string(entitlements.FeatureCodeScanning),
		UpgradeHref: "/organizations/" + ownerSlug + "/settings/billing",
		UpgradeText: "Upgrade to Team",
	}
	if row.Visibility != reposdb.RepoVisibilityPrivate || !row.OwnerOrgID.Valid {
		return gate
	}
	decision, err := entitlements.CheckPrincipalFeature(ctx,
		entitlements.Deps{Pool: h.d.Pool},
		billing.PrincipalForOrg(row.OwnerOrgID.Int64),
		entitlements.FeatureCodeScanning)
	if err != nil {
		gate.Allowed = false
		return gate
	}
	gate.Allowed = decision.Allowed
	return gate
}

type sarifUploadForm struct {
	values map[string]string
}

func (f sarifUploadForm) value(key string) string {
	if f.values == nil {
		return ""
	}
	return strings.TrimSpace(f.values[key])
}

func readSARIFUpload(w http.ResponseWriter, r *http.Request) ([]byte, sarifUploadForm, error) {
	r.Body = http.MaxBytesReader(w, r.Body, codescan.MaxSARIFBytes+1024)
	ct := strings.ToLower(r.Header.Get("Content-Type"))
	form := sarifUploadForm{values: map[string]string{}}
	switch {
	case strings.HasPrefix(ct, "multipart/form-data"):
		if err := r.ParseMultipartForm(codescan.MaxSARIFBytes + 1024); err != nil {
			return nil, form, errors.New("Could not read the SARIF upload.")
		}
		for _, key := range []string{"commit_sha", "ref_name", "category"} {
			form.values[key] = r.FormValue(key)
		}
		file, _, err := r.FormFile("sarif_file")
		if err != nil {
			return nil, form, errors.New("Choose a SARIF file to upload.")
		}
		defer file.Close()
		body, err := readLimitedSARIF(file)
		if err != nil {
			return nil, form, err
		}
		return body, form, nil
	case strings.HasPrefix(ct, "application/x-www-form-urlencoded"):
		if err := r.ParseForm(); err != nil {
			return nil, form, errors.New("Could not read the SARIF upload.")
		}
		for _, key := range []string{"commit_sha", "ref_name", "category"} {
			form.values[key] = r.FormValue(key)
		}
		body := []byte(r.FormValue("sarif"))
		if len(strings.TrimSpace(string(body))) == 0 {
			return nil, form, errors.New("Paste a SARIF payload or choose a SARIF file.")
		}
		if len(body) > codescan.MaxSARIFBytes {
			return nil, form, errors.New("SARIF uploads are limited to 5 MiB.")
		}
		return body, form, nil
	default:
		body, err := readLimitedSARIF(r.Body)
		if err != nil {
			return nil, form, err
		}
		return body, form, nil
	}
}

func readLimitedSARIF(r io.Reader) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, codescan.MaxSARIFBytes+1))
	if err != nil {
		return nil, errors.New("Could not read the SARIF upload.")
	}
	if len(body) > codescan.MaxSARIFBytes {
		return nil, errors.New("SARIF uploads are limited to 5 MiB.")
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return nil, errors.New("Paste a SARIF payload or choose a SARIF file.")
	}
	return body, nil
}

func codeScanUploadError(err error) string {
	switch {
	case errors.Is(err, codescan.ErrEmptySARIF):
		return "Paste a SARIF payload or choose a SARIF file."
	case errors.Is(err, codescan.ErrSARIFTooLarge):
		return "SARIF uploads are limited to 5 MiB."
	case errors.Is(err, codescan.ErrNoSARIFRuns):
		return "The SARIF payload does not contain any runs."
	default:
		return "The SARIF payload could not be parsed."
	}
}

func normalizeCodeScanStatus(raw string) string {
	switch strings.TrimSpace(raw) {
	case "open", "dismissed", "fixed":
		return raw
	default:
		return ""
	}
}

func firstNonBlank(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
