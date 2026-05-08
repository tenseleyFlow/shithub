// SPDX-License-Identifier: AGPL-3.0-or-later

package pulls

import (
	"regexp"
	"strconv"
)

// reLinkedIssue matches the GitHub-style auto-close keywords:
//
//	close|closes|closed | fix|fixes|fixed | resolve|resolves|resolved
//
// followed by `#N`. Case-insensitive. The keyword + `#N` must be a
// connected token (one space allowed) so plain "...closes the door
// on #5..." doesn't accidentally trigger an auto-close.
var reLinkedIssue = regexp.MustCompile(`(?i)\b(?:close[sd]?|fix(?:e[sd])?|resolve[sd]?)\s+#([0-9]{1,9})\b`)

// parseLinkedIssues returns the deduplicated set of issue numbers a
// PR body or commit message intends to auto-close.
func parseLinkedIssues(body string) []int64 {
	if body == "" {
		return nil
	}
	seen := map[int64]struct{}{}
	out := []int64{}
	for _, m := range reLinkedIssue.FindAllStringSubmatch(body, -1) {
		n, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil {
			continue
		}
		if _, dup := seen[n]; dup {
			continue
		}
		seen[n] = struct{}{}
		out = append(out, n)
	}
	return out
}
