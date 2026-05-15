// SPDX-License-Identifier: AGPL-3.0-or-later

package templateinit_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/repos/templateinit"
	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
)

const fixtureHash = "$argon2id$v=19$m=16384,t=1,p=1$" +
	"AAAAAAAAAAAAAAAA$" +
	"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

// makeUser inserts a fresh user and returns its row.
func makeUser(t *testing.T, ctx context.Context, pool reposdb.DBTX, username string) usersdb.User {
	t.Helper()
	u, err := usersdb.New().CreateUser(ctx, pool, usersdb.CreateUserParams{
		Username: username, DisplayName: username, PasswordHash: fixtureHash,
	})
	if err != nil {
		t.Fatalf("CreateUser %s: %v", username, err)
	}
	return u
}

// makeRepo inserts a repo with the given owner + visibility.
// `templated` flips is_template via UpdateRepoGeneralSettings.
func makeRepo(t *testing.T, ctx context.Context, pool reposdb.DBTX, ownerID int64, name, vis string, templated bool) reposdb.Repo {
	t.Helper()
	rq := reposdb.New()
	repo, err := rq.CreateRepo(ctx, pool, reposdb.CreateRepoParams{
		OwnerUserID:   pgtype.Int8{Int64: ownerID, Valid: true},
		Name:          name,
		DefaultBranch: "trunk",
		Visibility:    reposdb.RepoVisibility(vis),
	})
	if err != nil {
		t.Fatalf("CreateRepo %s: %v", name, err)
	}
	if templated {
		if err := rq.UpdateRepoGeneralSettings(ctx, pool, reposdb.UpdateRepoGeneralSettingsParams{
			ID: repo.ID, Description: repo.Description, HasIssues: repo.HasIssues, HasPulls: repo.HasPulls, IsTemplate: true,
		}); err != nil {
			t.Fatalf("UpdateRepoGeneralSettings %s: %v", name, err)
		}
		repo.IsTemplate = true
	}
	return repo
}

// TestCreate_RejectsNonTemplate verifies the orchestrator refuses a
// source repo that doesn't have is_template=true.
func TestCreate_RejectsNonTemplate(t *testing.T) {
	t.Parallel()
	pool := dbtest.NewTestDB(t)
	ctx := context.Background()
	user := makeUser(t, ctx, pool, "alice")
	source := makeRepo(t, ctx, pool, user.ID, "regular", "public", false)

	_, err := templateinit.Create(ctx, templateinit.Deps{Pool: pool}, templateinit.CreateParams{
		TemplateRepoID:  source.ID,
		ActorUserID:     user.ID,
		TargetOwnerKind: "user",
		TargetOwnerID:   user.ID,
		TargetName:      "from-template",
	})
	if !errors.Is(err, templateinit.ErrSourceNotTemplate) {
		t.Fatalf("expected ErrSourceNotTemplate, got: %v", err)
	}
}

// TestCreate_HappyPathProducesInitPendingRow verifies a successful
// create-from-template inserts the new repo at init_status=init_pending
// with no fork_of_repo_id set (independent repo, not a fork).
func TestCreate_HappyPathProducesInitPendingRow(t *testing.T) {
	t.Parallel()
	pool := dbtest.NewTestDB(t)
	ctx := context.Background()
	user := makeUser(t, ctx, pool, "alice")
	template := makeRepo(t, ctx, pool, user.ID, "starter", "public", true)

	res, err := templateinit.Create(ctx, templateinit.Deps{Pool: pool}, templateinit.CreateParams{
		TemplateRepoID:  template.ID,
		ActorUserID:     user.ID,
		TargetOwnerKind: "user",
		TargetOwnerID:   user.ID,
		TargetName:      "from-starter",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if res.Repo.Name != "from-starter" {
		t.Errorf("Name: got %q, want from-starter", res.Repo.Name)
	}
	if res.Repo.InitStatus != reposdb.RepoInitStatusInitPending {
		t.Errorf("InitStatus: got %q, want init_pending", res.Repo.InitStatus)
	}
	if res.Repo.ForkOfRepoID.Valid {
		t.Errorf("ForkOfRepoID set on template-init: got %d, want nil", res.Repo.ForkOfRepoID.Int64)
	}
	if res.Repo.IsTemplate {
		t.Errorf("IsTemplate set on new repo: should be false (templates copy content, not the template flag)")
	}
}

// TestCreate_VisibilityFloorEnforced verifies a private template can
// only produce a private target — public-target is rejected.
func TestCreate_VisibilityFloorEnforced(t *testing.T) {
	t.Parallel()
	pool := dbtest.NewTestDB(t)
	ctx := context.Background()
	user := makeUser(t, ctx, pool, "alice")
	template := makeRepo(t, ctx, pool, user.ID, "private-template", "private", true)

	_, err := templateinit.Create(ctx, templateinit.Deps{Pool: pool}, templateinit.CreateParams{
		TemplateRepoID:   template.ID,
		ActorUserID:      user.ID,
		TargetOwnerKind:  "user",
		TargetOwnerID:    user.ID,
		TargetName:       "from-private",
		TargetVisibility: "public",
	})
	if !errors.Is(err, templateinit.ErrVisibilityFloor) {
		t.Fatalf("expected ErrVisibilityFloor, got: %v", err)
	}

	// Same template, private target → succeeds.
	res, err := templateinit.Create(ctx, templateinit.Deps{Pool: pool}, templateinit.CreateParams{
		TemplateRepoID:   template.ID,
		ActorUserID:      user.ID,
		TargetOwnerKind:  "user",
		TargetOwnerID:    user.ID,
		TargetName:       "from-private",
		TargetVisibility: "private",
	})
	if err != nil {
		t.Fatalf("private→private should succeed: %v", err)
	}
	if string(res.Repo.Visibility) != "private" {
		t.Errorf("Visibility: got %q, want private", res.Repo.Visibility)
	}
}

// TestCreate_TargetNameTaken verifies a name collision in the target
// owner's namespace is rejected with ErrTargetNameTaken.
func TestCreate_TargetNameTaken(t *testing.T) {
	t.Parallel()
	pool := dbtest.NewTestDB(t)
	ctx := context.Background()
	user := makeUser(t, ctx, pool, "alice")
	template := makeRepo(t, ctx, pool, user.ID, "starter", "public", true)
	_ = makeRepo(t, ctx, pool, user.ID, "conflict", "public", false)

	_, err := templateinit.Create(ctx, templateinit.Deps{Pool: pool}, templateinit.CreateParams{
		TemplateRepoID:  template.ID,
		ActorUserID:     user.ID,
		TargetOwnerKind: "user",
		TargetOwnerID:   user.ID,
		TargetName:      "conflict",
	})
	if !errors.Is(err, templateinit.ErrTargetNameTaken) {
		t.Fatalf("expected ErrTargetNameTaken, got: %v", err)
	}
}
