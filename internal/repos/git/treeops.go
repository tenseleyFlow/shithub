// SPDX-License-Identifier: AGPL-3.0-or-later

package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sort"
	"strconv"
	"strings"
)

// ListRefs runs `git for-each-ref` and returns every ref under
// refs/heads/ and refs/tags/. Used by the ref-resolver to do
// longest-prefix matching against URLs like /tree/feature/x/sub/dir.
//
// Returns separate lists so callers can fork on UX (branches vs tags).
type RefListing struct {
	Branches []RefEntry // refs/heads/<name>
	Tags     []RefEntry // refs/tags/<name>
}

// RefEntry is one ref from for-each-ref.
type RefEntry struct {
	Name string // short name (without refs/heads/ or refs/tags/)
	OID  string // 40-char hex sha
}

// ListRefs enumerates branches and tags. Empty repos return empty
// slices, not an error.
func ListRefs(ctx context.Context, gitDir string) (RefListing, error) {
	ctx, cancel := readCtx(ctx)
	defer cancel()
	cmd := gitCmd(ctx, "-C", gitDir,
		"for-each-ref", "--format=%(refname)\x1f%(objectname)",
		"refs/heads/", "refs/tags/")
	out, err := cmd.Output()
	if err != nil {
		return RefListing{}, wrapExecErr(err)
	}
	var rl RefListing
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		if line == "" {
			continue
		}
		name, oid, ok := strings.Cut(line, "\x1f")
		if !ok {
			continue
		}
		switch {
		case strings.HasPrefix(name, "refs/heads/"):
			rl.Branches = append(rl.Branches, RefEntry{Name: strings.TrimPrefix(name, "refs/heads/"), OID: oid})
		case strings.HasPrefix(name, "refs/tags/"):
			rl.Tags = append(rl.Tags, RefEntry{Name: strings.TrimPrefix(name, "refs/tags/"), OID: oid})
		}
	}
	sort.Slice(rl.Branches, func(i, j int) bool { return rl.Branches[i].Name < rl.Branches[j].Name })
	sort.Slice(rl.Tags, func(i, j int) bool { return rl.Tags[i].Name < rl.Tags[j].Name })
	return rl, nil
}

// ResolveRef takes the URL segments after /tree/ or /blob/ and finds
// the longest-prefix match against the supplied refs. Hex-SHA shortcut:
// if the first segment is exactly 40 hex chars, treat it as a SHA. The
// returned `path` is the joined remainder after the matched ref (no
// leading slash).
//
// Example: refs = ["main", "feature/x"], URL = ["feature", "x", "sub", "f.go"]
// → ref="feature/x", path="sub/f.go".
func ResolveRef(refs []string, segments []string) (ref, path string, ok bool) {
	if len(segments) == 0 {
		return "", "", false
	}
	// Hex-SHA shortcut.
	if len(segments[0]) == 40 && isHex(segments[0]) {
		return segments[0], strings.Join(segments[1:], "/"), true
	}
	// Longest prefix wins. Sort the refs by descending length so the
	// first match is the longest.
	candidates := append([]string(nil), refs...)
	sort.Slice(candidates, func(i, j int) bool { return len(candidates[i]) > len(candidates[j]) })
	joined := strings.Join(segments, "/")
	for _, r := range candidates {
		if joined == r {
			return r, "", true
		}
		if strings.HasPrefix(joined, r+"/") {
			return r, strings.TrimPrefix(joined, r+"/"), true
		}
	}
	return "", "", false
}

func isHex(s string) bool {
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f', c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}

// TreeEntryKind is one of the four shapes git tree entries take.
type TreeEntryKind string

const (
	EntryTree    TreeEntryKind = "tree"
	EntryBlob    TreeEntryKind = "blob"
	EntrySubmod  TreeEntryKind = "commit" // a "commit" entry in a tree is a submodule pointer
	EntrySymlink TreeEntryKind = "symlink"
)

// TreeEntry is one row from `git ls-tree --long --full-tree`.
type TreeEntry struct {
	Kind TreeEntryKind
	Mode string // 100644, 100755, 040000, 160000, 120000
	OID  string
	Size int64  // -1 when N/A (trees, submodules)
	Name string // basename relative to the listed path
}

// TreeBlob is one blob returned by a recursive tree walk.
type TreeBlob struct {
	Path string
	Size int64
}

// TreeFile is one regular file-like entry returned by a recursive tree
// walk. It includes regular blobs and symlinks, but excludes trees and
// submodule commit entries.
type TreeFile struct {
	Path string
	Size int64
}

// LsTree lists entries at <ref>:<path>. Empty path lists the repo root.
// Returns an empty slice when the path doesn't exist or is itself a
// blob — callers should fall back to BlobInfo.
func LsTree(ctx context.Context, gitDir, ref, path string) ([]TreeEntry, error) {
	ctx, cancel := readCtx(ctx)
	defer cancel()
	target := ref + ":" + path
	if path == "" {
		target = ref + ":"
	}
	cmd := gitCmd(ctx, "-C", gitDir,
		"ls-tree", "--long", "--full-tree", target)
	out, err := cmd.Output()
	if err != nil {
		// `fatal: not a tree object` for a blob path; surface a typed err.
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			stderr := string(ee.Stderr)
			if strings.Contains(stderr, "Not a valid object name") || strings.Contains(stderr, "not a tree") {
				return nil, ErrNotATree
			}
		}
		return nil, wrapExecErr(err)
	}

	entries := make([]TreeEntry, 0, 32)
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		if line == "" {
			continue
		}
		// Format: "<mode> <type> <oid> <size>\t<name>"
		// Size is "-" for trees and submodules.
		tabIdx := strings.IndexByte(line, '\t')
		if tabIdx < 0 {
			continue
		}
		left, name := line[:tabIdx], line[tabIdx+1:]
		fields := strings.Fields(left)
		if len(fields) != 4 {
			continue
		}
		var size int64 = -1
		if fields[3] != "-" {
			size, _ = strconv.ParseInt(fields[3], 10, 64)
		}
		kind := classifyEntry(fields[0], fields[1])
		entries = append(entries, TreeEntry{
			Kind: kind, Mode: fields[0], OID: fields[2], Size: size, Name: name,
		})
	}
	// Spec: directories first, then files alphabetically.
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].Kind == EntryTree && entries[j].Kind != EntryTree {
			return true
		}
		if entries[i].Kind != EntryTree && entries[j].Kind == EntryTree {
			return false
		}
		return entries[i].Name < entries[j].Name
	})
	return entries, nil
}

// ListBlobs returns every blob under ref with its git-reported byte
// size. It is the recursive counterpart to LsTree and intentionally
// keeps only blobs because callers use it for file-level summaries such
// as language bars.
func ListBlobs(ctx context.Context, gitDir, ref string) ([]TreeBlob, error) {
	ctx, cancel := readCtx(ctx)
	defer cancel()
	cmd := gitCmd(ctx, "-C", gitDir,
		"ls-tree", "-r", "--long", "--full-tree", "-z", ref)
	out, err := cmd.Output()
	if err != nil {
		return nil, wrapExecErr(err)
	}

	records := bytes.Split(bytes.TrimRight(out, "\x00"), []byte{0})
	blobs := make([]TreeBlob, 0, len(records))
	for _, rec := range records {
		if len(rec) == 0 {
			continue
		}
		tabIdx := bytes.IndexByte(rec, '\t')
		if tabIdx < 0 {
			continue
		}
		left, name := string(rec[:tabIdx]), string(rec[tabIdx+1:])
		fields := strings.Fields(left)
		if len(fields) != 4 || classifyEntry(fields[0], fields[1]) != EntryBlob {
			continue
		}
		size, err := strconv.ParseInt(fields[3], 10, 64)
		if err != nil {
			continue
		}
		blobs = append(blobs, TreeBlob{Path: name, Size: size})
	}
	sort.Slice(blobs, func(i, j int) bool { return blobs[i].Path < blobs[j].Path })
	return blobs, nil
}

// ListFiles returns every regular file-like path under ref with its
// git-reported byte size. Unlike ListBlobs, it keeps symlinks because
// code search should still index the symlink path.
func ListFiles(ctx context.Context, gitDir, ref string) ([]TreeFile, error) {
	ctx, cancel := readCtx(ctx)
	defer cancel()
	cmd := gitCmd(ctx, "-C", gitDir,
		"ls-tree", "-r", "--long", "--full-tree", "-z", ref)
	out, err := cmd.Output()
	if err != nil {
		return nil, wrapExecErr(err)
	}

	records := bytes.Split(bytes.TrimRight(out, "\x00"), []byte{0})
	files := make([]TreeFile, 0, len(records))
	for _, rec := range records {
		if len(rec) == 0 {
			continue
		}
		tabIdx := bytes.IndexByte(rec, '\t')
		if tabIdx < 0 {
			continue
		}
		left, name := string(rec[:tabIdx]), string(rec[tabIdx+1:])
		fields := strings.Fields(left)
		if len(fields) != 4 {
			continue
		}
		kind := classifyEntry(fields[0], fields[1])
		if kind != EntryBlob && kind != EntrySymlink {
			continue
		}
		size, err := strconv.ParseInt(fields[3], 10, 64)
		if err != nil {
			continue
		}
		files = append(files, TreeFile{Path: name, Size: size})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, nil
}

// classifyEntry maps git's mode+type fields to our four kinds.
// Symlinks come in with mode 120000 type=blob; we surface them as
// symlink so the UI can avoid Reading them.
func classifyEntry(mode, gitType string) TreeEntryKind {
	if mode == "120000" {
		return EntrySymlink
	}
	switch gitType {
	case "tree":
		return EntryTree
	case "commit":
		return EntrySubmod
	default:
		return EntryBlob
	}
}

// ErrNotATree is returned by LsTree when the requested path turns out
// to be a blob (so the caller should fall through to BlobInfo).
var ErrNotATree = errors.New("git: not a tree")

// ErrNotABlob is the inverse — used by ReadBlob.
var ErrNotABlob = errors.New("git: not a blob")

// ErrPathNotFound is for paths that don't exist on the ref.
var ErrPathNotFound = errors.New("git: path not found")

// BlobInfo is the result of `git cat-file -e -p` style introspection.
type BlobInfo struct {
	OID  string
	Size int64
}

// StatPath returns the kind + OID + size for `<ref>:<path>`. Used by
// the handler to decide whether to render tree or blob without a
// second round-trip.
func StatPath(ctx context.Context, gitDir, ref, path string) (kind TreeEntryKind, oid string, size int64, err error) {
	ctx, cancel := readCtx(ctx)
	defer cancel()
	target := ref + ":" + path
	if path == "" {
		target = ref + ":"
	}
	// `git cat-file -t <ref>:<path>` returns the type.
	tCmd := gitCmd(ctx, "-C", gitDir, "cat-file", "-t", target)
	tOut, tErr := tCmd.Output()
	if tErr != nil {
		var ee *exec.ExitError
		if errors.As(tErr, &ee) {
			stderr := string(ee.Stderr)
			if strings.Contains(stderr, "Not a valid object name") ||
				strings.Contains(stderr, "does not exist") ||
				strings.Contains(stderr, "fatal: path") {
				return "", "", 0, ErrPathNotFound
			}
		}
		return "", "", 0, wrapExecErr(tErr)
	}
	gitType := strings.TrimSpace(string(tOut))

	switch gitType {
	case "tree":
		return EntryTree, "", -1, nil
	case "commit":
		// commit object referenced from a tree → submodule pointer.
		return EntrySubmod, "", -1, nil
	case "blob":
	default:
		return "", "", 0, fmt.Errorf("git: unexpected type %q", gitType)
	}

	sCmd := gitCmd(ctx, "-C", gitDir, "cat-file", "-s", target)
	sOut, err := sCmd.Output()
	if err != nil {
		return "", "", 0, wrapExecErr(err)
	}
	sz, err := strconv.ParseInt(strings.TrimSpace(string(sOut)), 10, 64)
	if err != nil {
		return "", "", 0, fmt.Errorf("git: parse size: %w", err)
	}
	return EntryBlob, "", sz, nil
}

// ReadBlobBytes reads the entire blob at `<ref>:<path>`. Caller-imposed
// max-size limit is the right guard — git itself doesn't bound the
// stream. Pass 0 for "no cap"; otherwise an oversize read returns
// ErrBlobTooLarge.
func ReadBlobBytes(ctx context.Context, gitDir, ref, path string, maxBytes int64) ([]byte, error) {
	ctx, cancel := readCtx(ctx)
	target := ref + ":" + path
	cmd := gitCmd(ctx, "-C", gitDir, "cat-file", "-p", target)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, err
	}
	// Cancel BEFORE Wait: when the LimitReader stops short of the blob
	// (an oversize file), git is still blocked writing into a full
	// pipe and Wait alone would never return. Cancelling kills it, then
	// Wait reaps.
	defer func() { cancel(); _ = cmd.Wait() }()
	var r io.Reader = stdout
	if maxBytes > 0 {
		// LimitReader so giant blobs don't OOM us.
		r = io.LimitReader(stdout, maxBytes+1)
	}
	body, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	if maxBytes > 0 && int64(len(body)) > maxBytes {
		return body[:maxBytes], ErrBlobTooLarge
	}
	return body, nil
}

// StreamBlob writes the blob bytes to w. For raw downloads we never
// buffer; this lets the response stream as `git cat-file -p` produces.
func StreamBlob(ctx context.Context, gitDir, ref, path string, w io.Writer) error {
	target := ref + ":" + path
	cmd := gitCmd(ctx, "-C", gitDir, "cat-file", "-p", target)
	cmd.Stdout = w
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git cat-file: %w (%s)", err, stderr.String())
	}
	return nil
}

// ErrBlobTooLarge is returned when a maxBytes cap is hit on ReadBlobBytes.
var ErrBlobTooLarge = errors.New("git: blob exceeds size cap")

// ListAllPaths runs `git ls-tree -r --name-only` and returns every
// blob path under the ref. Used by the "Go to file" finder. Filters
// out submodule-style entries (commit type) which shouldn't surface
// in the file finder.
func ListAllPaths(ctx context.Context, gitDir, ref string) ([]string, error) {
	ctx, cancel := readCtx(ctx)
	defer cancel()
	cmd := gitCmd(ctx, "-C", gitDir,
		"ls-tree", "-r", "--full-tree", "--name-only", ref)
	out, err := cmd.Output()
	if err != nil {
		return nil, wrapExecErr(err)
	}
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	out2 := make([]string, 0, len(lines))
	for _, l := range lines {
		if l == "" {
			continue
		}
		out2 = append(out2, l)
	}
	return out2, nil
}
