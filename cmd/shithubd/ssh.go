// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/tenseleyFlow/shithub/internal/git/protocol"
	"github.com/tenseleyFlow/shithub/internal/infra/config"
	"github.com/tenseleyFlow/shithub/internal/infra/db"
	"github.com/tenseleyFlow/shithub/internal/infra/storage"
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

// sshShellCmd is the forced-command target sshd invokes after the
// AuthorizedKeysCommand handshake binds the connection to a user.
//
// Flow on a successful clone/push:
//
//	sshd ──► shithubd ssh-shell <user_id>
//	         ├─ ParseSSHCommand(SSH_ORIGINAL_COMMAND)
//	         ├─ Resolve user + repo against the DB
//	         ├─ Inline owner-only authz (S15 will refactor)
//	         ├─ Build SHITHUB_* env (so post-receive hooks identify the actor)
//	         ├─ Close the DB pool (syscall.Exec preserves all open FDs)
//	         └─ syscall.Exec git-{upload,receive}-pack <bare-repo>
//
// On any error: write a friendly line to stderr (the user sees it in
// their git client), log structured, exit non-zero. defer does NOT
// fire on syscall.Exec — every cleanup happens BEFORE the exec call.
var sshShellCmd = &cobra.Command{
	Use:    "ssh-shell <user_id>",
	Short:  "Forced-command target invoked by sshd via AuthorizedKeysCommand",
	Args:   cobra.ExactArgs(1),
	Hidden: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		userID, err := strconv.ParseInt(args[0], 10, 64)
		if err != nil {
			_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "shithub: invalid user")
			return fmt.Errorf("ssh-shell: bad user_id %q: %w", args[0], err)
		}
		original := os.Getenv("SSH_ORIGINAL_COMMAND")
		remoteIP := protocol.ParseRemoteIP(os.Getenv("SSH_CONNECTION"))
		logger := slog.New(slog.NewTextHandler(cmd.ErrOrStderr(), &slog.HandlerOptions{Level: slog.LevelInfo}))

		cfg, err := config.Load(nil)
		if err != nil || cfg.DB.URL == "" || cfg.Storage.ReposRoot == "" {
			_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "shithub: server misconfigured")
			return fmt.Errorf("ssh-shell: cfg: %w", err)
		}
		root, err := filepath.Abs(cfg.Storage.ReposRoot)
		if err != nil {
			_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "shithub: server misconfigured")
			return fmt.Errorf("ssh-shell: repos_root: %w", err)
		}
		rfs, err := storage.NewRepoFS(root)
		if err != nil {
			_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "shithub: server misconfigured")
			return fmt.Errorf("ssh-shell: NewRepoFS: %w", err)
		}

		ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Second)
		defer cancel()
		pool, err := db.Open(ctx, db.Config{
			URL: cfg.DB.URL, MaxConns: 2, MinConns: 0,
			ConnectTimeout: 1500 * time.Millisecond,
		})
		if err != nil {
			_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "shithub: temporary failure (try again)")
			return fmt.Errorf("ssh-shell: db open: %w", err)
		}

		res, parsed, dispatchErr := protocol.PrepareDispatch(ctx, protocol.SSHDispatchDeps{
			Pool: pool, RepoFS: rfs,
		}, protocol.SSHDispatchInput{
			OriginalCommand: original,
			UserID:          userID,
			RemoteIP:        remoteIP,
		})
		if dispatchErr != nil {
			pool.Close()
			_, _ = fmt.Fprintln(cmd.ErrOrStderr(), protocol.FriendlyMessageFor(dispatchErr, ""))
			logger.WarnContext(
				ctx, "ssh-shell: denied",
				"user_id", userID,
				"original", original,
				"remote_ip", remoteIP,
				"error", dispatchErr,
			)
			return dispatchErr
		}
		logger.InfoContext(
			ctx, "ssh-shell: dispatch",
			"user_id", userID,
			"op", string(parsed.Service),
			"owner", parsed.Owner,
			"repo", parsed.Repo,
			"remote_ip", remoteIP,
		)

		// CRITICAL: close DB pool before syscall.Exec. defer doesn't
		// fire on exec, and the pgx pool's connections would otherwise
		// leak into the new process's FD table.
		pool.Close()

		bin, err := exec.LookPath(res.Argv0)
		if err != nil {
			_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "shithub: server misconfigured")
			return fmt.Errorf("ssh-shell: lookup %s: %w", res.Argv0, err)
		}
		if err := sysExec(bin, res.Argv0Args, res.Env); err != nil {
			_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "shithub: internal error")
			return fmt.Errorf("ssh-shell: exec %s: %w", bin, err)
		}
		// Unreachable on success — syscall.Exec replaces this process.
		return nil
	},
}

// sysExec is split out so tests can stub it. bin is exec.LookPath of a
// fixed service name (git-{upload,receive}-pack); argv[1] is the
// sanitized bare-repo path from storage.RepoFS.
//
//nolint:gosec // G204: inputs are constrained as documented above.
var sysExec = syscall.Exec

// authorizedKeysLine builds the single line sshd consumes. The forced
// command runs `shithubd ssh-shell <user_id>`; the option set strips
// every interactive affordance.
//
// The command uses the BARE binary name (`shithubd`), not the absolute
// path of os.Args[0]. Reason: when the git user's login shell is
// /usr/bin/git-shell (kept as a defense-in-depth wall — git-shell
// only allows git's own commands plus entries in ~/git-shell-commands/),
// git-shell rejects any first token that contains a slash. With a
// bare token, git-shell looks for ~git/git-shell-commands/shithubd
// (must be a symlink to /usr/local/bin/shithubd installed by ansible)
// and execs it.
//
// PATH on the AKC's child process inherits sshd's env, which on
// Debian includes /usr/local/bin — so a fallback `shithubd` lookup
// would also find the binary even without git-shell-commands. The
// git-shell-commands symlink is the authoritative path.
func authorizedKeysLine(row usersdb.UserSshKey) string {
	binary := filepath.Base(os.Args[0])
	// user_id is a digit string so it can never contain shell metacharacters.
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
