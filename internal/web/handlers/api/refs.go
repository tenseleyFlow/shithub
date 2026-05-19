// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"context"
	"errors"
	"fmt"

	repogit "github.com/tenseleyFlow/shithub/internal/repos/git"
)

// validateRepoRefExists returns nil when `ref` resolves in the repo's
// git dir, or when `ref` is empty (no filter requested). A missing
// ref returns a 422-shaped error so list endpoints can reject typos
// instead of silently returning empty/unfiltered.
//
// G5 (F15/F2-2): pre-fix `pr list --base BOGUS` returned silent-empty
// and `--head BOGUS` returned silent-all (the latter pre-G1; post-G1
// it post-filters to empty). Both are misleading — gh's behavior
// when a base/head ref doesn't exist is an explicit error. Fail open
// when the storage layer can't supply a gitDir (test contexts with
// no RepoFS) so existing test fixtures keep working.
func (h *Handlers) validateRepoRefExists(ctx context.Context, ownerLogin, repoName, ref string) error {
	if ref == "" {
		return nil
	}
	if h.d.RepoFS == nil {
		return nil
	}
	gitDir, err := h.d.RepoFS.RepoPath(ownerLogin, repoName)
	if err != nil {
		// Storage error — fail open rather than block the read.
		return nil
	}
	if _, err := repogit.ResolveRefOID(ctx, gitDir, ref); err != nil {
		if errors.Is(err, repogit.ErrRefNotFound) {
			return fmt.Errorf("ref %q not found in this repo", ref)
		}
		// Unexpected git error — fail open. We'd rather over-list than
		// 500 on a list query.
		return nil
	}
	return nil
}
