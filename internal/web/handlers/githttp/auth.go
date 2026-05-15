// SPDX-License-Identifier: AGPL-3.0-or-later

package githttp

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"time"

	actionsdb "github.com/tenseleyFlow/shithub/internal/actions/sqlc"
	"github.com/tenseleyFlow/shithub/internal/auth/password"
	"github.com/tenseleyFlow/shithub/internal/auth/pat"
	"github.com/tenseleyFlow/shithub/internal/auth/runnerjwt"
)

// resolvedAuth carries the resolved identity for a git-over-HTTPS
// request. Anonymous = true is the "no Authorization header" case;
// callers decide whether anonymous is allowed (yes for pulling a public
// repo, no for everything else).
type resolvedAuth struct {
	Anonymous          bool
	UserID             int64
	Username           string
	ViaPAT             bool
	ViaRunnerCheckout  bool
	RunnerCheckoutRepo int64
	// PATRepoBinding is the repo this PAT is locked to (PRO-EXT01-11b)
	// or 0 if the token is unbound. Callers MUST compare against the
	// request's resolved repo before serving data when ViaPAT is true.
	PATRepoBinding int64
}

// errBadCredentials is the catch-all for "creds were sent but didn't
// resolve." We DON'T distinguish the failure reasons (wrong username,
// wrong password, revoked PAT, etc.) so the response is identical to
// "no creds at all" — preventing username probes via timing/messaging.
var errBadCredentials = errors.New("githttp: bad credentials")

// resolveBasicAuth parses the Authorization header and resolves it
// against the DB. Three outcomes:
//
//	Anonymous=true, err=nil         — no credentials supplied
//	Anonymous=false, err=nil         — credentials matched a real user
//	Anonymous=false, err!=nil        — credentials present but invalid
//
// PAT path is preferred when the secret carries the canonical
// `shithub_pat_` prefix; password path is the fallback. A failed PAT
// lookup falls through to password — if a user happens to set their
// account password to a string that starts with our PAT prefix, we
// still try to authenticate them.
func (h *Handlers) resolveBasicAuth(ctx context.Context, header string) (resolvedAuth, error) {
	if header == "" {
		return resolvedAuth{Anonymous: true}, nil
	}
	scheme, rest, ok := strings.Cut(header, " ")
	if !ok || !strings.EqualFold(scheme, "Basic") {
		return resolvedAuth{}, errBadCredentials
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(rest))
	if err != nil {
		return resolvedAuth{}, errBadCredentials
	}
	user, secret, ok := strings.Cut(string(decoded), ":")
	if !ok {
		return resolvedAuth{}, errBadCredentials
	}

	if got, err := h.resolveViaRunnerCheckout(ctx, secret); err == nil {
		return got, nil
	}
	if strings.HasPrefix(secret, pat.Prefix) {
		if got, err := h.resolveViaPAT(ctx, secret); err == nil {
			return got, nil
		}
		// Fall through — a non-matching PAT prefix could still be a
		// user-chosen password.
	}
	return h.resolveViaPassword(ctx, user, secret)
}

func (h *Handlers) resolveViaRunnerCheckout(ctx context.Context, raw string) (resolvedAuth, error) {
	if h.d.RunnerJWT == nil || raw == "" {
		return resolvedAuth{}, errBadCredentials
	}
	claims, err := h.d.RunnerJWT.Verify(raw)
	if err != nil || claims.Purpose != runnerjwt.PurposeCheckout {
		return resolvedAuth{}, errBadCredentials
	}
	runnerID, err := claims.RunnerID()
	if err != nil {
		return resolvedAuth{}, errBadCredentials
	}
	job, err := h.aq.GetWorkflowJobByID(ctx, h.d.Pool, claims.JobID)
	if err != nil {
		return resolvedAuth{}, errBadCredentials
	}
	run, err := h.aq.GetWorkflowRunByID(ctx, h.d.Pool, job.RunID)
	if err != nil {
		return resolvedAuth{}, errBadCredentials
	}
	if job.RunID != claims.RunID ||
		run.RepoID != claims.RepoID ||
		job.Status != actionsdb.WorkflowJobStatusRunning ||
		!job.RunnerID.Valid ||
		job.RunnerID.Int64 != runnerID {
		return resolvedAuth{}, errBadCredentials
	}
	return resolvedAuth{
		Username:           "shithub-actions",
		ViaRunnerCheckout:  true,
		RunnerCheckoutRepo: claims.RepoID,
	}, nil
}

// resolveViaPAT looks up the token by its sha256 hash, checks
// revoked/expired/suspended, and returns the owning user. Returns
// errBadCredentials on any failure (no leak about which check failed).
func (h *Handlers) resolveViaPAT(ctx context.Context, raw string) (resolvedAuth, error) {
	hash, err := pat.HashOf(raw)
	if err != nil {
		return resolvedAuth{}, errBadCredentials
	}
	row, err := h.uq.GetUserTokenByHash(ctx, h.d.Pool, hash)
	if err != nil {
		return resolvedAuth{}, errBadCredentials
	}
	if row.RevokedAt.Valid {
		return resolvedAuth{}, errBadCredentials
	}
	if row.ExpiresAt.Valid && time.Now().After(row.ExpiresAt.Time) {
		return resolvedAuth{}, errBadCredentials
	}
	user, err := h.uq.GetUserByID(ctx, h.d.Pool, row.UserID)
	if err != nil || user.SuspendedAt.Valid {
		return resolvedAuth{}, errBadCredentials
	}
	var binding int64
	if row.RepoID.Valid {
		binding = row.RepoID.Int64
	}
	return resolvedAuth{
		UserID: user.ID, Username: user.Username, ViaPAT: true,
		PATRepoBinding: binding,
	}, nil
}

// resolveViaPassword verifies the supplied secret against the user's
// argon2id hash. The username supplied in the Basic header is taken at
// face value here — git's credential prompt typically asks "Username
// for shithub:" and the user types their shithub username.
//
// Constant-time discipline: when the username doesn't exist we still
// run VerifyAgainstDummy so the response time is the same as a wrong
// password.
func (h *Handlers) resolveViaPassword(ctx context.Context, username, secret string) (resolvedAuth, error) {
	if username == "" {
		// No way to look up; still burn time on a dummy to avoid timing leaks.
		password.VerifyAgainstDummy(secret)
		return resolvedAuth{}, errBadCredentials
	}
	user, err := h.uq.GetUserByUsername(ctx, h.d.Pool, username)
	if err != nil {
		password.VerifyAgainstDummy(secret)
		return resolvedAuth{}, errBadCredentials
	}
	if user.SuspendedAt.Valid || user.DeletedAt.Valid {
		password.VerifyAgainstDummy(secret)
		return resolvedAuth{}, errBadCredentials
	}
	ok, err := password.Verify(secret, user.PasswordHash)
	if err != nil || !ok {
		return resolvedAuth{}, errBadCredentials
	}
	return resolvedAuth{
		UserID: user.ID, Username: user.Username, ViaPAT: false,
	}, nil
}
