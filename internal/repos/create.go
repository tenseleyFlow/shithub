// SPDX-License-Identifier: AGPL-3.0-or-later

package repos

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/auth/audit"
	"github.com/tenseleyFlow/shithub/internal/auth/throttle"
	"github.com/tenseleyFlow/shithub/internal/entitlements"
	"github.com/tenseleyFlow/shithub/internal/git/hooks"
	"github.com/tenseleyFlow/shithub/internal/infra/storage"
	"github.com/tenseleyFlow/shithub/internal/issues"
	issuesdb "github.com/tenseleyFlow/shithub/internal/issues/sqlc"
	"github.com/tenseleyFlow/shithub/internal/notif"
	repogit "github.com/tenseleyFlow/shithub/internal/repos/git"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/repos/templates"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
)

// CreateRateLimit caps how many repos one user can create per hour.
// 20/hour leaves headroom for bulk-migrating an existing org without
// being a license to spam; site admins bypass the cap entirely.
const (
	CreateRateLimitMax    = 20
	CreateRateLimitWindow = time.Hour
)

// Deps wires the repo orchestrator. Inject from the web layer; no
// global state.
type Deps struct {
	Pool    *pgxpool.Pool
	RepoFS  *storage.RepoFS
	Audit   *audit.Recorder
	Limiter *throttle.Limiter
	Logger  *slog.Logger
	Now     func() time.Time
	// ShithubdPath is the absolute path to the running shithubd binary,
	// baked into the hook shims so push -> hook -> shithubd round-trip
	// works in dev and prod. Empty disables hook installation (tests
	// that don't care about hooks; the full E2E happy path provides it).
	ShithubdPath string
}

// Params describes one repo-create request as it arrives from the
// handler, normalized but not yet validated against the DB.
//
// Owner is XOR (S30): either OwnerUserID set OR OwnerOrgID set.
// OwnerUsername / OwnerSlug carry the slug for path generation
// (the FS layer's per-owner directory uses it). ActorUserID is who
// initiated the create — defaults to OwnerUserID for personal repos
// and is required for org-owned creates.
type Params struct {
	OwnerUserID   int64
	OwnerUsername string
	OwnerOrgID    int64
	OwnerSlug     string

	// ActorUserID is the user performing the create. Used for
	// audit-log + rate-limiting + initial-commit author. Defaults to
	// OwnerUserID for personal repos when zero.
	ActorUserID int64

	// ActorIsSiteAdmin, when true, bypasses the per-actor create
	// rate-limit. Site admins are trusted operators (bulk migration,
	// fixture seeding) and the cap exists to deter abuse from regular
	// accounts, not to throttle staff.
	ActorIsSiteAdmin bool

	// BypassCreateRateLimit lets trusted server-side bulk operations
	// create many repos for the same actor without tripping the browser
	// anti-abuse throttle. Keep false for direct user submits.
	BypassCreateRateLimit bool

	Name        string // already lowercased + trimmed
	Description string
	Visibility  string // "public" | "private"

	InitReadme   bool
	LicenseKey   string // "" = none
	GitignoreKey string // "" = none

	// Optional override for the initial commit timestamp; tests pin this
	// for determinism. Production callers leave it zero and let
	// orchestrator default to deps.Now().
	InitialCommitWhen time.Time
}

// Result is what Create returns on success.
type Result struct {
	Repo             reposdb.Repo
	InitialCommitOID string // "" when InitReadme/License/Gitignore were all unset
	DiskPath         string // bare-repo on-disk path
}

// Create validates, rate-limits, inserts the DB row, initializes the
// bare repo on disk, optionally builds the initial commit, audit-logs,
// and returns. On post-DB failure the tx rolls back and the partial
// repo dir is best-effort removed.
func Create(ctx context.Context, deps Deps, p Params) (Result, error) {
	if deps.Pool == nil || deps.RepoFS == nil || deps.Audit == nil || deps.Limiter == nil {
		return Result{}, errors.New("repos: Deps missing required field")
	}
	now := deps.Now
	if now == nil {
		now = time.Now
	}

	if err := ValidateName(p.Name); err != nil {
		return Result{}, err
	}
	if err := ValidateDescription(p.Description); err != nil {
		return Result{}, err
	}
	if p.Visibility != "public" && p.Visibility != "private" {
		return Result{}, fmt.Errorf("repos: visibility must be public or private (got %q)", p.Visibility)
	}
	if p.LicenseKey != "" && !templates.HasLicense(p.LicenseKey) {
		return Result{}, fmt.Errorf("%w: %s", ErrUnknownLicense, p.LicenseKey)
	}
	if p.GitignoreKey != "" && !templates.HasGitignore(p.GitignoreKey) {
		return Result{}, fmt.Errorf("%w: %s", ErrUnknownGitignore, p.GitignoreKey)
	}

	// Owner XOR — exactly one kind. Org-owner path: actor must be set
	// (so we know who initiated for audit + initial commit).
	switch {
	case p.OwnerUserID != 0 && p.OwnerOrgID == 0:
		if p.ActorUserID == 0 {
			p.ActorUserID = p.OwnerUserID
		}
	case p.OwnerOrgID != 0 && p.OwnerUserID == 0:
		if p.ActorUserID == 0 {
			return Result{}, errors.New("repos: ActorUserID required for org-owned create")
		}
	default:
		return Result{}, errors.New("repos: owner is XOR — set OwnerUserID OR OwnerOrgID, not both")
	}
	if p.OwnerOrgID != 0 && p.Visibility == "private" {
		check, err := entitlements.CheckPrivateRepositoryCreation(ctx, entitlements.Deps{Pool: deps.Pool}, p.OwnerOrgID)
		if err != nil {
			return Result{}, err
		}
		if err := check.Err(); err != nil {
			return Result{}, err
		}
	}

	// Rate-limit per actor (NOT per owner) so a user can't bypass the
	// per-account cap by spreading creates across orgs they manage.
	// Site admins skip the cap entirely.
	if !p.ActorIsSiteAdmin && !p.BypassCreateRateLimit {
		if err := deps.Limiter.Hit(ctx, deps.Pool, throttle.Limit{
			Scope:      "repo_create",
			Identifier: fmt.Sprintf("user:%d", p.ActorUserID),
			Max:        CreateRateLimitMax,
			Window:     CreateRateLimitWindow,
		}); err != nil {
			return Result{}, err
		}
	}

	// Resolve author identity for the initial commit. The actor (the
	// human who clicked "create") is the author — even on org repos,
	// the seed commit attributes to them.
	authorName, authorEmail, err := resolveAuthor(ctx, deps.Pool, p.ActorUserID)
	wantInit := p.InitReadme || p.LicenseKey != "" || p.GitignoreKey != ""
	if wantInit && err != nil {
		return Result{}, err
	}

	// Pre-compute disk path from RepoFS. Doing this before the tx avoids
	// inserting a DB row for a name that fails the path-validation
	// whitelist (which mostly mirrors our own ValidateName, but
	// defense-in-depth never hurts). Org-owned repos use the org slug
	// as the per-owner directory — same shape as user-owned, no
	// `org/` prefix on disk (matches the GitHub URL layout).
	ownerSlug := p.OwnerUsername
	if p.OwnerOrgID != 0 {
		ownerSlug = p.OwnerSlug
	}
	diskPath, err := deps.RepoFS.RepoPath(ownerSlug, p.Name)
	if err != nil {
		return Result{}, fmt.Errorf("%w: %v", ErrInvalidName, err)
	}

	tx, err := deps.Pool.Begin(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("repos: begin tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()

	q := reposdb.New()
	lockKey, err := createRepoNameLockKey(p)
	if err != nil {
		return Result{}, err
	}
	if err := q.LockRepoOwnerName(ctx, tx, lockKey); err != nil {
		return Result{}, fmt.Errorf("repos: lock owner/name: %w", err)
	}
	row, err := q.CreateRepo(ctx, tx, reposdb.CreateRepoParams{
		OwnerUserID:     pgtype.Int8{Int64: p.OwnerUserID, Valid: p.OwnerUserID != 0},
		OwnerOrgID:      pgtype.Int8{Int64: p.OwnerOrgID, Valid: p.OwnerOrgID != 0},
		Name:            p.Name,
		Description:     p.Description,
		Visibility:      reposdb.RepoVisibility(p.Visibility),
		DefaultBranch:   "trunk",
		LicenseKey:      pgtype.Text{String: p.LicenseKey, Valid: p.LicenseKey != ""},
		PrimaryLanguage: pgtype.Text{Valid: false},
	})
	if err != nil {
		if isUniqueViolation(err) {
			return Result{}, ErrTaken
		}
		return Result{}, fmt.Errorf("repos: insert: %w", err)
	}

	// FS init AFTER DB insert. If this fails the deferred Rollback
	// reverses the row; we also best-effort RemoveAll the directory in
	// case it got partially created.
	if err := deps.RepoFS.InitBare(ctx, diskPath); err != nil {
		if errors.Is(err, storage.ErrAlreadyExists) {
			displaced, displaceErr := displaceDeletedRepoPath(ctx, deps, q, tx, p, ownerSlug, diskPath)
			if displaceErr != nil {
				return Result{}, fmt.Errorf("repos: reclaim deleted repo path: %w", displaceErr)
			}
			if displaced {
				err = deps.RepoFS.InitBare(ctx, diskPath)
			}
		}
		if err != nil {
			if !errors.Is(err, storage.ErrAlreadyExists) {
				_ = os.RemoveAll(diskPath)
			}
			return Result{}, fmt.Errorf("repos: init bare: %w", err)
		}
	}

	// Install push-pipeline hooks. Skipped when ShithubdPath is empty
	// (test fixtures that exercise repo creation without the hook
	// stack). The plumbing-driven initial commit doesn't fire hooks —
	// hooks only run on user-driven pushes — so this is the right
	// boundary.
	if deps.ShithubdPath != "" {
		if err := hooks.Install(diskPath, deps.ShithubdPath); err != nil {
			_ = os.RemoveAll(diskPath)
			return Result{}, fmt.Errorf("repos: install hooks: %w", err)
		}
	}

	// Seed the issue subsystem state for the new repo: counter row +
	// default label set. Runs inside the create tx so a failed seed
	// rolls the whole repo back. Cheap (10 inserts), and folding it in
	// here keeps the "fresh repo is fully usable" invariant. Issues
	// orchestrator's SeedDefaultLabels swallows unique-violations so a
	// re-run is a no-op (defensive against partially-seeded migrations).
	iq := issuesdb.New()
	if err := iq.EnsureRepoIssueCounter(ctx, tx, row.ID); err != nil {
		return Result{}, fmt.Errorf("repos: issue counter: %w", err)
	}
	if err := issues.SeedDefaultLabels(ctx, tx, row.ID); err != nil {
		return Result{}, fmt.Errorf("repos: seed labels: %w", err)
	}

	var commitOID string
	if wantInit {
		commitWhen := p.InitialCommitWhen
		if commitWhen.IsZero() {
			commitWhen = now()
		}
		oid, err := buildInitialCommit(ctx, repogit.InitialCommit{
			GitDir:      diskPath,
			AuthorName:  authorName,
			AuthorEmail: authorEmail,
			Branch:      "trunk",
			When:        commitWhen,
			Files:       initFiles(p, authorName, commitWhen.Year()),
		})
		if err != nil {
			_ = os.RemoveAll(diskPath)
			return Result{}, fmt.Errorf("repos: initial commit: %w", err)
		}
		commitOID = oid
	}

	if err := tx.Commit(ctx); err != nil {
		_ = os.RemoveAll(diskPath)
		return Result{}, fmt.Errorf("repos: commit tx: %w", err)
	}
	committed = true

	if err := notif.Emit(ctx, deps.Pool, notif.Event{
		ActorUserID: p.ActorUserID,
		Kind:        "repo_created",
		RepoID:      row.ID,
		SourceKind:  "repo",
		SourceID:    row.ID,
		Public:      p.Visibility == "public",
		Extra: map[string]any{
			"repo_name": p.Name,
		},
	}); err != nil && deps.Logger != nil {
		deps.Logger.WarnContext(ctx, "repos: emit repo_created", "repo_id", row.ID, "error", err)
	}

	if err := deps.Audit.Record(ctx, deps.Pool, p.ActorUserID,
		audit.ActionRepoCreated, audit.TargetRepo, row.ID, map[string]any{
			"name":       p.Name,
			"visibility": p.Visibility,
			"init":       wantInit,
			"license":    p.LicenseKey,
			"gitignore":  p.GitignoreKey,
		}); err != nil {
		if deps.Logger != nil {
			deps.Logger.WarnContext(ctx, "repos: audit", "error", err)
		}
	}

	return Result{Repo: row, InitialCommitOID: commitOID, DiskPath: diskPath}, nil
}

// initFiles assembles the FileEntry slice for the initial commit based
// on which init checkboxes the user ticked.
func initFiles(p Params, author string, year int) []repogit.FileEntry {
	var files []repogit.FileEntry
	if p.InitReadme {
		files = append(files, repogit.FileEntry{
			Path: "README.md",
			Body: []byte(templates.ReadmeText(p.Name, p.Description)),
		})
	}
	if p.LicenseKey != "" {
		body, err := templates.LicenseText(p.LicenseKey, year, author)
		if err == nil {
			files = append(files, repogit.FileEntry{
				Path: "LICENSE",
				Body: []byte(body),
			})
		}
	}
	if p.GitignoreKey != "" {
		body, err := templates.GitignoreText(p.GitignoreKey)
		if err == nil {
			files = append(files, repogit.FileEntry{
				Path: ".gitignore",
				Body: []byte(body),
			})
		}
	}
	return files
}

// buildInitialCommit is a thin pass-through so tests can swap it (post-MVP).
var buildInitialCommit = func(ctx context.Context, ic repogit.InitialCommit) (string, error) {
	return ic.Build(ctx)
}

// resolveAuthor reads the user's display name + verified primary email.
// Returns ErrNoVerifiedEmail if the user has no primary email or the
// primary isn't verified.
func resolveAuthor(ctx context.Context, pool *pgxpool.Pool, userID int64) (name, addr string, err error) {
	uq := usersdb.New()
	user, err := uq.GetUserByID(ctx, pool, userID)
	if err != nil {
		return "", "", fmt.Errorf("repos: load user: %w", err)
	}
	if !user.PrimaryEmailID.Valid {
		return "", "", ErrNoVerifiedEmail
	}
	em, err := uq.GetUserEmailByID(ctx, pool, user.PrimaryEmailID.Int64)
	if err != nil {
		return "", "", fmt.Errorf("repos: load primary email: %w", err)
	}
	if !em.Verified {
		return "", "", ErrNoVerifiedEmail
	}
	display := strings.TrimSpace(user.DisplayName)
	if display == "" {
		display = user.Username
	}
	return display, string(em.Email), nil
}

func createRepoNameLockKey(p Params) (string, error) {
	name := strings.ToLower(p.Name)
	switch {
	case p.OwnerUserID != 0 && p.OwnerOrgID == 0:
		return fmt.Sprintf("repo-name:user:%d:%s", p.OwnerUserID, name), nil
	case p.OwnerOrgID != 0 && p.OwnerUserID == 0:
		return fmt.Sprintf("repo-name:org:%d:%s", p.OwnerOrgID, name), nil
	default:
		return "", errors.New("repos: owner is XOR — set OwnerUserID OR OwnerOrgID, not both")
	}
}

func displaceDeletedRepoPath(
	ctx context.Context,
	deps Deps,
	q *reposdb.Queries,
	db reposdb.DBTX,
	p Params,
	ownerSlug string,
	diskPath string,
) (bool, error) {
	deleted, err := softDeletedRepoForCreate(ctx, q, db, p)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	deletedPath, err := deps.RepoFS.DeletedRepoPath(ownerSlug, p.Name, deleted.ID)
	if err != nil {
		return false, err
	}
	if err := deps.RepoFS.Move(diskPath, deletedPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func softDeletedRepoForCreate(ctx context.Context, q *reposdb.Queries, db reposdb.DBTX, p Params) (reposdb.Repo, error) {
	if p.OwnerUserID != 0 {
		return q.GetSoftDeletedRepoByOwnerUserAndName(ctx, db, reposdb.GetSoftDeletedRepoByOwnerUserAndNameParams{
			OwnerUserID: pgtype.Int8{Int64: p.OwnerUserID, Valid: true},
			Name:        p.Name,
		})
	}
	return q.GetSoftDeletedRepoByOwnerOrgAndName(ctx, db, reposdb.GetSoftDeletedRepoByOwnerOrgAndNameParams{
		OwnerOrgID: pgtype.Int8{Int64: p.OwnerOrgID, Valid: true},
		Name:       p.Name,
	})
}

// isUniqueViolation matches Postgres SQLSTATE 23505. Used to surface
// the friendly "name taken" error from the unique-by-owner-and-name
// indexes when the pre-check raced.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "23505"
	}
	return false
}
