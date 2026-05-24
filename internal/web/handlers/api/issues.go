// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"context"
	"encoding/base64"
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
	"github.com/tenseleyFlow/shithub/internal/issues"
	issuesdb "github.com/tenseleyFlow/shithub/internal/issues/sqlc"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/handlers/api/apipage"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

// mountIssues registers the S50 §3 issue REST surface.
//
//	GET    /api/v1/repos/{o}/{r}/issues                     list
//	POST   /api/v1/repos/{o}/{r}/issues                     create
//	GET    /api/v1/repos/{o}/{r}/issues/{number}            get
//	PATCH  /api/v1/repos/{o}/{r}/issues/{number}            update (title, body, state, state_reason)
//	GET    /api/v1/repos/{o}/{r}/issues/{number}/comments   list comments
//	POST   /api/v1/repos/{o}/{r}/issues/{number}/comments   add comment
//	PATCH  /api/v1/repos/{o}/{r}/issues/comments/{id}       edit comment
//	DELETE /api/v1/repos/{o}/{r}/issues/comments/{id}       delete comment
//	PUT    /api/v1/repos/{o}/{r}/issues/{number}/lock       lock
//	DELETE /api/v1/repos/{o}/{r}/issues/{number}/lock       unlock
//
// PAT scopes: repo:read on GETs, repo:write on mutations. Policy gates
// (ActionIssueRead/Create/Close/etc.) layer on top of the scope check;
// existence-leak-safe 404 on visibility miss.
func (h *Handlers) mountIssues(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(middleware.RequireScope(pat.ScopeRepoRead))
		r.Get("/api/v1/repos/{owner}/{repo}/issues", h.issuesList)
		r.Get("/api/v1/repos/{owner}/{repo}/issues/{number}", h.issueGet)
		r.Get("/api/v1/repos/{owner}/{repo}/issues/{number}/comments", h.issueCommentsList)
	})
	r.Group(func(r chi.Router) {
		r.Use(middleware.RequireScope(pat.ScopeRepoWrite))
		r.Post("/api/v1/repos/{owner}/{repo}/issues", h.issueCreate)
		r.Patch("/api/v1/repos/{owner}/{repo}/issues/{number}", h.issuePatch)
		// G8 (F45): DELETE /issues/{N} is admin-only — mounted in the
		// write group so the scope check passes, then the handler
		// re-gates on ActionRepoAdmin so non-admin write collaborators
		// can't hard-delete issues.
		r.Delete("/api/v1/repos/{owner}/{repo}/issues/{number}", h.issueDelete)
		r.Post("/api/v1/repos/{owner}/{repo}/issues/{number}/comments", h.issueCommentCreate)
		r.Patch("/api/v1/repos/{owner}/{repo}/issues/comments/{cid}", h.issueCommentUpdate)
		r.Delete("/api/v1/repos/{owner}/{repo}/issues/comments/{cid}", h.issueCommentDelete)
		r.Put("/api/v1/repos/{owner}/{repo}/issues/{number}/lock", h.issueLock)
		r.Delete("/api/v1/repos/{owner}/{repo}/issues/{number}/lock", h.issueUnlock)
	})
}

// ─── presentation ───────────────────────────────────────────────────

type issueResponse struct {
	ID     int64  `json:"id"`
	Number int64  `json:"number"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	State  string `json:"state"`
	// StateReason is `*string` so the key surfaces with explicit `null`
	// on open issues (I7a audit-I12). gh-compat clients parse against
	// presence-of-key, not omitempty.
	StateReason *string `json:"state_reason"`
	Locked      bool    `json:"locked"`
	LockReason  string  `json:"lock_reason,omitempty"`
	// ActiveLockReason mirrors LockReason but is the gh-canonical field
	// name (audit-I12); kept alongside for one release cycle.
	ActiveLockReason *string `json:"active_lock_reason"`
	// User is the gh-compat author envelope. I7b (audit-I10) stripped
	// the legacy `author_id` raw-FK field: two ways to identify the
	// author was a data-model leak and a per-resource consistency
	// hazard. Consumers that need the integer can read `user.id`.
	User *userEnvelope `json:"user,omitempty"`
	// Assignees mirrors gh's issue payload (C-audit C20a). nil when
	// no assignees; non-nil empty slice when the issue exists but is
	// unassigned (gh shape). Always populated by presentIssue (the
	// caller resolves and passes through, just like Labels).
	Assignees []userEnvelope `json:"assignees"`
	// HTMLURL is the user-facing page for this issue (B-audit B7).
	// Populated when ownerLogin is available at the callsite — list/
	// get/create/patch all have it; legacy paths without it gracefully
	// omit the key via omitempty.
	HTMLURL string `json:"html_url,omitempty"`
	// Labels mirrors Assignees: always present, never nil. gh-compat
	// clients parse against the key being present (E27); an empty slice
	// serializes as `[]` rather than disappearing into `omitempty`.
	Labels []labelEnvelope `json:"labels"`
	// Milestone is the issue's milestone object, or null when none. gh
	// emits the field with `null` when unset; we mirror that so the
	// `--json milestone` CLI exporter and gh-compat decoders see a
	// stable key (E3). Absent before — the field has never been
	// surfaced.
	Milestone *milestoneIssueEnvelope `json:"milestone"`
	CreatedAt string                  `json:"created_at"`
	UpdatedAt string                  `json:"updated_at"`
	// ClosedAt is `*string` so the key is `null` on open issues and an
	// RFC3339 timestamp once closed (I7a audit-I12).
	ClosedAt *string `json:"closed_at"`
	// ClosedBy is the user who closed the issue. Null when open or
	// when the close pre-dates the columns being populated. Best-effort
	// lookup — a join failure emits null rather than fails the GET.
	ClosedBy *userEnvelope `json:"closed_by"`

	// ─── I7a (audit-I12): gh-compat expansion ─────────────────────────
	//
	// NodeID is the opaque base64 of `gid://shithub/Issue/{id}`.
	NodeID string `json:"node_id,omitempty"`
	// Comments is the count of comments on this issue. Distinct from
	// the `/comments` collection endpoint, which returns the rows.
	Comments int64 `json:"comments"`
	// Reactions is gh's reaction-count rollup. shithub doesn't ship
	// reactions today — stub `{total_count: 0, url: "…/reactions"}`
	// so gh-compat clients see the key and skip cleanly.
	Reactions *issueReactionsEnvelope `json:"reactions,omitempty"`
	// AuthorAssociation is the gh-compat 5-value enum describing how
	// the issue author relates to the repo (OWNER / MEMBER /
	// COLLABORATOR / CONTRIBUTOR / NONE). See policy.AuthorAssociation.
	AuthorAssociation string `json:"author_association,omitempty"`
	// RepositoryURL is the API URL of the owning repo. gh emits this
	// on every issue-shaped response.
	RepositoryURL string `json:"repository_url,omitempty"`
	// EventsURL / LabelsURL / CommentsURL are sub-resource URLs gh
	// emits as a small bundle. The {/name} placeholder on LabelsURL
	// is gh's documented form for url-template hints.
	EventsURL   string `json:"events_url,omitempty"`
	LabelsURL   string `json:"labels_url,omitempty"`
	CommentsURL string `json:"comments_url,omitempty"`
	// PerformedViaGitHubApp is always null on shithub (no GitHub Apps
	// surface). Emitting it explicitly lets gh-compat clients null-check
	// instead of crashing on the missing key.
	PerformedViaGitHubApp *struct{} `json:"performed_via_github_app"`
}

// issueReactionsEnvelope is the gh-compat reactions bundle. shithub
// hasn't shipped reactions, so the count is hard-zero and the URL
// points back at where the collection *would* live. Clients use
// `total_count` to feature-detect.
type issueReactionsEnvelope struct {
	URL        string `json:"url"`
	TotalCount int64  `json:"total_count"`
	PlusOne    int64  `json:"+1"`
	MinusOne   int64  `json:"-1"`
	Laugh      int64  `json:"laugh"`
	Hooray     int64  `json:"hooray"`
	Confused   int64  `json:"confused"`
	Heart      int64  `json:"heart"`
	Rocket     int64  `json:"rocket"`
	Eyes       int64  `json:"eyes"`
}

// milestoneIssueEnvelope is the trimmed milestone shape that
// surfaces on issue responses. The full milestoneResponse carries
// open_issues / closed_issues counts; we omit them here because
// they'd cost an extra COUNT(*) per issue on the list endpoint.
// Callers needing counts hit /api/v1/repos/.../milestones/{id}.
type milestoneIssueEnvelope struct {
	ID          int64  `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
	State       string `json:"state"`
	DueOn       string `json:"due_on,omitempty"`
	CreatedAt   string `json:"created_at"`
	ClosedAt    string `json:"closed_at,omitempty"`
}

func presentMilestoneIssueEnvelope(m issuesdb.Milestone) *milestoneIssueEnvelope {
	out := &milestoneIssueEnvelope{
		ID:          m.ID,
		Title:       m.Title,
		Description: m.Description,
		State:       string(m.State),
		CreatedAt:   m.CreatedAt.Time.UTC().Format(time.RFC3339),
	}
	if m.DueOn.Valid {
		out.DueOn = m.DueOn.Time.UTC().Format(time.RFC3339)
	}
	if m.ClosedAt.Valid {
		out.ClosedAt = m.ClosedAt.Time.UTC().Format(time.RFC3339)
	}
	return out
}

// presentIssue fills the issue envelope. The user pointer is optional;
// callers pass it pre-resolved so list endpoints can batch the lookup
// (and pass nil for the freshly-created-just-now path where the author
// is the authenticated caller and we can construct the envelope from
// the auth context cheaper than a round-trip).
//
// assignees is the resolved slice; pass a non-nil empty slice (never
// nil) so the field always serializes — gh-compat clients expect the
// key to be present (C20a). The single-issue paths build it via
// assigneeEnvelopesFor; the list endpoint batches.
//
// milestone is the issue's milestone envelope (or nil when none) —
// callers resolve it via milestoneEnvelopeFor so the lookup happens
// outside this pure builder.
func presentIssue(i issuesdb.Issue, labels []labelEnvelope, user *userEnvelope, assignees []userEnvelope, milestone *milestoneIssueEnvelope) issueResponse {
	// Back-compat thin wrapper for the legacy 5-arg shape. List + create
	// paths use this signature; the single-issue GET path (which has
	// the actor + repo context needed for permissions/associations)
	// calls presentIssueFull directly.
	return presentIssueFull(i, labels, user, assignees, milestone, issueExtras{})
}

// issueExtras carries the I7a (audit-I12) gh-compat expansion inputs.
// All fields are optional — the zero value emits the legacy shape plus
// gh-compat default null/empty values for the new keys, which is the
// right behavior for code paths (issue create response) where the
// caller doesn't have the data yet.
type issueExtras struct {
	NodeID            string
	CommentsCount     int64
	AuthorAssociation string
	ClosedBy          *userEnvelope
	RepositoryURL     string
	EventsURL         string
	LabelsURL         string
	CommentsURL       string
}

func presentIssueFull(
	i issuesdb.Issue,
	labels []labelEnvelope,
	user *userEnvelope,
	assignees []userEnvelope,
	milestone *milestoneIssueEnvelope,
	extras issueExtras,
) issueResponse {
	if assignees == nil {
		assignees = []userEnvelope{}
	}
	if labels == nil {
		labels = []labelEnvelope{}
	}
	out := issueResponse{
		ID:                    i.ID,
		Number:                i.Number,
		Title:                 i.Title,
		Body:                  i.Body,
		State:                 string(i.State),
		Locked:                i.Locked,
		Labels:                labels,
		User:                  user,
		Assignees:             assignees,
		Milestone:             milestone,
		CreatedAt:             i.CreatedAt.Time.UTC().Format(time.RFC3339),
		UpdatedAt:             i.UpdatedAt.Time.UTC().Format(time.RFC3339),
		NodeID:                extras.NodeID,
		Comments:              extras.CommentsCount,
		Reactions:             zeroReactionsEnvelope(extras.RepositoryURL, i.Number),
		AuthorAssociation:     extras.AuthorAssociation,
		ClosedBy:              extras.ClosedBy,
		RepositoryURL:         extras.RepositoryURL,
		EventsURL:             extras.EventsURL,
		LabelsURL:             extras.LabelsURL,
		CommentsURL:           extras.CommentsURL,
		PerformedViaGitHubApp: nil, // gh-compat: always null on shithub
	}
	if i.StateReason.Valid {
		s := string(i.StateReason.IssueStateReason)
		out.StateReason = &s
	}
	if i.LockReason.Valid {
		out.LockReason = i.LockReason.String
		// I7a (audit-I12): active_lock_reason is gh's canonical name.
		// Emit it whenever the legacy LockReason carries content so
		// gh-compat clients see the right key.
		s := i.LockReason.String
		out.ActiveLockReason = &s
	}
	// I7b (audit-I10): the raw author_id FK field was dropped from the
	// response shape; user.id carries the integer for consumers that
	// still want one. Author resolution flows through the userEnvelope
	// argument the caller passes in.
	if i.ClosedAt.Valid {
		s := i.ClosedAt.Time.UTC().Format(time.RFC3339)
		out.ClosedAt = &s
	}
	return out
}

// zeroReactionsEnvelope returns the gh-compat stub for an issue with
// no reactions. Returns nil when we don't have a repository URL to
// build the reactions link (the legacy presentIssue signature path).
func zeroReactionsEnvelope(repoURL string, issueNumber int64) *issueReactionsEnvelope {
	if repoURL == "" {
		return nil
	}
	return &issueReactionsEnvelope{
		URL:        fmt.Sprintf("%s/issues/%d/reactions", repoURL, issueNumber),
		TotalCount: 0,
	}
}

// issueNodeID is the opaque base64 of `gid://shithub/Issue/{id}`.
// gh-compat clients use it as the GraphQL node cache key.
func issueNodeID(id int64) string {
	raw := "gid://shithub/Issue/" + strconv.FormatInt(id, 10)
	return base64.StdEncoding.EncodeToString([]byte(raw))
}

// buildIssueSubURLs returns the (repository, events, labels, comments)
// URL bundle for an issue. baseURL must be non-empty; callers check
// that upstream. The labels URL carries gh's documented {/name} URL
// template hint.
func buildIssueSubURLs(baseURL, ownerLogin, repoName string, issueNumber int64) (repoURL, eventsURL, labelsURL, commentsURL string) {
	repoPrefix := strings.TrimRight(baseURL, "/") + "/api/v1/repos/" + ownerLogin + "/" + repoName
	issuePrefix := repoPrefix + "/issues/" + strconv.FormatInt(issueNumber, 10)
	return repoPrefix,
		issuePrefix + "/events",
		issuePrefix + "/labels{/name}",
		issuePrefix + "/comments"
}

type commentResponse struct {
	ID      int64 `json:"id"`
	IssueID int64 `json:"issue_id"`
	// I7b (audit-I10): the raw author_id FK field was dropped — read
	// user.id when an integer is needed.
	User      *userEnvelope `json:"user,omitempty"`
	Body      string        `json:"body"`
	CreatedAt string        `json:"created_at"`
	UpdatedAt string        `json:"updated_at"`
	EditedAt  string        `json:"edited_at,omitempty"`
}

func presentComment(c issuesdb.IssueComment, user *userEnvelope) commentResponse {
	out := commentResponse{
		ID:        c.ID,
		IssueID:   c.IssueID,
		User:      user,
		Body:      c.Body,
		CreatedAt: c.CreatedAt.Time.UTC().Format(time.RFC3339),
		UpdatedAt: c.UpdatedAt.Time.UTC().Format(time.RFC3339),
	}
	if c.EditedAt.Valid {
		out.EditedAt = c.EditedAt.Time.UTC().Format(time.RFC3339)
	}
	return out
}

// ─── list ───────────────────────────────────────────────────────────

func (h *Handlers) issuesList(w http.ResponseWriter, r *http.Request) {
	repo, ownerLogin, ok := h.resolveAPIRepoWithLogin(w, r, policy.ActionIssueRead)
	if !ok {
		return
	}
	page, perPage := apipage.ParseQuery(r, apipage.DefaultPerPage, apipage.MaxPerPage)
	stateFilter, serr := strictIssueState(r.URL.Query().Get("state"))
	if serr != nil {
		writeAPIError(w, http.StatusUnprocessableEntity, serr.Error())
		return
	}
	// F2-4: strict sort/direction validation. gh's documented sort set
	// for /issues is created|updated|comments; direction is asc|desc.
	// Pre-fix bogus values were silently dropped and a full unsorted
	// list came back.
	if err := validateSortDirection(r, []string{"created", "updated", "comments"}); err != nil {
		writeAPIError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	q := issuesdb.New()

	// E-audit E4: filters previously silently dropped — assignee, author,
	// milestone, mention. Resolve each into a typed predicate now and
	// 422 on unknown values so callers stop getting unfiltered lists.
	// G1: accept gh-canonical aliases (`creator`→author, `mentioned`→
	// mention) so the gh-clone CLI's wire shape lands on validation
	// instead of being silently dropped.
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
	milestoneID, merr := parseOptionalMilestoneID(r.URL.Query().Get("milestone"))
	if merr != nil {
		writeAPIError(w, http.StatusUnprocessableEntity, "milestone: "+merr.Error())
		return
	}
	// G5 (F13/F30): pre-fix `?milestone=999` (nonexistent ID) silently
	// returned the unfiltered empty result; create-side already 422s on
	// bad milestone. Make the list endpoint match — 422 when the ID
	// doesn't correspond to a milestone on this repo.
	if milestoneID != 0 {
		m, err := q.GetMilestone(r.Context(), h.d.Pool, milestoneID)
		if err != nil || m.RepoID != repo.ID {
			writeAPIError(w, http.StatusUnprocessableEntity,
				fmt.Sprintf("milestone: %d not found in this repo", milestoneID))
			return
		}
	}
	if mention := firstQueryParam(r, "mention", "mentioned"); mention != "" {
		// Mention search needs body-text scanning + an @-mention index
		// we don't have yet. Reject explicitly rather than silently
		// returning unfiltered results (the pre-E4 lie).
		writeAPIError(w, http.StatusUnprocessableEntity, "mention filter is not yet supported")
		return
	}

	// C-audit C8: `labels=foo,bar` query filter. Pre-D fix, this was
	// silently dropped — the parameter never made it into the query
	// and unfiltered results came back. Strict gh-compat now: any
	// label name that doesn't exist on the repo returns 422. The
	// actual filtering happens after the DB fetch (TODO: dedicated
	// sqlc query with INNER JOIN on issue_labels for proper page-
	// accurate pagination; for v1 the post-filter is correct but the
	// Link-header page count reflects the pre-filter row count).
	wantedLabelIDs, lerr := h.parseAndValidateLabelsFilter(r, repo.ID)
	if lerr != nil {
		writeAPIError(w, http.StatusUnprocessableEntity, lerr.Error())
		return
	}

	total, err := q.CountIssues(r.Context(), h.d.Pool, issuesdb.CountIssuesParams{
		RepoID:      repo.ID,
		StateFilter: stateFilter,
		Kind:        issuesdb.NullIssueKind{IssueKind: issuesdb.IssueKindIssue, Valid: true},
	})
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: count issues", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "list failed")
		return
	}
	rows, err := q.ListIssues(r.Context(), h.d.Pool, issuesdb.ListIssuesParams{
		RepoID:      repo.ID,
		Limit:       int32(perPage),
		Offset:      int32((page - 1) * perPage),
		StateFilter: stateFilter,
		Kind:        issuesdb.NullIssueKind{IssueKind: issuesdb.IssueKindIssue, Valid: true},
	})
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: list issues", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "list failed")
		return
	}

	// Post-filter rows by the validated label set. Match gh's AND
	// semantic: a row stays only if it has every requested label.
	if len(wantedLabelIDs) > 0 {
		rows = filterRowsByLabels(r.Context(), h.d.Pool, rows, wantedLabelIDs)
	}
	// E4 post-filters. Author + milestone are cheap (row-local).
	// Assignee requires a per-row lookup; we only pay it when the
	// filter is set.
	if authorID != 0 {
		filtered := rows[:0]
		for _, row := range rows {
			if row.AuthorUserID.Valid && row.AuthorUserID.Int64 == authorID {
				filtered = append(filtered, row)
			}
		}
		rows = filtered
	}
	if milestoneID != 0 {
		filtered := rows[:0]
		for _, row := range rows {
			if row.MilestoneID.Valid && row.MilestoneID.Int64 == milestoneID {
				filtered = append(filtered, row)
			}
		}
		rows = filtered
	}
	if assigneeID != 0 {
		filtered := rows[:0]
		for _, row := range rows {
			as, err := issuesdb.New().ListIssueAssignees(r.Context(), h.d.Pool, row.ID)
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

	link := apipage.Page{Current: page, PerPage: perPage, Total: int(total)}.LinkHeader(h.d.BaseURL, sanitizedURL(r))
	if link != "" {
		w.Header().Set("Link", link)
	}
	// Batch-resolve the unique author set in one query so the list
	// endpoint doesn't fan out one GetUserByID per row.
	authorIDs := make([]int64, 0, len(rows))
	for _, row := range rows {
		if row.AuthorUserID.Valid {
			authorIDs = append(authorIDs, row.AuthorUserID.Int64)
		}
	}
	users := h.resolveUserEnvelopesBatch(r.Context(), authorIDs)
	out := make([]issueResponse, 0, len(rows))
	for _, row := range rows {
		var u *userEnvelope
		if row.AuthorUserID.Valid {
			u = users[row.AuthorUserID.Int64]
		}
		resp := presentIssue(row, h.labelEnvelopesFor(r.Context(), row.ID), u,
			h.assigneeEnvelopesFor(r.Context(), row.ID),
			h.milestoneEnvelopeFor(r.Context(), row))
		resp.HTMLURL = h.issueHTMLURL(ownerLogin, repo.Name, row.Number)
		out = append(out, resp)
	}
	writeJSON(w, http.StatusOK, out)
}

// strictIssueState validates the `state` query parameter against the
// closed set {open, closed, all, ""} and returns a pgtype.Text encoded
// for the sqlc filter ("" / "all" become NULL = no filter). Unknown
// values return an error so the handler can 422 instead of silently
// returning unfiltered rows (E-audit E5; matches gh's strict semantic).
func strictIssueState(s string) (pgtype.Text, error) {
	// H3 (H8): byte-exact match. Pre-fix the `ToLower(TrimSpace(...))`
	// chain silently normalized "OPEN", "open " (trailing space), and
	// $'open\n' to "open"; user input that wasn't exactly in the set
	// shouldn't pass — surface a 422 so typos are visible.
	switch s {
	case "open":
		return pgtype.Text{String: "open", Valid: true}, nil
	case "closed":
		return pgtype.Text{String: "closed", Valid: true}, nil
	case "", "all":
		return pgtype.Text{}, nil
	default:
		return pgtype.Text{}, fmt.Errorf("state: must be one of open, closed, all (got %q)", s)
	}
}

// resolveOptionalUserID resolves a `?author=foo` / `?assignee=foo`
// username to a user_id. Empty returns (0, nil) — no filter. An
// unknown username is 422.
func (h *Handlers) resolveOptionalUserID(ctx context.Context, username string) (int64, error) {
	username = strings.TrimSpace(username)
	if username == "" {
		return 0, nil
	}
	user, err := usersdb.New().GetUserByUsername(ctx, h.d.Pool, username)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, fmt.Errorf("user %q not found", username)
		}
		return 0, fmt.Errorf("lookup failed")
	}
	return user.ID, nil
}

// parseOptionalMilestoneID validates a `?milestone=N` query parameter.
// Empty returns (0, nil) — no filter. Non-numeric is 422. Server-side
// we accept only the numeric form; gh's CLI maps title→number client-
// side. The CLI's --milestone flag already passes the resolved int.
func parseOptionalMilestoneID(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("milestone must be a positive integer (got %q)", s)
	}
	return n, nil
}

// issueHTMLURL composes the user-facing page URL for an issue. Empty
// when the deployment has no BaseURL configured (e.g. test contexts);
// the omitempty tag on issueResponse.HTMLURL then drops the field.
func (h *Handlers) issueHTMLURL(ownerLogin, repoName string, number int64) string {
	if h.d.BaseURL == "" || ownerLogin == "" || repoName == "" {
		return ""
	}
	return strings.TrimRight(h.d.BaseURL, "/") + "/" + ownerLogin + "/" + repoName + "/issues/" + strconv.FormatInt(number, 10)
}

// parseAndValidateLabelsFilter reads the `labels` query parameter
// (gh-compat: comma-separated, AND semantic) and resolves each name
// to a label ID for the supplied repo. Unknown names return an
// errUnknownLabel-shaped error which the handler maps to 422 (C8).
// Returns the empty slice when no filter was supplied — callers
// treat that as "no label filter".
func (h *Handlers) parseAndValidateLabelsFilter(r *http.Request, repoID int64) ([]int64, error) {
	// G1: accept gh-canonical singular `label` as an alias for `labels`.
	// Either form is comma-separated; AND semantic applies regardless.
	raw := firstQueryParam(r, "labels", "label")
	if raw == "" {
		return nil, nil
	}
	names := strings.Split(raw, ",")
	q := issuesdb.New()
	ids := make([]int64, 0, len(names))
	for _, n := range names {
		n = strings.TrimSpace(n)
		if n == "" {
			continue
		}
		l, err := q.GetLabelByName(r.Context(), h.d.Pool, issuesdb.GetLabelByNameParams{
			RepoID: repoID, Name: n,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, fmt.Errorf("unknown label: %s", n)
			}
			return nil, err
		}
		ids = append(ids, l.ID)
	}
	return ids, nil
}

// filterRowsByLabels keeps only rows where the issue carries every
// label in wanted (gh-compat AND semantic). O(N rows × N labels per
// row × N wanted). Acceptable for typical repo sizes; a dedicated
// sqlc INNER JOIN query would do it server-side. TODO when issues
// pagination correctness becomes load-bearing.
func filterRowsByLabels(ctx context.Context, pool issuesdb.DBTX, rows []issuesdb.Issue, wanted []int64) []issuesdb.Issue {
	q := issuesdb.New()
	out := rows[:0]
	for _, row := range rows {
		labels, err := q.ListLabelsOnIssue(ctx, pool, row.ID)
		if err != nil {
			continue
		}
		have := make(map[int64]struct{}, len(labels))
		for _, l := range labels {
			have[l.ID] = struct{}{}
		}
		ok := true
		for _, w := range wanted {
			if _, has := have[w]; !has {
				ok = false
				break
			}
		}
		if ok {
			out = append(out, row)
		}
	}
	return out
}

// labelEnvelopesFor returns the labels on an issue as GitHub-compat
// objects. Replaces pre-S60 `labelNamesFor` which returned bare names
// in a string array — that shape couldn't be decoded by gh-compat
// clients (CLI issue view/close/reopen panicked on any labeled issue).
func (h *Handlers) labelEnvelopesFor(ctx context.Context, issueID int64) []labelEnvelope {
	rows, err := issuesdb.New().ListLabelsOnIssue(ctx, h.d.Pool, issueID)
	if err != nil {
		return nil
	}
	return presentLabelEnvelopes(rows)
}

// milestoneEnvelopeFor returns the milestone object attached to an
// issue, or nil if the issue has none. Pre-E3 the field was absent
// from the response entirely; now we surface it (matching gh's shape
// and the CLI's `--json milestone` exporter).
func (h *Handlers) milestoneEnvelopeFor(ctx context.Context, issue issuesdb.Issue) *milestoneIssueEnvelope {
	if !issue.MilestoneID.Valid {
		return nil
	}
	m, err := issuesdb.New().GetMilestone(ctx, h.d.Pool, issue.MilestoneID.Int64)
	if err != nil {
		return nil
	}
	return presentMilestoneIssueEnvelope(m)
}

// assigneeEnvelopesFor returns the assignees on an issue as gh-compat
// user envelopes (C20a). Mirrors labelEnvelopesFor's shape. Returns a
// non-nil empty slice on no-assignees so the response always carries
// the `assignees: []` key — gh clients parse against the field being
// present. Uses the batch user lookup to avoid N+1.
func (h *Handlers) assigneeEnvelopesFor(ctx context.Context, issueID int64) []userEnvelope {
	rows, err := issuesdb.New().ListIssueAssignees(ctx, h.d.Pool, issueID)
	if err != nil || len(rows) == 0 {
		return []userEnvelope{}
	}
	ids := make([]int64, 0, len(rows))
	for _, a := range rows {
		ids = append(ids, a.UserID)
	}
	byID := h.resolveUserEnvelopesBatch(ctx, ids)
	out := make([]userEnvelope, 0, len(rows))
	for _, a := range rows {
		if env := byID[a.UserID]; env != nil {
			out = append(out, *env)
		}
	}
	return out
}

// ─── single get ─────────────────────────────────────────────────────

func (h *Handlers) issueGet(w http.ResponseWriter, r *http.Request) {
	repo, ownerLogin, ok := h.resolveAPIRepoWithLogin(w, r, policy.ActionIssueRead)
	if !ok {
		return
	}
	// I9 (F26 carryover via I41): pre-fix this handler refused PR
	// numbers with "issue not found". The CLI's `pr edit --add-label`
	// / `--add-assignee` flow GETs the issue first (to compute the
	// merged label/assignee set), then PATCHes — H11 fixed the PATCH
	// half via resolveIssueOrPRByNumber but the GET half still threw
	// 404 on PRs, breaking the entire flow. Make GET symmetric: PRs
	// share the issues table, and the issue-shape response (labels,
	// assignees, milestone) is identical for both kinds. Dedicated
	// PR-specific routes still live under /pulls/{N}.
	issue, ok := h.resolveIssueOrPRByNumber(w, r, repo.ID, chi.URLParam(r, "number"))
	if !ok {
		return
	}
	var u *userEnvelope
	if issue.AuthorUserID.Valid {
		u = h.resolveUserEnvelope(r.Context(), issue.AuthorUserID.Int64)
	}
	// I7a (audit-I12): single-issue GET carries the full gh-compat
	// surface — node_id + comments count + author_association + the
	// sub-resource URL bundle + closed_by + reactions stub. The list
	// endpoint stays on the legacy presentIssue signature; cost of the
	// extra lookups doesn't amortize across N issues per page.
	extras := h.buildIssueExtras(r.Context(), repo, ownerLogin, issue)
	resp := presentIssueFull(issue, h.labelEnvelopesFor(r.Context(), issue.ID), u,
		h.assigneeEnvelopesFor(r.Context(), issue.ID),
		h.milestoneEnvelopeFor(r.Context(), issue),
		extras)
	resp.HTMLURL = h.issueHTMLURL(ownerLogin, repo.Name, issue.Number)
	writeJSON(w, http.StatusOK, resp)
}

// buildIssueExtras resolves all the I7a-expansion inputs for a single
// issue GET. Best-effort: a failed comment count emits zero rather
// than failing the GET. Skips entirely when BaseURL is empty.
func (h *Handlers) buildIssueExtras(ctx context.Context, repo reposdb.Repo, ownerLogin string, issue issuesdb.Issue) issueExtras {
	extras := issueExtras{NodeID: issueNodeID(issue.ID)}
	if h.d.BaseURL != "" {
		repoURL, eventsURL, labelsURL, commentsURL := buildIssueSubURLs(h.d.BaseURL, ownerLogin, repo.Name, issue.Number)
		extras.RepositoryURL = repoURL
		extras.EventsURL = eventsURL
		extras.LabelsURL = labelsURL
		extras.CommentsURL = commentsURL
	}
	if c, err := issuesdb.New().CountIssueComments(ctx, h.d.Pool, issue.ID); err == nil {
		extras.CommentsCount = c
	}
	if issue.AuthorUserID.Valid {
		auth := middleware.PATAuthFromContext(ctx)
		// AuthorAssociation describes how the *issue author* relates to
		// the repo. The author was resolved against AuthorUserID, but
		// the helper takes a policy.Actor — construct one for the
		// author here. The actor's IsAnonymous/IsSuspended flags don't
		// affect the association mapping (they're for write gates), so
		// the minimal shape is enough.
		_ = auth // suppress unused; reserved for follow-up if author-vs-viewer association becomes needed
		authorActor := policy.UserActor(issue.AuthorUserID.Int64, "", false, false)
		extras.AuthorAssociation = policy.AuthorAssociation(ctx, policy.Deps{Pool: h.d.Pool}, authorActor, policy.NewRepoRefFromRepo(repo))
	}
	if issue.ClosedAt.Valid && issue.ClosedByUserID.Valid {
		extras.ClosedBy = h.resolveUserEnvelope(ctx, issue.ClosedByUserID.Int64)
	}
	return extras
}

// ─── create ─────────────────────────────────────────────────────────

type issueCreateRequest struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	// Labels names to attach on create. Unknown names → 422.
	Labels []string `json:"labels,omitempty"`
	// Assignees usernames to attach on create. Unknown usernames → 422.
	Assignees []string `json:"assignees,omitempty"`
	// Milestone id (repo-local). 0/omitted leaves the issue with no
	// milestone; an unrelated repo's id → 422.
	Milestone *int64 `json:"milestone,omitempty"`
}

func (h *Handlers) issueCreate(w http.ResponseWriter, r *http.Request) {
	repo, ownerLogin, ok := h.resolveAPIRepoWithLogin(w, r, policy.ActionIssueCreate)
	if !ok {
		return
	}
	auth := middleware.PATAuthFromContext(r.Context())
	var body issueCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	// Resolve labels/assignees up front so a bad name fails the request
	// before we write the issue row — avoids a half-created issue with
	// no labels when the caller passed a typo.
	var labelIDs []int64
	if len(body.Labels) > 0 {
		ids, err := h.resolveLabelIDs(r, repo.ID, body.Labels)
		if err != nil {
			writeAPIError(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		labelIDs = ids
	}
	assigneeIDs := make([]int64, 0, len(body.Assignees))
	for _, name := range body.Assignees {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		u, err := h.q.GetUserByUsername(r.Context(), h.d.Pool, name)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				writeAPIError(w, http.StatusUnprocessableEntity, "unknown assignee: "+name)
				return
			}
			h.d.Logger.ErrorContext(r.Context(), "api: create issue assignee lookup", "error", err)
			writeAPIError(w, http.StatusInternalServerError, "internal error")
			return
		}
		assigneeIDs = append(assigneeIDs, u.ID)
	}
	if body.Milestone != nil && *body.Milestone != 0 {
		m, err := issuesdb.New().GetMilestone(r.Context(), h.d.Pool, *body.Milestone)
		if err != nil || m.RepoID != repo.ID {
			writeAPIError(w, http.StatusUnprocessableEntity, "milestone does not belong to this repo")
			return
		}
	}

	issue, err := issues.Create(r.Context(), h.issuesDeps(), issues.CreateParams{
		RepoID:       repo.ID,
		AuthorUserID: auth.UserID,
		Title:        body.Title,
		Body:         body.Body,
		Kind:         "issue",
	})
	if err != nil {
		writeIssuesError(w, err)
		return
	}

	// Apply optional attachments. Each is best-effort in the sense that
	// the issue row already exists if these fail; we surface the first
	// error and let the caller PATCH to retry. Author/create context is
	// the same actor, so no extra policy gates here (anyone who can
	// create an issue can set its initial labels/assignees/milestone —
	// matches gh's POST /repos/{}/{}/issues behavior).
	if len(labelIDs) > 0 {
		if err := issues.ApplyLabels(r.Context(), h.issuesDeps(), auth.UserID, issue.ID, labelIDs); err != nil {
			writeIssuesError(w, err)
			return
		}
	}
	for _, aid := range assigneeIDs {
		if err := issues.AssignUser(r.Context(), h.issuesDeps(), auth.UserID, issue.ID, aid); err != nil {
			writeIssuesError(w, err)
			return
		}
	}
	if body.Milestone != nil && *body.Milestone != 0 {
		if err := issues.AssignMilestone(r.Context(), h.issuesDeps(), auth.UserID, issue.ID, *body.Milestone); err != nil {
			writeIssuesError(w, err)
			return
		}
	}

	// Re-fetch so updated_at + any milestone-induced denorms reflect
	// the final state. Cheap — single PK lookup.
	fresh, err := issuesdb.New().GetIssueByID(r.Context(), h.d.Pool, issue.ID)
	if err != nil {
		fresh = issue
	}
	var labels []labelEnvelope
	if len(labelIDs) > 0 {
		labels = h.labelEnvelopesFor(r.Context(), fresh.ID)
	}
	// On create the author is always the authenticated caller; pull
	// their envelope so the response is fully populated on the first
	// round-trip.
	u := h.resolveUserEnvelope(r.Context(), auth.UserID)
	resp := presentIssue(fresh, labels, u,
		h.assigneeEnvelopesFor(r.Context(), fresh.ID),
		h.milestoneEnvelopeFor(r.Context(), fresh))
	resp.HTMLURL = h.issueHTMLURL(ownerLogin, repo.Name, fresh.Number)
	writeJSON(w, http.StatusCreated, resp)
}

// ─── patch ──────────────────────────────────────────────────────────

type issuePatchRequest struct {
	Title       *string `json:"title,omitempty"`
	Body        *string `json:"body,omitempty"`
	State       *string `json:"state,omitempty"`
	StateReason *string `json:"state_reason,omitempty"`
	// Labels, when non-nil, replaces the issue's label set verbatim
	// (matches GitHub's PATCH semantics — passing `["bug"]` strips
	// every other label). Passing an empty slice clears all labels;
	// omitting the field leaves them untouched.
	Labels *[]string `json:"labels,omitempty"`
	// Assignees, when non-nil, replaces the assignee set verbatim
	// (same convention as Labels). Each entry is a username; unknown
	// usernames return 422.
	Assignees *[]string `json:"assignees,omitempty"`
	// Milestone, when non-nil, sets the issue's milestone id. Pass
	// 0 to clear; omit to leave unchanged.
	Milestone *int64 `json:"milestone,omitempty"`
}

func (h *Handlers) issuePatch(w http.ResponseWriter, r *http.Request) {
	repo, ownerLogin, ok := h.resolveAPIRepoWithLogin(w, r, policy.ActionIssueRead)
	if !ok {
		return
	}
	// E25: PATCH is a mutation; the read gate above lets the request
	// reach archived repos. Explicitly refuse writes on archived
	// repos here so author-self-edit + label/assignee/milestone/state
	// changes don't slip through. POST /issues already 404s on
	// archived (different code path); this aligns PATCH.
	if policy.NewRepoRefFromRepo(repo).Archived() {
		writeAPIError(w, http.StatusForbidden, "repository is archived")
		return
	}
	auth := middleware.PATAuthFromContext(r.Context())
	if auth.UserID == 0 {
		writeAPIError(w, http.StatusUnauthorized, "unauthenticated")
		return
	}
	issue, ok := h.resolveIssueOrPRByNumber(w, r, repo.ID, chi.URLParam(r, "number"))
	if !ok {
		return
	}
	q := issuesdb.New()
	var body issuePatchRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	// G2 (F26): PRs share the issues table — label/assignee/milestone
	// edits route through here per gh-compat. But title/body/state on a
	// PR belong on `PATCH /pulls/{N}`; rejecting here keeps the two
	// surfaces honest. (Issue rows still allow the full surface.)
	if issue.Kind == issuesdb.IssueKindPr && (body.Title != nil || body.Body != nil || body.State != nil || body.StateReason != nil) {
		writeAPIError(w, http.StatusUnprocessableEntity, "title, body, state, and state_reason on pull requests must be edited via PATCH /pulls/{N}")
		return
	}

	// Title/body: only the author or a repo collaborator with write
	// access can edit. We deliberately gate via ActionRepoWrite (not
	// ActionIssueComment) — comment-create is open to any logged-in
	// reader on a public repo, but editing someone else's issue is a
	// moderation action.
	if body.Title != nil || body.Body != nil {
		canEdit := issue.AuthorUserID.Valid && issue.AuthorUserID.Int64 == auth.UserID
		if !canEdit {
			canEdit = policy.Can(r.Context(), policy.Deps{Pool: h.d.Pool}, auth.PolicyActor(), policy.ActionRepoWrite, policy.NewRepoRefFromRepo(repo)).Allow
		}
		if !canEdit {
			writeAPIError(w, http.StatusForbidden, "only the author or a repo collaborator may edit this issue")
			return
		}
		updated, err := issues.Edit(r.Context(), h.issuesDeps(), issues.EditParams{
			IssueID: issue.ID,
			Title:   body.Title,
			Body:    body.Body,
		})
		if err != nil {
			writeIssuesError(w, err)
			return
		}
		issue = updated
	}

	if body.State != nil {
		// State changes require ActionIssueClose.
		if !policy.Can(r.Context(), policy.Deps{Pool: h.d.Pool}, auth.PolicyActor(), policy.ActionIssueClose, policy.NewRepoRefFromRepo(repo)).Allow {
			writeAPIError(w, http.StatusForbidden, "lack permission to change issue state")
			return
		}
		newState := strings.ToLower(*body.State)
		if newState != "open" && newState != "closed" {
			writeAPIError(w, http.StatusUnprocessableEntity, "state must be open or closed")
			return
		}
		reason := ""
		if body.StateReason != nil {
			reason = strings.ToLower(*body.StateReason)
			switch reason {
			case "", "completed", "not_planned", "duplicate", "reopened":
			default:
				writeAPIError(w, http.StatusUnprocessableEntity, "state_reason must be one of completed, not_planned, duplicate, reopened")
				return
			}
		}
		if err := issues.SetState(r.Context(), h.issuesDeps(), auth.UserID, issue.ID, newState, reason); err != nil {
			// H14: when the caller explicitly passed `state_reason` but
			// the state didn't transition, surface a typed 422 so the
			// CLI can tell the user their reason-change intent was lost.
			// Bare `state` change on already-matching state stays
			// idempotent success — matches gh-compat (and absorbs the
			// sentinel here).
			if errors.Is(err, issues.ErrAlreadyInState) {
				if body.StateReason != nil {
					writeAPIError(w, http.StatusUnprocessableEntity,
						"issue is already "+newState+"; pass state_reason via a separate edit if you want to change the reason")
					return
				}
				// Idempotent success — fall through.
			} else {
				writeIssuesError(w, err)
				return
			}
		}
	}

	// Labels, assignees, milestone — each gates on its own policy
	// action so a write collaborator who lacks (e.g.) ActionIssueLabel
	// still gets a clean 403 instead of a partial update.
	if body.Labels != nil {
		if !policy.Can(r.Context(), policy.Deps{Pool: h.d.Pool}, auth.PolicyActor(), policy.ActionIssueLabel, policy.NewRepoRefFromRepo(repo)).Allow {
			writeAPIError(w, http.StatusForbidden, "lack permission to set labels")
			return
		}
		labelIDs, err := h.resolveLabelIDs(r, repo.ID, *body.Labels)
		if err != nil {
			writeAPIError(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		if err := issues.ApplyLabels(r.Context(), h.issuesDeps(), auth.UserID, issue.ID, labelIDs); err != nil {
			writeIssuesError(w, err)
			return
		}
	}

	if body.Assignees != nil {
		if !policy.Can(r.Context(), policy.Deps{Pool: h.d.Pool}, auth.PolicyActor(), policy.ActionIssueAssign, policy.NewRepoRefFromRepo(repo)).Allow {
			writeAPIError(w, http.StatusForbidden, "lack permission to set assignees")
			return
		}
		if err := h.applyIssueAssignees(r, auth.UserID, issue.ID, *body.Assignees); err != nil {
			if errors.Is(err, errUnknownAssignee) {
				writeAPIError(w, http.StatusUnprocessableEntity, err.Error())
				return
			}
			writeIssuesError(w, err)
			return
		}
	}

	if body.Milestone != nil {
		if !policy.Can(r.Context(), policy.Deps{Pool: h.d.Pool}, auth.PolicyActor(), policy.ActionIssueAssign, policy.NewRepoRefFromRepo(repo)).Allow {
			writeAPIError(w, http.StatusForbidden, "lack permission to set milestone")
			return
		}
		mid := *body.Milestone
		if mid != 0 {
			m, err := q.GetMilestone(r.Context(), h.d.Pool, mid)
			if err != nil || m.RepoID != repo.ID {
				writeAPIError(w, http.StatusUnprocessableEntity, "milestone does not belong to this repo")
				return
			}
		}
		if err := issues.AssignMilestone(r.Context(), h.issuesDeps(), auth.UserID, issue.ID, mid); err != nil {
			writeIssuesError(w, err)
			return
		}
	}

	fresh, err := q.GetIssueByID(r.Context(), h.d.Pool, issue.ID)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "reload failed")
		return
	}
	var u *userEnvelope
	if fresh.AuthorUserID.Valid {
		u = h.resolveUserEnvelope(r.Context(), fresh.AuthorUserID.Int64)
	}
	resp := presentIssue(fresh, h.labelEnvelopesFor(r.Context(), fresh.ID), u,
		h.assigneeEnvelopesFor(r.Context(), fresh.ID),
		h.milestoneEnvelopeFor(r.Context(), fresh))
	// G2: PR rows ride through this handler for label/assignee/milestone
	// edits — point their html_url at the /pulls/{N} surface so clients
	// don't get an issue URL for a PR.
	if fresh.Kind == issuesdb.IssueKindPr {
		resp.HTMLURL = h.pullHTMLURL(ownerLogin, repo.Name, fresh.Number)
	} else {
		resp.HTMLURL = h.issueHTMLURL(ownerLogin, repo.Name, fresh.Number)
	}
	writeJSON(w, http.StatusOK, resp)
}

// ─── comments ───────────────────────────────────────────────────────

func (h *Handlers) issueCommentsList(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.resolveAPIRepo(w, r, policy.ActionIssueRead)
	if !ok {
		return
	}
	issue, ok := h.resolveIssueOrPRByNumber(w, r, repo.ID, chi.URLParam(r, "number"))
	if !ok {
		return
	}
	rows, err := issuesdb.New().ListIssueComments(r.Context(), h.d.Pool, issue.ID)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: list comments", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "list failed")
		return
	}
	authorIDs := make([]int64, 0, len(rows))
	for _, c := range rows {
		if c.AuthorUserID.Valid {
			authorIDs = append(authorIDs, c.AuthorUserID.Int64)
		}
	}
	users := h.resolveUserEnvelopesBatch(r.Context(), authorIDs)
	out := make([]commentResponse, 0, len(rows))
	for _, c := range rows {
		var u *userEnvelope
		if c.AuthorUserID.Valid {
			u = users[c.AuthorUserID.Int64]
		}
		out = append(out, presentComment(c, u))
	}
	writeJSON(w, http.StatusOK, out)
}

type commentCreateRequest struct {
	Body string `json:"body"`
}

func (h *Handlers) issueCommentCreate(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.resolveAPIRepo(w, r, policy.ActionIssueComment)
	if !ok {
		return
	}
	auth := middleware.PATAuthFromContext(r.Context())
	issue, ok := h.resolveIssueOrPRByNumber(w, r, repo.ID, chi.URLParam(r, "number"))
	if !ok {
		return
	}
	var body commentCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	isCollab := policy.Can(r.Context(), policy.Deps{Pool: h.d.Pool}, auth.PolicyActor(), policy.ActionRepoWrite, policy.NewRepoRefFromRepo(*repo)).Allow
	c, err := issues.AddComment(r.Context(), h.issuesDeps(), issues.CommentCreateParams{
		IssueID:      issue.ID,
		AuthorUserID: auth.UserID,
		Body:         body.Body,
		IsCollab:     isCollab,
	})
	if err != nil {
		writeIssuesError(w, err)
		return
	}
	u := h.resolveUserEnvelope(r.Context(), auth.UserID)
	writeJSON(w, http.StatusCreated, presentComment(c, u))
}

type commentUpdateRequest struct {
	Body string `json:"body"`
}

func (h *Handlers) issueCommentUpdate(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.resolveAPIRepo(w, r, policy.ActionIssueRead)
	if !ok {
		return
	}
	auth := middleware.PATAuthFromContext(r.Context())
	cid, err := strconv.ParseInt(chi.URLParam(r, "cid"), 10, 64)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "comment not found")
		return
	}
	q := issuesdb.New()
	comment, err := q.GetIssueComment(r.Context(), h.d.Pool, cid)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "comment not found")
		return
	}
	// Cross-repo guard: the comment must belong to an issue in this
	// repo. Without this, a caller could /repos/foo/bar/issues/comments/{id}
	// against an unrelated comment id.
	issue, err := q.GetIssueByID(r.Context(), h.d.Pool, comment.IssueID)
	if err != nil || issue.RepoID != repo.ID {
		writeAPIError(w, http.StatusNotFound, "comment not found")
		return
	}
	if !canEditComment(comment, auth.UserID) {
		writeAPIError(w, http.StatusForbidden, "only the author may edit this comment")
		return
	}
	var body commentUpdateRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	trimmed := strings.TrimSpace(body.Body)
	if trimmed == "" {
		writeAPIError(w, http.StatusUnprocessableEntity, "body is required")
		return
	}
	if len(trimmed) > 65535 {
		writeAPIError(w, http.StatusUnprocessableEntity, "body too long")
		return
	}
	if err := q.UpdateIssueCommentBody(r.Context(), h.d.Pool, issuesdb.UpdateIssueCommentBodyParams{
		ID: comment.ID, Body: trimmed,
		// body_html_cached is cleared; the next render path picks the
		// fresh body up. Matches how the HTML comment editor handles
		// re-renders (lazy regeneration on read).
		BodyHtmlCached: pgtype.Text{},
	}); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: update comment", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "update failed")
		return
	}
	fresh, _ := q.GetIssueComment(r.Context(), h.d.Pool, comment.ID)
	var u *userEnvelope
	if fresh.AuthorUserID.Valid {
		u = h.resolveUserEnvelope(r.Context(), fresh.AuthorUserID.Int64)
	}
	writeJSON(w, http.StatusOK, presentComment(fresh, u))
}

func (h *Handlers) issueCommentDelete(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.resolveAPIRepo(w, r, policy.ActionIssueRead)
	if !ok {
		return
	}
	auth := middleware.PATAuthFromContext(r.Context())
	cid, err := strconv.ParseInt(chi.URLParam(r, "cid"), 10, 64)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "comment not found")
		return
	}
	q := issuesdb.New()
	comment, err := q.GetIssueComment(r.Context(), h.d.Pool, cid)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "comment not found")
		return
	}
	issue, err := q.GetIssueByID(r.Context(), h.d.Pool, comment.IssueID)
	if err != nil || issue.RepoID != repo.ID {
		writeAPIError(w, http.StatusNotFound, "comment not found")
		return
	}
	// Delete is broader than edit: a repo collaborator with write
	// access can remove any comment (matches GitHub's "moderation"
	// affordance), the comment author can remove their own.
	canDelete := comment.AuthorUserID.Valid && comment.AuthorUserID.Int64 == auth.UserID
	if !canDelete {
		canDelete = policy.Can(r.Context(), policy.Deps{Pool: h.d.Pool}, auth.PolicyActor(), policy.ActionRepoWrite, policy.NewRepoRefFromRepo(*repo)).Allow
	}
	if !canDelete {
		writeAPIError(w, http.StatusForbidden, "lack permission to delete this comment")
		return
	}
	if err := q.DeleteIssueComment(r.Context(), h.d.Pool, comment.ID); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: delete comment", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "delete failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func canEditComment(c issuesdb.IssueComment, actorUserID int64) bool {
	if !c.AuthorUserID.Valid {
		return false
	}
	return c.AuthorUserID.Int64 == actorUserID
}

// ─── lock ───────────────────────────────────────────────────────────

type issueLockRequest struct {
	Reason string `json:"lock_reason"`
}

func (h *Handlers) issueLock(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.resolveAPIRepo(w, r, policy.ActionIssueClose)
	if !ok {
		return
	}
	auth := middleware.PATAuthFromContext(r.Context())
	issue, ok := h.resolveIssueOrPRByNumber(w, r, repo.ID, chi.URLParam(r, "number"))
	if !ok {
		return
	}
	var body issueLockRequest
	_ = json.NewDecoder(r.Body).Decode(&body) // body is optional
	if err := issues.SetLock(r.Context(), h.issuesDeps(), auth.UserID, issue.ID, true, body.Reason); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: lock", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "lock failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handlers) issueUnlock(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.resolveAPIRepo(w, r, policy.ActionIssueClose)
	if !ok {
		return
	}
	auth := middleware.PATAuthFromContext(r.Context())
	issue, ok := h.resolveIssueOrPRByNumber(w, r, repo.ID, chi.URLParam(r, "number"))
	if !ok {
		return
	}
	if err := issues.SetLock(r.Context(), h.issuesDeps(), auth.UserID, issue.ID, false, ""); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: unlock", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "unlock failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ─── delete ─────────────────────────────────────────────────────────

// issueDelete hard-deletes an issue. G8 (F45). The CLI's `issue
// delete` command shipped against this endpoint long before the
// server implemented it — pre-G8 the verb returned `405 (no message)`
// and the audit caught it. Now: admin-only, returns 204 on success,
// CASCADE handles comments/labels/assignees/milestone/events/refs.
// PR rows reject with 404 (this is the issues-only surface; PRs have
// no parallel delete in v1 — closing is sufficient for PRs and the
// audit doesn't flag the absence).
func (h *Handlers) issueDelete(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.resolveAPIRepo(w, r, policy.ActionIssueRead)
	if !ok {
		return
	}
	// Repo-admin gate matches gh's behavior: writers can edit/lock,
	// but only admins can hard-delete because the cascade is non-
	// recoverable.
	auth := middleware.PATAuthFromContext(r.Context())
	if !policy.Can(r.Context(), policy.Deps{Pool: h.d.Pool}, auth.PolicyActor(), policy.ActionRepoAdmin, policy.NewRepoRefFromRepo(*repo)).Allow {
		writeAPIError(w, http.StatusForbidden, "only repo admins may delete issues")
		return
	}
	issue, ok := h.resolveIssueByNumberStrict(w, r, repo.ID, chi.URLParam(r, "number"))
	if !ok {
		return
	}
	if err := issues.Delete(r.Context(), h.issuesDeps(), auth.UserID, issue.ID); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: delete issue", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "delete failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// resolveIssueByNumberStrict is the kind=issue-only resolver for the
// delete endpoint. PRs route through their own /pulls surface; this
// keeps the delete verb scoped to actual issues and lets us 404 on
// PR numbers explicitly (matches issueGet's gate).
func (h *Handlers) resolveIssueByNumberStrict(w http.ResponseWriter, r *http.Request, repoID int64, numberRaw string) (issuesdb.Issue, bool) {
	issue, ok := h.resolveIssueOrPRByNumber(w, r, repoID, numberRaw)
	if !ok {
		return issuesdb.Issue{}, false
	}
	if issue.Kind != issuesdb.IssueKindIssue {
		writeAPIError(w, http.StatusNotFound, "issue not found")
		return issuesdb.Issue{}, false
	}
	return issue, true
}

// ─── helpers ────────────────────────────────────────────────────────

// resolveIssueOrPRByNumber is the variant for sub-routes that live in
// the gh-compat shared issue/PR namespace: `/issues/{N}/comments`,
// `/issues/{N}/lock`, `/issues/{N}/events`, and (selectively) the
// label/assignee/milestone branches of `PATCH /issues/{N}`. PR rows
// share the `issues` table (kind='pr'), so per-issue queries
// (comments, lock, events, labels, assignees) work uniformly across
// both kinds — the only thing the helper drops is the kind gate.
//
// G2 (F3/F26/F44): pre-fix the strict `kind == issue` check 404'd PR
// numbers across the entire shared sub-route surface, breaking
// `pr comment`, `pr edit --add-label/--add-assignee`, and `pr lock`
// end-to-end. The fix is structural: accept either kind here and keep
// the kind-strict resolver for endpoints that genuinely don't apply
// to PRs (GET / full PATCH of issue title-body-state).
func (h *Handlers) resolveIssueOrPRByNumber(w http.ResponseWriter, r *http.Request, repoID int64, numberRaw string) (issuesdb.Issue, bool) {
	num, err := strconv.ParseInt(numberRaw, 10, 64)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "issue not found")
		return issuesdb.Issue{}, false
	}
	issue, err := issuesdb.New().GetIssueByNumber(r.Context(), h.d.Pool, issuesdb.GetIssueByNumberParams{
		RepoID: repoID, Number: num,
	})
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "issue not found")
		return issuesdb.Issue{}, false
	}
	return issue, true
}

func (h *Handlers) issuesDeps() issues.Deps {
	return issues.Deps{
		Pool:    h.d.Pool,
		Limiter: h.d.Throttle,
		Logger:  h.d.Logger,
		Audit:   h.d.Audit,
	}
}

func writeIssuesError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, issues.ErrEmptyTitle),
		errors.Is(err, issues.ErrTitleTooLong),
		errors.Is(err, issues.ErrBodyTooLong),
		errors.Is(err, issues.ErrEmptyComment),
		errors.Is(err, issues.ErrCommentTooLong),
		// H3 — null-byte rejections.
		errors.Is(err, issues.ErrNullByteInTitle),
		errors.Is(err, issues.ErrNullByteInBody),
		errors.Is(err, issues.ErrNullByteInComment),
		// I52 — multi-line titles rejected at the orchestrator gate.
		errors.Is(err, issues.ErrMultilineTitle):
		writeAPIError(w, http.StatusUnprocessableEntity, err.Error())
	case errors.Is(err, issues.ErrCommentRateLimit):
		writeAPIError(w, http.StatusTooManyRequests, "comment rate limit exceeded")
	case errors.Is(err, issues.ErrIssueLocked):
		writeAPIError(w, http.StatusLocked, "issue is locked")
	case errors.Is(err, issues.ErrIssueNotFound):
		writeAPIError(w, http.StatusNotFound, "issue not found")
	default:
		writeAPIError(w, http.StatusInternalServerError, "internal error")
	}
}

// errUnknownAssignee is the sentinel applyIssueAssignees returns when
// the caller asked for a username that has no matching user row. The
// handler converts it into a 422.
var errUnknownAssignee = errors.New("issues: unknown assignee username")

// resolveLabelIDs maps caller-supplied label names → ids for the
// supplied repo. Names are case-sensitive (the schema is). Returns
// errUnknownAssignee-shaped error for missing names (422-mapped).
func (h *Handlers) resolveLabelIDs(r *http.Request, repoID int64, names []string) ([]int64, error) {
	q := issuesdb.New()
	ids := make([]int64, 0, len(names))
	for _, n := range names {
		n = strings.TrimSpace(n)
		if n == "" {
			continue
		}
		l, err := q.GetLabelByName(r.Context(), h.d.Pool, issuesdb.GetLabelByNameParams{
			RepoID: repoID, Name: n,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, errors.New("unknown label: " + n)
			}
			return nil, err
		}
		ids = append(ids, l.ID)
	}
	return ids, nil
}

// applyIssueAssignees diffs the requested assignee set against the
// current one and emits the minimal AssignUser / UnassignUser calls.
func (h *Handlers) applyIssueAssignees(r *http.Request, actorUserID, issueID int64, want []string) error {
	wantIDs := make(map[int64]struct{}, len(want))
	for _, name := range want {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		u, err := h.q.GetUserByUsername(r.Context(), h.d.Pool, name)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return errUnknownAssignee
			}
			return err
		}
		wantIDs[u.ID] = struct{}{}
	}
	current, err := issuesdb.New().ListIssueAssignees(r.Context(), h.d.Pool, issueID)
	if err != nil {
		return err
	}
	haveIDs := make(map[int64]struct{}, len(current))
	for _, c := range current {
		haveIDs[c.UserID] = struct{}{}
	}
	for id := range wantIDs {
		if _, ok := haveIDs[id]; ok {
			continue
		}
		if err := issues.AssignUser(r.Context(), h.issuesDeps(), actorUserID, issueID, id); err != nil {
			return err
		}
	}
	for id := range haveIDs {
		if _, ok := wantIDs[id]; ok {
			continue
		}
		if err := issues.UnassignUser(r.Context(), h.issuesDeps(), actorUserID, issueID, id); err != nil {
			return err
		}
	}
	return nil
}
