// SPDX-License-Identifier: AGPL-3.0-or-later

// Package runnerlabels normalizes Actions runner labels for registration and
// heartbeat matching.
package runnerlabels

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

var labelRE = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// ParseCSV parses a comma-separated label list.
func ParseCSV(raw string) ([]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return []string{}, nil
	}
	return Normalize(strings.Split(raw, ","))
}

// Normalize trims, validates, and de-duplicates labels while preserving order.
func Normalize(labels []string) ([]string, error) {
	if len(labels) == 0 {
		return []string{}, nil
	}
	seen := make(map[string]struct{}, len(labels))
	out := make([]string, 0, len(labels))
	for _, label := range labels {
		label = strings.TrimSpace(label)
		if label == "" {
			return nil, errors.New("runner labels must not contain empty entries")
		}
		if len(label) > 100 || !labelRE.MatchString(label) {
			return nil, fmt.Errorf("invalid runner label %q", label)
		}
		if _, ok := seen[label]; ok {
			continue
		}
		seen[label] = struct{}{}
		out = append(out, label)
	}
	return out, nil
}
