// SPDX-License-Identifier: AGPL-3.0-or-later

package scrub

import "testing"

func TestScrubber_MasksPlainAndMultilineSecrets(t *testing.T) {
	t.Parallel()
	s := New([]string{"hunter2", "line1\nline2"})
	got := string(s.Scrub([]byte("token=hunter2\nkey=line1\nline2\n"))) + string(s.Flush())
	want := "token=***\nkey=***\n"
	if got != want {
		t.Fatalf("scrubbed:\ngot  %q\nwant %q", got, want)
	}
	if s.Replacements() != 2 {
		t.Fatalf("replacements: got %d, want 2", s.Replacements())
	}
}

func TestScrubber_MasksAcrossChunkBoundary(t *testing.T) {
	t.Parallel()
	s := New([]string{"hunter2"})
	got := string(s.Scrub([]byte("before hun")))
	got += string(s.Scrub([]byte("ter2 after")))
	got += string(s.Flush())
	want := "before *** after"
	if got != want {
		t.Fatalf("scrubbed:\ngot  %q\nwant %q", got, want)
	}
	if s.Replacements() != 1 {
		t.Fatalf("replacements: got %d, want 1", s.Replacements())
	}
}

func TestScrubber_NoSecretsIsCopyingNoop(t *testing.T) {
	t.Parallel()
	s := New(nil)
	in := []byte("hello")
	got := s.Scrub(in)
	in[0] = 'x'
	if string(got) != "hello" {
		t.Fatalf("scrubbed: %q", got)
	}
}
