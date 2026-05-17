// SPDX-License-Identifier: AGPL-3.0-or-later

// Package packages owns repository package metadata. The blob bytes live in
// object storage; rows here provide repository-scoped listing, download, and
// quota accounting.
package packages

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	packagesdb "github.com/tenseleyFlow/shithub/internal/packages/sqlc"
)

const (
	PackageTypeGeneric  = "generic"
	MaxPackageFileBytes = 512 * 1024 * 1024
)

var (
	ErrPoolRequired      = errors.New("packages: pool is required")
	ErrRepoIDRequired    = errors.New("packages: repo id is required")
	ErrPackageIDRequired = errors.New("packages: package id is required")
	ErrFileIDRequired    = errors.New("packages: file id is required")
	ErrInvalidName       = errors.New("packages: invalid package name")
	ErrInvalidVersion    = errors.New("packages: invalid package version")
	ErrInvalidFilename   = errors.New("packages: invalid package filename")
	ErrInvalidFileSize   = errors.New("packages: invalid package file size")
	ErrInvalidObjectKey  = errors.New("packages: invalid object key")
	ErrUnsupportedType   = errors.New("packages: unsupported package type")
	ErrPackageFileExists = errors.New("packages: file already exists for package version")
	ErrPackageNotFound   = errors.New("packages: package not found")
	ErrFileNotFound      = errors.New("packages: package file not found")
)

var (
	nameRE     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	versionRE  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]*$`)
	filenameRE = regexp.MustCompile(`^[^/:\x00]+$`)
)

type Deps struct {
	Pool *pgxpool.Pool
}

type (
	RepoPackage        = packagesdb.RepoPackage
	RepoPackageFile    = packagesdb.RepoPackageFile
	RepoPackageVersion = packagesdb.RepoPackageVersion
	ListPackageRow     = packagesdb.ListRepoPackagesRow
	ListVersionRow     = packagesdb.ListRepoPackageVersionsRow
	ListFileRow        = packagesdb.ListRepoPackageFilesRow
	GetFileRow         = packagesdb.GetRepoPackageFileRow
)

type PublishInput struct {
	RepoID      int64
	Name        string
	Version     string
	Description string
	Filename    string
	ObjectKey   string
	ContentType string
	SizeBytes   int64
	ETag        string
	ActorUserID int64
	PackageType string
}

type PublishResult struct {
	Package RepoPackage
	Version RepoPackageVersion
	File    RepoPackageFile
}

func ListRepoPackages(ctx context.Context, deps Deps, repoID int64) ([]ListPackageRow, error) {
	if err := validateDeps(deps); err != nil {
		return nil, err
	}
	if repoID == 0 {
		return nil, ErrRepoIDRequired
	}
	return packagesdb.New().ListRepoPackages(ctx, deps.Pool, repoID)
}

func ListRepoPackageVersions(ctx context.Context, deps Deps, repoID, packageID int64) ([]ListVersionRow, error) {
	if err := validateDeps(deps); err != nil {
		return nil, err
	}
	if repoID == 0 {
		return nil, ErrRepoIDRequired
	}
	if packageID == 0 {
		return nil, ErrPackageIDRequired
	}
	return packagesdb.New().ListRepoPackageVersions(ctx, deps.Pool, packagesdb.ListRepoPackageVersionsParams{
		RepoID:    repoID,
		PackageID: packageID,
	})
}

func ListRepoPackageFiles(ctx context.Context, deps Deps, repoID, packageID int64) ([]ListFileRow, error) {
	if err := validateDeps(deps); err != nil {
		return nil, err
	}
	if repoID == 0 {
		return nil, ErrRepoIDRequired
	}
	if packageID == 0 {
		return nil, ErrPackageIDRequired
	}
	return packagesdb.New().ListRepoPackageFiles(ctx, deps.Pool, packagesdb.ListRepoPackageFilesParams{
		RepoID:    repoID,
		PackageID: packageID,
	})
}

func GetRepoPackageFile(ctx context.Context, deps Deps, repoID, fileID int64) (GetFileRow, error) {
	if err := validateDeps(deps); err != nil {
		return GetFileRow{}, err
	}
	if repoID == 0 {
		return GetFileRow{}, ErrRepoIDRequired
	}
	if fileID == 0 {
		return GetFileRow{}, ErrFileIDRequired
	}
	row, err := packagesdb.New().GetRepoPackageFile(ctx, deps.Pool, packagesdb.GetRepoPackageFileParams{
		RepoID: repoID,
		FileID: fileID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return GetFileRow{}, ErrFileNotFound
	}
	return row, err
}

func PublishFile(ctx context.Context, deps Deps, in PublishInput) (PublishResult, error) {
	if err := validateDeps(deps); err != nil {
		return PublishResult{}, err
	}
	if err := validatePublishInput(in); err != nil {
		return PublishResult{}, err
	}
	tx, err := deps.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return PublishResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	q := packagesdb.New()
	pkg, err := q.UpsertRepoPackage(ctx, tx, packagesdb.UpsertRepoPackageParams{
		RepoID:      in.RepoID,
		Name:        in.Name,
		PackageType: packageType(in.PackageType),
		Description: strings.TrimSpace(in.Description),
		UserID:      optionalUserID(in.ActorUserID),
	})
	if err != nil {
		return PublishResult{}, err
	}
	version, err := q.EnsureRepoPackageVersion(ctx, tx, packagesdb.EnsureRepoPackageVersionParams{
		PackageID: pkg.ID,
		Version:   in.Version,
		UserID:    optionalUserID(in.ActorUserID),
	})
	if err != nil {
		return PublishResult{}, err
	}
	file, err := q.InsertRepoPackageFile(ctx, tx, packagesdb.InsertRepoPackageFileParams{
		VersionID:   version.ID,
		Filename:    in.Filename,
		ObjectKey:   in.ObjectKey,
		ContentType: contentType(in.ContentType),
		SizeBytes:   in.SizeBytes,
		Etag:        strings.TrimSpace(in.ETag),
		UserID:      optionalUserID(in.ActorUserID),
	})
	if err != nil {
		if isUniqueViolation(err) {
			return PublishResult{}, ErrPackageFileExists
		}
		return PublishResult{}, err
	}
	version, err = q.RefreshRepoPackageVersionStats(ctx, tx, version.ID)
	if err != nil {
		return PublishResult{}, err
	}
	pkg, err = q.RefreshRepoPackageStats(ctx, tx, pkg.ID)
	if err != nil {
		return PublishResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return PublishResult{}, err
	}
	return PublishResult{Package: pkg, Version: version, File: file}, nil
}

func DeleteRepoPackage(ctx context.Context, deps Deps, repoID, packageID int64) ([]string, error) {
	if err := validateDeps(deps); err != nil {
		return nil, err
	}
	if repoID == 0 {
		return nil, ErrRepoIDRequired
	}
	if packageID == 0 {
		return nil, ErrPackageIDRequired
	}
	tx, err := deps.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	q := packagesdb.New()
	keys, err := q.ListRepoPackageObjectKeys(ctx, tx, packagesdb.ListRepoPackageObjectKeysParams{
		RepoID:    repoID,
		PackageID: packageID,
	})
	if err != nil {
		return nil, err
	}
	rows, err := q.DeleteRepoPackage(ctx, tx, packagesdb.DeleteRepoPackageParams{
		RepoID:    repoID,
		PackageID: packageID,
	})
	if err != nil {
		return nil, err
	}
	if rows == 0 {
		return nil, ErrPackageNotFound
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return keys, nil
}

func NewObjectKey(repoID int64, packageName, version, filename string) (string, error) {
	if repoID == 0 {
		return "", ErrRepoIDRequired
	}
	if !validName(packageName) {
		return "", ErrInvalidName
	}
	if !validVersion(version) {
		return "", ErrInvalidVersion
	}
	if !validFilename(filename) {
		return "", ErrInvalidFilename
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return fmt.Sprintf("packages/repos/%d/%s/%s/%s/%s", repoID, strings.ToLower(packageName), version, hex.EncodeToString(b[:]), filename), nil
}

func validateDeps(deps Deps) error {
	if deps.Pool == nil {
		return ErrPoolRequired
	}
	return nil
}

func validatePublishInput(in PublishInput) error {
	if in.RepoID == 0 {
		return ErrRepoIDRequired
	}
	if !validName(in.Name) {
		return ErrInvalidName
	}
	if !validVersion(in.Version) {
		return ErrInvalidVersion
	}
	if !validFilename(in.Filename) {
		return ErrInvalidFilename
	}
	if in.SizeBytes < 0 || in.SizeBytes > MaxPackageFileBytes {
		return ErrInvalidFileSize
	}
	if strings.TrimSpace(in.ObjectKey) == "" || len(in.ObjectKey) > 1024 {
		return ErrInvalidObjectKey
	}
	if packageType(in.PackageType) != PackageTypeGeneric {
		return ErrUnsupportedType
	}
	return nil
}

func validName(v string) bool {
	return len(v) >= 1 && len(v) <= 128 && nameRE.MatchString(v)
}

func validVersion(v string) bool {
	return len(v) >= 1 && len(v) <= 128 && versionRE.MatchString(v)
}

func validFilename(v string) bool {
	return len(v) >= 1 && len(v) <= 255 && filenameRE.MatchString(v) && v != "." && v != ".."
}

func packageType(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return PackageTypeGeneric
	}
	return v
}

func contentType(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return "application/octet-stream"
	}
	return v
}

func optionalUserID(id int64) pgtype.Int8 {
	return pgtype.Int8{Int64: id, Valid: id != 0}
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
