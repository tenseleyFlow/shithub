// SPDX-License-Identifier: AGPL-3.0-or-later

package repos_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/tenseleyFlow/shithub/internal/repos"
)

func TestValidateName_Accepts(t *testing.T) {
	t.Parallel()
	for _, name := range []string{
		"a", "abc", "rust-by-example", "name.with.dots", "name_under",
		"a1.b2-c3_d4", "letter9", strings.Repeat("a", 100),
	} {
		if err := repos.ValidateName(name); err != nil {
			t.Errorf("ValidateName(%q): %v", name, err)
		}
	}
}

func TestValidateName_Rejects(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		want error
		why  string
	}{
		{"", repos.ErrInvalidName, "empty"},
		{strings.Repeat("a", 101), repos.ErrInvalidName, "too long"},
		{"UPPER", repos.ErrInvalidName, "uppercase"},
		{".dotfile", repos.ErrInvalidName, "leading dot"},
		{"-dash", repos.ErrInvalidName, "leading dash"},
		{"trailing-", repos.ErrInvalidName, "trailing dash"},
		{"trailing.", repos.ErrInvalidName, "trailing dot"},
		{"two..dots", repos.ErrInvalidName, "dot-dot"},
		{"a@b", repos.ErrInvalidName, "punctuation"},
		{"with space", repos.ErrInvalidName, "space"},
		{"café", repos.ErrInvalidName, "non-ASCII"},
		{".git", repos.ErrInvalidName, "leading dot beats reserved"},
		{"head", repos.ErrReservedName, "reserved (head)"},
		{"refs", repos.ErrReservedName, "reserved (refs)"},
		{"objects", repos.ErrReservedName, "reserved (objects)"},
		{"hooks", repos.ErrReservedName, "reserved (hooks)"},
	}
	for _, c := range cases {
		err := repos.ValidateName(c.name)
		if err == nil {
			t.Errorf("%s: expected error", c.why)
			continue
		}
		if !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want sentinel %v", c.why, err, c.want)
		}
	}
}

func TestValidateDescription(t *testing.T) {
	t.Parallel()
	if err := repos.ValidateDescription(""); err != nil {
		t.Errorf("empty desc: %v", err)
	}
	if err := repos.ValidateDescription(strings.Repeat("a", 350)); err != nil {
		t.Errorf("at-cap desc: %v", err)
	}
	if err := repos.ValidateDescription(strings.Repeat("a", 351)); err == nil {
		t.Errorf("over-cap desc should error")
	}
}
