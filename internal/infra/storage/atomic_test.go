// SPDX-License-Identifier: AGPL-3.0-or-later

package storage

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteAtomic_HappyPath(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "out.txt")
	body := "hello atomic"
	if err := WriteAtomic(path, strings.NewReader(body)); err != nil {
		t.Fatalf("WriteAtomic: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != body {
		t.Fatalf("got %q, want %q", got, body)
	}
}

// failingReader returns N bytes then an error. Used to inject a failure
// after partial write — WriteAtomic must NOT leave a file at path.
type failingReader struct {
	data    []byte
	off     int
	failAt  int
	failErr error
}

func (f *failingReader) Read(p []byte) (int, error) {
	if f.off >= f.failAt {
		return 0, f.failErr
	}
	n := copy(p, f.data[f.off:])
	if f.off+n > f.failAt {
		n = f.failAt - f.off
	}
	f.off += n
	if f.off >= f.failAt {
		return n, f.failErr
	}
	return n, nil
}

func TestWriteAtomic_PartialWriteLeavesNoFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "should-not-exist.txt")

	r := &failingReader{
		data:    bytes.Repeat([]byte("x"), 1024),
		failAt:  256,
		failErr: errors.New("simulated crash"),
	}
	err := WriteAtomic(path, r)
	if err == nil {
		t.Fatal("expected error from failing reader")
	}

	// Destination must not exist.
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("destination exists after failed atomic write: stat err=%v", err)
	}

	// No leftover .tmp.* files in the same directory either.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp.") {
			t.Fatalf("leftover temp file: %s", e.Name())
		}
	}
}

func TestWriteAtomic_OverwritesExisting(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "out.txt")
	if err := os.WriteFile(path, []byte("old content"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := WriteAtomic(path, strings.NewReader("new content")); err != nil {
		t.Fatalf("WriteAtomic: %v", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "new content" {
		t.Fatalf("got %q, want %q", got, "new content")
	}
}

func TestWriteAtomic_LargeBody(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "big.bin")
	body := bytes.Repeat([]byte{0xab}, 5*1024*1024) // 5 MiB
	if err := WriteAtomic(path, bytes.NewReader(body)); err != nil {
		t.Fatalf("WriteAtomic: %v", err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = f.Close() }()
	h, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(h, body) {
		t.Fatalf("body mismatch (len got=%d want=%d)", len(h), len(body))
	}
}
