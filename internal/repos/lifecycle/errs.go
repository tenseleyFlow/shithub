// SPDX-License-Identifier: AGPL-3.0-or-later

package lifecycle

import "errors"

// errAs is a tiny wrapper so the package's call sites read as a single
// expression. errors.As can't be used inline in a type-switch context.
func errAs(err error, target any) bool {
	return errors.As(err, target)
}
