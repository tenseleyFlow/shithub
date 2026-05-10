// SPDX-License-Identifier: AGPL-3.0-or-later

package orgs_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	orgsdb "github.com/tenseleyFlow/shithub/internal/orgs/sqlc"
	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
	orgsh "github.com/tenseleyFlow/shithub/internal/web/handlers/orgs"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
	"github.com/tenseleyFlow/shithub/internal/web/render"
)

func TestTeamsListRequiresOrgMemberAndFiltersSecretTeams(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := dbtest.NewTestDB(t)
	ownerID := insertOrgAvatarUser(t, pool, "owner")
	memberID := insertOrgAvatarUser(t, pool, "member")
	outsiderID := insertOrgAvatarUser(t, pool, "outsider")
	orgID := insertOrgAvatarOrg(t, pool, ownerID, "acme")
	if _, err := pool.Exec(ctx, `INSERT INTO org_members (org_id, user_id, role) VALUES ($1, $2, 'member')`, orgID, memberID); err != nil {
		t.Fatalf("insert org member: %v", err)
	}
	visibleTeamID := insertTeamForTest(t, pool, orgID, "engineering", "Engineering", "visible")
	insertTeamForTest(t, pool, orgID, "security", "Security", "secret")
	if _, err := pool.Exec(ctx, `INSERT INTO team_members (team_id, user_id, role) VALUES ($1, $2, 'member')`, visibleTeamID, memberID); err != nil {
		t.Fatalf("insert team member: %v", err)
	}

	memberBody, memberStatus, _ := performTeamsListRequest(t, pool, middleware.CurrentUser{ID: memberID, Username: "member"}, "/acme/teams")
	if memberStatus != http.StatusOK {
		t.Fatalf("member status=%d body=%s", memberStatus, memberBody)
	}
	if !strings.Contains(memberBody, "TEAM=engineering:Engineering:1:0") {
		t.Fatalf("expected visible team with counts, got %s", memberBody)
	}
	if strings.Contains(memberBody, "security") {
		t.Fatalf("secret team leaked to non-team member: %s", memberBody)
	}

	outsiderBody, outsiderStatus, _ := performTeamsListRequest(t, pool, middleware.CurrentUser{ID: outsiderID, Username: "outsider"}, "/acme/teams")
	if outsiderStatus != http.StatusNotFound {
		t.Fatalf("outsider status=%d body=%s", outsiderStatus, outsiderBody)
	}

	_, anonymousStatus, anonymousLocation := performTeamsListRequest(t, pool, middleware.CurrentUser{}, "/acme/teams")
	if anonymousStatus != http.StatusSeeOther {
		t.Fatalf("anonymous status=%d", anonymousStatus)
	}
	if !strings.HasPrefix(anonymousLocation, "/login?next=") {
		t.Fatalf("anonymous redirect=%q", anonymousLocation)
	}
}

func TestTeamMemberAddRejectsNonOrgUsers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := dbtest.NewTestDB(t)
	ownerID := insertOrgAvatarUser(t, pool, "owner")
	outsiderID := insertOrgAvatarUser(t, pool, "outsider")
	orgID := insertOrgAvatarOrg(t, pool, ownerID, "acme")
	teamID := insertTeamForTest(t, pool, orgID, "engineering", "Engineering", "visible")

	form := url.Values{"user_id": {strconv.FormatInt(outsiderID, 10)}, "role": {"member"}}
	body, status, _ := performTeamsRequest(t, pool, middleware.CurrentUser{ID: ownerID, Username: "owner"}, http.MethodPost, "/acme/teams/engineering/members", form)
	if status != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", status, body)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM team_members WHERE team_id = $1`, teamID).Scan(&count); err != nil {
		t.Fatalf("count team members: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected no team member insert, got %d", count)
	}
}

func performTeamsListRequest(t *testing.T, pool *pgxpool.Pool, viewer middleware.CurrentUser, target string) (string, int, string) {
	return performTeamsRequest(t, pool, viewer, http.MethodGet, target, nil)
}

func performTeamsRequest(t *testing.T, pool *pgxpool.Pool, viewer middleware.CurrentUser, method, target string, form url.Values) (string, int, string) {
	t.Helper()
	rr, err := render.New(fstest.MapFS{
		"_layout.html":         {Data: []byte(`{{ define "layout" }}<html><body>{{ template "page" . }}</body></html>{{ end }}`)},
		"orgs/_org_nav.html":   {Data: []byte(`{{ define "org-nav" }}NAV={{ .ActiveOrgTab }}:{{ .TeamCount }}{{ end }}`)},
		"orgs/teams_list.html": {Data: []byte(`{{ define "page" }}{{ template "org-nav" . }} TOTAL={{ .TeamTotalCount }}{{ range .Teams }} TEAM={{ .Slug }}:{{ .DisplayName }}:{{ .MemberCount }}:{{ .RepoCount }}{{ end }}{{ end }}`)},
		"orgs/team_view.html":  {Data: []byte(`{{ define "page" }}TEAM{{ end }}`)},
		"orgs/people.html":     {Data: []byte(`{{ define "page" }}PEOPLE{{ end }}`)},
		"errors/400.html":      {Data: []byte(`{{ define "page" }}400{{ end }}`)},
		"errors/403.html":      {Data: []byte(`{{ define "page" }}403{{ end }}`)},
		"errors/404.html":      {Data: []byte(`{{ define "page" }}404{{ end }}`)},
		"errors/500.html":      {Data: []byte(`{{ define "page" }}500{{ end }}`)},
	}, render.Options{})
	if err != nil {
		t.Fatalf("render.New: %v", err)
	}
	h, err := orgsh.New(orgsh.Deps{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Render: rr,
		Pool:   pool,
	})
	if err != nil {
		t.Fatalf("orgsh.New: %v", err)
	}
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(middleware.WithCurrentUserForTest(r.Context(), viewer)))
		})
	})
	h.MountOrgRoutes(r)

	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req := httptest.NewRequest(method, target, body)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec.Body.String(), rec.Code, rec.Header().Get("Location")
}

func insertTeamForTest(t *testing.T, db orgsdb.DBTX, orgID int64, slug, displayName, privacy string) int64 {
	t.Helper()
	var id int64
	if err := db.QueryRow(context.Background(),
		`INSERT INTO teams (org_id, slug, display_name, privacy)
		 VALUES ($1, $2, $3, $4)
		 RETURNING id`,
		orgID, slug, displayName, privacy,
	).Scan(&id); err != nil {
		t.Fatalf("insert team: %v", err)
	}
	return id
}
