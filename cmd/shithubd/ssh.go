// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"fmt"
	"net/netip"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/tenseleyFlow/shithub/internal/infra/config"
	"github.com/tenseleyFlow/shithub/internal/infra/db"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
)

// sshAuthkeysCmd implements sshd's AuthorizedKeysCommand contract:
//
//   - On a known fingerprint, write a single authorized_keys line on stdout
//     with a forced command and restrictive options.
//   - On an unknown fingerprint OR any error, write nothing and exit 0.
//     sshd uses STDOUT as the auth answer; non-zero exit is a config error,
//     not a deny. Failing closed is the right model: better to deny a
//     legitimate connection than accidentally authorize the wrong user.
//
// Latency is critical — every SSH connection waits on this. The pool is
// sized small (max 4 conns) to bound startup cost and tail-latency.
var sshAuthkeysCmd = &cobra.Command{
	Use:    "ssh-authkeys <fingerprint>",
	Short:  "AuthorizedKeysCommand handler for sshd",
	Args:   cobra.ExactArgs(1),
	Hidden: true, // not for direct human use
	RunE: func(cmd *cobra.Command, args []string) error {
		// Fail-closed wrapper: anything below that returns an error or
		// panics writes nothing to stdout. The exit code stays 0.
		defer func() {
			_ = recover()
		}()
		fp := strings.TrimSpace(args[0])
		if !isWellFormedFingerprint(fp) {
			return nil
		}

		cfg, err := config.Load(nil)
		if err != nil || cfg.DB.URL == "" {
			return nil
		}

		ctx, cancel := context.WithTimeout(cmd.Context(), 1500*time.Millisecond)
		defer cancel()

		pool, err := db.Open(ctx, db.Config{
			URL: cfg.DB.URL, MaxConns: 4, MinConns: 0,
			ConnectTimeout: 750 * time.Millisecond,
		})
		if err != nil {
			return nil
		}
		defer pool.Close()

		q := usersdb.New()
		row, err := q.GetUserSSHKeyByFingerprint(ctx, pool, fp)
		if err != nil {
			// pgx.ErrNoRows or any other error → silently empty.
			return nil
		}

		_, _ = fmt.Fprintln(cmd.OutOrStdout(), authorizedKeysLine(row))

		// Best-effort last-used update. 500ms cap; any error is dropped.
		updateCtx, updateCancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer updateCancel()
		_ = q.TouchSSHKeyLastUsed(updateCtx, pool, usersdb.TouchSSHKeyLastUsedParams{
			ID:         row.ID,
			LastUsedIp: clientAddrFromEnv(),
		})
		return nil
	},
}

// sshShellCmd is the placeholder for the forced-command target. S13 swaps
// this for the real git-over-SSH dispatcher; for S07 we just log the
// inbound command and exit non-zero with a friendly message so an
// operator (or test) can confirm the wiring works end-to-end.
var sshShellCmd = &cobra.Command{
	Use:    "ssh-shell <user_id>",
	Short:  "Forced-command target invoked by sshd via AuthorizedKeysCommand",
	Args:   cobra.ExactArgs(1),
	Hidden: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		userID := args[0]
		original := os.Getenv("SSH_ORIGINAL_COMMAND")
		// Log to stderr so it's captured by sshd's session log without
		// polluting the (silent-on-empty) stdout contract.
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
			"shithubd ssh-shell: user_id=%s original_command=%q (git-over-SSH lands in S13)\n",
			userID, original)
		return fmt.Errorf("git over SSH not enabled yet")
	},
}

// authorizedKeysLine builds the single line sshd consumes. The forced
// command runs `shithubd ssh-shell <user_id>`; the option set strips
// every interactive affordance.
func authorizedKeysLine(row usersdb.UserSshKey) string {
	binary := os.Args[0]
	// Quote-escape only the binary path; user_id is a digit string so it
	// can never contain shell metacharacters.
	cmd := fmt.Sprintf(`%s ssh-shell %d`, binary, row.UserID)
	options := strings.Join([]string{
		fmt.Sprintf(`command="%s"`, cmd),
		"no-port-forwarding",
		"no-X11-forwarding",
		"no-agent-forwarding",
		"no-pty",
	}, ",")
	return options + " " + row.PublicKey
}

// clientAddrFromEnv extracts the connecting client's address from
// $SSH_CONNECTION (sshd sets it to "<client> <cport> <server> <sport>").
// Returns nil when unavailable, which sqlc encodes as a SQL NULL.
func clientAddrFromEnv() *netip.Addr {
	conn := os.Getenv("SSH_CONNECTION")
	if conn == "" {
		return nil
	}
	parts := strings.Fields(conn)
	if len(parts) < 1 {
		return nil
	}
	addr, err := netip.ParseAddr(parts[0])
	if err != nil {
		return nil
	}
	return &addr
}

// isWellFormedFingerprint accepts only the canonical SHA256:<b64> shape
// our codebase emits. Defense against an attacker passing crafted strings
// to influence the SQL plan.
func isWellFormedFingerprint(s string) bool {
	if !strings.HasPrefix(s, "SHA256:") {
		return false
	}
	rest := s[len("SHA256:"):]
	if len(rest) < 30 || len(rest) > 80 {
		return false
	}
	for _, r := range rest {
		switch {
		case r >= 'A' && r <= 'Z',
			r >= 'a' && r <= 'z',
			r >= '0' && r <= '9',
			r == '+', r == '/', r == '=':
		default:
			return false
		}
	}
	return true
}

func init() {
	rootCmd.AddCommand(sshAuthkeysCmd)
	rootCmd.AddCommand(sshShellCmd)
}
