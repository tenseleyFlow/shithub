// SPDX-License-Identifier: AGPL-3.0-or-later

package jobs

import "fmt"

func ownerSlugString(v any) (string, error) {
	switch s := v.(type) {
	case string:
		return s, nil
	case []byte:
		return string(s), nil
	default:
		return "", fmt.Errorf("unexpected owner slug type %T", v)
	}
}
