// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"github.com/spf13/cobra"

	"github.com/tenseleyFlow/shithub/internal/web"
)

var webCmd = &cobra.Command{
	Use:   "web",
	Short: "Run the shithub HTTP web server",
	RunE: func(cmd *cobra.Command, args []string) error {
		addr, _ := cmd.Flags().GetString("addr")
		return web.Run(cmd.Context(), web.Options{Addr: addr})
	},
}

func init() {
	webCmd.Flags().StringP("addr", "a", ":8080", "Address to bind the web server to")
}
