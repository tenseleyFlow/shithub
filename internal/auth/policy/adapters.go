// SPDX-License-Identifier: AGPL-3.0-or-later

package policy

import (
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
)

// NewRepoRefFromRepo converts a reposdb.Repo into a policy RepoRef.
// Used at the boundary between data-access and policy code: any caller
// that already has a Repo row passes it in, and policy never imports
// reposdb at the rule layer.
func NewRepoRefFromRepo(r reposdb.Repo) RepoRef {
	ref := RepoRef{
		ID:         r.ID,
		Visibility: string(r.Visibility),
		IsArchived: r.IsArchived,
		IsPaused:   r.IsPaused,
		IsDeleted:  r.DeletedAt.Valid,
	}
	if r.OwnerUserID.Valid {
		ref.OwnerUserID = r.OwnerUserID.Int64
	}
	if r.OwnerOrgID.Valid {
		ref.OwnerOrgID = r.OwnerOrgID.Int64
	}
	return ref
}
