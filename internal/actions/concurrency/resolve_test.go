// SPDX-License-Identifier: AGPL-3.0-or-later

package concurrency_test

import (
	"strings"
	"testing"

	"github.com/tenseleyFlow/shithub/internal/actions/concurrency"
)

func TestResolveGroup_EvaluatesTriggerContext(t *testing.T) {
	t.Parallel()
	got, err := concurrency.ResolveGroup("branch-${{ shithub.ref }}-${{ shithub.event.head_commit.id }}", concurrency.EvalContext{
		EventPayload: map[string]any{
			"head_commit": map[string]any{"id": "abc123"},
		},
		HeadSHA: "abc123",
		HeadRef: "refs/heads/trunk",
	})
	if err != nil {
		t.Fatalf("ResolveGroup: %v", err)
	}
	want := "branch-refs/heads/trunk-abc123"
	if got != want {
		t.Fatalf("group: got %q want %q", got, want)
	}
}

func TestResolveGroup_GithubAliasAndMissingEventPath(t *testing.T) {
	t.Parallel()
	got, err := concurrency.ResolveGroup("${{ github.ref }}-${{ shithub.event.missing }}", concurrency.EvalContext{
		HeadRef: "refs/heads/main",
	})
	if err != nil {
		t.Fatalf("ResolveGroup: %v", err)
	}
	if got != "refs/heads/main-" {
		t.Fatalf("group: got %q", got)
	}
}

func TestResolveGroup_RejectsTooLongGroup(t *testing.T) {
	t.Parallel()
	_, err := concurrency.ResolveGroup(strings.Repeat("x", concurrency.MaxGroupChars+1), concurrency.EvalContext{})
	if err == nil {
		t.Fatal("ResolveGroup returned nil error for oversized group")
	}
}
