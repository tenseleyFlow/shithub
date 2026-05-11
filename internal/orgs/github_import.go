// SPDX-License-Identifier: AGPL-3.0-or-later

package orgs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/auth/secretbox"
	orgsdb "github.com/tenseleyFlow/shithub/internal/orgs/sqlc"
	"github.com/tenseleyFlow/shithub/internal/worker"
)

const GitHubHost = "github.com"

const (
	ImportStatusQueued      = "queued"
	ImportStatusDiscovering = "discovering"
	ImportStatusImporting   = "importing"
	ImportStatusCompleted   = "completed"
	ImportStatusFailed      = "failed"

	ImportRepoStatusQueued    = "queued"
	ImportRepoStatusImporting = "importing"
	ImportRepoStatusImported  = "imported"
	ImportRepoStatusSkipped   = "skipped"
	ImportRepoStatusFailed    = "failed"
)

var (
	ErrInvalidGitHubOrg     = errors.New("orgs: invalid GitHub organization")
	ErrImportTokenKeyNeeded = errors.New("orgs: import token encryption key is not configured")
)

var githubOrgRE = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,99})$`)

// ImportDeps wires org-import orchestration.
type ImportDeps struct {
	Pool   *pgxpool.Pool
	Box    *secretbox.Box
	Logger *slog.Logger
}

// StartGitHubImportParams describes a single org import request.
type StartGitHubImportParams struct {
	OrgID             int64
	SourceOrg         string
	RequestedByUserID int64
	Token             string
}

// StartGitHubImport persists a GitHub import request and enqueues discovery.
func StartGitHubImport(ctx context.Context, deps ImportDeps, p StartGitHubImportParams) (orgsdb.OrgGithubImport, error) {
	sourceOrg, err := NormalizeGitHubOrg(p.SourceOrg)
	if err != nil {
		return orgsdb.OrgGithubImport{}, err
	}
	token := strings.TrimSpace(p.Token)
	var ciphertext, nonce []byte
	tokenPresent := token != ""
	if tokenPresent {
		if deps.Box == nil {
			return orgsdb.OrgGithubImport{}, ErrImportTokenKeyNeeded
		}
		ciphertext, nonce, err = deps.Box.Seal([]byte(token))
		if err != nil {
			return orgsdb.OrgGithubImport{}, fmt.Errorf("github import: seal token: %w", err)
		}
	}

	tx, err := deps.Pool.Begin(ctx)
	if err != nil {
		return orgsdb.OrgGithubImport{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()

	q := orgsdb.New()
	row, err := q.CreateOrgGithubImport(ctx, tx, orgsdb.CreateOrgGithubImportParams{
		OrgID:             p.OrgID,
		SourceOrg:         sourceOrg,
		RequestedByUserID: pgtype.Int8{Int64: p.RequestedByUserID, Valid: p.RequestedByUserID != 0},
		IncludePrivate:    tokenPresent,
		TokenPresent:      tokenPresent,
		TokenCiphertext:   ciphertext,
		TokenNonce:        nonce,
	})
	if err != nil {
		return orgsdb.OrgGithubImport{}, fmt.Errorf("github import: create: %w", err)
	}
	if _, err := worker.Enqueue(ctx, tx, worker.KindOrgGitHubImportDiscover, map[string]any{
		"import_id": row.ID,
	}, worker.EnqueueOptions{}); err != nil {
		return orgsdb.OrgGithubImport{}, err
	}
	if err := worker.Notify(ctx, tx); err != nil && deps.Logger != nil {
		deps.Logger.WarnContext(ctx, "github import: notify", "error", err, "import_id", row.ID)
	}
	if err := tx.Commit(ctx); err != nil {
		return orgsdb.OrgGithubImport{}, err
	}
	committed = true
	return row, nil
}

func NormalizeGitHubOrg(raw string) (string, error) {
	org := strings.TrimSpace(raw)
	org = strings.TrimPrefix(org, "https://github.com/")
	org = strings.TrimPrefix(org, "http://github.com/")
	org = strings.Trim(org, "/")
	if org == "" || strings.Contains(org, "/") || !githubOrgRE.MatchString(org) {
		return "", ErrInvalidGitHubOrg
	}
	return org, nil
}

func DecryptGitHubImportToken(row orgsdb.OrgGithubImport, box *secretbox.Box) (string, error) {
	if len(row.TokenCiphertext) == 0 && len(row.TokenNonce) == 0 {
		return "", nil
	}
	if box == nil {
		return "", ErrImportTokenKeyNeeded
	}
	pt, err := box.Open(row.TokenCiphertext, row.TokenNonce)
	if err != nil {
		return "", fmt.Errorf("github import: decrypt token: %w", err)
	}
	return string(pt), nil
}

type GitHubClient struct {
	HTTPClient *http.Client
	BaseURL    string
	UserAgent  string
}

type GitHubRepo struct {
	ID            int64
	Name          string
	FullName      string
	CloneURL      string
	Description   string
	DefaultBranch string
	Private       bool
	Fork          bool
}

func (c GitHubClient) ListOrgRepos(ctx context.Context, org, token string) ([]GitHubRepo, error) {
	org, err := NormalizeGitHubOrg(org)
	if err != nil {
		return nil, err
	}
	base := strings.TrimRight(c.BaseURL, "/")
	if base == "" {
		base = "https://api.github.com"
	}
	client := c.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	token = strings.TrimSpace(token)
	repoType := "public"
	if token != "" {
		repoType = "all"
	}
	var out []GitHubRepo
	for page := 1; page <= 100; page++ {
		u, err := url.Parse(base + "/orgs/" + url.PathEscape(org) + "/repos")
		if err != nil {
			return nil, err
		}
		q := u.Query()
		q.Set("type", repoType)
		q.Set("per_page", "100")
		q.Set("page", strconv.Itoa(page))
		q.Set("sort", "full_name")
		u.RawQuery = q.Encode()

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		req.Header.Set("User-Agent", userAgent(c.UserAgent))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		repos, err := decodeGitHubRepos(resp)
		if err != nil {
			return nil, err
		}
		out = append(out, repos...)
		if len(repos) < 100 {
			return out, nil
		}
	}
	return nil, fmt.Errorf("github import: too many repositories in %s", org)
}

func decodeGitHubRepos(resp *http.Response) ([]GitHubRepo, error) {
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := strings.TrimSpace(string(body))
		if msg == "" {
			msg = resp.Status
		}
		return nil, fmt.Errorf("github import: GitHub API returned %s: %s", resp.Status, msg)
	}
	var payload []struct {
		ID            int64   `json:"id"`
		Name          string  `json:"name"`
		FullName      string  `json:"full_name"`
		CloneURL      string  `json:"clone_url"`
		Description   *string `json:"description"`
		DefaultBranch string  `json:"default_branch"`
		Private       bool    `json:"private"`
		Fork          bool    `json:"fork"`
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(&payload); err != nil {
		return nil, err
	}
	out := make([]GitHubRepo, 0, len(payload))
	for _, r := range payload {
		desc := ""
		if r.Description != nil {
			desc = strings.TrimSpace(*r.Description)
		}
		out = append(out, GitHubRepo{
			ID:            r.ID,
			Name:          r.Name,
			FullName:      r.FullName,
			CloneURL:      r.CloneURL,
			Description:   desc,
			DefaultBranch: strings.TrimSpace(r.DefaultBranch),
			Private:       r.Private,
			Fork:          r.Fork,
		})
	}
	return out, nil
}

func userAgent(custom string) string {
	custom = strings.TrimSpace(custom)
	if custom != "" {
		return custom
	}
	return "shithub"
}

func IsTerminalImportStatus(status string) bool {
	return status == ImportStatusCompleted || status == ImportStatusFailed
}

func IsTerminalImportRepoStatus(status string) bool {
	return status == ImportRepoStatusImported || status == ImportRepoStatusSkipped || status == ImportRepoStatusFailed
}

func IgnoreNoRows(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	return err
}
