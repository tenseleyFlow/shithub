// SPDX-License-Identifier: AGPL-3.0-or-later

// Package runuser resolves the Unix identity used inside job containers.
package runuser

import (
	"os"
	"strconv"
	"strings"
)

const Auto = "auto"

// Current returns the current process uid:gid in Docker/Podman --user form.
func Current() string {
	return strconv.Itoa(os.Geteuid()) + ":" + strconv.Itoa(os.Getegid())
}

// Resolve maps the default/auto marker to the current runner process identity.
func Resolve(raw string) string {
	user := strings.TrimSpace(raw)
	if user == "" || strings.EqualFold(user, Auto) {
		return Current()
	}
	return user
}
