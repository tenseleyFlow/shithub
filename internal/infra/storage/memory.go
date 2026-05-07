// SPDX-License-Identifier: AGPL-3.0-or-later

package storage

import (
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec // ETag, not security-sensitive — matches S3 etag derivation.
	"encoding/hex"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"
)

// MemoryStore is an in-process ObjectStore implementation. Used by tests.
// Honors the same If-None-Match semantics as the s3 backend.
type MemoryStore struct {
	mu      sync.RWMutex
	objects map[string]memObject
	// signedURLBase is the prefix of generated SignedURLs so tests can
	// assert their shape. Defaults to "mem://".
	signedURLBase string
}

type memObject struct {
	body         []byte
	etag         string
	contentType  string
	lastModified time.Time
}

// NewMemoryStore constructs an empty in-memory store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		objects:       make(map[string]memObject),
		signedURLBase: "mem://",
	}
}

// Put implements ObjectStore.
func (m *MemoryStore) Put(_ context.Context, key string, body io.Reader, opts PutOpts) (PutResult, error) {
	if key == "" {
		return PutResult{}, fmt.Errorf("storage: put: %w: empty key", ErrInvalidPath)
	}
	buf, err := io.ReadAll(body)
	if err != nil {
		return PutResult{}, fmt.Errorf("storage: put: read body: %w", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if opts.IfNoneMatch == "*" {
		if _, exists := m.objects[key]; exists {
			return PutResult{}, ErrPreconditionFailed
		}
	}
	sum := md5.Sum(buf) //nolint:gosec // not security-sensitive.
	etag := hex.EncodeToString(sum[:])
	m.objects[key] = memObject{
		body:         buf,
		etag:         etag,
		contentType:  opts.ContentType,
		lastModified: time.Now().UTC(),
	}
	return PutResult{ETag: etag, Size: int64(len(buf))}, nil
}

// Get implements ObjectStore.
func (m *MemoryStore) Get(_ context.Context, key string) (io.ReadCloser, ObjectMeta, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	o, ok := m.objects[key]
	if !ok {
		return nil, ObjectMeta{}, ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(o.body)), m.metaOf(key, o), nil
}

// Stat implements ObjectStore.
func (m *MemoryStore) Stat(_ context.Context, key string) (ObjectMeta, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	o, ok := m.objects[key]
	if !ok {
		return ObjectMeta{}, ErrNotFound
	}
	return m.metaOf(key, o), nil
}

// Delete implements ObjectStore. Idempotent.
func (m *MemoryStore) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objects, key)
	return nil
}

// List implements ObjectStore. ContinuationToken is the last key returned
// in the previous page; results are sorted lexicographically.
func (m *MemoryStore) List(_ context.Context, prefix string, opts ListOpts) (ListResult, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	keys := make([]string, 0, len(m.objects))
	for k := range m.objects {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)

	if opts.ContinuationToken != "" {
		i := sort.SearchStrings(keys, opts.ContinuationToken)
		if i < len(keys) && keys[i] == opts.ContinuationToken {
			i++
		}
		keys = keys[i:]
	}

	maxKeys := opts.MaxKeys
	if maxKeys <= 0 {
		maxKeys = 1000
	}

	var (
		out      []ObjectMeta
		prefixes []string
	)
	seenPrefix := map[string]struct{}{}

	for _, k := range keys {
		if !opts.Recursive {
			rest := strings.TrimPrefix(k, prefix)
			if i := strings.Index(rest, "/"); i >= 0 {
				cp := prefix + rest[:i+1]
				if _, ok := seenPrefix[cp]; !ok {
					seenPrefix[cp] = struct{}{}
					prefixes = append(prefixes, cp)
				}
				continue
			}
		}
		o := m.objects[k]
		out = append(out, m.metaOf(k, o))
		if len(out) >= maxKeys {
			break
		}
	}

	res := ListResult{Objects: out, CommonPrefixes: prefixes}
	if len(out) >= maxKeys && len(out) > 0 {
		res.IsTruncated = true
		res.NextContinuationToken = out[len(out)-1].Key
	}
	return res, nil
}

// SignedURL implements ObjectStore. Tests can rely on the prefix to
// distinguish memory-backed URLs from real ones.
func (m *MemoryStore) SignedURL(_ context.Context, key string, ttl time.Duration, method string) (string, error) {
	switch method {
	case "GET", "PUT":
	default:
		return "", fmt.Errorf("storage: signed url: unsupported method %q", method)
	}
	if key == "" {
		return "", fmt.Errorf("storage: signed url: %w: empty key", ErrInvalidPath)
	}
	return fmt.Sprintf("%s%s?method=%s&ttl=%s", m.signedURLBase, key, method, ttl), nil
}

func (m *MemoryStore) metaOf(key string, o memObject) ObjectMeta {
	return ObjectMeta{
		Key:          key,
		Size:         int64(len(o.body)),
		ETag:         o.etag,
		ContentType:  o.contentType,
		LastModified: o.lastModified,
	}
}
