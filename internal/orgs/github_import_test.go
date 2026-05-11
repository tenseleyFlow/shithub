// SPDX-License-Identifier: AGPL-3.0-or-later

package orgs_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/tenseleyFlow/shithub/internal/auth/secretbox"
	"github.com/tenseleyFlow/shithub/internal/orgs"
	"github.com/tenseleyFlow/shithub/internal/worker"
)

func TestNormalizeGitHubOrg(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		raw     string
		want    string
		wantErr bool
	}{
		{name: "bare", raw: "tenseleyFlow", want: "tenseleyFlow"},
		{name: "url", raw: "https://github.com/FortranGoingOnForty/", want: "FortranGoingOnForty"},
		{name: "path rejected", raw: "github.com/owner/repo", wantErr: true},
		{name: "trailing hyphen rejected", raw: "bad-", wantErr: true},
		{name: "double hyphen rejected", raw: "bad--name", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := orgs.NormalizeGitHubOrg(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("NormalizeGitHubOrg(%q) succeeded", tt.raw)
				}
				return
			}
			if err != nil {
				t.Fatalf("NormalizeGitHubOrg(%q): %v", tt.raw, err)
			}
			if got != tt.want {
				t.Fatalf("NormalizeGitHubOrg(%q)=%q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

func TestGitHubClientListOrgReposPaginatesAndUsesToken(t *testing.T) {
	t.Parallel()
	var seenPages []string
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if got, want := r.URL.Path, "/orgs/tenseleyFlow/repos"; got != want {
			t.Fatalf("path=%q, want %q", got, want)
		}
		if got, want := r.Header.Get("Authorization"), "Bearer test-token"; got != want {
			t.Fatalf("Authorization=%q, want %q", got, want)
		}
		if got, want := r.URL.Query().Get("type"), "all"; got != want {
			t.Fatalf("type=%q, want %q", got, want)
		}
		seenPages = append(seenPages, r.URL.Query().Get("page"))
		var body strings.Builder
		if r.URL.Query().Get("page") == "1" {
			writeGitHubRepoList(t, &body, 100)
		} else {
			_, _ = io.WriteString(&body, `[{"id":101,"name":"last","full_name":"tenseleyFlow/last","clone_url":"https://github.com/tenseleyFlow/last.git","description":"last repo","default_branch":"trunk"}]`)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body.String())),
			Request:    r,
		}, nil
	})

	repos, err := (orgs.GitHubClient{
		HTTPClient: &http.Client{Transport: rt},
		BaseURL:    "https://api.github.test",
		UserAgent:  "shithub-test",
	}).ListOrgRepos(context.Background(), "tenseleyFlow", "test-token")
	if err != nil {
		t.Fatalf("ListOrgRepos: %v", err)
	}
	if len(repos) != 101 {
		t.Fatalf("len(repos)=%d, want 101", len(repos))
	}
	if got := fmt.Sprint(seenPages); got != "[1 2]" {
		t.Fatalf("pages=%s, want [1 2]", got)
	}
	if repos[100].FullName != "tenseleyFlow/last" || repos[100].Description != "last repo" || repos[100].DefaultBranch != "trunk" {
		t.Fatalf("last repo decoded incorrectly: %+v", repos[100])
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func writeGitHubRepoList(t *testing.T, w io.Writer, count int) {
	t.Helper()
	payload := make([]map[string]any, 0, count)
	for i := 0; i < count; i++ {
		payload = append(payload, map[string]any{
			"id":             i + 1,
			"name":           fmt.Sprintf("repo-%03d", i),
			"full_name":      fmt.Sprintf("tenseleyFlow/repo-%03d", i),
			"clone_url":      fmt.Sprintf("https://github.com/tenseleyFlow/repo-%03d.git", i),
			"description":    nil,
			"default_branch": "trunk",
			"private":        false,
			"fork":           false,
		})
	}
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		t.Fatalf("encode GitHub response: %v", err)
	}
}

func TestStartGitHubImportPersistsEncryptedTokenAndDiscoveryJob(t *testing.T) {
	pool, deps, alice := setup(t)
	row, err := orgs.Create(context.Background(), deps, orgs.CreateParams{
		Slug: "acme", DisplayName: "Acme Inc", CreatedByUserID: alice,
	})
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	key, err := secretbox.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	box, err := secretbox.FromBytes(key)
	if err != nil {
		t.Fatalf("FromBytes: %v", err)
	}
	imp, err := orgs.StartGitHubImport(context.Background(), orgs.ImportDeps{
		Pool:   pool,
		Box:    box,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, orgs.StartGitHubImportParams{
		OrgID: row.ID, SourceOrg: "https://github.com/tenseleyFlow/",
		RequestedByUserID: alice, Token: "ghp_secret",
	})
	if err != nil {
		t.Fatalf("StartGitHubImport: %v", err)
	}
	if imp.SourceOrg != "tenseleyFlow" || !imp.IncludePrivate || !imp.TokenPresent {
		t.Fatalf("unexpected import row: %+v", imp)
	}
	token, err := orgs.DecryptGitHubImportToken(imp, box)
	if err != nil {
		t.Fatalf("DecryptGitHubImportToken: %v", err)
	}
	if token != "ghp_secret" {
		t.Fatalf("decrypted token=%q", token)
	}
	if string(imp.TokenCiphertext) == "ghp_secret" {
		t.Fatal("token stored in plaintext")
	}
	var jobs int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM jobs WHERE kind = $1 AND payload->>'import_id' = $2`,
		worker.KindOrgGitHubImportDiscover, strconv.FormatInt(imp.ID, 10),
	).Scan(&jobs); err != nil {
		t.Fatalf("query jobs: %v", err)
	}
	if jobs != 1 {
		t.Fatalf("discovery jobs=%d, want 1", jobs)
	}
}
