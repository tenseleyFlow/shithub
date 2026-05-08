// SPDX-License-Identifier: AGPL-3.0-or-later

// Package extensions hosts the AST transformer that adds shithub-
// specific inline patterns (`@user`, `#N`, `owner/repo#N`, commit
// SHAs, emoji shortcodes) to Goldmark's parsed text without ever
// touching the contents of code blocks or inline code.
//
// Approach: a single `parser.ASTTransformer` walks the document
// after parsing, visiting only `*ast.Text` nodes whose ancestors
// are NOT code/codespan/autolink/link nodes. Each visited text node
// is run through one combined regex; matches are replaced with
// `*ast.Link` (mention/ref/commit) or `*ast.String` (emoji) nodes,
// with the surrounding text preserved as `*ast.String` segments.
//
// Why an ASTTransformer instead of inline parsers: inline parsers
// run during the main parse pass and need a `Trigger()` byte set
// plus careful interaction with Goldmark's existing inline
// disambiguation. The transformer approach is simpler, well-trodden
// in other Go markdown stacks, and produces equivalent output for
// every input we care about.
package extensions

import (
	"bytes"
	"context"
	"regexp"
	"strconv"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

// Resolvers wires the transformer against the runtime. The fields
// are independent so the parent package can decide which flavors
// to enable. nil-resolver means "render this kind as plain text"
// (no link, no error).
//
// All resolvers MUST be visibility-aware. The transformer does not
// re-check visibility — it trusts the resolver's `ok` to gate
// existence.
type Resolvers struct {
	User func(ctx context.Context, username string) (href string, ok bool)
	// Issue covers both same-repo (#N when ownerHint == "") and
	// cross-repo (owner/repo#N).
	Issue func(ctx context.Context, ownerHint, repoHint string, number int64, viewerUserID int64) (href string, ok bool)
	// Commit is invoked only when RepoOwner+RepoName are both set
	// (a same-repo render) and the matched token is a 7-40 char
	// lowercase hex string at a word boundary.
	Commit func(ctx context.Context, repoOwner, repoName, shaPrefix string) (href, fullSHA string, ok bool)
}

// Options is the per-render config consumed by the transformer.
type Options struct {
	Ctx          context.Context
	RepoOwner    string
	RepoName     string
	ViewerUserID int64
	Resolvers    Resolvers
	// Refs and Mentions accumulate resolved references for the caller.
	// Pointers so the transformer can append.
	Refs     *[]Ref
	Mentions *[]Mention
}

// Ref / Mention mirror the parent-package types; we redeclare to
// avoid an import cycle.
type Ref struct {
	Kind    string
	Owner   string
	Repo    string
	Number  int64
	FullSHA string
	Href    string
}

type Mention struct {
	Username string
	Href     string
}

// reCombined matches every pattern in one pass. Order in the
// alternation is by how they appear in source after parsing — left
// to right. Capture groups:
//
//	(?:^|[^\w/])     leading boundary (consumed but reattached as text)
//	#1               cross-repo: owner / repo / number
//	#4               same-repo: number
//	#5               mention: username
//	#6               commit prefix
//	#7               emoji name
var reCombined = regexp.MustCompile(`` +
	// cross-repo: alice/proj#3
	`([A-Za-z0-9][A-Za-z0-9._-]*)/([A-Za-z0-9][A-Za-z0-9._-]*)#([0-9]{1,9})\b` +
	// or same-repo: #3 — must have non-word non-/ boundary on the left
	`|(?:^|[^\w/])#([0-9]{1,9})\b` +
	// or mention: @alice — must have non-word boundary on the left
	`|(?:^|[^\w])@([A-Za-z0-9][A-Za-z0-9_-]{0,38})\b` +
	// or commit SHA: 7–40 lowercase hex, word-boundary on both sides
	`|(?:^|[^\w/])([0-9a-f]{7,40})\b` +
	// or emoji shortcode: :smile:
	`|:([a-z0-9_+\-]+):`,
)

// Extension is a goldmark.Extender that registers the AST transformer.
type Extension struct{ Opts *Options }

// New constructs the extender with the given options.
func New(opts *Options) goldmark.Extender { return &Extension{Opts: opts} }

// Extend implements goldmark.Extender.
func (e *Extension) Extend(m goldmark.Markdown) {
	m.Parser().AddOptions(parser.WithASTTransformers(
		util.Prioritized(&transformer{opts: e.Opts}, 999),
	))
}

type transformer struct{ opts *Options }

// Transform walks the document and replaces matched text segments.
func (t *transformer) Transform(doc *ast.Document, reader text.Reader, _ parser.Context) {
	if t.opts == nil {
		return
	}
	source := reader.Source()
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		// Skip subtrees that should never be linkified.
		switch n.(type) {
		case *ast.CodeSpan, *ast.AutoLink, *ast.Link, *ast.Image,
			*ast.FencedCodeBlock, *ast.CodeBlock, *ast.RawHTML, *ast.HTMLBlock:
			return ast.WalkSkipChildren, nil
		}
		txt, ok := n.(*ast.Text)
		if !ok {
			return ast.WalkContinue, nil
		}
		t.replaceText(txt, source)
		return ast.WalkContinue, nil
	})
}

// replaceText finds matches in the segment of `txt` and inserts
// new sibling nodes (string runs + links) before the original text;
// the original text is removed once everything's stitched in.
func (t *transformer) replaceText(txt *ast.Text, source []byte) {
	body := txt.Segment.Value(source)
	matches := reCombined.FindAllSubmatchIndex(body, -1)
	if len(matches) == 0 {
		return
	}
	parent := txt.Parent()
	if parent == nil {
		return
	}

	cursor := 0
	for _, m := range matches {
		matchStart, matchEnd := m[0], m[1]

		// Determine which alternation captured + where the visible
		// content starts (excluding the regex-consumed boundary
		// char, if any).
		var (
			isCrossRepo = m[2] >= 0
			isSameRepo  = m[8] >= 0
			isMention   = m[10] >= 0
			isCommit    = m[12] >= 0
			isEmoji     = m[14] >= 0
		)
		var contentStart int
		switch {
		case isCrossRepo:
			contentStart = m[2]
		case isSameRepo:
			contentStart = m[8] - 1 // include `#`
		case isMention:
			contentStart = m[10] - 1 // include `@`
		case isCommit:
			contentStart = m[12]
		case isEmoji:
			contentStart = m[14] - 1 // include leading `:`
		}

		// Emit (a) any text between the previous cursor and the
		// match start, then (b) the consumed-but-not-content
		// boundary char (when contentStart > matchStart). Both into
		// the parent before the original text node.
		if matchStart > cursor {
			t.insertText(parent, txt, body[cursor:matchStart])
		}
		if contentStart > matchStart {
			t.insertText(parent, txt, body[matchStart:contentStart])
		}

		// Now emit the resolved (or fallback-plain) match content.
		display := body[contentStart:matchEnd]
		switch {
		case isCrossRepo:
			owner := string(body[m[2]:m[3]])
			repo := string(body[m[4]:m[5]])
			numStr := string(body[m[6]:m[7]])
			if !t.appendIssueLink(parent, txt, owner, repo, numStr, display) {
				t.insertText(parent, txt, display)
			}
		case isSameRepo:
			numStr := string(body[m[8]:m[9]])
			if !t.appendIssueLink(parent, txt, "", "", numStr, display) {
				t.insertText(parent, txt, display)
			}
		case isMention:
			name := string(body[m[10]:m[11]])
			if !t.appendMentionLink(parent, txt, name, display) {
				t.insertText(parent, txt, display)
			}
		case isCommit:
			sha := string(body[m[12]:m[13]])
			if !t.appendCommitLink(parent, txt, sha, display) {
				t.insertText(parent, txt, display)
			}
		case isEmoji:
			name := string(body[m[14]:m[15]])
			if uni, ok := lookupEmoji(name); ok {
				t.insertText(parent, txt, []byte(uni))
			} else {
				t.insertText(parent, txt, display)
			}
		}
		cursor = matchEnd
	}
	// Trailing text after the last match.
	if cursor < len(body) {
		t.insertText(parent, txt, body[cursor:])
	}
	parent.RemoveChild(parent, txt)
}

// insertText appends a string node before the original text node
// (which is removed at the end of replaceText).
func (t *transformer) insertText(parent, before ast.Node, b []byte) {
	if len(b) == 0 {
		return
	}
	s := ast.NewString(append([]byte(nil), b...))
	parent.InsertBefore(parent, before, s)
}

// appendIssueLink resolves an issue/PR ref and inserts a Link node.
// `display` is the visible text the user typed (e.g. "#42" or
// "alice/proj#5"). Returns false when the resolver declines (in
// which case the caller renders the display text as plain text —
// no link, no existence leak).
func (t *transformer) appendIssueLink(parent, before ast.Node, owner, repo, numStr string, display []byte) bool {
	if t.opts.Resolvers.Issue == nil {
		return false
	}
	num, err := strconv.ParseInt(numStr, 10, 64)
	if err != nil {
		return false
	}
	href, ok := t.opts.Resolvers.Issue(t.opts.Ctx, owner, repo, num, t.opts.ViewerUserID)
	if !ok {
		return false
	}
	link := ast.NewLink()
	link.Destination = []byte(href)
	link.AppendChild(link, ast.NewString(append([]byte(nil), display...)))
	parent.InsertBefore(parent, before, link)

	if t.opts.Refs != nil {
		*t.opts.Refs = append(*t.opts.Refs, Ref{
			Kind:   "issue",
			Owner:  owner,
			Repo:   repo,
			Number: num,
			Href:   href,
		})
	}
	return true
}

// appendMentionLink resolves a @username and inserts a Link node.
func (t *transformer) appendMentionLink(parent, before ast.Node, username string, display []byte) bool {
	if t.opts.Resolvers.User == nil {
		return false
	}
	href, ok := t.opts.Resolvers.User(t.opts.Ctx, username)
	if !ok {
		return false
	}
	link := ast.NewLink()
	link.Destination = []byte(href)
	link.AppendChild(link, ast.NewString(append([]byte(nil), display...)))
	parent.InsertBefore(parent, before, link)
	if t.opts.Mentions != nil {
		*t.opts.Mentions = append(*t.opts.Mentions, Mention{
			Username: username,
			Href:     href,
		})
	}
	return true
}

// appendCommitLink resolves a commit SHA prefix in the current repo.
func (t *transformer) appendCommitLink(parent, before ast.Node, shaPrefix string, display []byte) bool {
	if t.opts.Resolvers.Commit == nil || t.opts.RepoOwner == "" || t.opts.RepoName == "" {
		return false
	}
	href, full, ok := t.opts.Resolvers.Commit(t.opts.Ctx, t.opts.RepoOwner, t.opts.RepoName, shaPrefix)
	if !ok {
		return false
	}
	link := ast.NewLink()
	link.Destination = []byte(href)
	// Display the SHA as <code>; preserve the user's typed length.
	codeText := append([]byte(nil), display...)
	codeText = bytes.TrimSpace(codeText)
	link.AppendChild(link, ast.NewCodeSpan())
	cs := link.LastChild().(*ast.CodeSpan)
	cs.AppendChild(cs, ast.NewString(codeText))
	parent.InsertBefore(parent, before, link)

	if t.opts.Refs != nil {
		*t.opts.Refs = append(*t.opts.Refs, Ref{
			Kind:    "commit",
			FullSHA: full,
			Href:    href,
		})
	}
	return true
}

// trimLeadingNonWord drops the leading boundary char(s) — used when
// a same-repo or mention token is rendered as plain text fallback.
func trimLeadingNonWord(b []byte) []byte {
	for len(b) > 0 && !isWordByte(b[0]) {
		b = b[1:]
	}
	return b
}

func isWordByte(c byte) bool {
	return c == '_' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// silence unused-import warnings in a stripped build.
var (
	_ = strings.Builder{}
	_ = trimLeadingNonWord
)
