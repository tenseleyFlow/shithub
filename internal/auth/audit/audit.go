// SPDX-License-Identifier: AGPL-3.0-or-later

// Package audit writes structured rows into the auth_audit_log table for
// security-relevant events. The schema is generic — actor, action, target,
// meta JSON — so future sprints reuse this for permission changes,
// org-membership changes, admin actions, etc.
//
// Callers MUST NOT put secret material in the meta JSON. The values land
// unredacted in the DB; treat the table contents as confidential but
// never sensitive (no plaintext passwords, no TOTP secrets, no PATs).
package audit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"

	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
)

// DBTX matches sqlc's DBTX so callers can pass *pgxpool.Pool or a tx.
type DBTX = usersdb.DBTX

// Action is a typed action constant. Reuse these across packages so the
// set stays tight and grep-able.
type Action string

const (
	Action2FAEnabled            Action = "2fa_enabled"
	Action2FADisabled           Action = "2fa_disabled"
	ActionRecoveryCodesIssued   Action = "recovery_codes_issued"
	ActionRecoveryCodeUsed      Action = "recovery_code_used"
	ActionRecoveryRegenerated   Action = "recovery_codes_regenerated"
	ActionAdminCleared2FA       Action = "admin_cleared_2fa"
	ActionPasswordChanged       Action = "password_changed"
	ActionPasswordReset         Action = "password_reset_consumed"
	ActionLoginSucceeded        Action = "login_succeeded"
	ActionLoginFailedThrottled  Action = "login_failed_throttled"
	ActionAccountSuspended      Action = "account_suspended"
	ActionSSHKeyAdded           Action = "ssh_key_added"
	ActionSSHKeyDeleted         Action = "ssh_key_deleted"
	ActionPATCreated            Action = "pat_created"
	ActionPATRevoked            Action = "pat_revoked"
	ActionUsernameChanged       Action = "username_changed"
	ActionAccountDeleted        Action = "account_deleted"
	ActionAccountRestored       Action = "account_restored"
	ActionRepoCreated           Action = "repo_created"
	ActionRepoRenamed           Action = "repo_renamed"
	ActionRepoArchived          Action = "repo_archived"
	ActionRepoUnarchived        Action = "repo_unarchived"
	ActionRepoVisibilityChanged Action = "repo_visibility_changed"
	ActionRepoSoftDeleted       Action = "repo_soft_deleted"
	ActionRepoRestored          Action = "repo_restored"
	ActionRepoHardDeleted       Action = "repo_hard_deleted"
	ActionRepoTransferRequested Action = "repo_transfer_requested"
	ActionRepoTransferAccepted  Action = "repo_transfer_accepted"
	ActionRepoTransferDeclined  Action = "repo_transfer_declined"
	ActionRepoTransferCanceled  Action = "repo_transfer_canceled"
	ActionRepoTransferExpired   Action = "repo_transfer_expired"
	ActionIssueStateChanged     Action = "issue_state_changed"
	ActionIssueLockChanged      Action = "issue_lock_changed"
	ActionIssueCommentCreated   Action = "issue_comment_created"
	ActionPullStateChanged      Action = "pull_state_changed"
	ActionPullMerged            Action = "pull_merged"
	ActionStarCreated           Action = "star_created"
	ActionStarDeleted           Action = "star_deleted"
	ActionWatchSet              Action = "watch_set"
	ActionWatchUnset            Action = "watch_unset"
	ActionRepoForked            Action = "repo_forked"
	ActionRepoForkSynced        Action = "repo_fork_synced"

	// S34 — site admin actions. Always recorded with the real admin's
	// id in actor_id; impersonation flows additionally carry the
	// impersonated user's id in meta.impersonated_user_id.
	ActionAdminSiteAdminGranted   Action = "admin_site_admin_granted"
	ActionAdminSiteAdminRevoked   Action = "admin_site_admin_revoked"
	ActionAdminUserSuspended      Action = "admin_user_suspended"
	ActionAdminUserUnsuspended    Action = "admin_user_unsuspended"
	ActionAdminUserForceDeleted   Action = "admin_user_force_deleted"
	ActionAdminUserPasswordReset  Action = "admin_user_password_reset"
	ActionAdminRepoForceArchived  Action = "admin_repo_force_archived"
	ActionAdminRepoForceDeleted   Action = "admin_repo_force_deleted"
	ActionAdminJobRetried         Action = "admin_job_retried"
	ActionAdminJobDiscarded       Action = "admin_job_discarded"
	ActionAdminImpersonateStarted Action = "admin_impersonate_started"
	ActionAdminImpersonateStopped Action = "admin_impersonate_stopped"
	ActionAdminImpersonateWriteOn Action = "admin_impersonate_write_on"
)

// Target is a typed target-type constant.
type Target string

const (
	TargetUser  Target = "user"
	TargetRepo  Target = "repo"
	TargetIssue Target = "issue"
	TargetPull  Target = "pull"
)

// Recorder writes audit rows. Bound to the sqlc queries handle.
type Recorder struct {
	q *usersdb.Queries
}

// NewRecorder constructs a recorder.
func NewRecorder() *Recorder { return &Recorder{q: usersdb.New()} }

// Record inserts a single audit-log row. actorID is the user_id of the
// actor performing the action (use 0 for system / admin-CLI actions).
// targetID is the affected entity (use 0 for actions without a single
// concrete target).
//
// meta MUST be a JSON-serializable map; secrets and PII (tokens, raw
// passwords, etc.) MUST NOT appear here.
func (r *Recorder) Record(
	ctx context.Context, db DBTX,
	actorID int64, action Action, target Target, targetID int64, meta map[string]any,
) error {
	if action == "" || target == "" {
		return errors.New("audit: action and target_type required")
	}
	if meta == nil {
		meta = map[string]any{}
	}
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("audit: marshal meta: %w", err)
	}
	var actorParam pgtype.Int8
	if actorID != 0 {
		actorParam = pgtype.Int8{Int64: actorID, Valid: true}
	}
	var targetParam pgtype.Int8
	if targetID != 0 {
		targetParam = pgtype.Int8{Int64: targetID, Valid: true}
	}
	return r.q.InsertAuditLog(ctx, db, usersdb.InsertAuditLogParams{
		ActorID:    actorParam,
		Action:     string(action),
		TargetType: string(target),
		TargetID:   targetParam,
		Meta:       metaJSON,
	})
}
