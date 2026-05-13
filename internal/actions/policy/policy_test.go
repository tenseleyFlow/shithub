// SPDX-License-Identifier: AGPL-3.0-or-later

package actionspolicy_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	actionspolicy "github.com/tenseleyFlow/shithub/internal/actions/policy"
	actionsdb "github.com/tenseleyFlow/shithub/internal/actions/sqlc"
	"github.com/tenseleyFlow/shithub/internal/actions/trigger"
	orgsdb "github.com/tenseleyFlow/shithub/internal/orgs/sqlc"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
)

const policyFixtureHash = "$argon2id$v=19$m=16384,t=1,p=1$" +
	"AAAAAAAAAAAAAAAA$" +
	"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

type policyFx struct {
	ctx       context.Context
	pool      *pgxpool.Pool
	owner     usersdb.User
	writeUser usersdb.User
	readUser  usersdb.User
	outsider  usersdb.User
	suspended usersdb.User
	siteAdmin usersdb.User
	orgOwner  usersdb.User
	repo      reposdb.Repo
	orgRepo   reposdb.Repo
}

func setupPolicyFx(t *testing.T) policyFx {
	t.Helper()
	pool := dbtest.NewTestDB(t)
	ctx := context.Background()
	uq := usersdb.New()
	mkUser := func(name string) usersdb.User {
		t.Helper()
		u, err := uq.CreateUser(ctx, pool, usersdb.CreateUserParams{
			Username: name, DisplayName: name, PasswordHash: policyFixtureHash,
		})
		if err != nil {
			t.Fatalf("CreateUser %s: %v", name, err)
		}
		return u
	}
	owner := mkUser("owner")
	writeUser := mkUser("writer")
	readUser := mkUser("reader")
	outsider := mkUser("outsider")
	suspended := mkUser("suspended")
	siteAdmin := mkUser("siteadmin")
	orgOwner := mkUser("orgowner")
	if _, err := pool.Exec(ctx, `UPDATE users SET suspended_at = now() WHERE id = $1`, suspended.ID); err != nil {
		t.Fatalf("suspend user: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE users SET is_site_admin = true WHERE id = $1`, siteAdmin.ID); err != nil {
		t.Fatalf("site admin user: %v", err)
	}
	rq := reposdb.New()
	repo, err := rq.CreateRepo(ctx, pool, reposdb.CreateRepoParams{
		OwnerUserID:   pgtype.Int8{Int64: owner.ID, Valid: true},
		Name:          "demo",
		DefaultBranch: "trunk",
		Visibility:    reposdb.RepoVisibilityPublic,
	})
	if err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO repo_collaborators (repo_id, user_id, role, added_by_user_id) VALUES ($1, $2, 'write', $3), ($1, $4, 'read', $3)`,
		repo.ID, writeUser.ID, owner.ID, readUser.ID); err != nil {
		t.Fatalf("insert collaborators: %v", err)
	}
	org, err := orgsdb.New().CreateOrg(ctx, pool, orgsdb.CreateOrgParams{
		Slug:            "acme",
		DisplayName:     "Acme",
		CreatedByUserID: pgtype.Int8{Int64: orgOwner.ID, Valid: true},
	})
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO org_members (org_id, user_id, role) VALUES ($1, $2, 'owner')`, org.ID, orgOwner.ID); err != nil {
		t.Fatalf("insert org owner: %v", err)
	}
	orgRepo, err := rq.CreateRepo(ctx, pool, reposdb.CreateRepoParams{
		OwnerOrgID:    pgtype.Int8{Int64: org.ID, Valid: true},
		Name:          "org-demo",
		DefaultBranch: "trunk",
		Visibility:    reposdb.RepoVisibilityPublic,
	})
	if err != nil {
		t.Fatalf("CreateRepo org: %v", err)
	}
	return policyFx{
		ctx:       ctx,
		pool:      pool,
		owner:     owner,
		writeUser: writeUser,
		readUser:  readUser,
		outsider:  outsider,
		suspended: suspended,
		siteAdmin: siteAdmin,
		orgOwner:  orgOwner,
		repo:      repo,
		orgRepo:   orgRepo,
	}
}

func TestEvaluateTrigger_TrustMatrix(t *testing.T) {
	t.Parallel()
	f := setupPolicyFx(t)
	deps := actionspolicy.Deps{Pool: f.pool}
	tests := []struct {
		name         string
		repo         reposdb.Repo
		actorID      int64
		event        trigger.EventKind
		wantAllow    bool
		wantApproval bool
		wantErr      error
	}{
		{name: "owner push runs", repo: f.repo, actorID: f.owner.ID, event: trigger.EventPush, wantAllow: true},
		{name: "write collaborator push runs", repo: f.repo, actorID: f.writeUser.ID, event: trigger.EventPush, wantAllow: true},
		{name: "org owner push runs", repo: f.orgRepo, actorID: f.orgOwner.ID, event: trigger.EventPush, wantAllow: true},
		{name: "read collaborator pr pauses for approval", repo: f.repo, actorID: f.readUser.ID, event: trigger.EventPullRequest, wantAllow: true, wantApproval: true},
		{name: "outsider pr pauses for approval", repo: f.repo, actorID: f.outsider.ID, event: trigger.EventPullRequest, wantAllow: true, wantApproval: true},
		{name: "outsider push denied", repo: f.repo, actorID: f.outsider.ID, event: trigger.EventPush, wantErr: actionspolicy.ErrUnauthorized},
		{name: "suspended actor denied", repo: f.repo, actorID: f.suspended.ID, event: trigger.EventPush, wantErr: actionspolicy.ErrUnauthorized},
		{name: "site admin write does not bypass repo role", repo: f.repo, actorID: f.siteAdmin.ID, event: trigger.EventPush, wantErr: actionspolicy.ErrUnauthorized},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			dec, err := actionspolicy.EvaluateTrigger(f.ctx, deps, actionspolicy.TriggerRequest{
				Repo:        tt.repo,
				EventKind:   string(tt.event),
				ActorUserID: tt.actorID,
			})
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err=%v, want %v (decision=%+v)", err, tt.wantErr, dec)
				}
				return
			}
			if err != nil {
				t.Fatalf("EvaluateTrigger: %v", err)
			}
			if dec.Allow != tt.wantAllow || dec.NeedApproval != tt.wantApproval {
				t.Fatalf("decision=%+v, want allow=%t approval=%t", dec, tt.wantAllow, tt.wantApproval)
			}
		})
	}
}

func TestEvaluateTrigger_PullRequestApprovalPolicyCanAllowImmediateRun(t *testing.T) {
	t.Parallel()
	f := setupPolicyFx(t)
	if _, err := actionsdb.New().UpsertActionsRepoPolicy(f.ctx, f.pool, actionsdb.UpsertActionsRepoPolicyParams{
		RepoID:            f.repo.ID,
		ActionsEnabled:    actionsdb.ActionsPolicyStateInherit,
		RequirePrApproval: pgtype.Bool{Bool: false, Valid: true},
	}); err != nil {
		t.Fatalf("UpsertActionsRepoPolicy: %v", err)
	}

	dec, err := actionspolicy.EvaluateTrigger(f.ctx, actionspolicy.Deps{Pool: f.pool}, actionspolicy.TriggerRequest{
		Repo:        f.repo,
		EventKind:   string(trigger.EventPullRequest),
		ActorUserID: f.outsider.ID,
	})
	if err != nil {
		t.Fatalf("EvaluateTrigger: %v", err)
	}
	if !dec.Allow || dec.NeedApproval {
		t.Fatalf("decision=%+v, want immediate PR run", dec)
	}
}

func TestEvaluateTrigger_DeniesArchivedAndDisabledRepos(t *testing.T) {
	t.Parallel()
	f := setupPolicyFx(t)
	archived := f.repo
	archived.IsArchived = true
	if dec, err := actionspolicy.EvaluateTrigger(f.ctx, actionspolicy.Deps{Pool: f.pool}, actionspolicy.TriggerRequest{
		Repo:        archived,
		EventKind:   string(trigger.EventPush),
		ActorUserID: f.owner.ID,
	}); !errors.Is(err, actionspolicy.ErrUnauthorized) || dec.Allow {
		t.Fatalf("archived decision=%+v err=%v", dec, err)
	}

	if _, err := actionsdb.New().UpsertActionsRepoPolicy(f.ctx, f.pool, actionsdb.UpsertActionsRepoPolicyParams{
		RepoID:         f.repo.ID,
		ActionsEnabled: actionsdb.ActionsPolicyStateDisabled,
	}); err != nil {
		t.Fatalf("UpsertActionsRepoPolicy: %v", err)
	}
	if dec, err := actionspolicy.EvaluateTrigger(f.ctx, actionspolicy.Deps{Pool: f.pool}, actionspolicy.TriggerRequest{
		Repo:        f.repo,
		EventKind:   string(trigger.EventPush),
		ActorUserID: f.owner.ID,
	}); !errors.Is(err, actionspolicy.ErrActionsDisabled) || dec.Allow {
		t.Fatalf("disabled decision=%+v err=%v", dec, err)
	}
}

func TestEvaluateTrigger_EnforcesQueueAndActorCaps(t *testing.T) {
	t.Parallel()
	f := setupPolicyFx(t)
	q := actionsdb.New()
	if _, err := q.UpsertActionsRepoPolicy(f.ctx, f.pool, actionsdb.UpsertActionsRepoPolicyParams{
		RepoID:                   f.repo.ID,
		ActionsEnabled:           actionsdb.ActionsPolicyStateInherit,
		MaxRepoQueuedRuns:        pgtype.Int4{Int32: 1, Valid: true},
		ActorTriggerLimitPerHour: pgtype.Int4{Int32: 1, Valid: true},
	}); err != nil {
		t.Fatalf("UpsertActionsRepoPolicy: %v", err)
	}
	if _, err := q.InsertWorkflowRun(f.ctx, f.pool, actionsdb.InsertWorkflowRunParams{
		RepoID:       f.repo.ID,
		RunIndex:     1,
		WorkflowFile: ".shithub/workflows/ci.yml",
		WorkflowName: "CI",
		HeadSha:      strings.Repeat("a", 40),
		HeadRef:      "refs/heads/trunk",
		Event:        actionsdb.WorkflowRunEventPush,
		EventPayload: []byte("{}"),
		ActorUserID:  pgtype.Int8{Int64: f.owner.ID, Valid: true},
		NeedApproval: false,
	}); err != nil {
		t.Fatalf("InsertWorkflowRun: %v", err)
	}
	dec, err := actionspolicy.EvaluateTrigger(f.ctx, actionspolicy.Deps{Pool: f.pool}, actionspolicy.TriggerRequest{
		Repo:        f.repo,
		EventKind:   string(trigger.EventPush),
		ActorUserID: f.owner.ID,
	})
	if !errors.Is(err, actionspolicy.ErrRepoQueuedCap) || dec.Allow {
		t.Fatalf("queue cap decision=%+v err=%v", dec, err)
	}
	if _, err := f.pool.Exec(f.ctx, `
		UPDATE workflow_runs
		   SET status = 'completed',
		       conclusion = 'success',
		       started_at = now(),
		       completed_at = now()
		 WHERE repo_id = $1`,
		f.repo.ID); err != nil {
		t.Fatalf("complete queued run: %v", err)
	}
	dec, err = actionspolicy.EvaluateTrigger(f.ctx, actionspolicy.Deps{Pool: f.pool}, actionspolicy.TriggerRequest{
		Repo:        f.repo,
		EventKind:   string(trigger.EventPush),
		ActorUserID: f.owner.ID,
	})
	if !errors.Is(err, actionspolicy.ErrActorRateLimit) || dec.Allow {
		t.Fatalf("actor cap decision=%+v err=%v", dec, err)
	}
}
