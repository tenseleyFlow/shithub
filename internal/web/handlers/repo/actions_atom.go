// SPDX-License-Identifier: AGPL-3.0-or-later

package repo

import (
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	actionsdb "github.com/tenseleyFlow/shithub/internal/actions/sqlc"
	"github.com/tenseleyFlow/shithub/internal/auth/policy"
)

const actionsAtomRunLimit = int32(50)

func (h *Handlers) repoActionsAtom(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoRead)
	if !ok {
		return
	}
	runs, err := actionsdb.New().ListWorkflowRunsForRepo(r.Context(), h.d.Pool, actionsdb.ListWorkflowRunsForRepoParams{
		RepoID:     row.ID,
		PageLimit:  actionsAtomRunLimit,
		PageOffset: 0,
	})
	if err != nil {
		h.d.Logger.WarnContext(r.Context(), "repo actions atom: list workflow runs", "repo_id", row.ID, "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	w.Header().Set("Content-Type", "application/atom+xml; charset=utf-8")
	writeActionsAtom(w, owner.Username, row.Name, runs, time.Now())
}

func writeActionsAtom(w io.Writer, owner, repoName string, runs []actionsdb.ListWorkflowRunsForRepoRow, now time.Time) {
	type atomAuthor struct {
		Name string `xml:"name"`
	}
	type atomEntry struct {
		ID      string     `xml:"id"`
		Title   string     `xml:"title"`
		Updated string     `xml:"updated"`
		Author  atomAuthor `xml:"author"`
		Summary string     `xml:"summary"`
		Link    struct {
			Href string `xml:"href,attr"`
			Rel  string `xml:"rel,attr,omitempty"`
		} `xml:"link"`
	}
	type atomFeed struct {
		XMLName xml.Name `xml:"http://www.w3.org/2005/Atom feed"`
		Title   string   `xml:"title"`
		ID      string   `xml:"id"`
		Updated string   `xml:"updated"`
		Link    struct {
			Href string `xml:"href,attr"`
			Rel  string `xml:"rel,attr,omitempty"`
		} `xml:"link"`
		Entries []atomEntry `xml:"entry"`
	}

	feedUpdated := now.UTC()
	if len(runs) > 0 {
		feedUpdated = pgTime(runs[0].UpdatedAt, runs[0].CreatedAt.Time).UTC()
	}
	feed := atomFeed{
		Title:   fmt.Sprintf("%s/%s Actions runs", owner, repoName),
		ID:      fmt.Sprintf("urn:shithub:actions:%s:%s", owner, repoName),
		Updated: feedUpdated.Format(time.RFC3339),
	}
	feed.Link.Href = fmt.Sprintf("/%s/%s/actions.atom", owner, repoName)
	feed.Link.Rel = "self"

	for _, run := range runs {
		var e atomEntry
		e.ID = fmt.Sprintf("urn:shithub:workflow_run:%d", run.ID)
		e.Title = actionsAtomRunTitle(run)
		e.Updated = pgTime(run.UpdatedAt, run.CreatedAt.Time).UTC().Format(time.RFC3339)
		e.Author.Name = actionsAtomActor(run.ActorUsername)
		e.Summary = actionsAtomRunSummary(run)
		e.Link.Href = fmt.Sprintf("/%s/%s/actions/runs/%d", owner, repoName, run.RunIndex)
		feed.Entries = append(feed.Entries, e)
	}

	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	_, _ = io.WriteString(w, xml.Header)
	_ = enc.Encode(feed)
	_ = enc.Flush()
}

func actionsAtomRunTitle(run actionsdb.ListWorkflowRunsForRepoRow) string {
	title := workflowDisplayName(run.WorkflowName, run.WorkflowFile)
	state, _, _ := workflowRunState(run.Status, run.Conclusion)
	return fmt.Sprintf("%s #%d %s", title, run.RunIndex, strings.ToLower(state))
}

func actionsAtomRunSummary(run actionsdb.ListWorkflowRunsForRepoRow) string {
	parts := []string{
		"Workflow: " + workflowDisplayName(run.WorkflowName, run.WorkflowFile),
		"Event: " + workflowRunEventLabel(string(run.Event)),
		"Status: " + string(run.Status),
		"Branch: " + run.HeadRef,
		"Commit: " + shortSHA(run.HeadSha),
	}
	if run.Conclusion.Valid {
		parts = append(parts, "Conclusion: "+string(run.Conclusion.CheckConclusion))
	}
	parts = append(parts, "Run: #"+strconv.FormatInt(run.RunIndex, 10))
	return strings.Join(parts, "\n")
}

func actionsAtomActor(username string) string {
	username = strings.TrimSpace(username)
	if username == "" {
		return "shithub"
	}
	return username
}
