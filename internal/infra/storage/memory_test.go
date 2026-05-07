// SPDX-License-Identifier: AGPL-3.0-or-later

package storage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func TestMemoryStore_PutGetStat(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	m := NewMemoryStore()

	res, err := m.Put(ctx, "k1", strings.NewReader("hello"), PutOpts{ContentType: "text/plain"})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if res.Size != 5 || res.ETag == "" {
		t.Fatalf("unexpected put result: %+v", res)
	}

	rc, meta, err := m.Get(ctx, "k1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer func() { _ = rc.Close() }()
	body, _ := io.ReadAll(rc)
	if string(body) != "hello" {
		t.Fatalf("body = %q, want hello", body)
	}
	if meta.ContentType != "text/plain" || meta.Size != 5 {
		t.Fatalf("meta = %+v", meta)
	}

	stat, err := m.Stat(ctx, "k1")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if stat.ETag != res.ETag {
		t.Fatalf("etag mismatch: %s vs %s", stat.ETag, res.ETag)
	}
}

func TestMemoryStore_GetMissing(t *testing.T) {
	t.Parallel()
	m := NewMemoryStore()
	if _, _, err := m.Get(context.Background(), "absent"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestMemoryStore_PutIfNoneMatch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	m := NewMemoryStore()

	if _, err := m.Put(ctx, "k", strings.NewReader("first"), PutOpts{}); err != nil {
		t.Fatalf("first put: %v", err)
	}
	_, err := m.Put(ctx, "k", strings.NewReader("second"), PutOpts{IfNoneMatch: "*"})
	if !errors.Is(err, ErrPreconditionFailed) {
		t.Fatalf("expected ErrPreconditionFailed, got %v", err)
	}

	// Without IfNoneMatch, overwrite succeeds.
	if _, err := m.Put(ctx, "k", strings.NewReader("third"), PutOpts{}); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	rc, _, _ := m.Get(ctx, "k")
	defer func() { _ = rc.Close() }()
	body, _ := io.ReadAll(rc)
	if string(body) != "third" {
		t.Fatalf("got %q, want third", body)
	}
}

func TestMemoryStore_DeleteIdempotent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	m := NewMemoryStore()
	if err := m.Delete(ctx, "missing"); err != nil {
		t.Fatalf("delete missing: %v", err)
	}
	_, _ = m.Put(ctx, "k", strings.NewReader("x"), PutOpts{})
	if err := m.Delete(ctx, "k"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := m.Stat(ctx, "k"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("post-delete stat = %v, want ErrNotFound", err)
	}
}

func TestMemoryStore_ListRecursiveAndDelimited(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	m := NewMemoryStore()
	for _, k := range []string{
		"avatars/alice/64.png",
		"avatars/alice/128.png",
		"avatars/bob/64.png",
		"attachments/issue-1/x.txt",
	} {
		if _, err := m.Put(ctx, k, strings.NewReader("x"), PutOpts{}); err != nil {
			t.Fatalf("seed %s: %v", k, err)
		}
	}

	rec, err := m.List(ctx, "avatars/", ListOpts{Recursive: true})
	if err != nil {
		t.Fatalf("recursive list: %v", err)
	}
	if len(rec.Objects) != 3 {
		t.Fatalf("recursive: got %d objects, want 3", len(rec.Objects))
	}

	del, err := m.List(ctx, "avatars/", ListOpts{})
	if err != nil {
		t.Fatalf("delimited list: %v", err)
	}
	if len(del.CommonPrefixes) != 2 {
		t.Fatalf("delimited: got %d common prefixes, want 2: %v", len(del.CommonPrefixes), del.CommonPrefixes)
	}
}

func TestMemoryStore_LargeRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	m := NewMemoryStore()
	body := bytes.Repeat([]byte{0xcd}, 5*1024*1024) // 5 MiB
	if _, err := m.Put(ctx, "big", bytes.NewReader(body), PutOpts{ContentLength: int64(len(body))}); err != nil {
		t.Fatalf("Put big: %v", err)
	}
	rc, meta, err := m.Get(ctx, "big")
	if err != nil {
		t.Fatalf("Get big: %v", err)
	}
	defer func() { _ = rc.Close() }()
	got, _ := io.ReadAll(rc)
	if !bytes.Equal(got, body) {
		t.Fatalf("body mismatch (len got=%d want=%d)", len(got), len(body))
	}
	if meta.Size != int64(len(body)) {
		t.Fatalf("meta size = %d, want %d", meta.Size, len(body))
	}
}

func TestMemoryStore_SignedURL(t *testing.T) {
	t.Parallel()
	m := NewMemoryStore()
	u, err := m.SignedURL(context.Background(), "k1", time.Minute, "GET")
	if err != nil {
		t.Fatalf("SignedURL: %v", err)
	}
	if !strings.HasPrefix(u, "mem://k1") {
		t.Fatalf("unexpected url: %s", u)
	}
	if _, err := m.SignedURL(context.Background(), "k1", time.Minute, "POST"); err == nil {
		t.Fatal("expected error for unsupported method")
	}
}

func TestQuota(t *testing.T) {
	t.Parallel()
	q := Quota{Used: 100, Limit: 1000}
	if q.Available() != 900 {
		t.Fatalf("Available = %d, want 900", q.Available())
	}
	if q.WouldExceed(800) {
		t.Fatal("WouldExceed(800) = true, want false")
	}
	if !q.WouldExceed(901) {
		t.Fatal("WouldExceed(901) = false, want true")
	}
	unlimited := Quota{Used: 1 << 40}
	if unlimited.Available() != -1 {
		t.Fatalf("unlimited Available = %d, want -1", unlimited.Available())
	}
	if unlimited.WouldExceed(1 << 50) {
		t.Fatal("unlimited WouldExceed = true, want false")
	}
}
