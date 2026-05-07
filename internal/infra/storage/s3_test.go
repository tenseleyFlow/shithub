// SPDX-License-Identifier: AGPL-3.0-or-later

package storage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

// s3FromEnv returns an S3Store configured from SHITHUB_TEST_S3_* env vars,
// or skips the test when those aren't set. Tests that exercise the s3
// backend end-to-end run only when CI or a developer has wired up MinIO.
//
//	SHITHUB_TEST_S3_ENDPOINT          (e.g. 127.0.0.1:9000)
//	SHITHUB_TEST_S3_ACCESS_KEY_ID
//	SHITHUB_TEST_S3_SECRET_ACCESS_KEY
//	SHITHUB_TEST_S3_BUCKET            (e.g. shithub-dev)
func s3FromEnv(t *testing.T) *S3Store {
	t.Helper()
	endpoint := os.Getenv("SHITHUB_TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("SHITHUB_TEST_S3_ENDPOINT not set; skipping s3 integration test")
	}
	store, err := NewS3Store(S3Config{
		Endpoint:        endpoint,
		Region:          envOr("SHITHUB_TEST_S3_REGION", "us-east-1"),
		AccessKeyID:     os.Getenv("SHITHUB_TEST_S3_ACCESS_KEY_ID"),
		SecretAccessKey: os.Getenv("SHITHUB_TEST_S3_SECRET_ACCESS_KEY"),
		Bucket:          os.Getenv("SHITHUB_TEST_S3_BUCKET"),
		UseSSL:          false,
		ForcePathStyle:  true,
	})
	if err != nil {
		t.Fatalf("NewS3Store: %v", err)
	}
	return store
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func TestS3Store_RoundTrip(t *testing.T) {
	t.Parallel()
	s := s3FromEnv(t)
	ctx := context.Background()
	key := "test/round-trip-" + time.Now().UTC().Format("20060102-150405.000000000")

	body := strings.NewReader("integration body")
	res, err := s.Put(ctx, key, body, PutOpts{ContentType: "text/plain"})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	t.Cleanup(func() { _ = s.Delete(ctx, key) })

	if res.Size != int64(len("integration body")) {
		t.Fatalf("size = %d, want %d", res.Size, len("integration body"))
	}

	rc, meta, err := s.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer func() { _ = rc.Close() }()
	got, _ := io.ReadAll(rc)
	if string(got) != "integration body" {
		t.Fatalf("body mismatch: got %q", got)
	}
	if meta.Size != res.Size {
		t.Fatalf("meta size mismatch")
	}
}

func TestS3Store_PutIfNoneMatch(t *testing.T) {
	t.Parallel()
	s := s3FromEnv(t)
	ctx := context.Background()
	key := "test/inm-" + time.Now().UTC().Format("20060102-150405.000000000")

	if _, err := s.Put(ctx, key, strings.NewReader("first"), PutOpts{}); err != nil {
		t.Fatalf("first put: %v", err)
	}
	t.Cleanup(func() { _ = s.Delete(ctx, key) })

	_, err := s.Put(ctx, key, strings.NewReader("second"), PutOpts{IfNoneMatch: "*"})
	if !errors.Is(err, ErrPreconditionFailed) {
		t.Fatalf("expected ErrPreconditionFailed, got %v", err)
	}
}

func TestS3Store_LargeRoundTrip(t *testing.T) {
	t.Parallel()
	s := s3FromEnv(t)
	ctx := context.Background()
	key := "test/large-" + time.Now().UTC().Format("20060102-150405.000000000")

	body := bytes.Repeat([]byte{0xab}, 5*1024*1024) // 5 MiB
	if _, err := s.Put(ctx, key, bytes.NewReader(body), PutOpts{ContentLength: int64(len(body))}); err != nil {
		t.Fatalf("Put large: %v", err)
	}
	t.Cleanup(func() { _ = s.Delete(ctx, key) })

	rc, _, err := s.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get large: %v", err)
	}
	defer func() { _ = rc.Close() }()
	got, _ := io.ReadAll(rc)
	if !bytes.Equal(got, body) {
		t.Fatalf("body mismatch (len got=%d want=%d)", len(got), len(body))
	}
}

func TestS3Store_GetMissing(t *testing.T) {
	t.Parallel()
	s := s3FromEnv(t)
	_, _, err := s.Get(context.Background(), "test/should-not-exist-xyz-"+time.Now().UTC().Format("150405.000"))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}
