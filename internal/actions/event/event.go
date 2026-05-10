// SPDX-License-Identifier: AGPL-3.0-or-later

// Package event builds canonical `shithub.event` payloads from
// triggering domain events.
//
// The payload schema is the v1 contract documented in
// docs/internal/actions-schema.md. Workflow authors template against
// these field paths from `${{ shithub.event.X }}` (and the `github.event`
// alias) — once data is in `workflow_runs.event_payload`, schema
// changes are breaking. **Adding a field requires a reviewer-required
// note + a doc update.** Treat this package the same way we treat
// migration files.
//
// Each constructor takes typed inputs (so adding a field is visible
// in the function signature) and returns a `map[string]any` because
// that's exactly what `internal/actions/expr.evalEventPath` consumes.
// The returned map is also pgx-encodable to jsonb without further
// transformation, so callers (S41b's trigger pipeline) can pass it
// straight into `InsertWorkflowRun`.
//
// What's NOT here in v1: `workflow_run`-event payloads (re-runs),
// `release` events, `deployment_status`, anything outside the four
// triggers documented in actions-schema.md. Those land in v2 with
// their own constructors.
package event

// HeadCommit is the typed input shape for push-event head_commit.
// Fields mirror the documented v1 schema: message, id, author.
type HeadCommit struct {
	Message string
	ID      string
	Author  string
}

// Push builds the payload for a push trigger.
//
// Documented v1 keys: ref, before, after, head_commit{message,id,author}.
// The author field is a flat string (commit author name) in v1; if a
// future field needs the full author object, that's a v2 break.
func Push(ref, before, after string, hc HeadCommit) map[string]any {
	return map[string]any{
		"ref":    ref,
		"before": before,
		"after":  after,
		"head_commit": map[string]any{
			"message": hc.Message,
			"id":      hc.ID,
			"author":  hc.Author,
		},
	}
}

// PRRef is the head/base ref descriptor inside a pull_request payload.
type PRRef struct {
	Ref string // e.g. "feature/foo" or "main"
	SHA string // 40-char object id
}

// PullRequest builds the payload for a pull_request trigger.
//
// Documented v1 keys:
//
//	action, number,
//	pull_request{title, head{ref,sha}, base{ref,sha}, user{login}}.
//
// The action field tracks the activity type ("opened", "synchronize",
// "reopened", "closed", "edited", "labeled", "unlabeled"). v1 doesn't
// constrain the value here — S41b's pull_request emitter is the
// allowlist.
func PullRequest(action string, number int64, title string, head, base PRRef, userLogin string) map[string]any {
	return map[string]any{
		"action": action,
		"number": number,
		"pull_request": map[string]any{
			"title": title,
			"head": map[string]any{
				"ref": head.Ref,
				"sha": head.SHA,
			},
			"base": map[string]any{
				"ref": base.Ref,
				"sha": base.SHA,
			},
			"user": map[string]any{
				"login": userLogin,
			},
		},
	}
}

// Schedule is the payload for a cron-fired schedule trigger. Empty
// by design — the cron expression itself is on the workflow_runs row,
// not in the payload. Returned as a non-nil empty map so calling code
// can pgx-encode without a nil check.
func Schedule() map[string]any {
	return map[string]any{}
}

// WorkflowDispatch builds the payload for a manual workflow_dispatch
// trigger. Inputs are stringified (matching GHA semantics — even
// boolean inputs arrive as "true"/"false" strings) and wrapped under
// the `inputs` key so authors template `${{ shithub.event.inputs.foo }}`.
func WorkflowDispatch(inputs map[string]string) map[string]any {
	bag := make(map[string]any, len(inputs))
	for k, v := range inputs {
		bag[k] = v
	}
	return map[string]any{
		"inputs": bag,
	}
}
