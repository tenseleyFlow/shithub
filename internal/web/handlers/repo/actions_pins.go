// SPDX-License-Identifier: AGPL-3.0-or-later

package repo

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	actionsdb "github.com/tenseleyFlow/shithub/internal/actions/sqlc"
	"github.com/tenseleyFlow/shithub/internal/auth/policy"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

const actionsWorkflowPinMaxBody = 16 * 1024

func (h *Handlers) repoActionsWorkflowPin(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoRead)
	if !ok {
		return
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	if viewer.ID == 0 {
		h.d.Render.HTTPError(w, r, http.StatusForbidden, "Sign in to pin workflows.")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, actionsWorkflowPinMaxBody)
	if err := r.ParseForm(); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "invalid form body: "+err.Error())
		return
	}
	workflowFile, ok := normalizeActionsWorkflowFile(r.PostFormValue("workflow_file"))
	if !ok {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "invalid workflow file path")
		return
	}
	action := strings.TrimSpace(r.PostFormValue("action"))
	if action == "" {
		action = "pin"
	}

	q := actionsdb.New()
	workflowRows, err := q.ListWorkflowRunWorkflowsForRepo(r.Context(), h.d.Pool, row.ID)
	if err != nil {
		h.d.Logger.WarnContext(r.Context(), "repo actions: list workflows for pin", "repo_id", row.ID, "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	visibleFiles := actionsVisibleWorkflowFiles(workflowRows)
	if !visibleFiles[workflowFile] {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "workflow not found")
		return
	}

	switch action {
	case "pin":
		_, err = q.PinWorkflowForUserRepo(r.Context(), h.d.Pool, actionsdb.PinWorkflowForUserRepoParams{
			UserID:       viewer.ID,
			RepoID:       row.ID,
			WorkflowFile: workflowFile,
		})
	case "unpin":
		_, err = q.UnpinWorkflowForUserRepo(r.Context(), h.d.Pool, actionsdb.UnpinWorkflowForUserRepoParams{
			UserID:       viewer.ID,
			RepoID:       row.ID,
			WorkflowFile: workflowFile,
		})
	case "move_up", "move_down":
		err = h.moveWorkflowPin(r.Context(), viewer.ID, row.ID, workflowFile, action, visibleFiles)
	default:
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "unsupported pin action")
		return
	}
	if err != nil {
		h.d.Logger.WarnContext(r.Context(), "repo actions: update workflow pin", "repo_id", row.ID, "user_id", viewer.ID, "workflow_file", workflowFile, "action", action, "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}

	basePath := "/" + owner.Username + "/" + row.Name + "/actions"
	fallback := actionsWorkflowRoutePath(basePath, workflowFile)
	if fallback == "" {
		fallback = basePath
	}
	redirectAfterRepoAction(w, r, fallback)
}

func actionsVisibleWorkflowFiles(rows []actionsdb.ListWorkflowRunWorkflowsForRepoRow) map[string]bool {
	out := make(map[string]bool, len(rows))
	for _, row := range rows {
		if workflowFile, ok := normalizeActionsWorkflowFile(row.WorkflowFile); ok {
			out[workflowFile] = true
		}
	}
	return out
}

func (h *Handlers) moveWorkflowPin(ctx context.Context, userID, repoID int64, workflowFile, action string, visibleFiles map[string]bool) error {
	tx, err := h.d.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	q := actionsdb.New()
	pins, err := q.ListWorkflowPinsForUserRepo(ctx, tx, actionsdb.ListWorkflowPinsForUserRepoParams{
		UserID: userID,
		RepoID: repoID,
	})
	if err != nil {
		return err
	}
	visiblePins := make([]actionsdb.UserActionWorkflowPin, 0, len(pins))
	for _, pin := range pins {
		pinFile, ok := normalizeActionsWorkflowFile(pin.WorkflowFile)
		if !ok || !visibleFiles[pinFile] {
			continue
		}
		pin.WorkflowFile = pinFile
		visiblePins = append(visiblePins, pin)
	}
	target := -1
	for i, pin := range visiblePins {
		if pin.WorkflowFile == workflowFile {
			target = i
			break
		}
	}
	if target == -1 {
		return nil
	}
	switch action {
	case "move_up":
		if target == 0 {
			return tx.Commit(ctx)
		}
		visiblePins[target-1], visiblePins[target] = visiblePins[target], visiblePins[target-1]
	case "move_down":
		if target == len(visiblePins)-1 {
			return tx.Commit(ctx)
		}
		visiblePins[target+1], visiblePins[target] = visiblePins[target], visiblePins[target+1]
	default:
		return errors.New("unsupported pin move action")
	}
	for i, pin := range visiblePins {
		if _, err := q.UpdateWorkflowPinPosition(ctx, tx, actionsdb.UpdateWorkflowPinPositionParams{
			UserID:       userID,
			RepoID:       repoID,
			WorkflowFile: pin.WorkflowFile,
			Position:     int32(i + 1),
		}); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		if errors.Is(err, pgx.ErrTxClosed) {
			return nil
		}
		return err
	}
	return nil
}
