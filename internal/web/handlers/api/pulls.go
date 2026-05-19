// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tenseleyFlow/shithub/internal/auth/pat"
	"github.com/tenseleyFlow/shithub/internal/auth/policy"
	issuesdb "github.com/tenseleyFlow/shithub/internal/issues/sqlc"
	orgsdb "github.com/tenseleyFlow/shithub/internal/orgs/sqlc"
	"github.com/tenseleyFlow/shithub/internal/pulls"
	pullsdb "github.com/tenseleyFlow/shithub/internal/pulls/sqlc"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/handlers/api/apipage"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

// mountPulls registers the S50 §4 pull-request REST surface.
//
//	GET    /api/v1/repos/{o}/{r}/pulls                    list
//	POST   /api/v1/repos/{o}/{r}/pulls                    create
//	GET    /api/v1/repos/{o}/{r}/pulls/{number}           get
//	PATCH  /api/v1/repos/{o}/{r}/pulls/{number}           update (title/body/state/draft)
//	GET    /api/v1/repos/{o}/{r}/pulls/{number}/commits   list commits
//	GET    /api/v1/repos/{o}/{r}/pulls/{number}/files     list files
//	PUT    /api/v1/repos/{o}/{r}/pulls/{number}/merge     merge
//
// Reviews, review comments, requested reviewers, update-branch,
// auto-merge land in follow-up batches.
func (h *Handlers) mountPulls(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(middleware.RequireScope(pat.ScopeRepoRead))
		r.Get("/api/v1/repos/{owner}/{repo}/pulls", h.pullsList)
		r.Get("/api/v1/repos/{owner}/{repo}/pulls/{number}", h.pullGet)
		r.Get("/api/v1/repos/{owner}/{repo}/pulls/{number}/commits", h.pullCommitsList)
		r.Get("/api/v1/repos/{owner}/{repo}/pulls/{number}/files", h.pullFilesList)
	})
	r.Group(func(r chi.Router) {
		r.Use(middleware.RequireScope(pat.ScopeRepoWrite))
		r.Post("/api/v1/repos/{owner}/{repo}/pulls", h.pullCreate)
		r.Patch("/api/v1/repos/{owner}/{repo}/pulls/{number}", h.pullPatch)
		r.Put("/api/v1/repos/{owner}/{repo}/pulls/{number}/merge", h.pullMerge)
	})
}

// ─── presentation ───────────────────────────────────────────────────

// prRefEnvelope mirrors GitHub's nested base/head shape. The CLI's
// pulls.PullRequest type maps base/head as `{ref, sha, repo}` so the
// pre-S60 flat `base_ref`/`base_oid`/... fields rendered as empty
// strings in `shithub pr view`. We emit both shapes during transition.
//
// E-audit E2: added the `repo` envelope (A16 partial regression closeout).
// gh-compat fork-PR rendering needs the head/base repo nodes — without
// them the CLI can't tell whether a PR comes from a fork, and
// `--json baseRepository,headRepository` reads as null.
type prRefEnvelope struct {
	Ref  string          `json:"ref"`
	SHA  string          `json:"sha"`
	Repo *prRepoEnvelope `json:"repo"`
}

// prRepoEnvelope is the trimmed repo node that rides on PR base/head.
// Just enough for the CLI's `--json baseRepository,headRepository` to
// render owner/login plus the bits gh ships in this slot: id, name,
// full_name, owner, private, html_url. We avoid the full repoResponse
// shape so the per-PR lookup stays cheap.
type prRepoEnvelope struct {
	ID       int64              `json:"id"`
	Name     string             `json:"name"`
	FullName string             `json:"full_name"`
	Owner    *repoOwnerEnvelope `json:"owner"`
	Private  bool               `json:"private"`
	HTMLURL  string             `json:"html_url,omitempty"`
}

type pullResponse struct {
	ID     int64  `json:"id"`
	Number int64  `json:"number"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	State  string `json:"state"`
	Draft  bool   `json:"draft"`
	// Legacy flat fields; kept for one release cycle alongside the
	// nested envelopes. Prefer base/head for new clients.
	BaseRef string `json:"base_ref"`
	HeadRef string `json:"head_ref"`
	BaseOID string `json:"base_oid"`
	HeadOID string `json:"head_oid"`
	// GitHub-compat nested envelopes.
	Base           *prRefEnvelope `json:"base,omitempty"`
	Head           *prRefEnvelope `json:"head,omitempty"`
	Mergeable      *bool          `json:"mergeable,omitempty"`
	MergeableState string         `json:"mergeable_state"`
	Merged         bool           `json:"merged"`
	MergeCommit    string         `json:"merge_commit_sha,omitempty"`
	MergeMethod    string         `json:"merge_method,omitempty"`
	MergedAt       string         `json:"merged_at,omitempty"`
	// AuthorID is the legacy flat foreign key. Kept alongside the new
	// `user` envelope for one release cycle (S60 audit migration).
	AuthorID int64         `json:"author_id,omitempty"`
	User     *userEnvelope `json:"user,omitempty"`
	// HTMLURL is the user-facing page for this PR (B-audit B7).
	HTMLURL   string `json:"html_url,omitempty"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
	ClosedAt  string `json:"closed_at,omitempty"`
}

// presentPull is the pure builder; callers pass pre-resolved base+head
// repo envelopes (E2). For same-repo PRs (the common case) the caller
// can reuse one envelope for both slots; cross-repo PRs (forks)
// require a separate head lookup.
func presentPull(issue issuesdb.Issue, pr pullsdb.PullRequest, user *userEnvelope, baseRepo, headRepo *prRepoEnvelope) pullResponse {
	out := pullResponse{
		ID:             issue.ID,
		Number:         issue.Number,
		Title:          issue.Title,
		Body:           issue.Body,
		State:          string(issue.State),
		Draft:          pr.Draft,
		BaseRef:        pr.BaseRef,
		HeadRef:        pr.HeadRef,
		BaseOID:        pr.BaseOid,
		HeadOID:        pr.HeadOid,
		Base:           &prRefEnvelope{Ref: pr.BaseRef, SHA: pr.BaseOid, Repo: baseRepo},
		Head:           &prRefEnvelope{Ref: pr.HeadRef, SHA: pr.HeadOid, Repo: headRepo},
		MergeableState: string(pr.MergeableState),
		Merged:         pr.MergedAt.Valid,
		User:           user,
		CreatedAt:      issue.CreatedAt.Time.UTC().Format(time.RFC3339),
		UpdatedAt:      issue.UpdatedAt.Time.UTC().Format(time.RFC3339),
	}
	if pr.Mergeable.Valid {
		v := pr.Mergeable.Bool
		out.Mergeable = &v
	}
	if pr.MergeCommitSha.Valid {
		out.MergeCommit = pr.MergeCommitSha.String
	}
	if pr.MergeMethod.Valid {
		out.MergeMethod = string(pr.MergeMethod.PrMergeMethod)
	}
	if pr.MergedAt.Valid {
		out.MergedAt = pr.MergedAt.Time.UTC().Format(time.RFC3339)
	}
	if issue.AuthorUserID.Valid {
		out.AuthorID = issue.AuthorUserID.Int64
	}
	if issue.ClosedAt.Valid {
		out.ClosedAt = issue.ClosedAt.Time.UTC().Format(time.RFC3339)
	}
	return out
}

// prRepoEnvelopeFromRow builds the trimmed envelope from an already-
// fetched repo + its resolved owner login. Used by the same-repo
// path where the handler already has the row in hand (no extra DB
// hit needed).
func (h *Handlers) prRepoEnvelopeFromRow(repo reposdb.Repo, ownerLogin string) *prRepoEnvelope {
	ref := policy.NewRepoRefFromRepo(repo)
	ownerType := "user"
	if repo.OwnerOrgID.Valid {
		ownerType = "org"
	}
	env := &prRepoEnvelope{
		ID:       repo.ID,
		Name:     repo.Name,
		FullName: ownerLogin + "/" + repo.Name,
		Private:  ref.IsPrivate(),
		Owner: &repoOwnerEnvelope{
			Login: ownerLogin,
			Type:  capitalizeFirst(ownerType),
		},
	}
	if repo.OwnerUserID.Valid {
		env.Owner.ID = repo.OwnerUserID.Int64
	} else if repo.OwnerOrgID.Valid {
		env.Owner.ID = repo.OwnerOrgID.Int64
	}
	if h.d.BaseURL != "" {
		env.HTMLURL = strings.TrimRight(h.d.BaseURL, "/") + "/" + ownerLogin + "/" + repo.Name
	}
	return env
}

// prHeadRepoEnvelope returns the head-side envelope for a PR. Same-repo
// PRs reuse the base envelope to avoid an extra lookup; cross-repo PRs
// (forks) get a separate fetch keyed by HeadRepoID. Returns nil
// silently on lookup failure so the response degrades gracefully —
// a missing head repo node is preferable to a 500.
func (h *Handlers) prHeadRepoEnvelope(ctx context.Context, pr pullsdb.PullRequest, baseRepoID int64, baseEnv *prRepoEnvelope) *prRepoEnvelope {
	if pr.HeadRepoID == 0 || pr.HeadRepoID == baseRepoID {
		return baseEnv
	}
	headRepo, err := reposdb.New().GetRepoByID(ctx, h.d.Pool, pr.HeadRepoID)
	if err != nil {
		return nil
	}
	ownerRow, err := reposdb.New().GetRepoOwnerUsernameByID(ctx, h.d.Pool, pr.HeadRepoID)
	if err != nil {
		return nil
	}
	// sqlc types the COALESCE projection as interface{}; pgx unmarshals
	// it as string at runtime.
	owner, _ := ownerRow.OwnerUsername.(string)
	if owner == "" {
		return nil
	}
	return h.prRepoEnvelopeFromRow(headRepo, owner)
}

// pullHTMLURL composes the user-facing page URL for a PR. Mirrors
// issueHTMLURL's shape (B-audit B7).
func (h *Handlers) pullHTMLURL(ownerLogin, repoName string, number int64) string {
	if h.d.BaseURL == "" || ownerLogin == "" || repoName == "" {
		return ""
	}
	return strings.TrimRight(h.d.BaseURL, "/") + "/" + ownerLogin + "/" + repoName + "/pulls/" + strconv.FormatInt(number, 10)
}

type commitResponse2 struct {
	SHA            string `json:"sha"`
	Subject        string `json:"subject"`
	Body           string `json:"body,omitempty"`
	AuthorName     string `json:"author_name"`
	AuthorEmail    string `json:"author_email"`
	CommitterName  string `json:"committer_name"`
	CommitterEmail string `json:"committer_email"`
	AuthoredAt     string `json:"authored_at,omitempty"`
	CommittedAt    string `json:"committed_at,omitempty"`
}

type prFileResponse struct {
	Path      string `json:"path"`
	OldPath   string `json:"old_path,omitempty"`
	Status    string `json:"status"`
	Additions int32  `json:"additions"`
	Deletions int32  `json:"deletions"`
	Changes   int32  `json:"changes"`
}

// ─── list ───────────────────────────────────────────────────────────

func (h *Handlers) pullsList(w http.ResponseWriter, r *http.Request) {
	repo, ownerLogin, ok := h.resolveAPIRepoWithLogin(w, r, policy.ActionPullRead)
	if !ok {
		return
	}
	page, perPage := apipage.ParseQuery(r, apipage.DefaultPerPage, apipage.MaxPerPage)
	// E5: strict state. PR-specific value `merged` is post-filter
	// (merged_at IS NOT NULL); the sqlc query still takes open/closed/NULL.
	rawState := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("state")))
	var stateFilter pgtype.Text
	var wantMerged bool
	switch rawState {
	case "open":
		stateFilter = pgtype.Text{String: "open", Valid: true}
	case "closed":
		stateFilter = pgtype.Text{String: "closed", Valid: true}
	case "merged":
		// Merged PRs are closed; post-filter narrows further.
		stateFilter = pgtype.Text{String: "closed", Valid: true}
		wantMerged = true
	case "", "all":
		stateFilter = pgtype.Text{}
	default:
		writeAPIError(w, http.StatusUnprocessableEntity,
			fmt.Sprintf("state: must be one of open, closed, merged, all (got %q)", rawState))
		return
	}
	draftFilter, derr := strictDraftFilter(r.URL.Query().Get("draft"))
	if derr != nil {
		writeAPIError(w, http.StatusUnprocessableEntity, derr.Error())
		return
	}

	// E5: author/base/label filters — same treatment as the issue side.
	// G1: accept gh-canonical `creator` as an alias for `author`, and
	// add `assignee` + `head` filters so the CLI's gh-shape lands on
	// validation instead of silently passing through unfiltered.
	authorID, aerr := h.resolveOptionalUserID(r.Context(), firstQueryParam(r, "author", "creator"))
	if aerr != nil {
		writeAPIError(w, http.StatusUnprocessableEntity, "author: "+aerr.Error())
		return
	}
	assigneeID, aerr := h.resolveOptionalUserID(r.Context(), firstQueryParam(r, "assignee"))
	if aerr != nil {
		writeAPIError(w, http.StatusUnprocessableEntity, "assignee: "+aerr.Error())
		return
	}
	baseRef := firstQueryParam(r, "base")
	headRef := firstQueryParam(r, "head")
	// G5 (F15/F2-2): validate base + head against this repo's refs.
	// Pre-fix `--base BOGUS` silently empty and `--head BOGUS` silently
	// all/empty depending on G1 ordering — both shapes hide typos.
	// Now: ref doesn't exist → 422, matching the `?author=ghost` style.
	if err := h.validateRepoRefExists(r.Context(), ownerLogin, repo.Name, baseRef); err != nil {
		writeAPIError(w, http.StatusUnprocessableEntity, "base: "+err.Error())
		return
	}
	if err := h.validateRepoRefExists(r.Context(), ownerLogin, repo.Name, headRef); err != nil {
		writeAPIError(w, http.StatusUnprocessableEntity, "head: "+err.Error())
		return
	}
	wantedLabelIDs, lerr := h.parseAndValidateLabelsFilter(r, repo.ID)
	if lerr != nil {
		writeAPIError(w, http.StatusUnprocessableEntity, lerr.Error())
		return
	}

	q := pullsdb.New()
	total, err := q.CountPullRequestsByRepo(r.Context(), h.d.Pool, pullsdb.CountPullRequestsByRepoParams{
		RepoID:      repo.ID,
		StateFilter: stateFilter,
		Draft:       draftFilter,
	})
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: count pulls", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "list failed")
		return
	}
	rows, err := q.ListPullRequestsByRepo(r.Context(), h.d.Pool, pullsdb.ListPullRequestsByRepoParams{
		RepoID:      repo.ID,
		Limit:       int32(perPage),
		Offset:      int32((page - 1) * perPage),
		StateFilter: stateFilter,
		Draft:       draftFilter,
	})
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: list pulls", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "list failed")
		return
	}
	// E5 post-filters. Cheap row-local filters first; label post-filter
	// piggybacks on the issue-side helper since PRs share the issue
	// schema (kind='pr').
	if authorID != 0 {
		filtered := rows[:0]
		for _, row := range rows {
			if row.AuthorUserID.Valid && row.AuthorUserID.Int64 == authorID {
				filtered = append(filtered, row)
			}
		}
		rows = filtered
	}
	if baseRef != "" {
		filtered := rows[:0]
		for _, row := range rows {
			if row.BaseRef == baseRef {
				filtered = append(filtered, row)
			}
		}
		rows = filtered
	}
	if headRef != "" {
		filtered := rows[:0]
		for _, row := range rows {
			if row.HeadRef == headRef {
				filtered = append(filtered, row)
			}
		}
		rows = filtered
	}
	if assigneeID != 0 {
		// PRs share the issue assignee table (PR row joins to issues by
		// IssueID). Mirror the issue-side post-filter so `assignee=`
		// behaves consistently across both surfaces.
		filtered := rows[:0]
		for _, row := range rows {
			as, err := issuesdb.New().ListIssueAssignees(r.Context(), h.d.Pool, row.IssueID)
			if err != nil {
				continue
			}
			for _, a := range as {
				if a.UserID == assigneeID {
					filtered = append(filtered, row)
					break
				}
			}
		}
		rows = filtered
	}
	if wantMerged {
		filtered := rows[:0]
		for _, row := range rows {
			if row.MergedAt.Valid {
				filtered = append(filtered, row)
			}
		}
		rows = filtered
	}
	if len(wantedLabelIDs) > 0 {
		// Reuse the issue-side post-filter: it operates on rows that
		// have ID + RepoID, which both schemas share. Adapt via a
		// shim type so we can pass PR rows through.
		filtered := rows[:0]
		for _, row := range rows {
			matched := 0
			labels, err := issuesdb.New().ListLabelsOnIssue(r.Context(), h.d.Pool, row.IssueID)
			if err != nil {
				continue
			}
			for _, l := range labels {
				for _, wantID := range wantedLabelIDs {
					if l.ID == wantID {
						matched++
						break
					}
				}
			}
			if matched == len(wantedLabelIDs) {
				filtered = append(filtered, row)
			}
		}
		rows = filtered
	}

	link := apipage.Page{Current: page, PerPage: perPage, Total: int(total)}.LinkHeader(h.d.BaseURL, sanitizedURL(r))
	if link != "" {
		w.Header().Set("Link", link)
	}
	authorIDs := make([]int64, 0, len(rows))
	for _, row := range rows {
		if row.AuthorUserID.Valid {
			authorIDs = append(authorIDs, row.AuthorUserID.Int64)
		}
	}
	users := h.resolveUserEnvelopesBatch(r.Context(), authorIDs)
	// Build the base-repo envelope once — every row in this list lives
	// on this repo (the URL-resolved one), so base is shared. Head may
	// differ (fork PRs) and is resolved per-row via prHeadRepoEnvelope.
	baseRepoEnv := h.prRepoEnvelopeFromRow(repo, ownerLogin)
	out := make([]pullResponse, 0, len(rows))
	for _, row := range rows {
		var u *userEnvelope
		if row.AuthorUserID.Valid {
			u = users[row.AuthorUserID.Int64]
		}
		rowPR := pullsdb.PullRequest{
			IssueID:        row.IssueID,
			BaseRef:        row.BaseRef,
			HeadRef:        row.HeadRef,
			HeadRepoID:     row.HeadRepoID,
			BaseOid:        row.BaseOid,
			HeadOid:        row.HeadOid,
			Draft:          row.Draft,
			Mergeable:      row.Mergeable,
			MergeableState: row.MergeableState,
			MergeCommitSha: row.MergeCommitSha,
			MergedAt:       row.MergedAt,
			MergedByUserID: row.MergedByUserID,
			MergeMethod:    row.MergeMethod,
		}
		headRepoEnv := h.prHeadRepoEnvelope(r.Context(), rowPR, repo.ID, baseRepoEnv)
		resp := presentPull(issuesdb.Issue{
			ID:           row.ID,
			RepoID:       row.RepoID,
			Number:       row.Number,
			Title:        row.Title,
			Body:         row.Body,
			AuthorUserID: row.AuthorUserID,
			State:        issuesdb.IssueState(row.State),
			CreatedAt:    row.CreatedAt,
			UpdatedAt:    row.UpdatedAt,
		}, rowPR, u, baseRepoEnv, headRepoEnv)
		resp.HTMLURL = h.pullHTMLURL(ownerLogin, repo.Name, row.Number)
		out = append(out, resp)
	}
	writeJSON(w, http.StatusOK, out)
}

// strictDraftFilter validates the `draft` query parameter. Empty
// returns no filter; "true"/"false" set the filter; anything else
// 422s. Was previously lenient (silently dropped non-bool); tightened
// for the E5 cluster.
func strictDraftFilter(s string) (pgtype.Bool, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "true":
		return pgtype.Bool{Bool: true, Valid: true}, nil
	case "false":
		return pgtype.Bool{Bool: false, Valid: true}, nil
	case "":
		return pgtype.Bool{}, nil
	default:
		return pgtype.Bool{}, fmt.Errorf("draft: must be true or false (got %q)", s)
	}
}

// ─── single ─────────────────────────────────────────────────────────

func (h *Handlers) pullGet(w http.ResponseWriter, r *http.Request) {
	repo, ownerLogin, ok := h.resolveAPIRepoWithLogin(w, r, policy.ActionPullRead)
	if !ok {
		return
	}
	issue, pr, ok := h.resolvePRByNumber(w, r, repo.ID, chi.URLParam(r, "number"))
	if !ok {
		return
	}
	var u *userEnvelope
	if issue.AuthorUserID.Valid {
		u = h.resolveUserEnvelope(r.Context(), issue.AuthorUserID.Int64)
	}
	baseEnv := h.prRepoEnvelopeFromRow(repo, ownerLogin)
	headEnv := h.prHeadRepoEnvelope(r.Context(), pr, repo.ID, baseEnv)
	resp := presentPull(issue, pr, u, baseEnv, headEnv)
	resp.HTMLURL = h.pullHTMLURL(ownerLogin, repo.Name, issue.Number)
	writeJSON(w, http.StatusOK, resp)
}

// ─── create ─────────────────────────────────────────────────────────

type pullCreateRequest struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	Base  string `json:"base"`
	Head  string `json:"head"`
	Draft bool   `json:"draft"`
}

func (h *Handlers) pullCreate(w http.ResponseWriter, r *http.Request) {
	repo, ownerLogin, ok := h.resolveAPIRepoWithLogin(w, r, policy.ActionPullCreate)
	if !ok {
		return
	}
	auth := middleware.PATAuthFromContext(r.Context())
	var body pullCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	gitDir, err := h.repoGitDir(r.Context(), &repo)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: resolve gitDir", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "create failed")
		return
	}
	res, err := pulls.Create(r.Context(), pulls.Deps{Pool: h.d.Pool, Logger: h.d.Logger, Audit: h.d.Audit}, pulls.CreateParams{
		RepoID:       repo.ID,
		AuthorUserID: auth.UserID,
		Title:        body.Title,
		Body:         body.Body,
		BaseRef:      body.Base,
		HeadRef:      body.Head,
		Draft:        body.Draft,
		GitDir:       gitDir,
	})
	if err != nil {
		writePullsError(w, err)
		return
	}
	u := h.resolveUserEnvelope(r.Context(), auth.UserID)
	baseEnv := h.prRepoEnvelopeFromRow(repo, ownerLogin)
	headEnv := h.prHeadRepoEnvelope(r.Context(), res.PullRequest, repo.ID, baseEnv)
	resp := presentPull(res.Issue, res.PullRequest, u, baseEnv, headEnv)
	resp.HTMLURL = h.pullHTMLURL(ownerLogin, repo.Name, res.Issue.Number)
	writeJSON(w, http.StatusCreated, resp)
}

// ─── patch ──────────────────────────────────────────────────────────

type pullPatchRequest struct {
	Title *string `json:"title,omitempty"`
	Body  *string `json:"body,omitempty"`
	State *string `json:"state,omitempty"`
	Draft *bool   `json:"draft,omitempty"`
	// G7 (F27): gh-compat `base` ref change. Pre-fix the struct didn't
	// carry this field and JSON decode silently dropped it — every
	// `pr edit --base feature` reported success but kept the old
	// base. Now persisted (with same-as-head guard + git-side ref
	// existence check) via pulls.SetBase.
	Base *string `json:"base,omitempty"`
}

func (h *Handlers) pullPatch(w http.ResponseWriter, r *http.Request) {
	repo, ownerLogin, ok := h.resolveAPIRepoWithLogin(w, r, policy.ActionPullRead)
	if !ok {
		return
	}
	// E25 (PR side): PATCH must refuse on archived repos. The read
	// gate let us through; close the archive escape valve here.
	if policy.NewRepoRefFromRepo(repo).Archived() {
		writeAPIError(w, http.StatusForbidden, "repository is archived")
		return
	}
	auth := middleware.PATAuthFromContext(r.Context())
	if auth.UserID == 0 {
		writeAPIError(w, http.StatusUnauthorized, "unauthenticated")
		return
	}
	issue, pr, ok := h.resolvePRByNumber(w, r, repo.ID, chi.URLParam(r, "number"))
	if !ok {
		return
	}
	var body pullPatchRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	if body.Title != nil || body.Body != nil {
		canEdit := issue.AuthorUserID.Valid && issue.AuthorUserID.Int64 == auth.UserID
		if !canEdit {
			canEdit = policy.Can(r.Context(), policy.Deps{Pool: h.d.Pool}, auth.PolicyActor(), policy.ActionRepoWrite, policy.NewRepoRefFromRepo(repo)).Allow
		}
		if !canEdit {
			writeAPIError(w, http.StatusForbidden, "only the author or a repo collaborator may edit this pull request")
			return
		}
		title := issue.Title
		if body.Title != nil {
			title = *body.Title
		}
		bodyText := issue.Body
		if body.Body != nil {
			bodyText = *body.Body
		}
		if err := pulls.EditPR(r.Context(), pulls.Deps{Pool: h.d.Pool, Logger: h.d.Logger, Audit: h.d.Audit}, issue.ID, title, bodyText); err != nil {
			writePullsError(w, err)
			return
		}
	}

	if body.Draft != nil && pr.Draft && !*body.Draft {
		// Only the author can flip draft→ready in v1.
		isAuthor := issue.AuthorUserID.Valid && issue.AuthorUserID.Int64 == auth.UserID
		if !isAuthor {
			writeAPIError(w, http.StatusForbidden, "only the author may mark a draft PR as ready")
			return
		}
		if err := pulls.SetReady(r.Context(), pulls.Deps{Pool: h.d.Pool, Logger: h.d.Logger, Audit: h.d.Audit}, auth.UserID, issue.ID); err != nil {
			writePullsError(w, err)
			return
		}
	}
	if body.Draft != nil && !pr.Draft && *body.Draft {
		writeAPIError(w, http.StatusUnprocessableEntity, "ready→draft is not supported")
		return
	}

	if body.State != nil {
		if !policy.Can(r.Context(), policy.Deps{Pool: h.d.Pool}, auth.PolicyActor(), policy.ActionPullClose, policy.NewRepoRefFromRepo(repo)).Allow {
			writeAPIError(w, http.StatusForbidden, "lack permission to change PR state")
			return
		}
		newState := strings.ToLower(*body.State)
		if newState != "open" && newState != "closed" {
			writeAPIError(w, http.StatusUnprocessableEntity, "state must be open or closed")
			return
		}
		gitDir, err := h.repoGitDir(r.Context(), &repo)
		if err != nil {
			h.d.Logger.ErrorContext(r.Context(), "api: resolve gitDir", "error", err)
			writeAPIError(w, http.StatusInternalServerError, "state change failed")
			return
		}
		if err := pulls.SetState(r.Context(), pulls.Deps{Pool: h.d.Pool, Logger: h.d.Logger, Audit: h.d.Audit}, gitDir, auth.UserID, issue.ID, newState); err != nil {
			writePullsError(w, err)
			return
		}
	}

	// G7 (F27): `pr edit --base <ref>`. Author or repo-write
	// collaborator can rebase the PR onto a new base; mergeability
	// recomputes against the new base on the next worker tick. The
	// orchestrator validates ref existence + same-as-head.
	if body.Base != nil {
		canEdit := issue.AuthorUserID.Valid && issue.AuthorUserID.Int64 == auth.UserID
		if !canEdit {
			canEdit = policy.Can(r.Context(), policy.Deps{Pool: h.d.Pool}, auth.PolicyActor(), policy.ActionRepoWrite, policy.NewRepoRefFromRepo(repo)).Allow
		}
		if !canEdit {
			writeAPIError(w, http.StatusForbidden, "only the author or a repo collaborator may change the base")
			return
		}
		gitDir, err := h.repoGitDir(r.Context(), &repo)
		if err != nil {
			h.d.Logger.ErrorContext(r.Context(), "api: resolve gitDir", "error", err)
			writeAPIError(w, http.StatusInternalServerError, "base change failed")
			return
		}
		if _, err := pulls.SetBase(r.Context(),
			pulls.Deps{Pool: h.d.Pool, Logger: h.d.Logger, Audit: h.d.Audit},
			gitDir, auth.UserID, issue.ID, pr.HeadRef, *body.Base); err != nil {
			writePullsError(w, err)
			return
		}
	}

	// Reload everything for the response.
	freshIssue, _ := issuesdb.New().GetIssueByID(r.Context(), h.d.Pool, issue.ID)
	freshPR, _ := pullsdb.New().GetPullRequestByIssueID(r.Context(), h.d.Pool, issue.ID)
	var u *userEnvelope
	if freshIssue.AuthorUserID.Valid {
		u = h.resolveUserEnvelope(r.Context(), freshIssue.AuthorUserID.Int64)
	}
	baseEnv := h.prRepoEnvelopeFromRow(repo, ownerLogin)
	headEnv := h.prHeadRepoEnvelope(r.Context(), freshPR, repo.ID, baseEnv)
	resp := presentPull(freshIssue, freshPR, u, baseEnv, headEnv)
	resp.HTMLURL = h.pullHTMLURL(ownerLogin, repo.Name, freshIssue.Number)
	writeJSON(w, http.StatusOK, resp)
}

// ─── commits + files ────────────────────────────────────────────────

func (h *Handlers) pullCommitsList(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.resolveAPIRepo(w, r, policy.ActionPullRead)
	if !ok {
		return
	}
	_, pr, ok := h.resolvePRByNumber(w, r, repo.ID, chi.URLParam(r, "number"))
	if !ok {
		return
	}
	rows, err := pullsdb.New().ListPullRequestCommits(r.Context(), h.d.Pool, pr.IssueID)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: list pr commits", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "list failed")
		return
	}
	out := make([]commitResponse2, 0, len(rows))
	for _, c := range rows {
		entry := commitResponse2{
			SHA:            c.Sha,
			Subject:        c.Subject,
			Body:           c.Body,
			AuthorName:     c.AuthorName,
			AuthorEmail:    c.AuthorEmail,
			CommitterName:  c.CommitterName,
			CommitterEmail: c.CommitterEmail,
		}
		if c.AuthoredAt.Valid {
			entry.AuthoredAt = c.AuthoredAt.Time.UTC().Format(time.RFC3339)
		}
		if c.CommittedAt.Valid {
			entry.CommittedAt = c.CommittedAt.Time.UTC().Format(time.RFC3339)
		}
		out = append(out, entry)
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handlers) pullFilesList(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.resolveAPIRepo(w, r, policy.ActionPullRead)
	if !ok {
		return
	}
	_, pr, ok := h.resolvePRByNumber(w, r, repo.ID, chi.URLParam(r, "number"))
	if !ok {
		return
	}
	rows, err := pullsdb.New().ListPullRequestFiles(r.Context(), h.d.Pool, pr.IssueID)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: list pr files", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "list failed")
		return
	}
	out := make([]prFileResponse, 0, len(rows))
	for _, f := range rows {
		entry := prFileResponse{
			Path:      f.Path,
			Status:    string(f.Status),
			Additions: f.Additions,
			Deletions: f.Deletions,
			Changes:   f.Changes,
		}
		if f.OldPath.Valid {
			entry.OldPath = f.OldPath.String
		}
		out = append(out, entry)
	}
	writeJSON(w, http.StatusOK, out)
}

// ─── merge ──────────────────────────────────────────────────────────

type pullMergeRequest struct {
	CommitTitle   string `json:"commit_title"`
	CommitMessage string `json:"commit_message"`
	MergeMethod   string `json:"merge_method"`
	SHA           string `json:"sha"`
}

func (h *Handlers) pullMerge(w http.ResponseWriter, r *http.Request) {
	repo, ownerLogin, ok := h.resolveAPIRepoWithLogin(w, r, policy.ActionPullMerge)
	if !ok {
		return
	}
	auth := middleware.PATAuthFromContext(r.Context())
	if auth.UserID == 0 {
		writeAPIError(w, http.StatusUnauthorized, "unauthenticated")
		return
	}
	_, pr, ok := h.resolvePRByNumber(w, r, repo.ID, chi.URLParam(r, "number"))
	if !ok {
		return
	}
	var body pullMergeRequest
	_ = json.NewDecoder(r.Body).Decode(&body) // body is optional
	method := strings.ToLower(strings.TrimSpace(body.MergeMethod))
	if method == "" {
		method = string(repo.DefaultMergeMethod)
	}
	if body.SHA != "" && body.SHA != pr.HeadOid {
		writeAPIError(w, http.StatusConflict, "head sha mismatch")
		return
	}
	gitDir, err := h.repoGitDir(r.Context(), &repo)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: resolve gitDir", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "merge failed")
		return
	}
	if err := pulls.Merge(r.Context(), pulls.Deps{Pool: h.d.Pool, Logger: h.d.Logger, Audit: h.d.Audit}, pulls.MergeParams{
		PRID:        pr.IssueID,
		ActorUserID: auth.UserID,
		GitDir:      gitDir,
		Method:      method,
		Subject:     body.CommitTitle,
		Body:        body.CommitMessage,
	}); err != nil {
		writePullsError(w, err)
		return
	}
	freshIssue, _ := issuesdb.New().GetIssueByID(r.Context(), h.d.Pool, pr.IssueID)
	freshPR, _ := pullsdb.New().GetPullRequestByIssueID(r.Context(), h.d.Pool, pr.IssueID)
	var u *userEnvelope
	if freshIssue.AuthorUserID.Valid {
		u = h.resolveUserEnvelope(r.Context(), freshIssue.AuthorUserID.Int64)
	}
	baseEnv := h.prRepoEnvelopeFromRow(repo, ownerLogin)
	headEnv := h.prHeadRepoEnvelope(r.Context(), freshPR, repo.ID, baseEnv)
	resp := presentPull(freshIssue, freshPR, u, baseEnv, headEnv)
	resp.HTMLURL = h.pullHTMLURL(ownerLogin, repo.Name, freshIssue.Number)
	writeJSON(w, http.StatusOK, resp)
}

// ─── helpers ────────────────────────────────────────────────────────

// resolvePRByNumber resolves a PR by repo+number. The repo gate has
// already been satisfied by resolveAPIRepo; non-PR issues (kind="issue")
// 404 here.
func (h *Handlers) resolvePRByNumber(w http.ResponseWriter, r *http.Request, repoID int64, numberRaw string) (issuesdb.Issue, pullsdb.PullRequest, bool) {
	num, err := strconv.ParseInt(numberRaw, 10, 64)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "pull request not found")
		return issuesdb.Issue{}, pullsdb.PullRequest{}, false
	}
	issue, err := issuesdb.New().GetIssueByNumber(r.Context(), h.d.Pool, issuesdb.GetIssueByNumberParams{
		RepoID: repoID, Number: num,
	})
	if err != nil || issue.Kind != issuesdb.IssueKindPr {
		writeAPIError(w, http.StatusNotFound, "pull request not found")
		return issuesdb.Issue{}, pullsdb.PullRequest{}, false
	}
	pr, err := pullsdb.New().GetPullRequestByIssueID(r.Context(), h.d.Pool, issue.ID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeAPIError(w, http.StatusNotFound, "pull request not found")
			return issuesdb.Issue{}, pullsdb.PullRequest{}, false
		}
		h.d.Logger.ErrorContext(r.Context(), "api: load pr row", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "lookup failed")
		return issuesdb.Issue{}, pullsdb.PullRequest{}, false
	}
	return issue, pr, true
}

// writePullsError maps the orchestrator's typed errors to HTTP codes.
func writePullsError(w http.ResponseWriter, err error) {
	// G6 (F46): duplicate-PR is a typed error (carries the existing
	// number); map it to 422 with the orchestrator's message so the CLI
	// can surface "#N already exists" without re-fetching.
	var dup *pulls.DuplicatePRError
	if errors.As(err, &dup) {
		writeAPIError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	switch {
	case errors.Is(err, pulls.ErrSameBranch),
		errors.Is(err, pulls.ErrBaseNotFound),
		errors.Is(err, pulls.ErrHeadNotFound),
		errors.Is(err, pulls.ErrNoCommitsToMerge),
		errors.Is(err, pulls.ErrMergeMethodOff):
		writeAPIError(w, http.StatusUnprocessableEntity, err.Error())
	case errors.Is(err, pulls.ErrAlreadyMerged),
		errors.Is(err, pulls.ErrAlreadyClosed),
		errors.Is(err, pulls.ErrMergeBlocked):
		writeAPIError(w, http.StatusConflict, err.Error())
	case errors.Is(err, pulls.ErrConcurrentMerge):
		writeAPIError(w, http.StatusServiceUnavailable, "another merge is in flight")
	case errors.Is(err, pulls.ErrPRNotFound):
		writeAPIError(w, http.StatusNotFound, "pull request not found")
	default:
		writeAPIError(w, http.StatusInternalServerError, "internal error")
	}
}

// repoGitDir resolves the on-disk bare-repo path for a row through
// RepoFS. User-owned repos use the username as the on-disk slug;
// org-owned repos use the org slug. Mirrors the layout repos.Create
// established at creation time.
func (h *Handlers) repoGitDir(ctx context.Context, repo *reposdb.Repo) (string, error) {
	if h.d.RepoFS == nil {
		return "", errors.New("api: RepoFS not configured")
	}
	slug, err := h.repoOwnerSlug(ctx, repo)
	if err != nil {
		return "", err
	}
	return h.d.RepoFS.RepoPath(slug, repo.Name)
}

func (h *Handlers) repoOwnerSlug(ctx context.Context, repo *reposdb.Repo) (string, error) {
	if repo.OwnerUserID.Valid {
		user, err := usersdb.New().GetUserByID(ctx, h.d.Pool, repo.OwnerUserID.Int64)
		if err != nil {
			return "", err
		}
		return user.Username, nil
	}
	if repo.OwnerOrgID.Valid {
		org, err := orgsdb.New().GetOrgByID(ctx, h.d.Pool, repo.OwnerOrgID.Int64)
		if err != nil {
			return "", err
		}
		return string(org.Slug), nil
	}
	return "", errors.New("api: repo has no owner")
}
