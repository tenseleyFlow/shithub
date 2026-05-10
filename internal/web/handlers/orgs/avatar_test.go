// SPDX-License-Identifier: AGPL-3.0-or-later

package orgs_test

import (
	"bytes"
	"context"
	"html"
	"image"
	"image/color"
	"image/png"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/go-chi/chi/v5"

	"github.com/tenseleyFlow/shithub/internal/infra/storage"
	orgsdb "github.com/tenseleyFlow/shithub/internal/orgs/sqlc"
	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
	orgsh "github.com/tenseleyFlow/shithub/internal/web/handlers/orgs"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
	"github.com/tenseleyFlow/shithub/internal/web/render"
)

func TestOrgAvatarUploadRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := dbtest.NewTestDB(t)
	q := orgsdb.New()
	viewerID := insertOrgAvatarUser(t, pool, "mfwolffe")
	orgID := insertOrgAvatarOrg(t, pool, viewerID, "tenseleyFlow")

	tmplFS := fstest.MapFS{
		"_layout.html":               {Data: []byte(`{{ define "layout" }}<html><body>{{ template "page" . }}</body></html>{{ end }}`)},
		"orgs/settings_profile.html": {Data: []byte(`{{ define "page" }}{{ with .Error }}ERROR={{ . }}{{ end }}<form action="/organizations/{{ .Org.Slug }}/settings/profile/avatar"><input name=csrf_token value="{{.CSRFToken}}"></form>{{ if .HasAvatar }}REMOVE=/organizations/{{ .Org.Slug }}/settings/profile/avatar/remove{{ end }}{{ end }}`)},
		"errors/403.html":            {Data: []byte(`{{ define "page" }}403{{ end }}`)},
		"errors/404.html":            {Data: []byte(`{{ define "page" }}404{{ end }}`)},
		"errors/500.html":            {Data: []byte(`{{ define "page" }}500{{ end }}`)},
	}
	rr, err := render.New(tmplFS, render.Options{})
	if err != nil {
		t.Fatalf("render.New: %v", err)
	}
	h, err := orgsh.New(orgsh.Deps{
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		Render:      rr,
		Pool:        pool,
		ObjectStore: storage.NewMemoryStore(),
	})
	if err != nil {
		t.Fatalf("orgsh.New: %v", err)
	}
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			viewer := middleware.CurrentUser{ID: viewerID, Username: "mfwolffe"}
			next.ServeHTTP(w, r.WithContext(middleware.WithCurrentUserForTest(r.Context(), viewer)))
		})
	})
	h.MountCreate(r)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)

	cli := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}

	resp, err := cli.Get(srv.URL + "/organizations/tenseleyFlow/settings/profile")
	if err != nil {
		t.Fatalf("GET settings: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET status=%d body=%s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "/organizations/tenseleyFlow/settings/profile/avatar") {
		t.Fatalf("expected upload form, got %s", body)
	}

	resp = postOrgAvatar(t, cli, srv.URL+"/organizations/tenseleyFlow/settings/profile/avatar", makeOrgTestPNG(t))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("upload status=%d", resp.StatusCode)
	}
	if got := resp.Header.Get("Location"); got != "/organizations/tenseleyFlow/settings/profile" {
		t.Fatalf("upload Location=%q", got)
	}
	org, err := q.GetOrgByID(ctx, pool, orgID)
	if err != nil {
		t.Fatalf("GetOrgByID: %v", err)
	}
	if !org.AvatarObjectKey.Valid || !strings.HasPrefix(org.AvatarObjectKey.String, "avatars/orgs/") {
		t.Fatalf("avatar key=%q valid=%v", org.AvatarObjectKey.String, org.AvatarObjectKey.Valid)
	}

	resp, err = cli.PostForm(srv.URL+"/organizations/tenseleyFlow/settings/profile/avatar/remove", url.Values{})
	if err != nil {
		t.Fatalf("POST remove: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("remove status=%d", resp.StatusCode)
	}
	org, err = q.GetOrgByID(ctx, pool, orgID)
	if err != nil {
		t.Fatalf("GetOrgByID after remove: %v", err)
	}
	if org.AvatarObjectKey.Valid {
		t.Fatalf("expected cleared avatar key, got %q", org.AvatarObjectKey.String)
	}
}

func TestOrgSettingsProfileUpdate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := dbtest.NewTestDB(t)
	q := orgsdb.New()
	viewerID := insertOrgAvatarUser(t, pool, "mfwolffe")
	orgID := insertOrgAvatarOrg(t, pool, viewerID, "tenseleyFlow")

	tmplFS := fstest.MapFS{
		"_layout.html":               {Data: []byte(`{{ define "layout" }}<html><body>{{ template "page" . }}</body></html>{{ end }}`)},
		"orgs/settings_profile.html": {Data: []byte(`{{ define "page" }}{{ with .Error }}ERROR={{ . }}{{ end }}{{ with .Success }}SUCCESS={{ . }}{{ end }}DISPLAY={{ .Form.DisplayName }}{{ end }}`)},
		"errors/403.html":            {Data: []byte(`{{ define "page" }}403{{ end }}`)},
		"errors/404.html":            {Data: []byte(`{{ define "page" }}404{{ end }}`)},
		"errors/500.html":            {Data: []byte(`{{ define "page" }}500{{ end }}`)},
	}
	rr, err := render.New(tmplFS, render.Options{})
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
			viewer := middleware.CurrentUser{ID: viewerID, Username: "mfwolffe"}
			next.ServeHTTP(w, r.WithContext(middleware.WithCurrentUserForTest(r.Context(), viewer)))
		})
	})
	h.MountCreate(r)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)

	resp, err := http.PostForm(srv.URL+"/organizations/tenseleyFlow/settings/profile", url.Values{
		"display_name":             {"Tenseley Flow"},
		"description":              {"Workflow repositories"},
		"website":                  {"example.com"},
		"location":                 {"United States of America"},
		"billing_email":            {"billing@example.com"},
		"allow_member_repo_create": {"on"},
	})
	if err != nil {
		t.Fatalf("POST settings: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST status=%d body=%s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "SUCCESS=Organization profile updated.") {
		t.Fatalf("expected success render, got %s", body)
	}
	org, err := q.GetOrgByID(ctx, pool, orgID)
	if err != nil {
		t.Fatalf("GetOrgByID: %v", err)
	}
	if org.DisplayName != "Tenseley Flow" ||
		org.Description != "Workflow repositories" ||
		org.Website != "https://example.com" ||
		org.Location != "United States of America" ||
		org.BillingEmail != "billing@example.com" ||
		!org.AllowMemberRepoCreate {
		t.Fatalf("unexpected org after update: %#v", org)
	}

	resp, err = http.PostForm(srv.URL+"/organizations/tenseleyFlow/settings/profile", url.Values{
		"display_name":  {"Tenseley Flow"},
		"billing_email": {"billing@example.com"},
	})
	if err != nil {
		t.Fatalf("POST settings clear checkbox: %v", err)
	}
	_ = resp.Body.Close()
	org, err = q.GetOrgByID(ctx, pool, orgID)
	if err != nil {
		t.Fatalf("GetOrgByID after checkbox clear: %v", err)
	}
	if org.AllowMemberRepoCreate {
		t.Fatalf("expected unchecked allow_member_repo_create to persist false")
	}
}

func TestOrgSettingsDeleteRequiresSlugConfirmation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := dbtest.NewTestDB(t)
	q := orgsdb.New()
	viewerID := insertOrgAvatarUser(t, pool, "mfwolffe")
	insertOrgAvatarOrg(t, pool, viewerID, "tenseleyFlow")

	tmplFS := fstest.MapFS{
		"_layout.html":               {Data: []byte(`{{ define "layout" }}<html><body>{{ template "page" . }}</body></html>{{ end }}`)},
		"orgs/settings_profile.html": {Data: []byte(`{{ define "page" }}{{ with .Error }}ERROR={{ . }}{{ end }}{{ end }}`)},
		"errors/403.html":            {Data: []byte(`{{ define "page" }}403{{ end }}`)},
		"errors/404.html":            {Data: []byte(`{{ define "page" }}404{{ end }}`)},
		"errors/500.html":            {Data: []byte(`{{ define "page" }}500{{ end }}`)},
	}
	rr, err := render.New(tmplFS, render.Options{})
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
			viewer := middleware.CurrentUser{ID: viewerID, Username: "mfwolffe"}
			next.ServeHTTP(w, r.WithContext(middleware.WithCurrentUserForTest(r.Context(), viewer)))
		})
	})
	h.MountCreate(r)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	cli := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}

	resp, err := cli.PostForm(srv.URL+"/organizations/tenseleyFlow/settings/delete", url.Values{
		"confirm_slug": {"wrong"},
	})
	if err != nil {
		t.Fatalf("POST delete wrong confirmation: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("wrong confirmation status=%d body=%s", resp.StatusCode, body)
	}
	if !strings.Contains(html.UnescapeString(string(body)), "ERROR=Enter this organization's name to confirm deletion.") {
		t.Fatalf("expected confirmation error, got %s", body)
	}
	org, err := q.GetOrgBySlugIncludingDeleted(ctx, pool, "tenseleyFlow")
	if err != nil {
		t.Fatalf("GetOrgBySlugIncludingDeleted: %v", err)
	}
	if org.DeletedAt.Valid {
		t.Fatalf("org should not be deleted after wrong confirmation")
	}

	resp, err = cli.PostForm(srv.URL+"/organizations/tenseleyFlow/settings/delete", url.Values{
		"confirm_slug": {"TENSELEYFLOW"},
	})
	if err != nil {
		t.Fatalf("POST delete: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("delete status=%d", resp.StatusCode)
	}
	if got := resp.Header.Get("Location"); got != "/settings/organizations" {
		t.Fatalf("delete Location=%q", got)
	}
	org, err = q.GetOrgBySlugIncludingDeleted(ctx, pool, "tenseleyFlow")
	if err != nil {
		t.Fatalf("GetOrgBySlugIncludingDeleted after delete: %v", err)
	}
	if !org.DeletedAt.Valid {
		t.Fatalf("expected org to be soft-deleted")
	}
}

func postOrgAvatar(t *testing.T, cli *http.Client, endpoint string, png []byte) *http.Response {
	t.Helper()
	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	part, err := mw.CreateFormFile("avatar", "avatar.png")
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	if _, err := part.Write(png); err != nil {
		t.Fatalf("write png: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, endpoint, body)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := cli.Do(req)
	if err != nil {
		t.Fatalf("POST avatar: %v", err)
	}
	return resp
}

func makeOrgTestPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 64, 64))
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			img.Set(x, y, color.RGBA{R: 80, G: 30, B: 110, A: 255})
		}
	}
	buf := &bytes.Buffer{}
	if err := png.Encode(buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

func insertOrgAvatarUser(t *testing.T, db orgsdb.DBTX, username string) int64 {
	t.Helper()
	var id int64
	if err := db.QueryRow(context.Background(),
		`INSERT INTO users (username, password_hash) VALUES ($1, $2) RETURNING id`,
		username,
		"$argon2id$v=19$m=16384,t=1,p=1$AAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
	).Scan(&id); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	return id
}

func insertOrgAvatarOrg(t *testing.T, db orgsdb.DBTX, userID int64, slug string) int64 {
	t.Helper()
	var orgID int64
	if err := db.QueryRow(context.Background(),
		`INSERT INTO orgs (slug, display_name, created_by_user_id)
		 VALUES ($1, $2, $3)
		 RETURNING id`,
		slug, slug, userID,
	).Scan(&orgID); err != nil {
		t.Fatalf("insert org: %v", err)
	}
	if _, err := db.Exec(context.Background(),
		`INSERT INTO org_members (org_id, user_id, role) VALUES ($1, $2, 'owner')`,
		orgID, userID,
	); err != nil {
		t.Fatalf("insert org member: %v", err)
	}
	return orgID
}
