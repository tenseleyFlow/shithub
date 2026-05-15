// SPDX-License-Identifier: AGPL-3.0-or-later

package repo

import (
	"context"
	"testing"

	"github.com/tenseleyFlow/shithub/internal/infra/config"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
)

// PRO-EXT01-06 — privateTemplateCreateDenied gates the
// /new?template_repo_id=N path against the template *owner's* current
// plan. The consumer's plan is not consulted: Free users can spawn
// from a Pro user's private template, but if that owner's Pro lapses
// the template stops being usable.

// TestPrivateTemplateCreateDenied_PublicSkips confirms public templates
// fall through (the gate only applies to private templates).
func TestPrivateTemplateCreateDenied_PublicSkips(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t)
	markRepoAsTemplate(t, f, f.publicRepo.ID)
	template := reloadRepo(t, f, f.publicRepo.ID)

	denied, err := f.handlers.privateTemplateCreateDenied(context.Background(), template)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if denied {
		t.Fatalf("public template should never be denied")
	}
}

// TestPrivateTemplateCreateDenied_PrivateFreeOwnerReportOnly confirms a
// private template owned by a Free user is *not* denied in the default
// report-only mode (the would-deny is logged; create proceeds).
func TestPrivateTemplateCreateDenied_PrivateFreeOwnerReportOnly(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t)
	markRepoAsTemplate(t, f, f.privateRepo.ID)
	template := reloadRepo(t, f, f.privateRepo.ID)

	denied, err := f.handlers.privateTemplateCreateDenied(context.Background(), template)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if denied {
		t.Fatalf("report-only should not deny — would-deny is logged, not enforced")
	}
}

// TestPrivateTemplateCreateDenied_PrivateFreeOwnerEnforced confirms a
// private template owned by a Free user IS denied when the operator
// has flipped the enforce knob.
func TestPrivateTemplateCreateDenied_PrivateFreeOwnerEnforced(t *testing.T) {
	t.Parallel()
	f := newRepoFixtureWithEnforce(t, config.EnforceConfig{UserPrivateRepoTemplates: true})
	markRepoAsTemplate(t, f, f.privateRepo.ID)
	template := reloadRepo(t, f, f.privateRepo.ID)

	denied, err := f.handlers.privateTemplateCreateDenied(context.Background(), template)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if !denied {
		t.Fatalf("enforce should deny a Free-owned private template")
	}
}

// TestPrivateTemplateCreateDenied_PrivateProOwnerAllowed confirms a Pro
// owner's private template stays usable even with enforce on.
func TestPrivateTemplateCreateDenied_PrivateProOwnerAllowed(t *testing.T) {
	t.Parallel()
	f := newRepoFixtureWithEnforce(t, config.EnforceConfig{UserPrivateRepoTemplates: true})
	upgradeRepoFixtureOwnerToProForTemplate(t, f)
	markRepoAsTemplate(t, f, f.privateRepo.ID)
	template := reloadRepo(t, f, f.privateRepo.ID)

	denied, err := f.handlers.privateTemplateCreateDenied(context.Background(), template)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if denied {
		t.Fatalf("Pro-owned private template should never be denied")
	}
}

func markRepoAsTemplate(t *testing.T, f *repoFixture, repoID int64) {
	t.Helper()
	if _, err := f.pool.Exec(
		context.Background(),
		`UPDATE repos SET is_template = true WHERE id = $1`, repoID,
	); err != nil {
		t.Fatalf("UPDATE is_template: %v", err)
	}
}

func reloadRepo(t *testing.T, f *repoFixture, repoID int64) reposdb.Repo {
	t.Helper()
	row, err := reposdb.New().GetRepoByID(context.Background(), f.pool, repoID)
	if err != nil {
		t.Fatalf("GetRepoByID: %v", err)
	}
	return row
}
