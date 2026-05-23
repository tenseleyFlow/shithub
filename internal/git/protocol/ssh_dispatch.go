// SPDX-License-Identifier: AGPL-3.0-or-later

package protocol

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/auth/policy"
	"github.com/tenseleyFlow/shithub/internal/infra/storage"
	"github.com/tenseleyFlow/shithub/internal/orgs"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	repotraffic "github.com/tenseleyFlow/shithub/internal/repos/traffic"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
)

// SSHDispatchDeps wires the dispatcher. The DB pool is sized small at
// the call site (max 2 conns) — every SSH connection runs through here
// and the latency floor matters.
type SSHDispatchDeps struct {
	Pool   *pgxpool.Pool
	RepoFS *storage.RepoFS
}

// SSHDispatchInput is everything that comes from the OS environment at
// dispatch time. The cobra command in cmd/shithubd/ssh.go reads
// SSH_ORIGINAL_COMMAND / SSH_CONNECTION / argv and passes them in.
type SSHDispatchInput struct {
	OriginalCommand string
	UserID          int64
	RemoteIP        string // parsed from SSH_CONNECTION first field
}

// SSHDispatchResult is what the caller needs to syscall.Exec the right
// git plumbing binary. Argv0 is the canonical binary path resolved from
// PATH; Argv0Args is just `[Argv0, GitDir]` (git-upload-pack and
// git-receive-pack each take exactly one positional argument). Env is
// the full environment for the new process — already includes PATH and
// the SHITHUB_* vars.
type SSHDispatchResult struct {
	Argv0     string
	Argv0Args []string
	Env       []string
}

// Friendly stderr messages — these are what the user sees in their
// terminal when `git clone` fails. Keep them actionable.
const (
	MsgRepoNotFound = "shithub: repository not found"
	MsgPermDenied   = "shithub: permission denied"
	MsgArchived     = "shithub: this repository is archived; pushes are disabled"
	MsgPaused       = "shithub: this repository is paused; pushes are disabled until the owner unpauses"
	MsgSuspended    = "shithub: your account is suspended"
	MsgRequires2FA  = "shithub: this organization requires two-factor authentication"
)

// Sentinel errors callers can errors.Is to map to friendly messages.
var (
	ErrSSHRepoNotFound = errors.New("ssh dispatch: repo not found")
	ErrSSHPermDenied   = errors.New("ssh dispatch: permission denied")
	ErrSSHArchived     = errors.New("ssh dispatch: archived")
	ErrSSHPaused       = errors.New("ssh dispatch: paused")
	ErrSSHSuspended    = errors.New("ssh dispatch: suspended")
	ErrSSHRequires2FA  = errors.New("ssh dispatch: organization requires two-factor authentication")
	ErrSSHInternal     = errors.New("ssh dispatch: internal error")
)

// PrepareDispatch is the brains of the SSH-shell command. It parses
// SSH_ORIGINAL_COMMAND, resolves user + repo, runs the inline owner-
// only authorization, and builds the env + argv the caller needs to
// exec git. It does NOT call exec itself — the caller does, after
// closing the DB pool (syscall.Exec preserves all open FDs and we
// don't want a leaked Postgres connection).
func PrepareDispatch(ctx context.Context, deps SSHDispatchDeps, in SSHDispatchInput) (*SSHDispatchResult, ParsedSSHCommand, error) {
	parsed, err := ParseSSHCommand(in.OriginalCommand)
	if err != nil {
		return nil, ParsedSSHCommand{}, err
	}

	uq := usersdb.New()
	rq := reposdb.New()

	user, err := uq.GetUserByID(ctx, deps.Pool, in.UserID)
	if err != nil {
		return nil, parsed, fmt.Errorf("%w: %v", ErrSSHInternal, err)
	}
	if user.SuspendedAt.Valid {
		return nil, parsed, ErrSSHSuspended
	}
	if user.DeletedAt.Valid {
		return nil, parsed, ErrSSHSuspended
	}

	// Owner can be a user OR an org; orgs.Resolve hits the principals
	// table to dispatch on kind. Mirrors the same lookup the HTTP git
	// handler uses (web/handlers/githttp/handler.go::lookupRepo) — see
	// the regression that #20 fixed for the HTTPS path; this is the SSH
	// twin of that bug.
	principal, err := orgs.Resolve(ctx, deps.Pool, parsed.Owner)
	if err != nil {
		return nil, parsed, ErrSSHRepoNotFound
	}
	var repo reposdb.Repo
	switch principal.Kind {
	case orgs.PrincipalUser:
		repo, err = rq.GetRepoByOwnerUserAndName(ctx, deps.Pool, reposdb.GetRepoByOwnerUserAndNameParams{
			OwnerUserID: pgtype.Int8{Int64: principal.ID, Valid: true},
			Name:        parsed.Repo,
		})
	case orgs.PrincipalOrg:
		repo, err = rq.GetRepoByOwnerOrgAndName(ctx, deps.Pool, reposdb.GetRepoByOwnerOrgAndNameParams{
			OwnerOrgID: pgtype.Int8{Int64: principal.ID, Valid: true},
			Name:       parsed.Repo,
		})
	default:
		return nil, parsed, ErrSSHRepoNotFound
	}
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, parsed, ErrSSHRepoNotFound
		}
		return nil, parsed, fmt.Errorf("%w: %v", ErrSSHInternal, err)
	}

	// Authz via policy.Can. Map the policy decision back to the typed
	// SSH errors so the friendly-message catalogue keeps working.
	repoRef := policy.NewRepoRefFromRepo(repo)
	has2FA, err := uq.HasConfirmedUserTOTP(ctx, deps.Pool, user.ID)
	if err != nil {
		return nil, parsed, fmt.Errorf("%w: %v", ErrSSHInternal, err)
	}
	actor := policy.UserActorWithTwoFactor(user.ID, user.Username, user.SuspendedAt.Valid, false, has2FA)
	action := policy.ActionRepoRead
	if parsed.Service == ReceivePack {
		action = policy.ActionRepoWrite
	}
	decision := policy.Can(ctx, policy.Deps{Pool: deps.Pool}, actor, action, repoRef)
	if !decision.Allow {
		switch decision.Code {
		case policy.DenyActorSuspended:
			return nil, parsed, ErrSSHSuspended
		case policy.DenyArchived:
			return nil, parsed, ErrSSHArchived
		case policy.DenyPaused:
			return nil, parsed, ErrSSHPaused
		case policy.DenyOrgRequiresTwoFactor:
			return nil, parsed, ErrSSHRequires2FA
		case policy.DenyVisibility, policy.DenyRepoDeleted:
			// Existence-leak guard: pretend the repo doesn't exist.
			return nil, parsed, ErrSSHRepoNotFound
		default:
			return nil, parsed, ErrSSHPermDenied
		}
	}

	gitDir, err := deps.RepoFS.RepoPath(parsed.Owner, parsed.Repo)
	if err != nil {
		return nil, parsed, fmt.Errorf("%w: %v", ErrSSHInternal, err)
	}
	if parsed.Service == UploadPack {
		// Best-effort analytics: do not fail clone/fetch when the traffic
		// aggregate write is unavailable.
		_ = repotraffic.RecordClone(ctx, deps.Pool, repotraffic.Event{
			RepoID:     repo.ID,
			OccurredAt: time.Now(),
			VisitorKey: "ssh:user:" + strconv.FormatInt(user.ID, 10) + "|ip:" + in.RemoteIP,
		})
	}

	requestID, err := newRequestID()
	if err != nil {
		return nil, parsed, fmt.Errorf("%w: %v", ErrSSHInternal, err)
	}
	env := buildSSHEnv(user, parsed.Owner, repo, in.RemoteIP, requestID)

	return &SSHDispatchResult{
		Argv0:     string(parsed.Service),
		Argv0Args: []string{string(parsed.Service), gitDir},
		Env:       env,
	}, parsed, nil
}

// FriendlyMessageFor returns the user-facing stderr line for a typed
// dispatch error. Unknown errors collapse to a generic "internal error
// (request_id=...)" message — never leak the underlying cause.
func FriendlyMessageFor(err error, requestID string) string {
	switch {
	case errors.Is(err, ErrSSHRepoNotFound):
		return MsgRepoNotFound
	case errors.Is(err, ErrSSHPermDenied):
		return MsgPermDenied
	case errors.Is(err, ErrSSHArchived):
		return MsgArchived
	case errors.Is(err, ErrSSHPaused):
		return MsgPaused
	case errors.Is(err, ErrSSHSuspended):
		return MsgSuspended
	case errors.Is(err, ErrSSHRequires2FA):
		return MsgRequires2FA
	case errors.Is(err, ErrUnknownSSHCommand):
		return "shithub does not allow shell access"
	case errors.Is(err, ErrInvalidSSHPath):
		return MsgRepoNotFound
	}
	if requestID == "" {
		return "shithub: internal error"
	}
	return "shithub: internal error (request_id=" + requestID + ")"
}

// buildSSHEnv assembles the env we exec git-{upload,receive}-pack
// with. The shape matches the HTTP path so receive-pack hooks see
// identical vars regardless of transport.
//
// We INHERIT os.Environ() rather than building from scratch — the
// receive-pack process forks the pre-receive / post-receive hooks
// (`shithubd hook ...`), which call config.Load() and need the
// SHITHUB_* config keys (DATABASE_URL, REPOS_ROOT, etc.) sourced
// from /etc/shithub/web.env via the git-shell-commands wrapper.
//
// Pre-fix this function returned a minimal explicit list, so the
// hook's loadHookCtx exited with "DB URL not set" the moment a
// push tried to commit anything. The HTTPS-git path didn't see
// this because shithubd-web's systemd unit sources web.env and
// receive-pack inherits it directly.
//
// Push-event metadata (SHITHUB_USER_ID/REPO_ID/...) is appended
// AFTER inherited env so the explicit values win on collision.
func buildSSHEnv(user usersdb.User, ownerName string, repo reposdb.Repo, remoteIP, requestID string) []string {
	env := os.Environ()
	env = append(
		env,
		"SHITHUB_USER_ID="+strconv.FormatInt(user.ID, 10),
		"SHITHUB_USERNAME="+user.Username,
		"SHITHUB_REPO_ID="+strconv.FormatInt(repo.ID, 10),
		"SHITHUB_REPO_FULL_NAME="+ownerName+"/"+repo.Name,
		"SHITHUB_PROTOCOL=ssh",
		"SHITHUB_REMOTE_IP="+remoteIP,
		"SHITHUB_REQUEST_ID="+requestID,
		// safe.directory: when sshd runs ssh-shell as the `git` user
		// but the bare repo dir is owned by `shithub`, git's
		// dubious-ownership check rejects the invocation. We're
		// invoking on a path shithubd resolved (not user input), so
		// the trust gate already happened upstream; tell git to
		// trust this directory. Inline GIT_CONFIG_* avoids touching
		// /etc/gitconfig and confines the override to this exec.
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=safe.directory",
		"GIT_CONFIG_VALUE_0=*",
	)
	return env
}

// newRequestID returns a 16-byte hex token suitable for log
// correlation. Production callers don't need cryptographic strength
// here, but using crypto/rand keeps the codebase consistent.
func newRequestID() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf[:]), nil
}

// ParseRemoteIP extracts the connecting client's IP from the
// SSH_CONNECTION env var ("<client> <cport> <server> <sport>"). Returns
// "" when malformed; callers may pass the empty string through to the
// hook env.
func ParseRemoteIP(sshConnection string) string {
	if sshConnection == "" {
		return ""
	}
	fields := strings.Fields(sshConnection)
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}
