// SPDX-License-Identifier: AGPL-3.0-or-later

package search

import (
	"context"
	"fmt"
	"strings"

	"github.com/tenseleyFlow/shithub/internal/auth/policy"
)

// SearchIssues runs an issue search visible to actor. Issues
// inherit visibility from their repo via the same predicate.
//
// Ranking: `ts_rank_cd * state_weight` where open weighs 1.5×
// over closed (the spec doesn't pin a number; 1.5 is a sensible
// default that surfaces actionable issues first without burying
// the closed history).
//
// kindFilter is optional: pass "issue", "pr", or "" for both. The
// dropdown / Issues tab passes "issue"; the PR tab would pass "pr".
func SearchIssues(ctx context.Context, deps Deps, actor policy.Actor, q ParsedQuery, kindFilter string, limit, offset int) ([]IssueResult, int64, error) {
	if !q.HasContent() {
		return nil, 0, ErrEmptyQuery
	}
	tsText, tsCtor, hasFTS := tsQueryBindAndCtor(q)
	effectiveKind := kindFilter
	if q.KindFilter != "" {
		if effectiveKind != "" && effectiveKind != q.KindFilter {
			return nil, 0, nil
		}
		effectiveKind = q.KindFilter
	}
	if q.MergedStateFilter != "" {
		if effectiveKind == "issue" {
			return nil, 0, nil
		}
		effectiveKind = "pr"
	}
	if q.MergedFilter != nil {
		if effectiveKind == "issue" {
			return nil, 0, nil
		}
		effectiveKind = "pr"
	}
	if q.ReviewRequestedFilter != "" {
		if effectiveKind == "issue" {
			return nil, 0, nil
		}
		effectiveKind = "pr"
	}

	// At least one signal must drive the query: the FTS payload, a
	// repo:/owner filter, an actor filter, label/milestone, date
	// filters, or a state/kind filter.
	if !hasFTS && !hasIssueNarrowingFilter(q, effectiveKind) {
		return nil, 0, nil
	}

	// G11 (F49): if the user supplied free-text but no narrowing
	// filters, verify at least one token is long enough to match
	// indexed content. See repos.go for the heuristic rationale.
	if hasFTS && !hasIssueNarrowingFilter(q, effectiveKind) {
		if err := validateFTSNotShortOnly(tsText); err != nil {
			return nil, 0, err
		}
	}

	args := []any{}
	tsPlaceholder := 0
	if hasFTS {
		args = append(args, tsText)
		tsPlaceholder = len(args)
	}
	visClause, visArgs := policy.VisibilityPredicate(actor, "r", len(args)+1)
	args = append(args, visArgs...)

	whereExtras := ""

	if q.RepoFilter != nil {
		ownerPos := len(args) + 1
		namePos := len(args) + 2
		args = append(args, q.RepoFilter.Owner, q.RepoFilter.Name)
		whereExtras += repoFilterByOwnerName("r", ownerPos, namePos)
	}
	if q.OwnerFilter != "" {
		ownerPos := len(args) + 1
		args = append(args, q.OwnerFilter)
		whereExtras += fmt.Sprintf(
			" AND (u.username = $%d OR o.slug = $%d)",
			ownerPos, ownerPos,
		)
	}
	if q.StateFilter != "" {
		statePos := len(args) + 1
		args = append(args, q.StateFilter)
		whereExtras += fmt.Sprintf(" AND s.state::text = $%d", statePos)
	}
	if effectiveKind != "" {
		kindPos := len(args) + 1
		args = append(args, effectiveKind)
		whereExtras += fmt.Sprintf(" AND s.kind::text = $%d", kindPos)
	}
	if q.AuthorFilter != "" {
		author, ok := resolveIssueUserFilter(actor, q.AuthorFilter)
		if !ok {
			whereExtras += " AND FALSE"
		} else {
			authorPos := len(args) + 1
			args = append(args, author)
			whereExtras += fmt.Sprintf(
				" AND s.author_user_id = (SELECT id FROM users WHERE username = $%d)",
				authorPos,
			)
		}
	}
	if q.AssigneeFilter != "" {
		assignee, ok := resolveIssueUserFilter(actor, q.AssigneeFilter)
		if !ok {
			whereExtras += " AND FALSE"
		} else {
			assigneePos := len(args) + 1
			args = append(args, assignee)
			whereExtras += fmt.Sprintf(
				" AND EXISTS (SELECT 1 FROM issue_assignees ia"+
					" JOIN users au2 ON au2.id = ia.user_id"+
					" WHERE ia.issue_id = s.issue_id AND au2.username = $%d)",
				assigneePos,
			)
		}
	}
	if q.AssigneeAnyFilter {
		whereExtras += " AND EXISTS (SELECT 1 FROM issue_assignees ia WHERE ia.issue_id = s.issue_id)"
	}
	if q.CommenterFilter != "" {
		commenter, ok := resolveIssueUserFilter(actor, q.CommenterFilter)
		if !ok {
			whereExtras += " AND FALSE"
		} else {
			commenterPos := len(args) + 1
			args = append(args, commenter)
			whereExtras += fmt.Sprintf(
				" AND EXISTS (SELECT 1 FROM issue_comments ic"+
					" JOIN users cu ON cu.id = ic.author_user_id"+
					" WHERE ic.issue_id = s.issue_id AND cu.username = $%d)",
				commenterPos,
			)
		}
	}
	if q.MentionFilter != "" {
		mention, ok := resolveIssueUserFilter(actor, q.MentionFilter)
		if !ok {
			whereExtras += " AND FALSE"
		} else {
			whereExtras += appendIssueMentionFilter(&args, mention)
		}
	}
	if len(q.InvolvesFilters) > 0 {
		clauses := make([]string, 0, len(q.InvolvesFilters))
		for _, raw := range q.InvolvesFilters {
			involved, ok := resolveIssueUserFilter(actor, raw)
			if !ok {
				continue
			}
			clauses = append(clauses, appendIssueInvolvesClause(&args, involved))
		}
		if len(clauses) == 0 {
			whereExtras += " AND FALSE"
		} else {
			whereExtras += " AND (" + strings.Join(clauses, " OR ") + ")"
		}
	}
	if q.ReviewRequestedFilter != "" {
		reviewer, ok := resolveIssueUserFilter(actor, q.ReviewRequestedFilter)
		if !ok {
			whereExtras += " AND FALSE"
		} else {
			reviewerPos := len(args) + 1
			args = append(args, reviewer)
			whereExtras += fmt.Sprintf(
				" AND EXISTS (SELECT 1 FROM pr_review_requests prr"+
					" JOIN users ru ON ru.id = prr.requested_user_id"+
					" WHERE prr.pr_issue_id = s.issue_id AND ru.username = $%d"+
					" AND prr.dismissed_at IS NULL AND prr.satisfied_by_review_id IS NULL)",
				reviewerPos,
			)
		}
	}
	for _, label := range q.LabelFilters {
		labelPos := len(args) + 1
		args = append(args, label)
		whereExtras += fmt.Sprintf(
			" AND EXISTS (SELECT 1 FROM issue_labels il"+
				" JOIN labels l ON l.id = il.label_id"+
				" WHERE il.issue_id = s.issue_id AND l.name = $%d)",
			labelPos,
		)
	}
	if q.MilestoneFilter != "" {
		milestonePos := len(args) + 1
		args = append(args, q.MilestoneFilter)
		whereExtras += fmt.Sprintf(
			" AND EXISTS (SELECT 1 FROM milestones m"+
				" WHERE m.id = i.milestone_id AND m.title = $%d)",
			milestonePos,
		)
	}
	for _, missing := range q.MissingFilters {
		switch missing {
		case "label":
			whereExtras += " AND NOT EXISTS (SELECT 1 FROM issue_labels il WHERE il.issue_id = s.issue_id)"
		case "milestone":
			whereExtras += " AND i.milestone_id IS NULL"
		case "assignee":
			whereExtras += " AND NOT EXISTS (SELECT 1 FROM issue_assignees ia WHERE ia.issue_id = s.issue_id)"
		case "project":
			whereExtras += " AND NOT EXISTS (SELECT 1 FROM repo_project_items rpi WHERE rpi.issue_id = s.issue_id)"
		}
	}
	if q.LockedFilter != nil {
		if *q.LockedFilter {
			whereExtras += " AND i.locked"
		} else {
			whereExtras += " AND NOT i.locked"
		}
	}
	whereExtras += appendRepoQualifierFilters(&args, "r", q)
	whereExtras += appendDateRangeFilter(&args, "i.created_at", q.CreatedFilter)
	whereExtras += appendDateRangeFilter(&args, "i.updated_at", q.UpdatedFilter)
	whereExtras += appendDateRangeFilter(&args, "i.closed_at", q.ClosedFilter)
	whereExtras += appendDateRangeFilter(&args, "pr.merged_at", q.MergedFilter)
	switch q.MergedStateFilter {
	case "merged":
		whereExtras += " AND pr.merged_at IS NOT NULL"
	case "unmerged":
		whereExtras += " AND pr.merged_at IS NULL"
	}

	pullJoin := ""
	if q.MergedFilter != nil || q.MergedStateFilter != "" {
		pullJoin = "JOIN pull_requests pr ON pr.issue_id = i.id"
	}

	whereFTS := "TRUE"
	rankExpr := "1.0"
	if hasFTS {
		whereFTS = fmt.Sprintf("s.tsv @@ %s('shithub_search', $%d)", tsCtor, tsPlaceholder)
		rankExpr = fmt.Sprintf("ts_rank_cd(s.tsv, %s('shithub_search', $%d))", tsCtor, tsPlaceholder)
	}

	limPos := len(args) + 1
	offPos := len(args) + 2
	args = append(args, limit, offset)

	orderBy := issueOrderBy(q.SortFilter)
	queryStr := fmt.Sprintf(`
		SELECT i.id, r.id, %[7]s, r.name, r.visibility::text, i.number, i.title,
		       i.state::text, i.kind::text,
		       coalesce(au.username, '') AS author_name,
		       i.updated_at,
		       %[1]s * CASE WHEN s.state = 'open' THEN 1.5 ELSE 1.0 END AS rank
		FROM issues_search s
		JOIN issues i  ON i.id = s.issue_id
		JOIN repos r   ON r.id = s.repo_id
		%[8]s
		%[9]s
		LEFT JOIN users au ON au.id = s.author_user_id
		WHERE %[2]s
		  AND %[3]s
		  %[4]s
		ORDER BY %[10]s
		LIMIT $%[5]d OFFSET $%[6]d
	`, rankExpr, whereFTS, visClause, whereExtras, limPos, offPos, repoOwnerNameExpr("u", "o"), repoOwnerJoin("r", "u", "o"), pullJoin, orderBy)

	rows, err := deps.Pool.Query(ctx, queryStr, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("search issues: %w", err)
	}
	defer rows.Close()
	out := make([]IssueResult, 0, limit)
	for rows.Next() {
		var r IssueResult
		if err := rows.Scan(&r.ID, &r.RepoID, &r.OwnerUsername, &r.RepoName,
			&r.RepoVisibility, &r.Number, &r.Title, &r.State, &r.Kind, &r.AuthorName,
			&r.UpdatedAt, &r.Rank); err != nil {
			return nil, 0, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}

	countQuery := fmt.Sprintf(`
		SELECT count(*)
		FROM issues_search s
		JOIN issues i  ON i.id = s.issue_id
		JOIN repos r   ON r.id = s.repo_id
		%[4]s
		%[5]s
		WHERE %[1]s AND %[2]s %[3]s
	`, whereFTS, visClause, whereExtras, repoOwnerJoin("r", "u", "o"), pullJoin)
	var total int64
	if err := deps.Pool.QueryRow(ctx, countQuery, args[:len(args)-2]...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count issues: %w", err)
	}
	return out, total, nil
}

func hasIssueNarrowingFilter(q ParsedQuery, effectiveKind string) bool {
	return q.RepoFilter != nil || q.OwnerFilter != "" ||
		q.AuthorFilter != "" || q.AssigneeFilter != "" || q.AssigneeAnyFilter ||
		q.CommenterFilter != "" || q.MentionFilter != "" || len(q.InvolvesFilters) > 0 ||
		q.StateFilter != "" || effectiveKind != "" || len(q.LabelFilters) > 0 ||
		q.MilestoneFilter != "" || len(q.MissingFilters) > 0 || q.LockedFilter != nil ||
		q.ReviewRequestedFilter != "" ||
		q.VisibilityFilter != "" || q.ForkFilter != nil || q.ArchivedFilter != nil ||
		len(q.TopicFilters) > 0 || q.CreatedFilter != nil || q.UpdatedFilter != nil ||
		q.ClosedFilter != nil || q.MergedFilter != nil || q.MergedStateFilter != ""
}

func issueOrderBy(sort string) string {
	switch sort {
	case "comments-asc":
		return "(SELECT COUNT(*) FROM issue_comments ic_order WHERE ic_order.issue_id = i.id) ASC, i.updated_at DESC, i.id DESC"
	case "comments-desc":
		return "(SELECT COUNT(*) FROM issue_comments ic_order WHERE ic_order.issue_id = i.id) DESC, i.updated_at DESC, i.id DESC"
	case "created-asc":
		return "i.created_at ASC, i.id ASC"
	case "created-desc":
		return "i.created_at DESC, i.id DESC"
	case "updated-asc":
		return "i.updated_at ASC, i.id ASC"
	case "updated-desc":
		return "i.updated_at DESC, i.id DESC"
	case "relevance-asc":
		return "rank ASC, i.updated_at DESC, i.id DESC"
	default:
		return "rank DESC, i.updated_at DESC, i.id DESC"
	}
}

func resolveIssueUserFilter(actor policy.Actor, raw string) (string, bool) {
	value := strings.TrimSpace(strings.TrimPrefix(raw, "@"))
	if value == "" {
		return "", false
	}
	if strings.EqualFold(value, "me") {
		if actor.IsAnonymous || actor.Username == "" {
			return "", false
		}
		return actor.Username, true
	}
	return value, true
}

func appendIssueMentionFilter(args *[]any, username string) string {
	pos := len(*args) + 1
	*args = append(*args, "%@"+username+"%")
	return fmt.Sprintf(
		" AND (i.title ILIKE $%[1]d OR i.body ILIKE $%[1]d"+
			" OR EXISTS (SELECT 1 FROM issue_comments im"+
			" WHERE im.issue_id = s.issue_id AND im.body ILIKE $%[1]d))",
		pos,
	)
}

func appendIssueInvolvesClause(args *[]any, username string) string {
	userPos := len(*args) + 1
	mentionPos := len(*args) + 2
	*args = append(*args, username, "%@"+username+"%")
	return fmt.Sprintf(
		"(s.author_user_id = (SELECT id FROM users WHERE username = $%[1]d)"+
			" OR EXISTS (SELECT 1 FROM issue_assignees ia"+
			" JOIN users au2 ON au2.id = ia.user_id"+
			" WHERE ia.issue_id = s.issue_id AND au2.username = $%[1]d)"+
			" OR EXISTS (SELECT 1 FROM issue_comments ic"+
			" JOIN users cu ON cu.id = ic.author_user_id"+
			" WHERE ic.issue_id = s.issue_id AND cu.username = $%[1]d)"+
			" OR i.title ILIKE $%[2]d OR i.body ILIKE $%[2]d"+
			" OR EXISTS (SELECT 1 FROM issue_comments im"+
			" WHERE im.issue_id = s.issue_id AND im.body ILIKE $%[2]d))",
		userPos, mentionPos,
	)
}
