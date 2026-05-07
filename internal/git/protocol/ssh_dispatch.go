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

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/auth/policy"
	"github.com/tenseleyFlow/shithub/internal/infra/storage"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
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
	MsgSuspended    = "shithub: your account is suspended"
)

// Sentinel errors callers can errors.Is to map to friendly messages.
var (
	ErrSSHRepoNotFound = errors.New("ssh dispatch: repo not found")
	ErrSSHPermDenied   = errors.New("ssh dispatch: permission denied")
	ErrSSHArchived     = errors.New("ssh dispatch: archived")
	ErrSSHSuspended    = errors.New("ssh dispatch: suspended")
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

	owner, err := uq.GetUserByUsername(ctx, deps.Pool, parsed.Owner)
	if err != nil {
		// Unknown owner — surface as not-found regardless of whether
		// the row never existed or was soft-deleted.
		return nil, parsed, ErrSSHRepoNotFound
	}
	repo, err := rq.GetRepoByOwnerUserAndName(ctx, deps.Pool, reposdb.GetRepoByOwnerUserAndNameParams{
		OwnerUserID: pgtype.Int8{Int64: owner.ID, Valid: true},
		Name:        parsed.Repo,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, parsed, ErrSSHRepoNotFound
		}
		return nil, parsed, fmt.Errorf("%w: %v", ErrSSHInternal, err)
	}

	// Authz via policy.Can. Map the policy decision back to the typed
	// SSH errors so the friendly-message catalogue keeps working.
	repoRef := policy.NewRepoRefFromRepo(repo)
	actor := policy.UserActor(user.ID, user.Username, user.SuspendedAt.Valid, false)
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

	requestID, err := newRequestID()
	if err != nil {
		return nil, parsed, fmt.Errorf("%w: %v", ErrSSHInternal, err)
	}
	env := buildSSHEnv(user, owner, repo, in.RemoteIP, requestID)

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
	case errors.Is(err, ErrSSHSuspended):
		return MsgSuspended
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

// buildSSHEnv assembles the SHITHUB_* env vars that S14's hooks read.
// The shape matches the HTTP path so receive-pack hooks see identical
// vars regardless of transport.
func buildSSHEnv(user usersdb.User, owner usersdb.User, repo reposdb.Repo, remoteIP, requestID string) []string {
	return []string{
		"SHITHUB_USER_ID=" + strconv.FormatInt(user.ID, 10),
		"SHITHUB_USERNAME=" + user.Username,
		"SHITHUB_REPO_ID=" + strconv.FormatInt(repo.ID, 10),
		"SHITHUB_REPO_FULL_NAME=" + owner.Username + "/" + repo.Name,
		"SHITHUB_PROTOCOL=ssh",
		"SHITHUB_REMOTE_IP=" + remoteIP,
		"SHITHUB_REQUEST_ID=" + requestID,
		// PATH must be inherited so the exec'd git binary can find its
		// sub-helpers (git-pack-objects, git-index-pack, etc.).
		"PATH=" + os.Getenv("PATH"),
	}
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
