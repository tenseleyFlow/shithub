// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGlobalIssueSearchTextDropsDashboardOperators(t *testing.T) {
	t.Parallel()

	got := globalIssueSearchText("is:issue state:open archived:false assignee:@me sort:updated-desc flaky deploy")
	if got != "flaky deploy" {
		t.Fatalf("globalIssueSearchText = %q, want %q", got, "flaky deploy")
	}
}

func TestGlobalIssueSearchTextDropsExecutableQualifiers(t *testing.T) {
	t.Parallel()

	got := globalIssueSearchText(`is:pr review-requested:@me sort:comments-desc "flaky deploy"`)
	if got != "flaky deploy" {
		t.Fatalf("globalIssueSearchText = %q, want %q", got, "flaky deploy")
	}
}

func TestDefaultQueriesOmitStateAllQualifier(t *testing.T) {
	t.Parallel()

	for _, query := range []string{
		defaultIssueQuery("assigned", "all"),
		defaultIssueQuery("recent", "all"),
		defaultPullQuery("review-requests", "all", "mfwolffe"),
	} {
		if strings.Contains(query, "state:all") {
			t.Fatalf("default query should omit state:all: %q", query)
		}
	}
}

func TestStateTabsDropStaleQueryStateOperators(t *testing.T) {
	t.Parallel()

	tabs := stateTabs("/issues/assigned", "is:issue state:closed archived:false bug", "closed", globalIssueCounts{
		Total:  3,
		Open:   1,
		Closed: 2,
	})
	if got := tabs[0].Href; got != "/issues/assigned?q=is%3Aissue+archived%3Afalse+bug" {
		t.Fatalf("open state href = %q", got)
	}
	if got := tabs[1].Href; got != "/issues/assigned?q=is%3Aissue+archived%3Afalse+bug&state=closed" {
		t.Fatalf("closed state href = %q", got)
	}
}

func TestGlobalStateFromRequestHonorsQueryOperators(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest("GET", "/issues/assigned?state=open", nil)
	if got := globalStateFromRequest(req, "is:issue state:closed archived:false"); got != "closed" {
		t.Fatalf("globalStateFromRequest state:closed = %q, want closed", got)
	}

	req = httptest.NewRequest("GET", "/issues/assigned?state=all", nil)
	if got := globalStateFromRequest(req, ""); got != "all" {
		t.Fatalf("globalStateFromRequest query param = %q, want all", got)
	}
}

func TestDashboardHrefOmitsEmptyQuery(t *testing.T) {
	t.Parallel()

	if got := dashboardHref("/pulls", queryValues("", "open", "created")); got != "/pulls" {
		t.Fatalf("dashboardHref default values = %q, want /pulls", got)
	}
	if got := dashboardHref("/pulls", queryValues("state:closed bug", "closed", "assigned")); got != "/pulls?q=state%3Aclosed+bug&state=closed&view=assigned" {
		t.Fatalf("dashboardHref populated values = %q", got)
	}
}
