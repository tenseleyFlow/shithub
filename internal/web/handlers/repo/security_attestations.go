// SPDX-License-Identifier: AGPL-3.0-or-later

package repo

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tenseleyFlow/shithub/internal/auth/policy"
	"github.com/tenseleyFlow/shithub/internal/billing"
	"github.com/tenseleyFlow/shithub/internal/entitlements"
	"github.com/tenseleyFlow/shithub/internal/repos/attestations"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

const repoAttestationListLimit = 100

type repoAttestationGate struct {
	Allowed     bool
	FeatureKey  string
	UpgradeHref string
	UpgradeText string
}

type repoArtifactAttestationView struct {
	ID            int64
	SubjectName   string
	SubjectDigest string
	ShortDigest   string
	PredicateType string
	ByteCount     int64
	ByteLabel     string
	SourceRunID   pgtype.Int8
	UploadedBy    pgtype.Int8
	CreatedAt     pgtype.Timestamptz
	DownloadHref  string
}

func (h *Handlers) repoAttestations(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoRead)
	if !ok {
		return
	}
	h.renderRepoAttestations(w, r, row, owner.Username, "")
}

func (h *Handlers) repoAttestationCreate(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoWrite)
	if !ok {
		return
	}
	gate := h.repoAttestationGate(r.Context(), row, owner.Username)
	if !gate.Allowed {
		h.renderRepoAttestations(w, r, row, owner.Username, "Artifact attestations require Team for private organization repositories.")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, attestations.MaxStatementBytes+64*1024)
	if err := r.ParseForm(); err != nil {
		h.renderRepoAttestations(w, r, row, owner.Username, "Could not read attestation form data.")
		return
	}
	normalized, err := attestations.NormalizeStatement([]byte(r.PostFormValue("statement")))
	if err != nil {
		h.renderRepoAttestations(w, r, row, owner.Username, err.Error())
		return
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	if _, err := h.rq.InsertRepoArtifactAttestation(r.Context(), h.d.Pool, reposdb.InsertRepoArtifactAttestationParams{
		RepoID:        row.ID,
		SubjectName:   normalized.SubjectName,
		SubjectDigest: normalized.SubjectDigest,
		PredicateType: normalized.PredicateType,
		Statement:     normalized.Statement,
		ByteCount:     normalized.ByteCount,
		UploadedBy:    pgtype.Int8{Int64: viewer.ID, Valid: viewer.ID != 0},
	}); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "artifact attestation: store", "repo_id", row.ID, "error", err)
		h.renderRepoAttestations(w, r, row, owner.Username, "Could not store the artifact attestation.")
		return
	}
	http.Redirect(w, r, "/"+owner.Username+"/"+row.Name+"/security/attestations?created=1", http.StatusSeeOther)
}

func (h *Handlers) repoAttestationDownload(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoRead)
	if !ok {
		return
	}
	gate := h.repoAttestationGate(r.Context(), row, owner.Username)
	if !gate.Allowed {
		h.d.Render.HTTPError(w, r, http.StatusPaymentRequired, "Artifact attestations require Team for private organization repositories.")
		return
	}
	attestationID, err := strconv.ParseInt(chi.URLParam(r, "attestationID"), 10, 64)
	if err != nil || attestationID <= 0 {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "Artifact attestation not found.")
		return
	}
	attestation, err := h.rq.GetRepoArtifactAttestationForRepo(r.Context(), h.d.Pool, reposdb.GetRepoArtifactAttestationForRepoParams{
		RepoID: row.ID,
		ID:     attestationID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			h.d.Render.HTTPError(w, r, http.StatusNotFound, "Artifact attestation not found.")
			return
		}
		h.d.Logger.ErrorContext(r.Context(), "artifact attestation: load", "repo_id", row.ID, "attestation_id", attestationID, "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "Could not download the artifact attestation.")
		return
	}
	filename := safeAttestationFilename(owner.Username, row.Name, attestation.ID)
	w.Header().Set("Content-Type", "application/vnd.in-toto+json; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	w.Header().Set("Content-Length", strconv.Itoa(len(attestation.Statement)))
	_, _ = w.Write(attestation.Statement)
}

func (h *Handlers) renderRepoAttestations(w http.ResponseWriter, r *http.Request, row reposdb.Repo, ownerSlug string, errMsg string) {
	gate := h.repoAttestationGate(r.Context(), row, ownerSlug)
	var views []repoArtifactAttestationView
	if gate.Allowed {
		rows, err := h.rq.ListRepoArtifactAttestations(r.Context(), h.d.Pool, reposdb.ListRepoArtifactAttestationsParams{
			RepoID: row.ID,
			Limit:  repoAttestationListLimit,
			Offset: 0,
		})
		if err != nil {
			h.d.Logger.ErrorContext(r.Context(), "artifact attestation: list", "repo_id", row.ID, "error", err)
			errMsg = "Could not load artifact attestations."
		} else {
			views = repoArtifactAttestationViews(ownerSlug, row.Name, rows)
		}
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	data := h.repoHeaderData(r, row, ownerSlug, "security")
	data["Title"] = "Artifact attestations · " + row.Name
	data["Gate"] = gate
	data["Attestations"] = views
	data["CanUpload"] = gate.Allowed && policy.Can(r.Context(), policy.Deps{Pool: h.d.Pool}, viewer.PolicyActor(), policy.ActionRepoWrite, policy.NewRepoRefFromRepo(row)).Allow
	data["Created"] = r.URL.Query().Get("created") == "1"
	data["Error"] = errMsg
	if err := h.d.Render.RenderPage(w, r, "repo/security_attestations", data); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "artifact attestations page render", "repo_id", row.ID, "error", err)
	}
}

func (h *Handlers) repoAttestationGate(ctx context.Context, row reposdb.Repo, ownerSlug string) repoAttestationGate {
	gate := repoAttestationGate{
		Allowed:     true,
		FeatureKey:  string(entitlements.FeatureArtifactAttestations),
		UpgradeHref: "/organizations/" + ownerSlug + "/settings/billing",
		UpgradeText: "Upgrade to Team",
	}
	if row.Visibility != reposdb.RepoVisibilityPrivate || !row.OwnerOrgID.Valid {
		return gate
	}
	decision, err := entitlements.CheckPrincipalFeature(ctx,
		entitlements.Deps{Pool: h.d.Pool},
		billing.PrincipalForOrg(row.OwnerOrgID.Int64),
		entitlements.FeatureArtifactAttestations)
	if err != nil {
		gate.Allowed = false
		return gate
	}
	gate.Allowed = decision.Allowed
	return gate
}

func repoArtifactAttestationViews(owner, repoName string, rows []reposdb.RepoArtifactAttestation) []repoArtifactAttestationView {
	out := make([]repoArtifactAttestationView, 0, len(rows))
	for _, row := range rows {
		out = append(out, repoArtifactAttestationView{
			ID:            row.ID,
			SubjectName:   row.SubjectName,
			SubjectDigest: row.SubjectDigest,
			ShortDigest:   shortAttestationDigest(row.SubjectDigest),
			PredicateType: row.PredicateType,
			ByteCount:     row.ByteCount,
			ByteLabel:     repoSBOMByteLabel(row.ByteCount),
			SourceRunID:   row.SourceRunID,
			UploadedBy:    row.UploadedBy,
			CreatedAt:     row.CreatedAt,
			DownloadHref:  fmt.Sprintf("/%s/%s/security/attestations/%d/download", owner, repoName, row.ID),
		})
	}
	return out
}

func shortAttestationDigest(digest string) string {
	parts := strings.SplitN(digest, ":", 2)
	if len(parts) != 2 {
		return digest
	}
	if len(parts[1]) <= 12 {
		return digest
	}
	return parts[0] + ":" + parts[1][:12]
}

func safeAttestationFilename(owner, repoName string, id int64) string {
	raw := fmt.Sprintf("%s-%s-attestation-%d.intoto.json", owner, repoName, id)
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
