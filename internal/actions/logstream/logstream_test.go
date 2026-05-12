// SPDX-License-Identifier: AGPL-3.0-or-later

package logstream

import "testing"

func TestChannelAndListenSQL(t *testing.T) {
	t.Parallel()
	if got := Channel(42); got != "step_log_42" {
		t.Fatalf("Channel=%q", got)
	}
	if got := ListenSQL(42); got != `LISTEN "step_log_42"` {
		t.Fatalf("ListenSQL=%q", got)
	}
	if got := UnlistenSQL(42); got != `UNLISTEN "step_log_42"` {
		t.Fatalf("UnlistenSQL=%q", got)
	}
}

func TestParsePayload(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		payload  string
		wantSeq  int32
		wantDone bool
		wantOK   bool
	}{
		{name: "chunk", payload: "7", wantSeq: 7, wantOK: true},
		{name: "done", payload: "done", wantDone: true, wantOK: true},
		{name: "trim", payload: " 8 ", wantSeq: 8, wantOK: true},
		{name: "negative", payload: "-1"},
		{name: "invalid", payload: "chunk:1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			gotSeq, gotDone, gotOK := ParsePayload(tt.payload)
			if gotSeq != tt.wantSeq || gotDone != tt.wantDone || gotOK != tt.wantOK {
				t.Fatalf("ParsePayload(%q)=(%d,%v,%v)", tt.payload, gotSeq, gotDone, gotOK)
			}
		})
	}
}
