// SPDX-License-Identifier: AGPL-3.0-or-later

package repo

import (
	"net/http"

	diffparse "github.com/tenseleyFlow/shithub/internal/repos/diff/parse"
	diffrender "github.com/tenseleyFlow/shithub/internal/repos/diff/render"
	diffsource "github.com/tenseleyFlow/shithub/internal/repos/diff/source"
)

// compareSourceMergeBase fetches the three-dot diff bytes for the
// compare view. Mirrored from history.go's commit-page wiring; both
// pages share the same renderer (S19).
func compareSourceMergeBase(r *http.Request, gitDir, base, head string) ([]byte, error) {
	hideWS := r.URL.Query().Get("w") == "1"
	return diffsource.FromMergeBase(r.Context(), gitDir, base, head, diffsource.Options{
		IgnoreWhitespace: hideWS, FindRenames: true,
	})
}

// renderCompareDiff parses + renders unified-mode by default. The
// commit page exposes split + WS toggles; the compare view picks them
// up via the same query params.
func renderCompareDiff(patch []byte) string {
	parsed, err := diffparse.ParseBytes(patch)
	if err != nil {
		return ""
	}
	return diffrender.Diff(parsed, diffrender.Options{Mode: diffrender.ModeUnified})
}
