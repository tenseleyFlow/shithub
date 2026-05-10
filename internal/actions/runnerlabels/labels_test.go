// SPDX-License-Identifier: AGPL-3.0-or-later

package runnerlabels_test

import (
	"reflect"
	"testing"

	"github.com/tenseleyFlow/shithub/internal/actions/runnerlabels"
)

func TestParseCSV(t *testing.T) {
	got, err := runnerlabels.ParseCSV(" self-hosted, linux,linux,ubuntu-24.04 ")
	if err != nil {
		t.Fatalf("ParseCSV: %v", err)
	}
	want := []string{"self-hosted", "linux", "ubuntu-24.04"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("labels: got %#v, want %#v", got, want)
	}
}

func TestNormalizeRejectsInvalidLabels(t *testing.T) {
	for _, labels := range [][]string{
		{"linux", ""},
		{"has space"},
		{"semi;colon"},
	} {
		if _, err := runnerlabels.Normalize(labels); err == nil {
			t.Fatalf("Normalize(%#v) returned nil error", labels)
		}
	}
}
