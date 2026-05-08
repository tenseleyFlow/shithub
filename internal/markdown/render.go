// SPDX-License-Identifier: AGPL-3.0-or-later

package markdown

import (
	"bytes"
	"context"
	"errors"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/renderer"
	gmhtml "github.com/yuin/goldmark/renderer/html"

	"github.com/tenseleyFlow/shithub/internal/markdown/extensions"
)

// ErrInputTooLarge is returned when source exceeds MaxRenderInputBytes.
// Callers should reject the input at the API layer before reaching
// Render; this is the renderer's defensive fallback.
var ErrInputTooLarge = errors.New("markdown: source exceeds MaxRenderInputBytes")

// Render is the canonical markdown entry point. The output bytes
// have already been Goldmark-rendered AND bluemonday-sanitized;
// callers can wrap them as `template.HTML` and inject into a
// template directly.
//
// `refs` and `mentions` carry the resolved cross-reference/mention
// state for downstream consumers (S29 notification fan-out, S21
// issue_references index). The order matches first-occurrence in
// source.
//
// A nil ctx is fine; resolvers receive context.Background() in
// that case.
func Render(ctx context.Context, src []byte, opts Options) (rendered []byte, refs []Ref, mentions []Mention, err error) {
	if len(src) == 0 {
		return nil, nil, nil, nil
	}
	if len(src) > MaxRenderInputBytes {
		return nil, nil, nil, ErrInputTooLarge
	}
	if ctx == nil {
		ctx = context.Background()
	}

	// Build a fresh Goldmark for each render so the per-call
	// extension Options can plug in without races. The build is
	// cheap (~µs) and removes any cross-render contamination of
	// the AST transformer state.
	xRefs := []extensions.Ref{}
	xMentions := []extensions.Mention{}
	xOpts := &extensions.Options{
		Ctx:          ctx,
		ViewerUserID: opts.ViewerUserID,
		Refs:         &xRefs,
		Mentions:     &xMentions,
		Resolvers: extensions.Resolvers{
			User:   opts.Resolvers.User,
			Issue:  opts.Resolvers.Issue,
			Commit: opts.Resolvers.Commit,
		},
	}
	if opts.Repo != nil {
		xOpts.RepoOwner = opts.Repo.OwnerUsername
		xOpts.RepoName = opts.Repo.RepoName
	}

	// gmhtml.WithUnsafe lets raw HTML through the parser → bluemonday
	// scrubs at the sanitizer pass. Without this, every <details>,
	// <kbd>, <sup>, etc. that users type would be HTML-escaped before
	// the sanitizer ever sees them. The strict policy in sanitize.go
	// is the security boundary.
	htmlOpts := []renderer.Option{gmhtml.WithXHTML(), gmhtml.WithUnsafe()}
	if opts.SoftBreakAsBR {
		htmlOpts = append(htmlOpts, gmhtml.WithHardWraps())
	}

	gm := goldmark.New(
		goldmark.WithExtensions(
			extension.GFM, // tables, strikethrough, autolinks, task list
			extensions.New(xOpts),
		),
		goldmark.WithParserOptions(parser.WithAutoHeadingID()),
		goldmark.WithRendererOptions(htmlOpts...),
	)

	var buf bytes.Buffer
	if err := gm.Convert(src, &buf); err != nil {
		return nil, nil, nil, err
	}
	clean := sanitizeBytes(buf.Bytes())

	// Convert extension-local types into the public types.
	if len(xRefs) > 0 {
		refs = make([]Ref, len(xRefs))
		for i, r := range xRefs {
			refs[i] = Ref{
				Kind:    r.Kind,
				Owner:   r.Owner,
				Repo:    r.Repo,
				Number:  r.Number,
				FullSHA: r.FullSHA,
				Href:    r.Href,
			}
		}
	}
	if len(xMentions) > 0 {
		mentions = make([]Mention, len(xMentions))
		for i, m := range xMentions {
			mentions[i] = Mention{Username: m.Username, Href: m.Href}
		}
	}
	return clean, refs, mentions, nil
}

// RenderHTML is the back-compat shim for callers that don't need
// resolved refs/mentions. SoftBreakAsBR defaults to true (matches
// the comment-style legacy behavior of the S17 helper this replaces).
//
// The signature matches `internal/repos/markdown.RenderHTML` so
// existing callers can swap the import path without code changes.
// New callers should prefer Render directly.
func RenderHTML(src []byte) (string, error) {
	out, _, _, err := Render(context.Background(), src, Options{SoftBreakAsBR: true})
	if err != nil {
		return "", err
	}
	return string(out), nil
}
