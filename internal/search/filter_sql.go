// SPDX-License-Identifier: AGPL-3.0-or-later

package search

import "fmt"

func appendCITextFilter(args *[]any, expr, value string) string {
	if value == "" {
		return ""
	}
	pos := len(*args) + 1
	*args = append(*args, value)
	return fmt.Sprintf(" AND lower(%s) = lower($%d)", expr, pos)
}

func appendCILikeFilter(args *[]any, expr, value string) string {
	if value == "" {
		return ""
	}
	pos := len(*args) + 1
	*args = append(*args, "%"+value+"%")
	return fmt.Sprintf(" AND %s ILIKE $%d", expr, pos)
}

func appendCISuffixFilter(args *[]any, expr, value string) string {
	if value == "" {
		return ""
	}
	pos := len(*args) + 1
	*args = append(*args, value)
	return fmt.Sprintf(" AND lower(%s) LIKE '%%.' || lower($%d)", expr, pos)
}

func appendDateRangeFilter(args *[]any, expr string, dr *DateRange) string {
	if dr == nil {
		return ""
	}
	out := ""
	if dr.HasFrom {
		pos := len(*args) + 1
		*args = append(*args, dr.From)
		out += fmt.Sprintf(" AND %s >= $%d", expr, pos)
	}
	if dr.HasTo {
		pos := len(*args) + 1
		*args = append(*args, dr.To)
		out += fmt.Sprintf(" AND %s < $%d", expr, pos)
	}
	return out
}
