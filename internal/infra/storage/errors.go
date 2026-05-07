// SPDX-License-Identifier: AGPL-3.0-or-later

package storage

import "errors"

var (
	// ErrNotFound is returned by Get/Stat for absent keys and by reposfs
	// helpers when a path doesn't exist.
	ErrNotFound = errors.New("storage: not found")

	// ErrPreconditionFailed is returned by Put when an If-None-Match check fails.
	ErrPreconditionFailed = errors.New("storage: precondition failed")

	// ErrInvalidPath is returned by RepoPath (and any helper that takes
	// owner/repo names) for inputs that violate the whitelist.
	ErrInvalidPath = errors.New("storage: invalid path")

	// ErrAlreadyExists is returned by Move and InitBare when the destination
	// is already populated. Lets callers distinguish a race from corruption.
	ErrAlreadyExists = errors.New("storage: already exists")

	// ErrEscapesRoot is returned by Delete and Move when a path resolves
	// outside the configured storage root. Hard fail — never silently ignore.
	ErrEscapesRoot = errors.New("storage: path escapes root")
)
