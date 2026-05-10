// SPDX-License-Identifier: AGPL-3.0-or-later

package repo

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/tenseleyFlow/shithub/internal/social"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

type repoActionView struct {
	IsLoggedIn   bool
	LoginURL     string
	ReturnTo     string
	Starred      bool
	WatchLevel   string
	WatchOptions []repoWatchOptionView
}

type repoWatchOptionView struct {
	Level       string
	Label       string
	Description string
	Checked     bool
}

func (h *Handlers) repoActions(r *http.Request, repoID int64) repoActionView {
	viewer := middleware.CurrentUserFromContext(r.Context())
	returnTo := r.URL.RequestURI()
	if returnTo == "" {
		returnTo = r.URL.Path
	}
	if returnTo == "" {
		returnTo = "/"
	}
	out := repoActionView{
		IsLoggedIn: !viewer.IsAnonymous(),
		LoginURL:   "/login?next=" + url.QueryEscape(returnTo),
		ReturnTo:   returnTo,
		WatchLevel: string(social.WatchParticipating),
	}
	if viewer.IsAnonymous() {
		out.WatchOptions = repoWatchOptions(social.WatchParticipating)
		return out
	}
	deps := h.socialDeps()
	starred, err := social.HasStar(r.Context(), deps, viewer.ID, repoID)
	if err != nil {
		h.d.Logger.WarnContext(r.Context(), "repo actions: star lookup", "error", err, "repo_id", repoID, "user_id", viewer.ID)
	} else {
		out.Starred = starred
	}
	level, err := social.CurrentLevel(r.Context(), deps, viewer.ID, repoID)
	if err != nil {
		h.d.Logger.WarnContext(r.Context(), "repo actions: watch lookup", "error", err, "repo_id", repoID, "user_id", viewer.ID)
		level = social.WatchParticipating
	}
	out.WatchLevel = string(level)
	out.WatchOptions = repoWatchOptions(level)
	return out
}

func repoWatchOptions(current social.WatchLevel) []repoWatchOptionView {
	return []repoWatchOptionView{
		{
			Level:       string(social.WatchParticipating),
			Label:       "Participating and @mentions",
			Description: "Only receive notifications from this repository when participating or mentioned.",
			Checked:     current == social.WatchParticipating,
		},
		{
			Level:       string(social.WatchAll),
			Label:       "All Activity",
			Description: "Notified of all notifications on this repository.",
			Checked:     current == social.WatchAll,
		},
		{
			Level:       string(social.WatchIgnore),
			Label:       "Ignore",
			Description: "Never notified.",
			Checked:     current == social.WatchIgnore,
		},
	}
}

func redirectAfterRepoAction(w http.ResponseWriter, r *http.Request, fallback string) {
	dest := fallback
	if err := r.ParseForm(); err == nil {
		if returnTo := strings.TrimSpace(r.PostFormValue("return_to")); safeLocalPath(returnTo) {
			dest = returnTo
		}
	}
	http.Redirect(w, r, dest, http.StatusSeeOther)
}

func safeLocalPath(path string) bool {
	if path == "" || !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") {
		return false
	}
	u, err := url.Parse(path)
	if err != nil {
		return false
	}
	return !u.IsAbs() && u.Host == "" && strings.HasPrefix(u.Path, "/")
}
