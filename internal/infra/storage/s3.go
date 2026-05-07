// SPDX-License-Identifier: AGPL-3.0-or-later

package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// S3Config configures the S3-compatible client. Mirrors config.S3StorageConfig.
type S3Config struct {
	Endpoint        string // host[:port], no scheme
	Region          string
	AccessKeyID     string
	SecretAccessKey string
	Bucket          string
	UseSSL          bool
	ForcePathStyle  bool
}

// S3Store is an ObjectStore backed by any S3-compatible endpoint.
type S3Store struct {
	client *minio.Client
	bucket string
}

// NewS3Store constructs the client. The bucket must already exist; this
// constructor does not create it (deploy/dev scripts seed buckets).
func NewS3Store(cfg S3Config) (*S3Store, error) {
	if cfg.Endpoint == "" {
		return nil, errors.New("storage: s3: endpoint required")
	}
	if cfg.Bucket == "" {
		return nil, errors.New("storage: s3: bucket required")
	}
	mc, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:        credentials.NewStaticV4(cfg.AccessKeyID, cfg.SecretAccessKey, ""),
		Secure:       cfg.UseSSL,
		Region:       cfg.Region,
		BucketLookup: bucketLookup(cfg.ForcePathStyle),
	})
	if err != nil {
		return nil, fmt.Errorf("storage: s3: client: %w", err)
	}
	return &S3Store{client: mc, bucket: cfg.Bucket}, nil
}

func bucketLookup(forcePathStyle bool) minio.BucketLookupType {
	if forcePathStyle {
		return minio.BucketLookupPath
	}
	return minio.BucketLookupDNS
}

// Put implements ObjectStore.
func (s *S3Store) Put(ctx context.Context, key string, body io.Reader, opts PutOpts) (PutResult, error) {
	if opts.IfNoneMatch == "*" {
		if _, err := s.client.StatObject(ctx, s.bucket, key, minio.StatObjectOptions{}); err == nil {
			return PutResult{}, ErrPreconditionFailed
		} else if !isNotFound(err) {
			return PutResult{}, fmt.Errorf("storage: s3: precondition stat: %w", err)
		}
	}
	putOpts := minio.PutObjectOptions{ContentType: opts.ContentType}
	size := opts.ContentLength
	if size <= 0 {
		size = -1
	}
	info, err := s.client.PutObject(ctx, s.bucket, key, body, size, putOpts)
	if err != nil {
		return PutResult{}, fmt.Errorf("storage: s3: put %s: %w", key, err)
	}
	return PutResult{ETag: info.ETag, Size: info.Size}, nil
}

// Get implements ObjectStore.
func (s *S3Store) Get(ctx context.Context, key string) (io.ReadCloser, ObjectMeta, error) {
	obj, err := s.client.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, ObjectMeta{}, fmt.Errorf("storage: s3: get %s: %w", key, err)
	}
	stat, err := obj.Stat()
	if err != nil {
		_ = obj.Close()
		if isNotFound(err) {
			return nil, ObjectMeta{}, ErrNotFound
		}
		return nil, ObjectMeta{}, fmt.Errorf("storage: s3: stat %s: %w", key, err)
	}
	return obj, metaFromStat(key, stat), nil
}

// Stat implements ObjectStore.
func (s *S3Store) Stat(ctx context.Context, key string) (ObjectMeta, error) {
	stat, err := s.client.StatObject(ctx, s.bucket, key, minio.StatObjectOptions{})
	if err != nil {
		if isNotFound(err) {
			return ObjectMeta{}, ErrNotFound
		}
		return ObjectMeta{}, fmt.Errorf("storage: s3: stat %s: %w", key, err)
	}
	return metaFromStat(key, stat), nil
}

// Delete implements ObjectStore. Returns nil for missing keys (idempotent).
func (s *S3Store) Delete(ctx context.Context, key string) error {
	err := s.client.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{})
	if err != nil && !isNotFound(err) {
		return fmt.Errorf("storage: s3: delete %s: %w", key, err)
	}
	return nil
}

// List implements ObjectStore. Recursive=false uses "/" as a delimiter.
func (s *S3Store) List(ctx context.Context, prefix string, opts ListOpts) (ListResult, error) {
	listOpts := minio.ListObjectsOptions{
		Prefix:     prefix,
		Recursive:  opts.Recursive,
		MaxKeys:    opts.MaxKeys,
		StartAfter: opts.ContinuationToken,
	}
	var (
		out      []ObjectMeta
		prefixes []string
	)
	for o := range s.client.ListObjects(ctx, s.bucket, listOpts) {
		if o.Err != nil {
			return ListResult{}, fmt.Errorf("storage: s3: list: %w", o.Err)
		}
		// minio-go signals common prefixes with size==0 and key ending in "/".
		// In delimited mode they appear in the same channel.
		if o.Size == 0 && len(o.Key) > 0 && o.Key[len(o.Key)-1] == '/' {
			prefixes = append(prefixes, o.Key)
			continue
		}
		out = append(out, ObjectMeta{
			Key:          o.Key,
			Size:         o.Size,
			ETag:         o.ETag,
			ContentType:  o.ContentType,
			LastModified: o.LastModified,
		})
		if opts.MaxKeys > 0 && len(out) >= opts.MaxKeys {
			break
		}
	}
	res := ListResult{Objects: out, CommonPrefixes: prefixes}
	if opts.MaxKeys > 0 && len(out) >= opts.MaxKeys && len(out) > 0 {
		res.IsTruncated = true
		res.NextContinuationToken = out[len(out)-1].Key
	}
	return res, nil
}

// SignedURL implements ObjectStore.
func (s *S3Store) SignedURL(ctx context.Context, key string, ttl time.Duration, method string) (string, error) {
	switch method {
	case "GET":
		u, err := s.client.PresignedGetObject(ctx, s.bucket, key, ttl, url.Values{})
		if err != nil {
			return "", fmt.Errorf("storage: s3: presign get: %w", err)
		}
		return u.String(), nil
	case "PUT":
		u, err := s.client.PresignedPutObject(ctx, s.bucket, key, ttl)
		if err != nil {
			return "", fmt.Errorf("storage: s3: presign put: %w", err)
		}
		return u.String(), nil
	default:
		return "", fmt.Errorf("storage: s3: unsupported signed-url method %q", method)
	}
}

func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	var resp minio.ErrorResponse
	if errors.As(err, &resp) {
		switch resp.Code {
		case "NoSuchKey", "NoSuchBucket", "NotFound":
			return true
		}
		if resp.StatusCode == 404 {
			return true
		}
	}
	return false
}

func metaFromStat(key string, s minio.ObjectInfo) ObjectMeta {
	return ObjectMeta{
		Key:          key,
		Size:         s.Size,
		ETag:         s.ETag,
		ContentType:  s.ContentType,
		LastModified: s.LastModified,
	}
}
