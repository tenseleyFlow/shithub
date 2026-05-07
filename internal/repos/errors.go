// SPDX-License-Identifier: AGPL-3.0-or-later

// Package repos owns repository creation, validation, and identity. It
// sits above sqlc-generated reposdb queries and orchestrates the
// DB + filesystem dance that produces a usable bare repo. The git
// plumbing helpers used to build the optional initial commit live in
// internal/repos/git.
package repos

import "errors"

// User-facing errors. Handlers map these to friendly UI strings; lower
// layers wrap them with %w so handlers can errors.Is for routing.
var (
	// ErrInvalidName is returned when the requested repo name fails the
	// shape whitelist (length, characters, edges, dot-dot, etc.).
	ErrInvalidName = errors.New("repos: invalid name")

	// ErrReservedName is returned when the name is on the reserved list.
	// Distinguished from ErrInvalidName so the UI can say "reserved" vs
	// "format" — they're different fixes for the user.
	ErrReservedName = errors.New("repos: reserved name")

	// ErrTaken is returned when an active repo (deleted_at IS NULL)
	// already exists for this owner with the same name. Surfaces as
	// either the explicit pre-check OR the unique-constraint error.
	ErrTaken = errors.New("repos: name taken for owner")

	// ErrNoVerifiedEmail is returned when the user has no verified
	// primary email. We refuse rather than fabricate an author identity.
	ErrNoVerifiedEmail = errors.New("repos: no verified primary email")

	// ErrUnknownLicense / ErrUnknownGitignore — picker chose a key not
	// in our curated set.
	ErrUnknownLicense   = errors.New("repos: unknown license key")
	ErrUnknownGitignore = errors.New("repos: unknown gitignore key")

	// ErrDescriptionTooLong mirrors the DB CHECK on description (≤350).
	ErrDescriptionTooLong = errors.New("repos: description too long")
)
