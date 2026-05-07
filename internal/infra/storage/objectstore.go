// SPDX-License-Identifier: AGPL-3.0-or-later

// Package storage provides shithub's storage abstractions:
//   - ObjectStore: a small interface for S3-compatible object storage
//     (works against MinIO in dev/test and DigitalOcean Spaces in prod).
//   - reposfs: filesystem-backed bare git repository helpers (sharded
//     layout, atomic helpers, strict path validation).
//   - Quota: a placeholder type for disk-quota plumbing (no enforcement
//     yet — wired up in later sprints).
//
// Path validation is the security boundary: every entry point that takes
// owner/repo names from outside the package goes through RepoPath, which
// rejects unsafe inputs against a strict whitelist.
package storage

import (
	"context"
	"io"
	"time"
)

// ObjectStore is the abstract interface every storage backend implements.
// Implementations: s3 (any S3-compatible endpoint via minio-go) and memory
// (in-process map for tests).
type ObjectStore interface {
	// Put writes body to key. Returns the resulting object's metadata
	// (etag, size). Honors opts.IfNoneMatch (when "*", fails with
	// ErrPreconditionFailed if the key already exists).
	Put(ctx context.Context, key string, body io.Reader, opts PutOpts) (PutResult, error)

	// Get returns a reader for the object at key. Caller must Close.
	// Returns ErrNotFound when key is absent.
	Get(ctx context.Context, key string) (io.ReadCloser, ObjectMeta, error)

	// Stat returns metadata for key without fetching the body.
	// Returns ErrNotFound when key is absent.
	Stat(ctx context.Context, key string) (ObjectMeta, error)

	// Delete removes key. Returns nil for a missing key (idempotent).
	Delete(ctx context.Context, key string) error

	// List enumerates objects under prefix. Pagination via opts.ContinuationToken.
	List(ctx context.Context, prefix string, opts ListOpts) (ListResult, error)

	// SignedURL returns a pre-signed URL for the given method ("GET" or
	// "PUT") on key, valid for ttl. The URL grants direct browser/client
	// access without exposing credentials — used for avatar/attachment
	// uploads and large downloads in later sprints.
	SignedURL(ctx context.Context, key string, ttl time.Duration, method string) (string, error)
}

// PutOpts controls a Put.
type PutOpts struct {
	ContentType string
	// IfNoneMatch, when "*", causes Put to fail with ErrPreconditionFailed
	// if the destination already exists. Other values are not supported.
	IfNoneMatch string
	// ContentLength, when > 0, is passed to the backend as a hint. When 0,
	// the backend buffers / streams as needed.
	ContentLength int64
}

// PutResult is what Put returns.
type PutResult struct {
	ETag string
	Size int64
}

// ObjectMeta is the metadata returned by Get/Stat.
type ObjectMeta struct {
	Key          string
	Size         int64
	ETag         string
	ContentType  string
	LastModified time.Time
}

// ListOpts controls a List.
type ListOpts struct {
	// ContinuationToken resumes pagination from a prior page.
	ContinuationToken string
	// MaxKeys caps the page size. Zero means backend default.
	MaxKeys int
	// Recursive, when false, treats "/" as a delimiter and surfaces common
	// prefixes (folders) in ListResult.CommonPrefixes.
	Recursive bool
}

// ListResult is one page of a List.
type ListResult struct {
	Objects               []ObjectMeta
	CommonPrefixes        []string
	NextContinuationToken string
	IsTruncated           bool
}
