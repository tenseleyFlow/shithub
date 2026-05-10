// SPDX-License-Identifier: AGPL-3.0-or-later

package search

import "fmt"

func repoOwnerJoin(repoAlias, userAlias, orgAlias string) string {
	return fmt.Sprintf(
		"LEFT JOIN users %s ON %s.id = %s.owner_user_id LEFT JOIN orgs %s ON %s.id = %s.owner_org_id",
		userAlias, userAlias, repoAlias, orgAlias, orgAlias, repoAlias,
	)
}

func repoOwnerNameExpr(userAlias, orgAlias string) string {
	return fmt.Sprintf("coalesce(%s.username, %s.slug)", userAlias, orgAlias)
}

func repoFilterByOwnerName(repoAlias string, ownerPos, namePos int) string {
	return fmt.Sprintf(
		" AND %s.id = (SELECT r2.id FROM repos r2 %s "+
			"WHERE %s = $%d AND r2.name = $%d AND r2.deleted_at IS NULL)",
		repoAlias,
		repoOwnerJoin("r2", "u2", "o2"),
		repoOwnerNameExpr("u2", "o2"),
		ownerPos,
		namePos,
	)
}
