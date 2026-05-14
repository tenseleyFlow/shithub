// SPDX-License-Identifier: AGPL-3.0-or-later

package repo

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/tenseleyFlow/shithub/internal/actions/dispatch"
	actionsevent "github.com/tenseleyFlow/shithub/internal/actions/event"
	actionspolicy "github.com/tenseleyFlow/shithub/internal/actions/policy"
	"github.com/tenseleyFlow/shithub/internal/actions/trigger"
	"github.com/tenseleyFlow/shithub/internal/actions/workflow"
	"github.com/tenseleyFlow/shithub/internal/auth/policy"
	repogit "github.com/tenseleyFlow/shithub/internal/repos/git"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

// dispatchRequest is the JSON body shape the dispatch endpoint
// accepts. Both fields are optional:
//
//   - ref defaults to the repo's default branch (e.g. "trunk").
//     Accepts short ("trunk") or fully-qualified ("refs/heads/trunk")
//     forms; we resolve to a SHA via git.
//   - inputs is the workflow_dispatch.inputs map. Values are
//     stringified to match GHA semantics (booleans arrive as
//     "true"/"false" strings).
type dispatchRequest struct {
	Ref    string            `json:"ref,omitempty"`
	Inputs map[string]string `json:"inputs,omitempty"`
}

// dispatchMaxBody bounds the request body to keep handler memory
// predictable. Inputs are key/value strings; 64 KiB is well above
// any realistic dispatch body.
const dispatchMaxBody = 64 * 1024

// repoActionsDispatch implements
//
//	POST /{owner}/{repo}/actions/workflows/{file}/dispatches
//
// 204 on success; the trigger pipeline runs synchronously here
// because the workflow file is already known (no discovery needed),
// so latency is the cost of one parse + one Enqueue.
//
// Auth: requires repo write access (policy.ActionRepoWrite). Anyone
// who can push to the repo can dispatch a workflow on it; same
// trust boundary as the runner that picks the resulting run up.
func (h *Handlers) repoActionsDispatch(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoWrite)
	if !ok {
		return
	}

	// {file} is URL-escaped (slashes in chi route params don't survive
	// without the * splat pattern; we use Path-Value-style escaping).
	file := chi.URLParam(r, "file")
	if file == "" {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "missing workflow file")
		return
	}
	// Workflows live under .shithub/workflows/. Authors can pass
	// either "ci.yml" (basename) or ".shithub/workflows/ci.yml" (full
	// path). Normalize so the trigger pipeline always sees the full path.
	file, err := dispatch.NormalizeFilePath(file)
	if err != nil {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "invalid workflow file path")
		return
	}

	req, formPost, ok := h.parseDispatchRequest(w, r)
	if !ok {
		return
	}

	ref := req.Ref
	if ref == "" {
		ref = row.DefaultBranch
	}
	// Strip refs/heads/ prefix if present so we match git's branch
	// resolution shape.
	branch := strings.TrimPrefix(ref, "refs/heads/")

	gitDir, err := h.d.RepoFS.RepoPath(owner.Username, row.Name)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "actions dispatch: repo path", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	headSHA, err := repogit.ResolveRefOID(r.Context(), gitDir, branch)
	if err != nil {
		if errors.Is(err, repogit.ErrRefNotFound) {
			h.d.Render.HTTPError(w, r, http.StatusNotFound, "ref "+branch+" not found")
			return
		}
		h.d.Logger.WarnContext(r.Context(), "actions dispatch: resolve ref", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "could not resolve ref")
		return
	}

	// Read + parse the specific workflow at this SHA. Saves a tree walk
	// since the dispatch knows exactly which file to run.
	bytes, err := repogit.ReadBlobBytes(r.Context(), gitDir, headSHA, file, int64(workflow.MaxWorkflowFileBytes))
	if err != nil {
		h.d.Render.HTTPError(w, r, http.StatusNotFound,
			fmt.Sprintf("workflow file %q not found at ref %s", file, branch))
		return
	}
	wf, diags, err := workflow.Parse(bytes)
	if err != nil {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "workflow parse: "+err.Error())
		return
	}
	for _, d := range diags {
		if d.Severity == workflow.Error {
			h.d.Render.HTTPError(w, r, http.StatusBadRequest, "workflow has Error diagnostics: "+d.String())
			return
		}
	}
	if wf.On.WorkflowDispatch == nil {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest,
			"workflow does not declare on.workflow_dispatch")
		return
	}
	inputs, err := dispatch.NormalizeInputs(req.Inputs, wf.On.WorkflowDispatch.Inputs)
	if err != nil {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, err.Error())
		return
	}

	// Each dispatch click produces a fresh trigger_event_id with a
	// unique random suffix — the same workflow file at the same SHA
	// can be dispatched multiple times by a human and each fires.
	// (Compare the push trigger, which dedups on push_event_id.)
	requestID, err := randHex(8)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "actions dispatch: rand", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	triggerID := fmt.Sprintf("dispatch:%s:%s:%s", file, headSHA, requestID)

	viewer := middleware.CurrentUserFromContext(r.Context())
	actorID := viewer.ID // 0 if anonymous, but RequireUser is in front of this route

	payload := actionsevent.WorkflowDispatch(inputs)
	decision, err := actionspolicy.EvaluateTrigger(r.Context(), actionspolicy.Deps{Pool: h.d.Pool}, actionspolicy.TriggerRequest{
		Repo:        row,
		EventKind:   string(trigger.EventWorkflowDispatch),
		ActorUserID: actorID,
	})
	if err != nil || !decision.Allow {
		h.d.Logger.WarnContext(r.Context(), "actions dispatch: blocked by actions policy",
			"repo_id", row.ID, "workflow_file", file, "reason", decision.Reason, "error", err)
		h.d.Render.HTTPError(w, r, http.StatusForbidden, "Actions are not allowed to run for this repository.")
		return
	}
	if _, err := trigger.Enqueue(r.Context(), trigger.Deps{Pool: h.d.Pool, Logger: h.d.Logger}, trigger.EnqueueParams{
		RepoID:         row.ID,
		WorkflowFile:   file,
		HeadSHA:        headSHA,
		HeadRef:        "refs/heads/" + branch,
		EventKind:      trigger.EventWorkflowDispatch,
		EventPayload:   payload,
		ActorUserID:    actorID,
		TriggerEventID: triggerID,
		Workflow:       wf,
	}); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "actions dispatch: enqueue", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	if formPost {
		basePath := "/" + owner.Username + "/" + row.Name + "/actions"
		redirectTo := actionsWorkflowRoutePath(basePath, file)
		q := url.Values{"query": []string{"event:workflow_dispatch"}}
		if redirectTo == "" {
			redirectTo = basePath
			q.Set("workflow", file)
		}
		redirectTo = pathWithQuery(redirectTo, q)
		http.Redirect(w, r, redirectTo, http.StatusSeeOther)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handlers) parseDispatchRequest(w http.ResponseWriter, r *http.Request) (dispatchRequest, bool, bool) {
	if mediaType := dispatchFormMediaType(r); mediaType != "" {
		r.Body = http.MaxBytesReader(w, r.Body, dispatchMaxBody)
		if err := r.ParseForm(); err != nil {
			h.d.Render.HTTPError(w, r, http.StatusBadRequest, "invalid form body: "+err.Error())
			return dispatchRequest{}, true, false
		}
		return dispatchRequest{
			Ref:    strings.TrimSpace(r.PostFormValue("ref")),
			Inputs: dispatch.InputsFromForm(r.PostForm),
		}, true, true
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, dispatchMaxBody+1))
	if err != nil {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "read body: "+err.Error())
		return dispatchRequest{}, false, false
	}
	if len(body) > dispatchMaxBody {
		h.d.Render.HTTPError(w, r, http.StatusRequestEntityTooLarge, "body exceeds 64 KiB")
		return dispatchRequest{}, false, false
	}
	var req dispatchRequest
	if len(body) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			h.d.Render.HTTPError(w, r, http.StatusBadRequest, "invalid JSON body: "+err.Error())
			return dispatchRequest{}, false, false
		}
	}
	return req, false, true
}

func dispatchFormMediaType(r *http.Request) string {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		return ""
	}
	switch mediaType {
	case "application/x-www-form-urlencoded":
		return mediaType
	default:
		return ""
	}
}

// randHex returns 2*n hex chars from crypto/rand. 8 bytes (16 hex
// chars) is plenty of entropy for a dispatch request id.
func randHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
