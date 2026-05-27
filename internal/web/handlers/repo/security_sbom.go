// SPDX-License-Identifier: AGPL-3.0-or-later

package repo

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tenseleyFlow/shithub/internal/auth/policy"
	"github.com/tenseleyFlow/shithub/internal/billing"
	"github.com/tenseleyFlow/shithub/internal/entitlements"
	"github.com/tenseleyFlow/shithub/internal/repos/sbom"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

type repoSBOMGate struct {
	Allowed     bool
	FeatureKey  string
	UpgradeHref string
	UpgradeText string
}

type repoSBOMExportView struct {
	Format       string
	HeadSHA      string
	ShortHeadSHA string
	ByteCount    int64
	ByteLabel    string
	GeneratedAt  pgtype.Timestamptz
	Stale        bool
}

func (h *Handlers) repoSBOM(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoRead)
	if !ok {
		return
	}
	h.renderRepoSBOM(w, r, row, owner.Username, "")
}

func (h *Handlers) repoSBOMGenerate(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoRead)
	if !ok {
		return
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	if viewer.IsAnonymous() {
		h.d.Render.HTTPError(w, r, http.StatusForbidden, "Sign in to generate an SBOM.")
		return
	}
	gate := h.repoSBOMGate(r.Context(), row, owner.Username)
	if !gate.Allowed {
		h.renderRepoSBOM(w, r, row, owner.Username, "SBOM exports require Team for private organization repositories.")
		return
	}
	snapshot, err := h.rq.GetRepoDependencySnapshot(r.Context(), h.d.Pool, row.ID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			h.renderRepoSBOM(w, r, row, owner.Username, "No dependency snapshot is available yet. Push to the default branch or run the dependency backfill job first.")
			return
		}
		h.d.Logger.ErrorContext(r.Context(), "sbom: load dependency snapshot", "repo_id", row.ID, "error", err)
		h.renderRepoSBOM(w, r, row, owner.Username, "Could not load dependency data.")
		return
	}
	deps, err := h.rq.ListRepoDependenciesForRepo(r.Context(), h.d.Pool, reposdb.ListRepoDependenciesForRepoParams{
		RepoID:       row.ID,
		IncludeStale: false,
	})
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "sbom: list dependencies", "repo_id", row.ID, "error", err)
		h.renderRepoSBOM(w, r, row, owner.Username, "Could not load dependency data.")
		return
	}
	body, err := sbom.BuildSPDXJSON(sbom.Input{
		Owner:        owner.Username,
		Repository:   row.Name,
		BaseURL:      h.d.CloneURLs.BaseURL,
		HeadSHA:      snapshot.HeadSha,
		GeneratedAt:  snapshot.GeneratedAt.Time,
		Dependencies: repoSBOMDependencies(deps),
	})
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "sbom: build SPDX JSON", "repo_id", row.ID, "error", err)
		h.renderRepoSBOM(w, r, row, owner.Username, "Could not generate the SBOM.")
		return
	}
	if _, err := h.rq.UpsertRepoSBOMExport(r.Context(), h.d.Pool, reposdb.UpsertRepoSBOMExportParams{
		RepoID:                        row.ID,
		Format:                        sbom.FormatSPDXJSON,
		SourceHeadSha:                 snapshot.HeadSha,
		DependencySnapshotGeneratedAt: snapshot.GeneratedAt,
		Document:                      body,
		ByteCount:                     int64(len(body)),
		GeneratedBy:                   pgtype.Int8{Int64: viewer.ID, Valid: viewer.ID != 0},
	}); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "sbom: store export", "repo_id", row.ID, "error", err)
		h.renderRepoSBOM(w, r, row, owner.Username, "Could not store the SBOM export.")
		return
	}
	http.Redirect(w, r, "/"+owner.Username+"/"+row.Name+"/security/sbom?generated=1", http.StatusSeeOther)
}

func (h *Handlers) repoSBOMDownload(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoRead)
	if !ok {
		return
	}
	gate := h.repoSBOMGate(r.Context(), row, owner.Username)
	if !gate.Allowed {
		h.d.Render.HTTPError(w, r, http.StatusPaymentRequired, "SBOM exports require Team for private organization repositories.")
		return
	}
	export, err := h.rq.GetRepoSBOMExport(r.Context(), h.d.Pool, reposdb.GetRepoSBOMExportParams{
		RepoID: row.ID,
		Format: sbom.FormatSPDXJSON,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			h.d.Render.HTTPError(w, r, http.StatusNotFound, "Generate an SBOM before downloading it.")
			return
		}
		h.d.Logger.ErrorContext(r.Context(), "sbom: load export", "repo_id", row.ID, "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "Could not download the SBOM.")
		return
	}
	filename := safeSBOMFilename(owner.Username, row.Name)
	w.Header().Set("Content-Type", "application/spdx+json; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	w.Header().Set("Content-Length", strconv.Itoa(len(export.Document)))
	_, _ = w.Write(export.Document)
}

func (h *Handlers) renderRepoSBOM(w http.ResponseWriter, r *http.Request, row reposdb.Repo, ownerSlug string, errMsg string) {
	gate := h.repoSBOMGate(r.Context(), row, ownerSlug)
	var (
		snapshot reposdb.RepoDependencySnapshot
		hasSnap  bool
		deps     []reposdb.RepoDependency
		latest   *repoSBOMExportView
	)
	if gate.Allowed {
		var err error
		snapshot, err = h.rq.GetRepoDependencySnapshot(r.Context(), h.d.Pool, row.ID)
		if err == nil {
			hasSnap = true
			deps, err = h.rq.ListRepoDependenciesForRepo(r.Context(), h.d.Pool, reposdb.ListRepoDependenciesForRepoParams{
				RepoID:       row.ID,
				IncludeStale: false,
			})
			if err != nil {
				h.d.Logger.ErrorContext(r.Context(), "sbom: list dependencies for page", "repo_id", row.ID, "error", err)
				errMsg = "Could not load dependency data."
			}
		} else if !errors.Is(err, pgx.ErrNoRows) {
			h.d.Logger.ErrorContext(r.Context(), "sbom: load snapshot for page", "repo_id", row.ID, "error", err)
			errMsg = "Could not load dependency data."
		}
		if export, err := h.rq.GetRepoSBOMExport(r.Context(), h.d.Pool, reposdb.GetRepoSBOMExportParams{
			RepoID: row.ID,
			Format: sbom.FormatSPDXJSON,
		}); err == nil {
			latest = repoSBOMExportViewFromRow(export, snapshot.HeadSha)
		} else if !errors.Is(err, pgx.ErrNoRows) {
			h.d.Logger.ErrorContext(r.Context(), "sbom: load export for page", "repo_id", row.ID, "error", err)
		}
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	data := h.repoHeaderData(r, row, ownerSlug, "security")
	data["Title"] = "SBOM · " + row.Name
	data["Gate"] = gate
	data["Snapshot"] = snapshot
	data["HasSnapshot"] = hasSnap
	data["Dependencies"] = deps
	data["LatestExport"] = latest
	data["CanGenerate"] = gate.Allowed && hasSnap && !viewer.IsAnonymous()
	data["SignInRequired"] = gate.Allowed && hasSnap && viewer.IsAnonymous()
	data["Generated"] = r.URL.Query().Get("generated") == "1"
	data["Error"] = errMsg
	if err := h.d.Render.RenderPage(w, r, "repo/security_sbom", data); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "sbom page render", "repo_id", row.ID, "error", err)
	}
}

func (h *Handlers) repoSBOMGate(ctx context.Context, row reposdb.Repo, ownerSlug string) repoSBOMGate {
	gate := repoSBOMGate{
		Allowed:     true,
		FeatureKey:  string(entitlements.FeatureSBOMs),
		UpgradeHref: "/organizations/" + ownerSlug + "/settings/billing",
		UpgradeText: "Upgrade to Team",
	}
	if row.Visibility != reposdb.RepoVisibilityPrivate || !row.OwnerOrgID.Valid {
		return gate
	}
	decision, err := entitlements.CheckPrincipalFeature(ctx,
		entitlements.Deps{Pool: h.d.Pool},
		billing.PrincipalForOrg(row.OwnerOrgID.Int64),
		entitlements.FeatureSBOMs)
	if err != nil {
		gate.Allowed = false
		return gate
	}
	gate.Allowed = decision.Allowed
	return gate
}

func repoSBOMDependencies(rows []reposdb.RepoDependency) []sbom.Dependency {
	out := make([]sbom.Dependency, 0, len(rows))
	for _, row := range rows {
		out = append(out, sbom.Dependency{
			Ecosystem:      row.Ecosystem,
			PackageName:    row.PackageName,
			PackageVersion: row.PackageVersion,
			ManifestPath:   row.ManifestPath,
			PackageManager: row.PackageManager,
			Direct:         row.Direct,
		})
	}
	return out
}

func repoSBOMExportViewFromRow(row reposdb.RepoSbomExport, currentHead string) *repoSBOMExportView {
	return &repoSBOMExportView{
		Format:       row.Format,
		HeadSHA:      row.SourceHeadSha,
		ShortHeadSHA: shortSHA(row.SourceHeadSha),
		ByteCount:    row.ByteCount,
		ByteLabel:    repoSBOMByteLabel(row.ByteCount),
		GeneratedAt:  row.GeneratedAt,
		Stale:        currentHead != "" && row.SourceHeadSha != currentHead,
	}
}

func repoSBOMByteLabel(n int64) string {
	switch {
	case n >= 1024*1024:
		return fmt.Sprintf("%.1f MB", float64(n)/(1024*1024))
	case n >= 1024:
		return fmt.Sprintf("%.1f KB", float64(n)/1024)
	default:
		return fmt.Sprintf("%d bytes", n)
	}
}

func safeSBOMFilename(owner, repoName string) string {
	raw := owner + "-" + repoName + "-sbom.spdx.json"
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z':
			return r
		case r >= 'A' && r <= 'Z':
			return r
		case r >= '0' && r <= '9':
			return r
		case r == '.', r == '-', r == '_':
			return r
		default:
			return '-'
		}
	}, raw)
}
