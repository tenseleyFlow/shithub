// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spf13/cobra"

	"github.com/tenseleyFlow/shithub/internal/auth/policy"
	"github.com/tenseleyFlow/shithub/internal/infra/config"
	"github.com/tenseleyFlow/shithub/internal/infra/db"
	"github.com/tenseleyFlow/shithub/internal/infra/storage"
	"github.com/tenseleyFlow/shithub/internal/repos/protection"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
	"github.com/tenseleyFlow/shithub/internal/worker"
	workerdb "github.com/tenseleyFlow/shithub/internal/worker/sqlc"
)

// hookCmd is the umbrella for `shithubd hook <name>`. Each named hook
// is a leaf subcommand; the symlink shim installed by hooks.Install
// invokes one of them. Hidden because no human runs these directly.
var hookCmd = &cobra.Command{
	Use:    "hook",
	Short:  "Git hook entrypoints (post-receive, pre-receive)",
	Hidden: true,
}

// hookPreReceiveCmd implements the minimum-gates pre-receive hook
// described in S14. Full branch-protection gates land in S20.
//
// Stdin lines: "<old_sha> <new_sha> <ref>".
//
// Exit codes:
//   - 0: accept the push.
//   - 1: reject; git aborts and prints whatever we wrote to stderr.
//
// Latency budget: under 100ms for the common case (no archive/suspension).
// We re-check user/repo state from the DB to avoid trusting potentially
// stale env vars from long-lived SSH sessions.
var hookPreReceiveCmd = &cobra.Command{
	Use:    "pre-receive",
	Short:  "Hook: pre-receive — minimum-gates accept/reject",
	Hidden: true,
	RunE: func(cmd *cobra.Command, _ []string) error {
		ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Second)
		defer cancel()

		hook, err := loadHookCtx(ctx)
		if err != nil {
			fmt.Fprintln(cmd.ErrOrStderr(), friendlyHookErr(err))
			return err
		}
		defer hook.pool.Close()

		refs, err := readRefLines(cmd.InOrStdin())
		if err != nil {
			fmt.Fprintln(cmd.ErrOrStderr(), "shithub: failed to read ref updates")
			return err
		}

		if err := preReceiveCheck(ctx, hook); err != nil {
			fmt.Fprintln(cmd.ErrOrStderr(), friendlyHookErr(err))
			return err
		}

		// Branch-protection enforcement (S20). Per-ref check against
		// the rule set; longest-pattern match wins. A single rejected
		// ref aborts the entire push (git's standard non-atomic
		// per-ref accept/reject still applies — pre-receive nonzero
		// rejects all refs in the push, which matches our intent for
		// "any rule says no, the whole push stops").
		gitDir, err := repoGitDir(ctx, hook)
		if err != nil {
			fmt.Fprintln(cmd.ErrOrStderr(), friendlyHookErr(err))
			return err
		}
		for _, rf := range refs {
			d, perr := protection.Enforce(ctx, hook.pool, gitDir, hook.repoID, protection.Update{
				OldSHA: rf.before, NewSHA: rf.after, Ref: rf.ref, Pusher: hook.userID,
			})
			if perr != nil {
				fmt.Fprintln(cmd.ErrOrStderr(), "shithub: protection check failed (transient); please retry")
				return perr
			}
			if !d.Allow {
				fmt.Fprintln(cmd.ErrOrStderr(), protection.FriendlyMessage(d))
				return errors.New("protection denied")
			}
		}
		return nil
	},
}

// hookPostReceiveCmd records each pushed ref as a push_events row,
// enqueues a push:process job per ref, and NOTIFYs idle workers.
// Latency budget: under 100ms for typical small pushes; we keep the
// hook to INSERT + NOTIFY + exit. No HTTP calls, no derivation work.
var hookPostReceiveCmd = &cobra.Command{
	Use:    "post-receive",
	Short:  "Hook: post-receive — enqueue async processing",
	Hidden: true,
	RunE: func(cmd *cobra.Command, _ []string) error {
		ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Second)
		defer cancel()

		hook, err := loadHookCtx(ctx)
		if err != nil {
			// post-receive is non-fatal: the push has already landed. We
			// log to stderr (the user's git client sees it) but exit 0
			// so the push isn't reported as failed.
			fmt.Fprintln(cmd.ErrOrStderr(), "shithub: warning: post-receive enqueue skipped:", err)
			return nil
		}
		defer hook.pool.Close()

		refs, err := readRefLines(cmd.InOrStdin())
		if err != nil || len(refs) == 0 {
			return nil
		}

		if err := postReceiveEnqueue(ctx, hook, refs); err != nil {
			fmt.Fprintln(cmd.ErrOrStderr(), "shithub: warning: post-receive enqueue:", err)
		}
		return nil
	},
}

// hookCtx bundles the deps each hook subcommand needs. Loaded once per
// invocation; closed by the caller via defer.
type hookCtx struct {
	cfg    config.Config
	pool   *pgxpool.Pool
	logger *slog.Logger

	userID     int64
	username   string
	repoID     int64
	repoFull   string
	protocol   string
	remoteIP   string
	requestID  string
}

func loadHookCtx(ctx context.Context) (*hookCtx, error) {
	cfg, err := config.Load(nil)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	if cfg.DB.URL == "" {
		return nil, errors.New("DB URL not set")
	}

	pool, err := db.Open(ctx, db.Config{
		URL: cfg.DB.URL, MaxConns: 2, MinConns: 0,
		ConnectTimeout: 1500 * time.Millisecond,
	})
	if err != nil {
		return nil, fmt.Errorf("db: %w", err)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	uid, _ := strconv.ParseInt(os.Getenv("SHITHUB_USER_ID"), 10, 64)
	rid, _ := strconv.ParseInt(os.Getenv("SHITHUB_REPO_ID"), 10, 64)
	return &hookCtx{
		cfg:       cfg,
		pool:      pool,
		logger:    logger,
		userID:    uid,
		username:  os.Getenv("SHITHUB_USERNAME"),
		repoID:    rid,
		repoFull:  os.Getenv("SHITHUB_REPO_FULL_NAME"),
		protocol:  os.Getenv("SHITHUB_PROTOCOL"),
		remoteIP:  os.Getenv("SHITHUB_REMOTE_IP"),
		requestID: os.Getenv("SHITHUB_REQUEST_ID"),
	}, nil
}

// errHookGate is the typed error pre-receive returns for each rejection
// reason. friendlyHookErr maps these back to user-facing messages.
type errHookGate struct{ kind string }

func (e errHookGate) Error() string { return "shithub-hook: " + e.kind }

var (
	errHookSuspended  = errHookGate{"user suspended"}
	errHookArchived   = errHookGate{"repo archived"}
	errHookDeleted    = errHookGate{"repo deleted"}
	errHookMissing    = errHookGate{"missing context"}
	errHookPermDenied = errHookGate{"permission denied"}
)

// repoGitDir resolves the bare-repo on-disk path for the hook's repo.
// Used by the protection enforcer's IsAncestor check. Hook env carries
// SHITHUB_REPO_FULL_NAME ("owner/name") so we don't need a DB hit.
func repoGitDir(ctx context.Context, h *hookCtx) (string, error) {
	owner, name, ok := strings.Cut(h.repoFull, "/")
	if !ok {
		return "", fmt.Errorf("repoGitDir: bad repo full name %q", h.repoFull)
	}
	root, err := filepath.Abs(h.cfg.Storage.ReposRoot)
	if err != nil {
		return "", fmt.Errorf("repoGitDir: abs: %w", err)
	}
	rfs, err := storage.NewRepoFS(root)
	if err != nil {
		return "", fmt.Errorf("repoGitDir: fs: %w", err)
	}
	return rfs.RepoPath(owner, name)
}

func friendlyHookErr(err error) string {
	switch {
	case errors.Is(err, errHookSuspended):
		return "shithub: your account is suspended; pushes are disabled."
	case errors.Is(err, errHookArchived):
		return "shithub: this repository is archived; pushes are disabled."
	case errors.Is(err, errHookDeleted):
		return "shithub: this repository has been deleted."
	case errors.Is(err, errHookPermDenied):
		return "shithub: you do not have write access to this repository."
	case errors.Is(err, errHookMissing):
		return "shithub: server error: hook context missing. Contact the operator."
	default:
		return "shithub: server error: " + err.Error()
	}
}

func preReceiveCheck(ctx context.Context, h *hookCtx) error {
	if h.userID == 0 || h.repoID == 0 {
		return errHookMissing
	}
	uq := usersdb.New()
	user, err := uq.GetUserByID(ctx, h.pool, h.userID)
	if err != nil {
		return fmt.Errorf("user lookup: %w", err)
	}

	rq := reposdb.New()
	repo, err := rq.GetRepoByID(ctx, h.pool, h.repoID)
	if err != nil {
		return fmt.Errorf("repo lookup: %w", err)
	}

	actor := policy.UserActor(user.ID, user.Username, user.SuspendedAt.Valid, false)
	repoRef := policy.NewRepoRefFromRepo(repo)
	decision := policy.Can(ctx, policy.Deps{Pool: h.pool}, actor, policy.ActionRepoWrite, repoRef)
	if decision.Allow {
		return nil
	}
	switch decision.Code {
	case policy.DenyRepoDeleted:
		return errHookDeleted
	case policy.DenyActorSuspended:
		return errHookSuspended
	case policy.DenyArchived:
		return errHookArchived
	default:
		return errHookPermDenied
	}
}

func postReceiveEnqueue(ctx context.Context, h *hookCtx, refs []refUpdate) error {
	if h.repoID == 0 {
		return errHookMissing
	}

	tx, err := h.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()

	wq := workerdb.New()
	protocol := h.protocol
	if protocol == "" {
		protocol = "ssh" // safe fallback when env is missing
	}
	for _, r := range refs {
		event, err := wq.InsertPushEvent(ctx, tx, workerdb.InsertPushEventParams{
			RepoID:        h.repoID,
			BeforeSha:     r.before,
			AfterSha:      r.after,
			Ref:           r.ref,
			Protocol:      protocol,
			PusherUserID:  pgtype.Int8{Int64: h.userID, Valid: h.userID != 0},
			RequestID:     pgtype.Text{String: h.requestID, Valid: h.requestID != ""},
		})
		if err != nil {
			return fmt.Errorf("insert push_event: %w", err)
		}
		if _, err := worker.Enqueue(ctx, tx, worker.KindPushProcess,
			map[string]any{"push_event_id": event.ID},
			worker.EnqueueOptions{}); err != nil {
			return fmt.Errorf("enqueue push:process: %w", err)
		}
	}
	if err := worker.Notify(ctx, tx); err != nil {
		// Notify failure inside tx is non-fatal — workers also poll.
		h.logger.WarnContext(ctx, "post-receive: NOTIFY failed", "error", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	committed = true
	return nil
}

// refUpdate is one stdin line as parsed by readRefLines.
type refUpdate struct {
	before, after, ref string
}

func readRefLines(r io.Reader) ([]refUpdate, error) {
	var out []refUpdate
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) != 3 {
			continue
		}
		out = append(out, refUpdate{before: parts[0], after: parts[1], ref: parts[2]})
	}
	return out, sc.Err()
}

// hooksReinstallCmd reinstalls hook symlinks on every active repo, used
// after a binary path change in production deploys. --repo runs against
// a single owner/name; --all walks every repo via the DB.
var hooksReinstallCmd = &cobra.Command{
	Use:   "reinstall",
	Short: "Reinstall hook symlinks on existing repos",
	RunE: func(cmd *cobra.Command, _ []string) error {
		all, _ := cmd.Flags().GetBool("all")
		repo, _ := cmd.Flags().GetString("repo")
		if !all && repo == "" {
			return errors.New("hooks reinstall: pass --all or --repo owner/name")
		}
		return runHooksReinstall(cmd.Context(), all, repo, cmd.OutOrStdout())
	},
}

// hooksParentCmd is the umbrella so the operator command reads as
// `shithubd hooks reinstall ...`.
var hooksParentCmd = &cobra.Command{
	Use:   "hooks",
	Short: "Operator commands for git hook installation",
}

func init() {
	hookCmd.AddCommand(hookPreReceiveCmd)
	hookCmd.AddCommand(hookPostReceiveCmd)
	hooksReinstallCmd.Flags().Bool("all", false, "Reinstall on every active repo")
	hooksReinstallCmd.Flags().String("repo", "", "Reinstall on owner/name only")
	hooksParentCmd.AddCommand(hooksReinstallCmd)

	rootCmd.AddCommand(hookCmd)
	rootCmd.AddCommand(hooksParentCmd)
}

