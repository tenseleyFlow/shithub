// SPDX-License-Identifier: AGPL-3.0-or-later

package repo

import (
	"context"

	"github.com/tenseleyFlow/shithub/internal/repos"
	repogit "github.com/tenseleyFlow/shithub/internal/repos/git"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
)

func (h *Handlers) saveRepoSourceRemote(ctx context.Context, repoID int64, rawURL string) (string, error) {
	return repos.SaveSourceRemote(ctx, h.sourceRemoteDeps(""), repoID, rawURL)
}

func (h *Handlers) fetchRepoSourceRemote(ctx context.Context, row reposdb.Repo, ownerSlug, remoteURL string) error {
	return repos.FetchSourceRemote(ctx, h.sourceRemoteDeps(""), row, ownerSlug, remoteURL)
}

func chooseFetchedDefaultBranch(current string, branches []repogit.RefEntry) (name, oid string) {
	return repos.ChooseFetchedDefaultBranch(current, branches)
}

func (h *Handlers) markRepoSourceRemoteFetchError(ctx context.Context, repoID int64, err error) {
	repos.MarkSourceRemoteFetchError(ctx, h.sourceRemoteDeps(""), repoID, err)
}

func isInvalidSourceRemote(err error) bool {
	return repos.IsInvalidSourceRemote(err)
}

func (h *Handlers) sourceRemoteDeps(token string) repos.SourceRemoteDeps {
	return repos.SourceRemoteDeps{
		Pool:       h.d.Pool,
		RepoFS:     h.d.RepoFS,
		Logger:     h.d.Logger,
		FetchToken: token,
	}
}
