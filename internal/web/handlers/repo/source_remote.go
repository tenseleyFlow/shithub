// SPDX-License-Identifier: AGPL-3.0-or-later

package repo

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tenseleyFlow/shithub/internal/repos"
	repogit "github.com/tenseleyFlow/shithub/internal/repos/git"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/worker"
)

const sourceRemoteFetchTimeout = 45 * time.Second

func (h *Handlers) saveRepoSourceRemote(ctx context.Context, repoID int64, rawURL string) (string, error) {
	remoteURL, err := repos.ValidateSourceRemoteURL(ctx, rawURL)
	if err != nil || remoteURL == "" {
		return remoteURL, err
	}
	_, err = h.rq.UpsertRepoSourceRemote(ctx, h.d.Pool, reposdb.UpsertRepoSourceRemoteParams{
		RepoID:    repoID,
		RemoteUrl: remoteURL,
	})
	return remoteURL, err
}

func (h *Handlers) fetchRepoSourceRemote(ctx context.Context, row reposdb.Repo, ownerSlug, remoteURL string) error {
	remoteURL, err := repos.ValidateSourceRemoteURL(ctx, remoteURL)
	if err != nil {
		h.markRepoSourceRemoteFetchError(ctx, row.ID, err)
		return err
	}
	gitDir, err := h.d.RepoFS.RepoPath(ownerSlug, row.Name)
	if err != nil {
		h.markRepoSourceRemoteFetchError(ctx, row.ID, err)
		return err
	}
	fetchCtx, cancel := context.WithTimeout(ctx, sourceRemoteFetchTimeout)
	defer cancel()
	if err := repogit.FetchRemoteHeadsAndTags(fetchCtx, gitDir, remoteURL); err != nil {
		h.markRepoSourceRemoteFetchError(ctx, row.ID, err)
		return err
	}
	if err := h.refreshFetchedRepoState(ctx, row, gitDir); err != nil {
		h.markRepoSourceRemoteFetchError(ctx, row.ID, err)
		return err
	}
	if err := h.rq.MarkRepoSourceRemoteFetched(ctx, h.d.Pool, row.ID); err != nil && h.d.Logger != nil {
		h.d.Logger.WarnContext(ctx, "source-remote: mark fetched", "error", err, "repo_id", row.ID)
	}
	return nil
}

func (h *Handlers) refreshFetchedRepoState(ctx context.Context, row reposdb.Repo, gitDir string) error {
	refs, err := repogit.ListRefs(ctx, gitDir)
	if err != nil {
		return err
	}
	branch, oid := chooseFetchedDefaultBranch(row.DefaultBranch, refs.Branches)
	if branch == "" {
		return nil
	}
	if branch != row.DefaultBranch {
		if err := h.rq.UpdateRepoDefaultBranch(ctx, h.d.Pool, reposdb.UpdateRepoDefaultBranchParams{
			ID:            row.ID,
			DefaultBranch: branch,
		}); err != nil {
			return err
		}
		if err := repogit.SetSymbolicRef(ctx, gitDir, "HEAD", "refs/heads/"+branch); err != nil && h.d.Logger != nil {
			h.d.Logger.WarnContext(ctx, "source-remote: set symbolic head", "error", err, "repo_id", row.ID, "branch", branch)
		}
	}
	if !row.DefaultBranchOid.Valid || row.DefaultBranchOid.String != oid {
		if err := h.rq.UpdateRepoDefaultBranchOID(ctx, h.d.Pool, reposdb.UpdateRepoDefaultBranchOIDParams{
			ID:               row.ID,
			DefaultBranchOid: pgtype.Text{String: oid, Valid: true},
		}); err != nil {
			return err
		}
		if _, err := worker.Enqueue(ctx, h.d.Pool, worker.KindRepoIndexCode, map[string]any{"repo_id": row.ID}, worker.EnqueueOptions{}); err != nil && h.d.Logger != nil {
			h.d.Logger.WarnContext(ctx, "source-remote: enqueue index", "error", err, "repo_id", row.ID)
		}
	}
	if _, err := worker.Enqueue(ctx, h.d.Pool, worker.KindRepoSizeRecalc, map[string]any{"repo_id": row.ID}, worker.EnqueueOptions{}); err != nil && h.d.Logger != nil {
		h.d.Logger.WarnContext(ctx, "source-remote: enqueue size", "error", err, "repo_id", row.ID)
	}
	_ = worker.Notify(ctx, h.d.Pool)
	return nil
}

func chooseFetchedDefaultBranch(current string, branches []repogit.RefEntry) (name, oid string) {
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

func (h *Handlers) markRepoSourceRemoteFetchError(ctx context.Context, repoID int64, err error) {
	if err == nil {
		return
	}
	msg := strings.TrimSpace(err.Error())
	if len(msg) > 500 {
		msg = msg[:500]
	}
	if markErr := h.rq.MarkRepoSourceRemoteFetchError(ctx, h.d.Pool, reposdb.MarkRepoSourceRemoteFetchErrorParams{
		RepoID:    repoID,
		LastError: pgtype.Text{String: msg, Valid: true},
	}); markErr != nil && h.d.Logger != nil {
		h.d.Logger.WarnContext(ctx, "source-remote: mark fetch error", "error", markErr, "cause", err, "repo_id", repoID)
	}
}

func isInvalidSourceRemote(err error) bool {
	return errors.Is(err, repos.ErrInvalidSourceRemote)
}
