// SPDX-License-Identifier: AGPL-3.0-or-later

package auth

import "testing"

func TestIsReserved(t *testing.T) {
	t.Parallel()
	yes := []string{"login", "logout", "settings", "api", "admin", "shithub", "explore"}
	for _, n := range yes {
		if !IsReserved(n) {
			t.Errorf("expected %q to be reserved", n)
		}
	}
	if !IsReserved("LOGIN") {
		t.Error("reserved check should be case-insensitive")
	}
}

func TestIsReserved_AllowsRegularUsernames(t *testing.T) {
	t.Parallel()
	no := []string{"alice", "bob-dev", "myproject1", "carol"}
	for _, n := range no {
		if IsReserved(n) {
			t.Errorf("did not expect %q to be reserved", n)
		}
	}
}
