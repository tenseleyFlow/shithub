// SPDX-License-Identifier: AGPL-3.0-or-later

// Package scrub masks configured secret values from runner log output.
package scrub

import (
	"sort"
	"strings"
)

const Mask = "***"

type Scrubber struct {
	values   []string
	replacer *strings.Replacer
	tail     string
}

func New(values []string) *Scrubber {
	values = normalize(values)
	if len(values) == 0 {
		return &Scrubber{}
	}
	pairs := make([]string, 0, len(values)*2)
	for _, v := range values {
		pairs = append(pairs, v, Mask)
	}
	return &Scrubber{values: values, replacer: strings.NewReplacer(pairs...)}
}

func (s *Scrubber) Scrub(chunk []byte) []byte {
	if s == nil || s.replacer == nil {
		return append([]byte(nil), chunk...)
	}
	combined := s.tail + string(chunk)
	keep := s.pendingSuffixLen(combined)
	if keep == len(combined) {
		s.tail = combined
		return nil
	}
	emit := combined[:len(combined)-keep]
	s.tail = combined[len(combined)-keep:]
	return []byte(s.replacer.Replace(emit))
}

func (s *Scrubber) Flush() []byte {
	if s == nil || s.tail == "" {
		return nil
	}
	tail := s.tail
	s.tail = ""
	if s.replacer == nil {
		return []byte(tail)
	}
	return []byte(s.replacer.Replace(tail))
}

func normalize(values []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(values))
	for _, v := range values {
		if v == "" {
			continue
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool {
		return len(out[i]) > len(out[j])
	})
	return out
}

func (s *Scrubber) pendingSuffixLen(combined string) int {
	keep := 0
	for _, secret := range s.values {
		max := len(secret) - 1
		if max > len(combined) {
			max = len(combined)
		}
		for n := max; n > keep; n-- {
			if strings.HasSuffix(combined, secret[:n]) {
				keep = n
				break
			}
		}
	}
	return keep
}
