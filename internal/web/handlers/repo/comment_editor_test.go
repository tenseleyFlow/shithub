// SPDX-License-Identifier: AGPL-3.0-or-later

package repo

import (
	"strings"
	"testing"
)

func TestCommentEditorConfigJSONEscapesScriptBreakout(t *testing.T) {
	t.Parallel()

	got := string(commentEditorConfigJSON(commentEditorConfig{
		Mentions: []commentEditorMention{{
			Username:    "alice",
			DisplayName: `</script><script>alert(1)</script>`,
		}},
	}))

	if strings.Contains(got, "</script>") {
		t.Fatalf("config JSON contains raw script terminator: %s", got)
	}
	if !strings.Contains(got, `\u003c/script\u003e`) {
		t.Fatalf("config JSON did not preserve escaped display name: %s", got)
	}
}

func TestCommentEditorAvatarURLPathEscapesUsername(t *testing.T) {
	t.Parallel()

	got := commentEditorAvatarURL("team/user")
	if got != "/avatars/team%2Fuser" {
		t.Fatalf("avatar URL = %q, want escaped path segment", got)
	}
}
