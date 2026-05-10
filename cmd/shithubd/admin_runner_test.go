// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"reflect"
	"testing"
)

func TestParseRunnerLabels(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want []string
	}{
		{name: "empty", raw: "", want: []string{}},
		{name: "trim and dedupe", raw: " self-hosted, linux,linux,ubuntu-24.04 ", want: []string{"self-hosted", "linux", "ubuntu-24.04"}},
		{name: "underscore dot dash", raw: "gpu_cuda-12.4", want: []string{"gpu_cuda-12.4"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseRunnerLabels(tt.raw)
			if err != nil {
				t.Fatalf("parseRunnerLabels: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("labels: got %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestParseRunnerLabelsRejectsInvalid(t *testing.T) {
	tests := []string{
		"linux,",
		"linux,,x64",
		"has space",
		"semi;colon",
	}
	for _, raw := range tests {
		t.Run(raw, func(t *testing.T) {
			if _, err := parseRunnerLabels(raw); err == nil {
				t.Fatal("parseRunnerLabels returned nil error")
			}
		})
	}
}
