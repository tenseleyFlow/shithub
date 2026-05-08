// SPDX-License-Identifier: AGPL-3.0-or-later

package repo

import (
	"encoding/xml"
	"fmt"
	"io"
	"time"

	repogit "github.com/tenseleyFlow/shithub/internal/repos/git"
)

// writeAtom emits an RFC 4287 Atom feed of the most recent commits.
// The feed is intentionally minimal — title, author, updated, link,
// per-entry id/title/updated/author/summary. Skips fancy bits like
// `<content>` HTML to keep the surface tight (S25 owns rich
// content-rendering; this feed targets bots and aggregators).
func writeAtom(w io.Writer, owner, repoName, ref string, commits []repogit.Commit) {
	type atomAuthor struct {
		Name  string `xml:"name"`
		Email string `xml:"email,omitempty"`
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

	now := time.Now().UTC().Format(time.RFC3339)
	feed := atomFeed{
		Title:   fmt.Sprintf("%s/%s commits — %s", owner, repoName, ref),
		ID:      fmt.Sprintf("urn:shithub:%s:%s:%s", owner, repoName, ref),
		Updated: now,
	}
	feed.Link.Href = fmt.Sprintf("/%s/%s/commits/%s", owner, repoName, ref)
	feed.Link.Rel = "self"
	for _, c := range commits {
		var e atomEntry
		e.ID = "urn:shithub:commit:" + c.OID
		e.Title = c.Subject
		e.Updated = c.AuthorWhen.UTC().Format(time.RFC3339)
		e.Author.Name = c.AuthorName
		e.Author.Email = c.AuthorEmail
		e.Summary = c.Body
		e.Link.Href = fmt.Sprintf("/%s/%s/commit/%s", owner, repoName, c.OID)
		feed.Entries = append(feed.Entries, e)
	}
	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	_, _ = io.WriteString(w, xml.Header)
	_ = enc.Encode(feed)
	_ = enc.Flush()
}
