// SPDX-License-Identifier: AGPL-3.0-or-later

package repos

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/infra/storage"
	repogit "github.com/tenseleyFlow/shithub/internal/repos/git"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/security/ssrf"
	"github.com/tenseleyFlow/shithub/internal/worker"
)

const MaxSourceRemoteURLLen = 2048
const SourceRemoteFetchTimeout = 45 * time.Second

var ErrInvalidSourceRemote = errors.New("repos: invalid source remote URL")

// NormalizeSourceRemoteURL validates and canonicalizes the public Git
// remote URL shithub is allowed to fetch from for source imports and
// submodule commit backfills. Credentials are deliberately not allowed
// here; private import credentials need a separate secret-backed design.
func NormalizeSourceRemoteURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	if len(raw) > MaxSourceRemoteURLLen {
		return "", fmt.Errorf("%w: too long", ErrInvalidSourceRemote)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("%w: malformed URL", ErrInvalidSourceRemote)
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	default:
		return "", fmt.Errorf("%w: source imports currently support http(s) git remotes", ErrInvalidSourceRemote)
	}
	if u.Hostname() == "" {
		return "", fmt.Errorf("%w: missing host", ErrInvalidSourceRemote)
	}
	if u.User != nil {
		return "", fmt.Errorf("%w: credentials are not supported in source remote URLs", ErrInvalidSourceRemote)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("%w: query strings and fragments are not supported", ErrInvalidSourceRemote)
	}
	if strings.Trim(u.EscapedPath(), "/") == "" {
		return "", fmt.Errorf("%w: missing repository path", ErrInvalidSourceRemote)
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	return u.String(), nil
}

// ValidateSourceRemoteURL runs the same SSRF defenses used for webhooks
// before a URL is persisted or fetched by git. Git still receives the URL
// as argv (never through a shell), and fetch disables submodule recursion.
func ValidateSourceRemoteURL(ctx context.Context, raw string) (string, error) {
	normalized, err := NormalizeSourceRemoteURL(raw)
	if err != nil || normalized == "" {
		return normalized, err
	}
	if err := ssrf.Default().ValidateWithResolve(ctx, normalized); err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidSourceRemote, err)
	}
	return normalized, nil
}

// SourceRemoteDeps wires source-remote fetches. FetchToken is optional and
// only used for private GitHub imports; it is not stored in repo_source_remotes.
type SourceRemoteDeps struct {
	Pool       *pgxpool.Pool
	RepoFS     *storage.RepoFS
	Logger     *slog.Logger
	FetchToken string
}

// SaveSourceRemote validates and persists the credential-free source remote.
func SaveSourceRemote(ctx context.Context, deps SourceRemoteDeps, repoID int64, rawURL string) (string, error) {
	remoteURL, err := ValidateSourceRemoteURL(ctx, rawURL)
	if err != nil || remoteURL == "" {
		return remoteURL, err
	}
	_, err = reposdb.New().UpsertRepoSourceRemote(ctx, deps.Pool, reposdb.UpsertRepoSourceRemoteParams{
		RepoID:    repoID,
		RemoteUrl: remoteURL,
	})
	return remoteURL, err
}

// FetchSourceRemote imports public heads/tags from a configured source remote
// and updates cached default-branch/index/size state.
func FetchSourceRemote(ctx context.Context, deps SourceRemoteDeps, row reposdb.Repo, ownerSlug, remoteURL string) error {
	remoteURL, err := ValidateSourceRemoteURL(ctx, remoteURL)
	if err != nil {
		MarkSourceRemoteFetchError(ctx, deps, row.ID, err)
		return err
	}
	gitDir, err := deps.RepoFS.RepoPath(ownerSlug, row.Name)
	if err != nil {
		MarkSourceRemoteFetchError(ctx, deps, row.ID, err)
		return err
	}
	fetchCtx, cancel := context.WithTimeout(ctx, SourceRemoteFetchTimeout)
	defer cancel()
	if strings.TrimSpace(deps.FetchToken) != "" {
		err = repogit.FetchRemoteHeadsAndTagsWithToken(fetchCtx, gitDir, remoteURL, deps.FetchToken)
	} else {
		err = repogit.FetchRemoteHeadsAndTags(fetchCtx, gitDir, remoteURL)
	}
	if err != nil {
		MarkSourceRemoteFetchError(ctx, deps, row.ID, err)
		return err
	}
	if err := RefreshFetchedRepoState(ctx, deps, row, gitDir); err != nil {
		MarkSourceRemoteFetchError(ctx, deps, row.ID, err)
		return err
	}
	q := reposdb.New()
	if err := q.MarkRepoSourceRemoteFetched(ctx, deps.Pool, row.ID); err != nil && deps.Logger != nil {
		deps.Logger.WarnContext(ctx, "source-remote: mark fetched", "error", err, "repo_id", row.ID)
	}
	return nil
}

// RefreshFetchedRepoState reconciles the repo row after a source fetch.
func RefreshFetchedRepoState(ctx context.Context, deps SourceRemoteDeps, row reposdb.Repo, gitDir string) error {
	refs, err := repogit.ListRefs(ctx, gitDir)
	if err != nil {
		return err
	}
	branch, oid := ChooseFetchedDefaultBranch(row.DefaultBranch, refs.Branches)
	if branch == "" {
		return nil
	}
	q := reposdb.New()
	if branch != row.DefaultBranch {
		if err := q.UpdateRepoDefaultBranch(ctx, deps.Pool, reposdb.UpdateRepoDefaultBranchParams{
			ID:            row.ID,
			DefaultBranch: branch,
		}); err != nil {
			return err
		}
		if err := repogit.SetSymbolicRef(ctx, gitDir, "HEAD", "refs/heads/"+branch); err != nil && deps.Logger != nil {
			deps.Logger.WarnContext(ctx, "source-remote: set symbolic head", "error", err, "repo_id", row.ID, "branch", branch)
		}
	}
	if !row.DefaultBranchOid.Valid || row.DefaultBranchOid.String != oid {
		if err := q.UpdateRepoDefaultBranchOID(ctx, deps.Pool, reposdb.UpdateRepoDefaultBranchOIDParams{
			ID:               row.ID,
			DefaultBranchOid: pgtype.Text{String: oid, Valid: true},
		}); err != nil {
			return err
		}
		if _, err := worker.Enqueue(ctx, deps.Pool, worker.KindRepoIndexCode, map[string]any{"repo_id": row.ID}, worker.EnqueueOptions{}); err != nil && deps.Logger != nil {
			deps.Logger.WarnContext(ctx, "source-remote: enqueue index", "error", err, "repo_id", row.ID)
		}
	}
	if _, err := worker.Enqueue(ctx, deps.Pool, worker.KindRepoSizeRecalc, map[string]any{"repo_id": row.ID}, worker.EnqueueOptions{}); err != nil && deps.Logger != nil {
		deps.Logger.WarnContext(ctx, "source-remote: enqueue size", "error", err, "repo_id", row.ID)
	}
	_ = worker.Notify(ctx, deps.Pool)
	return nil
}

// ChooseFetchedDefaultBranch mirrors GitHub import behavior: keep the current
// default if present, otherwise prefer trunk/main/master before falling back to
// the first fetched branch.
func ChooseFetchedDefaultBranch(current string, branches []repogit.RefEntry) (name, oid string) {
	if len(branches) == 0 {
		return "", ""
	}
	for _, candidate := range []string{current, "trunk", "main", "master"} {
		if candidate == "" {
			continue
		}
		for _, branch := range branches {
			if branch.Name == candidate {
				return branch.Name, branch.OID
			}
		}
	}
	return branches[0].Name, branches[0].OID
}

// MarkSourceRemoteFetchError stores the latest source-fetch failure without
// leaking credentials; callers pass credential-free remote URLs.
func MarkSourceRemoteFetchError(ctx context.Context, deps SourceRemoteDeps, repoID int64, err error) {
	if err == nil {
		return
	}
	msg := strings.TrimSpace(err.Error())
	if len(msg) > 500 {
		msg = msg[:500]
	}
	if markErr := reposdb.New().MarkRepoSourceRemoteFetchError(ctx, deps.Pool, reposdb.MarkRepoSourceRemoteFetchErrorParams{
		RepoID:    repoID,
		LastError: pgtype.Text{String: msg, Valid: true},
	}); markErr != nil && deps.Logger != nil {
		deps.Logger.WarnContext(ctx, "source-remote: mark fetch error", "error", markErr, "cause", err, "repo_id", repoID)
	}
}

func IsInvalidSourceRemote(err error) bool {
	return errors.Is(err, ErrInvalidSourceRemote)
}
