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
	Action2FAEnabled           Action = "2fa_enabled"
	Action2FADisabled          Action = "2fa_disabled"
	ActionRecoveryCodesIssued  Action = "recovery_codes_issued"
	ActionRecoveryCodeUsed     Action = "recovery_code_used"
	ActionRecoveryRegenerated  Action = "recovery_codes_regenerated"
	ActionAdminCleared2FA      Action = "admin_cleared_2fa"
	ActionPasswordChanged      Action = "password_changed"
	ActionPasswordReset        Action = "password_reset_consumed"
	ActionLoginSucceeded       Action = "login_succeeded"
	ActionLoginFailedThrottled Action = "login_failed_throttled"
	ActionAccountSuspended     Action = "account_suspended"
	ActionSSHKeyAdded          Action = "ssh_key_added"
	ActionSSHKeyDeleted        Action = "ssh_key_deleted"
	ActionGPGKeyAdded          Action = "gpg_key_added"
	ActionGPGKeyDeleted        Action = "gpg_key_deleted"
	ActionPATCreated           Action = "pat_created"
	ActionPATRevoked           Action = "pat_revoked"
	// Device-flow OAuth (S55). One row per state transition. Issuance
	// of the eventual PAT continues to fire ActionPATCreated; these
	// three constants exist so forensics can correlate
	// `device_grant_*` actions with the PAT row issued at Exchange.
	ActionDeviceGrantRequested  Action = "device_grant_requested"
	ActionDeviceGrantApproved   Action = "device_grant_approved"
	ActionDeviceGrantDenied     Action = "device_grant_denied"
	ActionUsernameChanged       Action = "username_changed"
	ActionAccountDeleted        Action = "account_deleted"
	ActionAccountRestored       Action = "account_restored"
	ActionRepoCreated           Action = "repo_created"
	ActionRepoRenamed           Action = "repo_renamed"
	ActionRepoArchived          Action = "repo_archived"
	ActionRepoUnarchived        Action = "repo_unarchived"
	ActionRepoPaused            Action = "repo_paused"
	ActionRepoUnpaused          Action = "repo_unpaused"
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
	// G8 (F45): hard-deletion of an issue row. Security-sensitive
	// because the cascade removes comments + history; gated on
	// ActionRepoAdmin in the API layer.
	ActionIssueDeleted     Action = "issue_deleted"
	ActionPullStateChanged Action = "pull_state_changed"
	ActionPullMerged       Action = "pull_merged"
	ActionStarCreated      Action = "star_created"
	ActionStarDeleted      Action = "star_deleted"
	ActionWatchSet         Action = "watch_set"
	ActionWatchUnset       Action = "watch_unset"
	ActionFollowCreated    Action = "follow_created"
	ActionFollowDeleted    Action = "follow_deleted"
	ActionRepoForked       Action = "repo_forked"
	ActionRepoForkSynced   Action = "repo_fork_synced"

	// S33 / SR2 H1 — webhook lifecycle. Pre-SR2 webhook create/update
	// overloaded ActionRepoCreated; delete/toggle/ping/redeliver were
	// not audited at all. Each event now has its own enum.
	ActionWebhookCreated     Action = "webhook_created"
	ActionWebhookUpdated     Action = "webhook_updated"
	ActionWebhookDeleted     Action = "webhook_deleted"
	ActionWebhookActiveSet   Action = "webhook_active_set"
	ActionWebhookActiveUnset Action = "webhook_active_unset"
	ActionWebhookPinged      Action = "webhook_pinged"
	ActionWebhookRedelivered Action = "webhook_redelivered"

	// S41c — Actions secret/variable lifecycle. Metadata must include
	// names only, never secret values.
	ActionActionsSecretSet       Action = "actions_secret_set"
	ActionActionsSecretDeleted   Action = "actions_secret_deleted"
	ActionActionsVariableSet     Action = "actions_variable_set"
	ActionActionsVariableDeleted Action = "actions_variable_deleted"
	ActionActionsPolicyUpdated   Action = "actions_policy_updated"
	ActionOrgRequired2FAUpdated  Action = "org_required_2fa_updated"

	// SP26b — secret push-protection bypass lifecycle. Metadata must
	// stay structural: request id, pattern name, path, line, commit oid,
	// and expiry. Never include raw match bytes or excerpts.
	ActionSecretBypassRequested Action = "secret_bypass_requested"
	ActionSecretBypassApproved  Action = "secret_bypass_approved"
	ActionSecretBypassDenied    Action = "secret_bypass_denied"

	// S41h — Actions run/job lifecycle. Metadata must stay structural:
	// run/job ids, status, conclusion, workflow path/name. Never include
	// event payloads, env, logs, permissions, tokens, or secret values.
	ActionWorkflowRunCreated   Action = "workflow_run_created"
	ActionWorkflowRunStarted   Action = "workflow_run_started"
	ActionWorkflowRunCompleted Action = "workflow_run_completed"
	ActionWorkflowJobCreated   Action = "workflow_job_created"
	ActionWorkflowJobStarted   Action = "workflow_job_started"
	ActionWorkflowJobCompleted Action = "workflow_job_completed"
	ActionWorkflowJobCancelled Action = "workflow_job_cancelled"
	ActionWorkflowRunApproved  Action = "workflow_run_approved"
	ActionWorkflowRunRejected  Action = "workflow_run_rejected"

	// S34 — site admin actions. Always recorded with the real admin's
	// id in actor_id; impersonation flows additionally carry the
	// impersonated user's id in meta.impersonated_user_id.
	ActionAdminSiteAdminGranted        Action = "admin_site_admin_granted"
	ActionAdminSiteAdminRevoked        Action = "admin_site_admin_revoked"
	ActionAdminUserSuspended           Action = "admin_user_suspended"
	ActionAdminUserUnsuspended         Action = "admin_user_unsuspended"
	ActionAdminUserForceDeleted        Action = "admin_user_force_deleted"
	ActionAdminUserPasswordReset       Action = "admin_user_password_reset"
	ActionAdminRepoForceArchived       Action = "admin_repo_force_archived"
	ActionAdminRepoForceUnarchived     Action = "admin_repo_force_unarchived"
	ActionAdminRepoForceDeleted        Action = "admin_repo_force_deleted"
	ActionAdminJobRetried              Action = "admin_job_retried"
	ActionAdminJobDiscarded            Action = "admin_job_discarded"
	ActionAdminImpersonateStarted      Action = "admin_impersonate_started"
	ActionAdminImpersonateStopped      Action = "admin_impersonate_stopped"
	ActionAdminImpersonateWriteOn      Action = "admin_impersonate_write_on"
	ActionAdminOrgBillingSeatsRepaired Action = "admin_org_billing_seats_repaired"
)

// Target is a typed target-type constant.
type Target string

const (
	TargetUser        Target = "user"
	TargetRepo        Target = "repo"
	TargetOrg         Target = "org"
	TargetIssue       Target = "issue"
	TargetPull        Target = "pull"
	TargetDeviceGrant Target = "device_grant"
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
