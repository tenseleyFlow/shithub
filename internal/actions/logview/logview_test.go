// SPDX-License-Identifier: AGPL-3.0-or-later

package logview

import "testing"

func TestParseGroupsAndAnchors(t *testing.T) {
	t.Parallel()
	doc := Parse("before\n::group::Build <tag>\ninside\n::endgroup::\nafter\n")
	if doc.LineCount != 5 {
		t.Fatalf("LineCount=%d", doc.LineCount)
	}
	if !doc.HasGroups() {
		t.Fatal("expected group")
	}
	if len(doc.Nodes) != 3 {
		t.Fatalf("root nodes=%d", len(doc.Nodes))
	}
	if got := doc.Nodes[0].Line; got.Number != 1 || got.Anchor != "L1" || got.Text != "before" {
		t.Fatalf("line 1 = %+v", got)
	}
	group := doc.Nodes[1].Group
	if group == nil {
		t.Fatalf("node 2 is not a group: %+v", doc.Nodes[1])
	}
	if group.Number != 2 || group.Anchor != "L2" || group.Title != "Build <tag>" || !group.Closed {
		t.Fatalf("group = %+v", group)
	}
	if len(group.Children) != 1 {
		t.Fatalf("group children=%d", len(group.Children))
	}
	if got := group.Children[0].Line; got.Number != 3 || got.Anchor != "L3" || got.Text != "inside" {
		t.Fatalf("group line = %+v", got)
	}
	if got := doc.Nodes[2].Line; got.Number != 5 || got.Anchor != "L5" || got.Text != "after" {
		t.Fatalf("after line = %+v", got)
	}
}

func TestParseUnmatchedEndGroupStaysPlain(t *testing.T) {
	t.Parallel()
	doc := Parse("::endgroup::\nnext\n")
	if doc.HasGroups() {
		t.Fatal("unexpected group")
	}
	if got := doc.Nodes[0].Line; got.Number != 1 || got.Text != "::endgroup::" {
		t.Fatalf("unmatched endgroup = %+v", got)
	}
}

func TestParseUnclosedAndNestedGroups(t *testing.T) {
	t.Parallel()
	doc := Parse("::group::outer\none\n::group::inner\nmasked ***\n::endgroup::\n")
	if len(doc.Nodes) != 1 {
		t.Fatalf("root nodes=%d", len(doc.Nodes))
	}
	outer := doc.Nodes[0].Group
	if outer == nil || outer.Closed {
		t.Fatalf("outer group = %+v", outer)
	}
	if len(outer.Children) != 2 {
		t.Fatalf("outer children=%d", len(outer.Children))
	}
	inner := outer.Children[1].Group
	if inner == nil || !inner.Closed {
		t.Fatalf("inner group = %+v", inner)
	}
	if got := inner.Children[0].Line.Text; got != "masked ***" {
		t.Fatalf("masked line changed: %q", got)
	}
}

func TestParseKeepsEscapableTextAsText(t *testing.T) {
	t.Parallel()
	doc := Parse("::group::<script>alert(1)</script>\n<svg onload=alert(1)>\n::endgroup::")
	group := doc.Nodes[0].Group
	if group.Title != "<script>alert(1)</script>" {
		t.Fatalf("title changed: %q", group.Title)
	}
	if got := group.Children[0].Line.Text; got != "<svg onload=alert(1)>" {
		t.Fatalf("body changed: %q", got)
	}
}
