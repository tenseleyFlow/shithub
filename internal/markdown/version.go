// SPDX-License-Identifier: AGPL-3.0-or-later

package markdown

// Version is the canonical pipeline-version stamp written to
// `body_html_cached`-style columns whenever a comment / issue body /
// PR body is rendered. Bumping this constant invalidates cached HTML
// lazily on read: callers compare the stored `md_pipeline_version` to
// `Version` and re-render when they don't match.
//
// Bump rules:
//   - Sanitizer policy change (allow/disallow tag, attribute, scheme).
//   - New AST extension or rendering output change.
//   - Goldmark / bluemonday major-version upgrade with output drift.
//
// Don't bump for:
//   - Bug fixes that don't change rendered HTML for any input.
//   - Performance-only changes.
const Version int32 = 1
