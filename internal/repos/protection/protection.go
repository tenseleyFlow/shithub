// SPDX-License-Identifier: AGPL-3.0-or-later

// Package protection enforces repository protection rules on incoming
// pushes. The pre-receive hook (S14) calls into Enforce once per
// pushed ref; this package owns the matching, the per-rule checks,
// and the rejection messages.
//
// Rules target either `refs/heads/*` or `refs/tags/*`. Branch rules
// carry the full pull-request/check semantics; tag rules protect tag
// deletion, tag movement, and allowed-pusher restrictions.
package protection

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	repogit "github.com/tenseleyFlow/shithub/internal/repos/git"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
)

// Decision is the result of evaluating a single ref update against
// the rule set. Allow=true means the push proceeds; Allow=false
// surfaces the reason+rule pattern back to the user via stderr.
type Decision struct {
	Allow   bool
	Reason  string
	RuleID  int64
	Pattern string
}

// Update is one ref update from the pre-receive hook's stdin.
type Update struct {
	OldSHA string
	NewSHA string
	Ref    string // "refs/heads/<name>" or "refs/tags/<name>"; other namespaces are skipped
	Pusher int64  // user_id; 0 means anonymous which any rule that requires explicit pushers will reject
}

// Enforce evaluates the rule set against `u`. Returns Allow=true
// when no rule rejects; otherwise Allow=false with a human-readable
// reason naming the pattern that matched.
//
// Rule precedence: longest-pattern-match wins (alphabetical tiebreak).
// Rules don't apply to non-heads/non-tags namespaces.
func Enforce(ctx context.Context, pool *pgxpool.Pool, gitDir string, repoID int64, u Update) (Decision, error) {
	target, refName, ok := targetAndNameForRef(u.Ref)
	if !ok {
		return Decision{Allow: true, Reason: "non-protected ref"}, nil
	}

	rq := reposdb.New()
	rules, err := rq.ListBranchProtectionRules(ctx, pool, repoID)
	if err != nil {
		return Decision{}, fmt.Errorf("load rules: %w", err)
	}
	rule, ok := matchRuleForTarget(rules, target, refName)
	if !ok {
		return Decision{Allow: true, Reason: "no rule matched"}, nil
	}

	isCreate := isAllZeros(u.OldSHA)
	isDelete := isAllZeros(u.NewSHA)

	// 1. Deletion gate.
	if isDelete && rule.PreventDeletion {
		if target == "tag" {
			return deny(rule, "deletion of this tag is blocked by protection rule"), nil
		}
		return deny(rule, "deletion of this branch is blocked by protection rule"), nil
	}

	if target == "tag" {
		// Tag refs are not a commit history, so a non-fast-forward
		// ancestry check is not meaningful. Treat changing an existing
		// tag as the tag equivalent of a force-push.
		if !isCreate && !isDelete && rule.PreventForcePush {
			return deny(rule, "moving this tag is blocked by protection rule"), nil
		}
		return allowedPusherDecision(rule, u.Pusher, "tag"), nil
	}

	// 2. Pull-request-only gate. Direct web edits and git pushes both
	// advance refs directly, so rules requiring PRs reject creates and
	// updates here. Deletions remain governed by prevent_deletion above.
	if !isDelete && rule.RequirePrForPush {
		return deny(rule, "direct pushes to this branch must go through a pull request"), nil
	}

	// 3. Force-push gate. Only meaningful when this is an update of an
	// existing branch (both sides non-zero). Skipping allows the create
	// case (oldSHA all-zero) and the delete case (handled above).
	if !isCreate && !isDelete && rule.PreventForcePush {
		ff, err := repogit.IsAncestor(ctx, gitDir, u.OldSHA, u.NewSHA)
		if err != nil {
			return Decision{}, fmt.Errorf("ancestor check: %w", err)
		}
		if !ff {
			return deny(rule, "force-push to this branch is blocked by protection rule"), nil
		}
	}

	// 4. Allowed-pushers gate.
	if decision := allowedPusherDecision(rule, u.Pusher, "branch"); !decision.Allow {
		return decision, nil
	}

	// 5. require_signed_commits and status_checks_required are placeholder
	//    columns wired by S20's migration; their owning sprints flip them on.
	//    No-op here.

	return Decision{Allow: true, Reason: "passed all rules", RuleID: rule.ID, Pattern: rule.Pattern}, nil
}

func deny(r reposdb.BranchProtectionRule, reason string) Decision {
	return Decision{
		Allow:   false,
		Reason:  reason,
		RuleID:  r.ID,
		Pattern: r.Pattern,
	}
}

func allowedPusherDecision(rule reposdb.BranchProtectionRule, pusher int64, label string) Decision {
	if len(rule.AllowedPusherUserIds) == 0 {
		return Decision{Allow: true, Reason: "no allowed-pushers restriction", RuleID: rule.ID, Pattern: rule.Pattern}
	}
	for _, id := range rule.AllowedPusherUserIds {
		if id == pusher {
			return Decision{Allow: true, Reason: "pusher is allowed", RuleID: rule.ID, Pattern: rule.Pattern}
		}
	}
	return deny(rule, "pusher is not on the allowed list for this "+label)
}

// MatchLongestRule returns the rule with the longest pattern matching
// branch (alphabetical tiebreaker). Returns ok=false when no rule
// matches. Exposed as the canonical pattern-match helper so the
// `pulls` and `pulls/review` packages don't reimplement it (the audit
// caught two near-identical copies — S00-S25 audit, M).
//
// Patterns use filepath.Match semantics:
//   - `*` matches any sequence of non-separator chars (NOT crossing `/`)
//   - `?` matches a single non-separator char
//   - `[abc]` matches one of a/b/c
//
// `release/*` matches `release/v1.0` but NOT `release/v1.0/sub`.
func MatchLongestRule(rules []reposdb.BranchProtectionRule, branch string) (reposdb.BranchProtectionRule, bool) {
	return matchRuleForTarget(rules, "branch", branch)
}

func matchRule(rules []reposdb.BranchProtectionRule, branch string) (reposdb.BranchProtectionRule, bool) {
	return matchRuleForTarget(rules, "branch", branch)
}

func matchRuleForTarget(rules []reposdb.BranchProtectionRule, target, name string) (reposdb.BranchProtectionRule, bool) {
	type cand struct {
		rule reposdb.BranchProtectionRule
	}
	var matches []cand
	for _, r := range rules {
		if protectionRuleTarget(r) != target {
			continue
		}
		ok, err := filepath.Match(r.Pattern, name)
		if err != nil {
			continue // bad pattern — admin should fix; treat as no-match
		}
		if ok {
			matches = append(matches, cand{rule: r})
		}
	}
	if len(matches) == 0 {
		return reposdb.BranchProtectionRule{}, false
	}
	sort.Slice(matches, func(i, j int) bool {
		li, lj := len(matches[i].rule.Pattern), len(matches[j].rule.Pattern)
		if li != lj {
			return li > lj
		}
		return matches[i].rule.Pattern < matches[j].rule.Pattern
	})
	return matches[0].rule, true
}

func protectionRuleTarget(r reposdb.BranchProtectionRule) string {
	if r.Target == "tag" {
		return "tag"
	}
	return "branch"
}

func targetAndNameForRef(ref string) (target, name string, ok bool) {
	switch {
	case strings.HasPrefix(ref, "refs/heads/"):
		return "branch", strings.TrimPrefix(ref, "refs/heads/"), true
	case strings.HasPrefix(ref, "refs/tags/"):
		return "tag", strings.TrimPrefix(ref, "refs/tags/"), true
	default:
		return "", "", false
	}
}

// isAllZeros reports whether a SHA string is git's "this side is
// absent" sentinel (40 zeros). Both pre-receive lines use this.
func isAllZeros(sha string) bool {
	if len(sha) != 40 {
		return false
	}
	for _, c := range sha {
		if c != '0' {
			return false
		}
	}
	return true
}

// FriendlyMessage formats a deny Decision for the user's git client.
// The pre-receive hook writes this to stderr.
func FriendlyMessage(d Decision) string {
	if d.Allow {
		return ""
	}
	return fmt.Sprintf("shithub: %s (rule pattern %q).", d.Reason, d.Pattern)
}
