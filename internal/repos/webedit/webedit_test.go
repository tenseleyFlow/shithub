// SPDX-License-Identifier: AGPL-3.0-or-later

package webedit

import (
	"errors"
	"testing"
)

func TestValidateFilePath(t *testing.T) {
	t.Parallel()
	valid := []string{
		"README.md",
		"docs/CONTRIBUTING.md",
		".github/workflows/ci.yml",
		"space name/file.txt",
	}
	for _, p := range valid {
		if err := ValidateFilePath(p); err != nil {
			t.Errorf("ValidateFilePath(%q) = %v, want nil", p, err)
		}
	}

	invalid := []string{
		"",
		"/README.md",
		"docs/",
		"docs//README.md",
		"../README.md",
		"docs/../README.md",
		"docs/./README.md",
		`docs\README.md`,
		"bad\x00name",
	}
	for _, p := range invalid {
		if err := ValidateFilePath(p); !errors.Is(err, ErrInvalidPath) {
			t.Errorf("ValidateFilePath(%q) = %v, want ErrInvalidPath", p, err)
		}
	}
}

func TestDefaultMessage(t *testing.T) {
	t.Parallel()
	cases := []struct {
		op     Op
		source string
		target string
		files  []File
		want   string
	}{
		{op: OpEdit, source: "README.md", want: "Update README.md"},
		{op: OpCreate, target: "docs/usage.md", want: "Create docs/usage.md"},
		{op: OpRename, source: "old.md", target: "new.md", want: "Rename old.md to new.md"},
		{op: OpDelete, source: "SECURITY.md", want: "Delete SECURITY.md"},
		{op: OpUpload, files: []File{{Path: "asset.png"}}, want: "Upload asset.png"},
		{op: OpUpload, files: []File{{Path: "a"}, {Path: "b"}}, want: "Upload files"},
	}
	for _, c := range cases {
		if got := DefaultMessage(c.op, c.source, c.target, c.files); got != c.want {
			t.Errorf("DefaultMessage(%q) = %q, want %q", c.op, got, c.want)
		}
	}
}

func TestIsBinary(t *testing.T) {
	t.Parallel()
	if IsBinary([]byte("plain text\n")) {
		t.Fatal("text detected as binary")
	}
	if !IsBinary([]byte{'h', 'i', 0, 'x'}) {
		t.Fatal("NUL-containing content was not detected as binary")
	}
}

func TestValidBranchNameRejectsDetachedInputs(t *testing.T) {
	t.Parallel()
	if !validBranchName("feature/editor-ui") {
		t.Fatal("branch with slash rejected")
	}
	if validBranchName("0123456789abcdef0123456789abcdef01234567") {
		t.Fatal("40-hex detached oid accepted as branch")
	}
	if validBranchName("release.lock") {
		t.Fatal(".lock branch accepted")
	}
}
