// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

// stubCmd returns a placeholder cobra command for subcommands scheduled in
// later sprints. The sprint identifier is surfaced in the error so an
// operator who hits the stub knows which sprint adds the implementation.
func stubCmd(name, short, sprint string) *cobra.Command {
	return &cobra.Command{
		Use:   name,
		Short: fmt.Sprintf("%s (planned in %s)", short, sprint),
		RunE: func(cmd *cobra.Command, args []string) error {
			return fmt.Errorf("%s: not yet implemented (planned in sprint %s)", name, sprint)
		},
	}
}
