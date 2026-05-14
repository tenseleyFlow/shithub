// SPDX-License-Identifier: AGPL-3.0-or-later

package annotations

import (
	"strings"
	"testing"
)

func TestParseChunkWarningCommand(t *testing.T) {
	got := ParseChunk([]byte("::warning file=cmd/main.go,line=12,col=3,endLine=14,endColumn=9,title=Build%3A slow::took %252C too long\n"), 7, nil)
	if len(got) != 1 {
		t.Fatalf("annotations=%#v", got)
	}
	ann := got[0]
	if ann.Level != LevelWarning || ann.Path != "cmd/main.go" || ann.StartLine != 12 || ann.StartColumn != 3 || ann.EndLine != 14 || ann.EndColumn != 9 {
		t.Fatalf("unexpected annotation: %#v", ann)
	}
	if ann.Title != "Build: slow" || ann.Message != "took %2C too long" || ann.LogLine != 1 || ann.LogChunkSeq != 7 {
		t.Fatalf("unexpected text/meta: %#v", ann)
	}
	if len(ann.Fingerprint) != 64 {
		t.Fatalf("fingerprint=%q", ann.Fingerprint)
	}
}

func TestParseChunkRedactsAndSanitizesFields(t *testing.T) {
	got := ParseChunk([]byte("::error file=secret/path,title=\x1b[31mhunter2::token hunter2 leaked\a\n"), 0, []string{"hunter2"})
	if len(got) != 1 {
		t.Fatalf("annotations=%#v", got)
	}
	ann := got[0]
	for _, field := range []string{ann.Title, ann.Message, ann.Path} {
		if strings.Contains(field, "hunter2") || strings.ContainsRune(field, '\x1b') || strings.ContainsRune(field, '\a') {
			t.Fatalf("field was not scrubbed: %#v", ann)
		}
	}
	if ann.Title != "***" || ann.Message != "token *** leaked" || ann.Path != "secret/path" {
		t.Fatalf("unexpected scrubbed values: %#v", ann)
	}
}

func TestParseChunkSkipsTrailingPartialLine(t *testing.T) {
	got := ParseChunk([]byte("::warning::partial secret hun"), 0, []string{"hunter2"})
	if len(got) != 0 {
		t.Fatalf("partial command parsed: %#v", got)
	}
}

func TestParseChunkCapsFields(t *testing.T) {
	msg := strings.Repeat("x", MaxMsgBytes+100)
	got := ParseChunk([]byte("::notice file="+strings.Repeat("p", MaxPathBytes+100)+"::"+msg+"\n"), 0, nil)
	if len(got) != 1 {
		t.Fatalf("annotations=%#v", got)
	}
	if len(got[0].Message) > MaxMsgBytes || len(got[0].Path) > MaxPathBytes {
		t.Fatalf("fields not capped: message=%d path=%d", len(got[0].Message), len(got[0].Path))
	}
}
