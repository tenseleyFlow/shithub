// SPDX-License-Identifier: AGPL-3.0-or-later

package orgs

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"

	orgdomain "github.com/tenseleyFlow/shithub/internal/orgs"
	orgsdb "github.com/tenseleyFlow/shithub/internal/orgs/sqlc"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

type scheduledReminderForm struct {
	ID                           int64
	Name                         string
	Target                       string
	RepoID                       int64
	TeamID                       int64
	CronExpr                     string
	Timezone                     string
	MinAgeMinutes                int32
	ConditionReviewRequested     bool
	ConditionTeamReviewRequested bool
	Paused                       bool
}

type scheduledReminderRepoOption struct {
	ID         int64
	Name       string
	Visibility string
}

type scheduledReminderTeamOption struct {
	ID          int64
	Slug        string
	DisplayName string
}

func (h *Handlers) settingsScheduledReminders(w http.ResponseWriter, r *http.Request) {
	org, ok := h.loadOrgSettingsOwner(w, r)
	if !ok {
		return
	}
	h.renderScheduledReminders(w, r, org, defaultScheduledReminderForm(), "", scheduledReminderNotice(r.URL.Query().Get("notice")))
}

func (h *Handlers) settingsScheduledReminderCreate(w http.ResponseWriter, r *http.Request) {
	org, ok := h.loadOrgSettingsOwner(w, r)
	if !ok {
		return
	}
	form, err := scheduledReminderFormFromRequest(r)
	if err != nil {
		h.renderScheduledReminders(w, r, org, form, "Could not parse scheduled reminder settings.", "")
		return
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	_, err = orgdomain.CreateScheduledReminder(r.Context(), h.deps(), org.ID, viewer.ID, form.toDomainInput())
	if err != nil {
		h.renderScheduledReminders(w, r, org, form, scheduledReminderError(err), "")
		return
	}
	http.Redirect(w, r, orgScheduledRemindersPath(org.Slug)+"?notice=created", http.StatusSeeOther)
}

func (h *Handlers) settingsScheduledReminderUpdate(w http.ResponseWriter, r *http.Request) {
	org, ok := h.loadOrgSettingsOwner(w, r)
	if !ok {
		return
	}
	reminderID, err := scheduledReminderIDParam(r)
	if err != nil {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		return
	}
	form, err := scheduledReminderFormFromRequest(r)
	if err != nil {
		h.renderScheduledReminders(w, r, org, form, "Could not parse scheduled reminder settings.", "")
		return
	}
	form.ID = reminderID
	if _, err := orgdomain.UpdateScheduledReminder(r.Context(), h.deps(), org.ID, reminderID, form.toDomainInput()); err != nil {
		h.renderScheduledReminders(w, r, org, form, scheduledReminderError(err), "")
		return
	}
	http.Redirect(w, r, orgScheduledRemindersPath(org.Slug)+"?notice=updated", http.StatusSeeOther)
}

func (h *Handlers) settingsScheduledReminderPause(w http.ResponseWriter, r *http.Request) {
	h.changeScheduledReminderState(w, r, "paused", orgdomain.PauseScheduledReminder)
}

func (h *Handlers) settingsScheduledReminderResume(w http.ResponseWriter, r *http.Request) {
	h.changeScheduledReminderState(w, r, "resumed", func(ctx context.Context, deps orgdomain.Deps, orgID, reminderID int64) error {
		_, err := orgdomain.ResumeScheduledReminder(ctx, deps, orgID, reminderID)
		return err
	})
}

func (h *Handlers) settingsScheduledReminderDelete(w http.ResponseWriter, r *http.Request) {
	h.changeScheduledReminderState(w, r, "deleted", orgdomain.DeleteScheduledReminder)
}

func (h *Handlers) changeScheduledReminderState(
	w http.ResponseWriter,
	r *http.Request,
	notice string,
	fn func(context.Context, orgdomain.Deps, int64, int64) error,
) {
	org, ok := h.loadOrgSettingsOwner(w, r)
	if !ok {
		return
	}
	reminderID, err := scheduledReminderIDParam(r)
	if err != nil {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		return
	}
	if err := fn(r.Context(), h.deps(), org.ID, reminderID); err != nil {
		h.renderScheduledReminders(w, r, org, defaultScheduledReminderForm(), scheduledReminderError(err), "")
		return
	}
	http.Redirect(w, r, orgScheduledRemindersPath(org.Slug)+"?notice="+url.QueryEscape(notice), http.StatusSeeOther)
}

func (h *Handlers) renderScheduledReminders(w http.ResponseWriter, r *http.Request, org orgsdb.Org, form scheduledReminderForm, errMsg, notice string) {
	reminders, err := orgdomain.ListScheduledReminders(r.Context(), h.deps(), org.ID)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "org scheduled reminders: list", "org_id", org.ID, "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	repos, teams, err := h.scheduledReminderTargets(r, org.ID)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "org scheduled reminders: target lists", "org_id", org.ID, "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	_ = h.d.Render.RenderPage(w, r, "orgs/settings_scheduled_reminders", map[string]any{
		"Title":             org.Slug + " - scheduled reminders",
		"CSRFToken":         middleware.CSRFTokenForRequest(r),
		"Org":               org,
		"ActiveOrgNav":      "settings",
		"OrgSettingsActive": "scheduled-reminders",
		"BillingEnabled":    h.d.BillingEnabled,
		"Error":             errMsg,
		"Notice":            notice,
		"Form":              form,
		"Reminders":         reminders,
		"Repos":             repos,
		"Teams":             teams,
	})
}

func (h *Handlers) scheduledReminderTargets(r *http.Request, orgID int64) ([]scheduledReminderRepoOption, []scheduledReminderTeamOption, error) {
	repos, err := reposdb.New().ListReposForOwnerOrg(r.Context(), h.d.Pool, pgtype.Int8{Int64: orgID, Valid: true})
	if err != nil {
		return nil, nil, err
	}
	repoOptions := make([]scheduledReminderRepoOption, 0, len(repos))
	for _, repo := range repos {
		if scheduledReminderRepoUnavailable(repo) {
			continue
		}
		repoOptions = append(repoOptions, scheduledReminderRepoOption{
			ID:         repo.ID,
			Name:       repo.Name,
			Visibility: string(repo.Visibility),
		})
	}
	teams, err := orgsdb.New().ListTeamsForOrg(r.Context(), h.d.Pool, orgID)
	if err != nil {
		return nil, nil, err
	}
	teamOptions := make([]scheduledReminderTeamOption, 0, len(teams))
	for _, team := range teams {
		teamOptions = append(teamOptions, scheduledReminderTeamOption{
			ID:          team.ID,
			Slug:        team.Slug,
			DisplayName: team.DisplayName,
		})
	}
	return repoOptions, teamOptions, nil
}

func scheduledReminderRepoUnavailable(repo reposdb.Repo) bool {
	return repo.DeletedAt.Valid || repo.IsArchived
}

func defaultScheduledReminderForm() scheduledReminderForm {
	return scheduledReminderForm{
		Target:                       string(orgsdb.OrgScheduledReminderTargetAllRepositories),
		CronExpr:                     "0 9 * * 1",
		Timezone:                     "UTC",
		MinAgeMinutes:                1440,
		ConditionReviewRequested:     true,
		ConditionTeamReviewRequested: true,
	}
}

func scheduledReminderFormFromRequest(r *http.Request) (scheduledReminderForm, error) {
	if err := r.ParseForm(); err != nil {
		return scheduledReminderForm{}, err
	}
	form := scheduledReminderForm{
		Name:                         strings.TrimSpace(r.PostFormValue("name")),
		Target:                       strings.TrimSpace(r.PostFormValue("target")),
		CronExpr:                     strings.TrimSpace(r.PostFormValue("cron_expr")),
		Timezone:                     strings.TrimSpace(r.PostFormValue("timezone")),
		ConditionReviewRequested:     r.PostFormValue("condition_review_requested") == "on",
		ConditionTeamReviewRequested: r.PostFormValue("condition_team_review_requested") == "on",
		Paused:                       r.PostFormValue("paused") == "on",
	}
	if form.Target == "" {
		form.Target = string(orgsdb.OrgScheduledReminderTargetAllRepositories)
	}
	if form.Timezone == "" {
		form.Timezone = "UTC"
	}
	if form.CronExpr == "" {
		form.CronExpr = "0 9 * * 1"
	}
	if v := strings.TrimSpace(r.PostFormValue("repo_id")); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return form, err
		}
		form.RepoID = id
	}
	if v := strings.TrimSpace(r.PostFormValue("team_id")); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return form, err
		}
		form.TeamID = id
	}
	if v := strings.TrimSpace(r.PostFormValue("min_age_minutes")); v != "" {
		minutes, err := strconv.ParseInt(v, 10, 32)
		if err != nil {
			return form, err
		}
		form.MinAgeMinutes = int32(minutes)
	}
	return form, nil
}

func (f scheduledReminderForm) toDomainInput() orgdomain.ScheduledReminderInput {
	return orgdomain.ScheduledReminderInput{
		Name:                         f.Name,
		Target:                       orgsdb.OrgScheduledReminderTarget(f.Target),
		RepoID:                       f.RepoID,
		TeamID:                       f.TeamID,
		CronExpr:                     f.CronExpr,
		Timezone:                     f.Timezone,
		ConditionReviewRequested:     f.ConditionReviewRequested,
		ConditionTeamReviewRequested: f.ConditionTeamReviewRequested,
		MinAgeMinutes:                f.MinAgeMinutes,
		Paused:                       f.Paused,
	}
}

func scheduledReminderIDParam(r *http.Request) (int64, error) {
	return strconv.ParseInt(chi.URLParam(r, "reminderID"), 10, 64)
}

func orgScheduledRemindersPath(slug string) string {
	return "/organizations/" + slug + "/settings/scheduled-reminders"
}

func scheduledReminderNotice(code string) string {
	switch code {
	case "created":
		return "Scheduled reminder created."
	case "updated":
		return "Scheduled reminder updated."
	case "paused":
		return "Scheduled reminder paused."
	case "resumed":
		return "Scheduled reminder resumed."
	case "deleted":
		return "Scheduled reminder deleted."
	default:
		return ""
	}
}

func scheduledReminderError(err error) string {
	switch {
	case errors.Is(err, orgdomain.ErrScheduledReminderRequiresTeam):
		return "Scheduled reminders for private repositories require a Team organization."
	case errors.Is(err, orgdomain.ErrScheduledReminderTarget):
		return "Choose a repository or team that belongs to this organization."
	case errors.Is(err, orgdomain.ErrScheduledReminderInvalid):
		return "Check the name, schedule, target, and reminder conditions."
	case errors.Is(err, orgdomain.ErrScheduledReminderNotFound):
		return "Scheduled reminder not found."
	default:
		return "Could not save scheduled reminder."
	}
}
