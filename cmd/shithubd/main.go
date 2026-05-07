// SPDX-License-Identifier: AGPL-3.0-or-later

// Command shithubd is the entrypoint binary for shithub.
//
// It exposes web/ssh/worker/migrate/admin subcommands. See `shithubd --help`
// for the full list, and `.docs/sprints/` for which subcommands are
// implemented in which sprint.
package main

func main() {
	Execute()
}
