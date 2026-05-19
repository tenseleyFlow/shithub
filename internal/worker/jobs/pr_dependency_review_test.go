// SPDX-License-Identifier: AGPL-3.0-or-later

package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/auth/audit"
	"github.com/tenseleyFlow/shithub/internal/auth/throttle"
	"github.com/tenseleyFlow/shithub/internal/billing"
	checksdb "github.com/tenseleyFlow/shithub/internal/checks/sqlc"
	"github.com/tenseleyFlow/shithub/internal/infra/storage"
	"github.com/tenseleyFlow/shithub/internal/orgs"
	"github.com/tenseleyFlow/shithub/internal/pulls"
	pullsdb "github.com/tenseleyFlow/shithub/internal/pulls/sqlc"
	"github.com/tenseleyFlow/shithub/internal/repos"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
)

func TestPRDependencyReview_TeamOrgPersistsFailureCheck(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := setupPRDependencyReviewFixture(t, true)

	if _, err := reposdb.New().UpsertDependencyAdvisory(ctx, env.pool, reposdb.UpsertDependencyAdvisoryParams{
		Source:          "test-fixture",
		ExternalID:      "GHSA-pr-deps",
		Ecosystem:       "go",
		PackageName:     "example.test/vulnerable",
		AffectedRange:   ">= v1.0.0, < v1.2.4",
		PatchedVersions: "v1.2.4",
		Severity:        "high",
		Summary:         "Fixture PR dependency vulnerability",
		Description:     "Only used by the dependency review test.",
		ReferenceUrls:   []byte("[]"),
	}); err != nil {
		t.Fatalf("UpsertDependencyAdvisory: %v", err)
	}

	if err := env.run(ctx); err != nil {
		t.Fatalf("PRDependencyReview: %v", err)
	}

	pq := pullsdb.New()
	review, err := pq.GetPullDependencyReviewForHead(ctx, env.pool, pullsdb.GetPullDependencyReviewForHeadParams{
		PrID:    env.pr.PullRequest.IssueID,
		HeadSha: env.pr.PullRequest.HeadOid,
	})
	if err != nil {
		t.Fatalf("GetPullDependencyReviewForHead: %v", err)
	}
	if review.Conclusion != "failure" || review.ChangeCount != 1 || review.VulnerableChangeCount != 1 {
		t.Fatalf("review = %+v, want one failing vulnerable change", review)
	}
	items, err := pq.ListPullDependencyReviewItems(ctx, env.pool, review.ID)
	if err != nil {
		t.Fatalf("ListPullDependencyReviewItems: %v", err)
	}
	if len(items) != 1 || items[0].PackageName != "example.test/vulnerable" || items[0].Severity != "high" {
		t.Fatalf("items = %+v, want high vulnerable dependency row", items)
	}

	run, err := checksdb.New().GetLatestCheckRunByName(ctx, env.pool, checksdb.GetLatestCheckRunByNameParams{
		RepoID:  env.repo.ID,
		HeadSha: env.pr.PullRequest.HeadOid,
		Name:    DependencyReviewCheckName,
	})
	if err != nil {
		t.Fatalf("GetLatestCheckRunByName: %v", err)
	}
	if run.Status != checksdb.CheckStatusCompleted || !run.Conclusion.Valid || run.Conclusion.CheckConclusion != checksdb.CheckConclusionFailure {
		t.Fatalf("check run = %+v, want completed failure", run)
	}
}

func TestPRDependencyReview_FreeOrgPublishesUpgradeOnlyCheck(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := setupPRDependencyReviewFixture(t, false)

	if err := env.run(ctx); err != nil {
		t.Fatalf("PRDependencyReview: %v", err)
	}

	if _, err := pullsdb.New().GetPullDependencyReviewForHead(ctx, env.pool, pullsdb.GetPullDependencyReviewForHeadParams{
		PrID:    env.pr.PullRequest.IssueID,
		HeadSha: env.pr.PullRequest.HeadOid,
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("free org review err = %v, want pgx.ErrNoRows", err)
	}
	run, err := checksdb.New().GetLatestCheckRunByName(ctx, env.pool, checksdb.GetLatestCheckRunByNameParams{
		RepoID:  env.repo.ID,
		HeadSha: env.pr.PullRequest.HeadOid,
		Name:    DependencyReviewCheckName,
	})
	if err != nil {
		t.Fatalf("GetLatestCheckRunByName: %v", err)
	}
	if run.Status != checksdb.CheckStatusCompleted || !run.Conclusion.Valid || run.Conclusion.CheckConclusion != checksdb.CheckConclusionActionRequired {
		t.Fatalf("check run = %+v, want completed action_required", run)
	}
}

type prDependencyReviewFixture struct {
	pool *pgxpool.Pool
	rfs  *storage.RepoFS
	log  *slog.Logger
	repo reposdb.Repo
	pr   pulls.CreateResult
}

func setupPRDependencyReviewFixture(t *testing.T, team bool) prDependencyReviewFixture {
	t.Helper()
	ctx := context.Background()
	pool := dbtest.NewTestDB(t)
	rfs, err := storage.NewRepoFS(t.TempDir())
	if err != nil {
		t.Fatalf("NewRepoFS: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	uq := usersdb.New()
	user, err := uq.CreateUser(ctx, pool, usersdb.CreateUserParams{
		Username: "alice", DisplayName: "Alice", PasswordHash: prJobsFixtureHash,
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	email, err := uq.CreateUserEmail(ctx, pool, usersdb.CreateUserEmailParams{
		UserID: user.ID, Email: "alice@example.com", IsPrimary: true, Verified: true,
	})
	if err != nil {
		t.Fatalf("CreateUserEmail: %v", err)
	}
	if err := uq.LinkUserPrimaryEmail(ctx, pool, usersdb.LinkUserPrimaryEmailParams{
		ID: user.ID, PrimaryEmailID: pgtype.Int8{Int64: email.ID, Valid: true},
	}); err != nil {
		t.Fatalf("LinkUserPrimaryEmail: %v", err)
	}

	org, err := orgs.Create(ctx, orgs.Deps{Pool: pool, Logger: logger}, orgs.CreateParams{
		Slug: "acme", DisplayName: "Acme", CreatedByUserID: user.ID,
	})
	if err != nil {
		t.Fatalf("orgs.Create: %v", err)
	}
	if team {
		now := time.Now().UTC().Truncate(time.Second)
		if _, err := billing.ApplySubscriptionSnapshot(ctx, billing.Deps{Pool: pool}, billing.SubscriptionSnapshot{
			OrgID:                    org.ID,
			Plan:                     billing.PlanTeam,
			Status:                   billing.SubscriptionStatusActive,
			StripeSubscriptionID:     "sub_test",
			StripeSubscriptionItemID: "si_test",
			LicensedSeats:            1,
			CurrentPeriodStart:       now,
			CurrentPeriodEnd:         now.Add(30 * 24 * time.Hour),
			LastWebhookEventID:       "evt_dep_review",
		}); err != nil {
			t.Fatalf("ApplySubscriptionSnapshot: %v", err)
		}
	}

	res, err := repos.Create(ctx, repos.Deps{
		Pool: pool, RepoFS: rfs, Audit: audit.NewRecorder(), Limiter: throttle.NewLimiter(), Logger: logger,
	}, repos.Params{
		OwnerOrgID: org.ID, OwnerSlug: string(org.Slug), ActorUserID: user.ID,
		Name: "demo", Visibility: "public", InitReadme: true,
	})
	if err != nil {
		t.Fatalf("repos.Create: %v", err)
	}
	gitDir, err := rfs.RepoPath(string(org.Slug), res.Repo.Name)
	if err != nil {
		t.Fatalf("RepoPath: %v", err)
	}
	prJobsCommitOnBranch(t, gitDir, "trunk", "add dependencies", "go.mod", "module example.test/app\n")
	prJobsCommitOnBranch(t, gitDir, "feature", "add vulnerable dependency", "go.mod", `module example.test/app

require example.test/vulnerable v1.2.3
`)

	pr, err := pulls.Create(ctx, pulls.Deps{Pool: pool, Logger: logger}, pulls.CreateParams{
		RepoID:       res.Repo.ID,
		AuthorUserID: user.ID,
		Title:        "Add dependency",
		BaseRef:      "trunk",
		HeadRef:      "feature",
		GitDir:       gitDir,
	})
	if err != nil {
		t.Fatalf("pulls.Create: %v", err)
	}
	return prDependencyReviewFixture{
		pool: pool,
		rfs:  rfs,
		log:  logger,
		repo: res.Repo,
		pr:   pr,
	}
}

func (f prDependencyReviewFixture) run(ctx context.Context) error {
	payload, _ := json.Marshal(PRDependencyReviewPayload{PRID: f.pr.PullRequest.IssueID})
	return PRDependencyReview(PRJobsDeps{Pool: f.pool, RepoFS: f.rfs, Logger: f.log})(ctx, payload)
}
