// SPDX-License-Identifier: AGPL-3.0-or-later

package checks

import (
	"testing"

	checksdb "github.com/tenseleyFlow/shithub/internal/checks/sqlc"
)

// run constructs a CheckRun with just the fields the rollup looks at.
func run(status string, conclusion string) checksdb.CheckRun {
	r := checksdb.CheckRun{Status: checksdb.CheckStatus(status)}
	if conclusion != "" {
		r.Conclusion = checksdb.NullCheckConclusion{
			CheckConclusion: checksdb.CheckConclusion(conclusion),
			Valid:           true,
		}
	}
	return r
}

func TestDeriveSuiteRollup(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		runs        []checksdb.CheckRun
		wantStatus  string
		wantConcl   string
	}{
		{
			"empty → queued",
			nil,
			"queued", "",
		},
		{
			"all queued → queued",
			[]checksdb.CheckRun{run("queued", ""), run("queued", "")},
			"queued", "",
		},
		{
			"one in_progress → in_progress",
			[]checksdb.CheckRun{run("queued", ""), run("in_progress", "")},
			"in_progress", "",
		},
		{
			"all completed success → success",
			[]checksdb.CheckRun{run("completed", "success"), run("completed", "success")},
			"completed", "success",
		},
		{
			"any failure → failure",
			[]checksdb.CheckRun{run("completed", "success"), run("completed", "failure")},
			"completed", "failure",
		},
		{
			"failure beats timed_out",
			[]checksdb.CheckRun{run("completed", "timed_out"), run("completed", "failure")},
			"completed", "failure",
		},
		{
			"timed_out beats cancelled beats action_required",
			[]checksdb.CheckRun{run("completed", "cancelled"), run("completed", "timed_out"), run("completed", "action_required")},
			"completed", "timed_out",
		},
		{
			"all neutral → neutral (no successes)",
			[]checksdb.CheckRun{run("completed", "neutral"), run("completed", "neutral")},
			"completed", "neutral",
		},
		{
			"success beats neutral",
			[]checksdb.CheckRun{run("completed", "success"), run("completed", "neutral")},
			"completed", "success",
		},
		{
			"all skipped → skipped",
			[]checksdb.CheckRun{run("completed", "skipped"), run("completed", "skipped")},
			"completed", "skipped",
		},
		{
			"all stale → stale",
			[]checksdb.CheckRun{run("completed", "stale")},
			"completed", "stale",
		},
		{
			"completed without conclusion → completed/neutral fallback",
			[]checksdb.CheckRun{run("completed", "")},
			"completed", "neutral",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotStatus, gotConcl := DeriveSuiteRollup(c.runs)
			if gotStatus != c.wantStatus || gotConcl != c.wantConcl {
				t.Errorf("got (%s, %s), want (%s, %s)", gotStatus, gotConcl, c.wantStatus, c.wantConcl)
			}
		})
	}
}
