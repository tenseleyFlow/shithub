// SPDX-License-Identifier: AGPL-3.0-or-later

// Package passwords exposes the embedded common-password blocklist used at
// signup and password-reset to reject the most-prevalent passwords.
//
// Source: SecLists 10k-most-common (descended from leaked-credential
// corpora; widely used by HIBP-aligned tooling). The cutoff at 10k is a
// pragmatic balance: blocks the high-prevalence head of the distribution
// without bloating the binary or pushing legitimate, hard-to-guess
// passwords into a false-positive tail.
//
// To refresh the list, replace internal/passwords/common_passwords.txt
// with a new SecLists snapshot and re-run the test suite.
package passwords

import (
	_ "embed"
	"strings"
	"sync"
)

//go:embed common_passwords.txt
var rawList string

var (
	once sync.Once
	set  map[string]struct{}
)

func loadOnce() {
	once.Do(func() {
		lines := strings.Split(rawList, "\n")
		set = make(map[string]struct{}, len(lines))
		for _, l := range lines {
			l = strings.TrimSpace(l)
			if l == "" {
				continue
			}
			set[strings.ToLower(l)] = struct{}{}
		}
	})
}

// IsCommon reports whether password matches a known common-password entry
// (case-insensitive). Used at signup and password-reset.
func IsCommon(password string) bool {
	loadOnce()
	_, ok := set[strings.ToLower(password)]
	return ok
}

// Size returns the number of entries in the embedded list. Test affordance.
func Size() int {
	loadOnce()
	return len(set)
}
