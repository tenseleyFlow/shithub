// SPDX-License-Identifier: AGPL-3.0-or-later

package repos

import (
	"context"
	"errors"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	mdrender "github.com/tenseleyFlow/shithub/internal/markdown"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
)

const (
	MaxProjectTitleLen       = 200
	MaxProjectDescriptionLen = 2000
	MaxWikiTitleLen          = 200
	MaxWikiBodyLen           = 262144
)

var (
	ErrProjectInvalid = errors.New("repos: invalid project")
	ErrProjectState   = errors.New("repos: invalid project state")
	ErrWikiInvalid    = errors.New("repos: invalid wiki page")
	ErrWikiExists     = errors.New("repos: wiki page already exists")
)

var wikiSlugRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

type ProjectInput struct {
	RepoID      int64
	Title       string
	Description string
	ActorUserID int64
}

func CreateProject(ctx context.Context, deps Deps, in ProjectInput) (reposdb.RepoProject, error) {
	title, description, err := normalizeProjectInput(in.Title, in.Description)
	if err != nil {
		return reposdb.RepoProject{}, err
	}
	return reposdb.New().CreateRepoProject(ctx, deps.Pool, reposdb.CreateRepoProjectParams{
		RepoID:          in.RepoID,
		Title:           title,
		Description:     description,
		CreatedByUserID: nullableUserID(in.ActorUserID),
	})
}

func UpdateProject(ctx context.Context, deps Deps, projectID int64, in ProjectInput) (reposdb.RepoProject, error) {
	title, description, err := normalizeProjectInput(in.Title, in.Description)
	if err != nil {
		return reposdb.RepoProject{}, err
	}
	return reposdb.New().UpdateRepoProject(ctx, deps.Pool, reposdb.UpdateRepoProjectParams{
		ID:          projectID,
		RepoID:      in.RepoID,
		Title:       title,
		Description: description,
	})
}

func SetProjectState(ctx context.Context, deps Deps, repoID, projectID int64, state string) (reposdb.RepoProject, error) {
	if state != string(reposdb.RepoProjectStateOpen) && state != string(reposdb.RepoProjectStateClosed) {
		return reposdb.RepoProject{}, ErrProjectState
	}
	return reposdb.New().SetRepoProjectState(ctx, deps.Pool, reposdb.SetRepoProjectStateParams{
		ID:     projectID,
		RepoID: repoID,
		State:  reposdb.RepoProjectState(state),
	})
}

func AddIssueToProject(ctx context.Context, deps Deps, projectID, issueID, actorUserID int64) (reposdb.RepoProjectItem, error) {
	return reposdb.New().AddIssueToRepoProject(ctx, deps.Pool, reposdb.AddIssueToRepoProjectParams{
		ProjectID:     projectID,
		IssueID:       issueID,
		AddedByUserID: nullableUserID(actorUserID),
	})
}

func normalizeProjectInput(title, description string) (string, string, error) {
	title = strings.TrimSpace(title)
	description = strings.TrimSpace(description)
	if title == "" || len(title) > MaxProjectTitleLen {
		return "", "", ErrProjectInvalid
	}
	if len(description) > MaxProjectDescriptionLen {
		return "", "", ErrProjectInvalid
	}
	return title, description, nil
}

type WikiPageInput struct {
	RepoID      int64
	Title       string
	Slug        string
	Body        string
	ActorUserID int64
}

func CreateWikiPage(ctx context.Context, deps Deps, in WikiPageInput) (reposdb.RepoWikiPage, error) {
	title, slug, body, html, err := normalizeWikiInput(in)
	if err != nil {
		return reposdb.RepoWikiPage{}, err
	}
	row, err := reposdb.New().CreateRepoWikiPage(ctx, deps.Pool, reposdb.CreateRepoWikiPageParams{
		RepoID:          in.RepoID,
		Slug:            slug,
		Title:           title,
		Body:            body,
		BodyHtmlCached:  pgtype.Text{String: html, Valid: true},
		CreatedByUserID: nullableUserID(in.ActorUserID),
		UpdatedByUserID: nullableUserID(in.ActorUserID),
	})
	if err != nil {
		if isUniqueConstraint(err) {
			return reposdb.RepoWikiPage{}, ErrWikiExists
		}
		return reposdb.RepoWikiPage{}, err
	}
	return row, nil
}

func UpdateWikiPage(ctx context.Context, deps Deps, pageID int64, in WikiPageInput) (reposdb.RepoWikiPage, error) {
	title, _, body, html, err := normalizeWikiInput(in)
	if err != nil {
		return reposdb.RepoWikiPage{}, err
	}
	return reposdb.New().UpdateRepoWikiPage(ctx, deps.Pool, reposdb.UpdateRepoWikiPageParams{
		ID:              pageID,
		RepoID:          in.RepoID,
		Title:           title,
		Body:            body,
		BodyHtmlCached:  pgtype.Text{String: html, Valid: true},
		UpdatedByUserID: nullableUserID(in.ActorUserID),
	})
}

func NormalizeWikiSlug(raw string) string {
	raw = strings.ToLower(strings.TrimSpace(raw))
	var b strings.Builder
	lastDash := false
	for _, r := range raw {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		case r == '-' || r == '_' || r == ' ' || r == '.':
			if b.Len() > 0 && !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		}
		if b.Len() >= 120 {
			break
		}
	}
	return strings.Trim(b.String(), "-")
}

func normalizeWikiInput(in WikiPageInput) (title, slug, body, html string, err error) {
	title = strings.TrimSpace(in.Title)
	if title == "" || len(title) > MaxWikiTitleLen {
		return "", "", "", "", ErrWikiInvalid
	}
	slug = NormalizeWikiSlug(in.Slug)
	if slug == "" {
		slug = NormalizeWikiSlug(title)
	}
	if slug == "" || len(slug) > 120 || !wikiSlugRE.MatchString(slug) {
		return "", "", "", "", ErrWikiInvalid
	}
	body = strings.TrimSpace(in.Body)
	if len(body) > MaxWikiBodyLen {
		return "", "", "", "", ErrWikiInvalid
	}
	html, err = mdrender.RenderDocumentHTML([]byte(body))
	if err != nil {
		return "", "", "", "", err
	}
	return title, slug, body, html, nil
}

func nullableUserID(id int64) pgtype.Int8 {
	return pgtype.Int8{Int64: id, Valid: id != 0}
}

func isUniqueConstraint(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
