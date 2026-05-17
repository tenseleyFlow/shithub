// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/tenseleyFlow/shithub/internal/actions/dispatch"
	actionsevent "github.com/tenseleyFlow/shithub/internal/actions/event"
	actionsdb "github.com/tenseleyFlow/shithub/internal/actions/sqlc"
	"github.com/tenseleyFlow/shithub/internal/actions/trigger"
	"github.com/tenseleyFlow/shithub/internal/actions/workflow"
	"github.com/tenseleyFlow/shithub/internal/auth/pat"
	"github.com/tenseleyFlow/shithub/internal/auth/policy"
	repogit "github.com/tenseleyFlow/shithub/internal/repos/git"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

// mountActionsWorkflows registers the S50 §13 workflow REST surface.
//
//	GET  /api/v1/repos/{owner}/{repo}/actions/workflows                    list workflows at default-branch HEAD
//	GET  /api/v1/repos/{owner}/{repo}/actions/workflows/{id_or_file}       single workflow
//	POST /api/v1/repos/{owner}/{repo}/actions/workflows/{id_or_file}/dispatches  workflow_dispatch
//
// Scopes: `repo:read` on the GETs, `repo:write` on dispatch. Mirrors
// gh's contract; the dispatch path reuses the shared
// `internal/actions/dispatch` validation so the HTML and REST
// surfaces share semantics verbatim.
//
// Workflows have no `workflows` table in shithub — they're discovered
// at request time by walking `.shithub/workflows/` at the repo's
// default-branch HEAD (or the `?ref=` override). `{id_or_file}` is
// matched against the basename of the discovered file path.
//
// Enable/disable knobs are deferred to a follow-up PR (needs a new
// `workflow_disabled` table). Every listed workflow is reported as
// `state: "active"` for now.
func (h *Handlers) mountActionsWorkflows(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(middleware.RequireScope(pat.ScopeRepoRead))
		r.Get("/api/v1/repos/{owner}/{repo}/actions/workflows", h.actionsWorkflowsList)
		r.Get("/api/v1/repos/{owner}/{repo}/actions/workflows/{workflow}", h.actionsWorkflowsGet)
	})
	r.Group(func(r chi.Router) {
		r.Use(middleware.RequireScope(pat.ScopeRepoWrite))
		r.Post("/api/v1/repos/{owner}/{repo}/actions/workflows/{workflow}/dispatches", h.actionsWorkflowsDispatch)
	})
}

type workflowResponse struct {
	// ID is a stable identifier within a repo derived from the file
	// path. Mirrors gh's numeric `id` shape using a 64-bit FNV hash of
	// the path so clients can round-trip it back as `{id_or_file}`;
	// the path itself is the canonical identifier.
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Path  string `json:"path"`
	File  string `json:"file"`
	State string `json:"state"`
}

// workflowIDFromPath is a stable, deterministic 64-bit id derived from
// the workflow's repo-relative path. We don't persist this — it's
// just an alternate addressing form for the `{id_or_file}` path
// parameter so gh-shaped clients work without modification.
func workflowIDFromPath(path string) int64 {
	const (
		offset = 1469598103934665603
		prime  = 1099511628211
	)
	h := uint64(offset)
	for i := 0; i < len(path); i++ {
		h ^= uint64(path[i])
		h *= prime
	}
	// Shift to a positive int64 so the JSON doesn't carry a negative.
	return int64(h >> 1)
}

func (h *Handlers) actionsWorkflowsList(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.resolveAPIRepo(w, r, policy.ActionRepoRead)
	if !ok {
		return
	}
	ref := strings.TrimSpace(r.URL.Query().Get("ref"))
	if ref == "" {
		ref = repo.DefaultBranch
	}
	files, _, err := h.discoverWorkflows(r, repo, ref)
	if err != nil {
		if errors.Is(err, repogit.ErrRefNotFound) {
			writeAPIError(w, http.StatusNotFound, "ref not found")
			return
		}
		h.d.Logger.ErrorContext(r.Context(), "api: discover workflows", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "list failed")
		return
	}
	disabled, err := actionsdb.New().ListDisabledWorkflowsForRepo(r.Context(), h.d.Pool, repo.ID)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: list disabled workflows", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "list failed")
		return
	}
	disabledSet := make(map[string]struct{}, len(disabled))
	for _, f := range disabled {
		disabledSet[f] = struct{}{}
	}
	out := make([]workflowResponse, 0, len(files))
	for _, f := range files {
		wr := presentWorkflow(f)
		if _, off := disabledSet[f.Path]; off {
			wr.State = "disabled"
		}
		out = append(out, wr)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	// S62 audit B9: gh-compat clients expect a `{total_count, workflows: [...]}`
	// envelope; pre-S62 we returned a bare array which broke
	// `shithub workflow list` (json decode panic).
	writeJSON(w, http.StatusOK, map[string]any{
		"total_count": len(out),
		"workflows":   out,
	})
}

func (h *Handlers) actionsWorkflowsGet(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.resolveAPIRepo(w, r, policy.ActionRepoRead)
	if !ok {
		return
	}
	param := chi.URLParam(r, "workflow")
	ref := strings.TrimSpace(r.URL.Query().Get("ref"))
	if ref == "" {
		ref = repo.DefaultBranch
	}
	files, _, err := h.discoverWorkflows(r, repo, ref)
	if err != nil {
		if errors.Is(err, repogit.ErrRefNotFound) {
			writeAPIError(w, http.StatusNotFound, "ref not found")
			return
		}
		h.d.Logger.ErrorContext(r.Context(), "api: discover workflows", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "lookup failed")
		return
	}
	match, found := matchWorkflow(files, param)
	if !found {
		writeAPIError(w, http.StatusNotFound, "workflow not found")
		return
	}
	writeJSON(w, http.StatusOK, presentWorkflow(match))
}

func (h *Handlers) actionsWorkflowsDispatch(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.resolveAPIRepo(w, r, policy.ActionRepoWrite)
	if !ok {
		return
	}
	param := chi.URLParam(r, "workflow")
	file, err := resolveWorkflowFile(param)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid workflow file path")
		return
	}

	var body dispatchAPIRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024)).Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	ref := strings.TrimSpace(body.Ref)
	if ref == "" {
		ref = repo.DefaultBranch
	}
	branch := strings.TrimPrefix(ref, "refs/heads/")

	gitDir, err := h.repoGitDir(r.Context(), repo)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: workflow dispatch repo path", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "internal error")
		return
	}
	headSHA, err := repogit.ResolveRefOID(r.Context(), gitDir, branch)
	if err != nil {
		if errors.Is(err, repogit.ErrRefNotFound) {
			writeAPIError(w, http.StatusNotFound, "ref "+branch+" not found")
			return
		}
		writeAPIError(w, http.StatusBadRequest, "could not resolve ref")
		return
	}
	bytes, err := repogit.ReadBlobBytes(r.Context(), gitDir, headSHA, file, int64(workflow.MaxWorkflowFileBytes))
	if err != nil || len(bytes) == 0 {
		// ReadBlobBytes silently returns (nil, nil) when the path is
		// absent at the ref (git cat-file's stderr is discarded). An
		// empty body is therefore indistinguishable from a missing
		// file, which is the correct caller-facing answer either way.
		writeAPIError(w, http.StatusNotFound, fmt.Sprintf("workflow file %q not found at ref %s", file, branch))
		return
	}
	wf, diags, err := workflow.Parse(bytes)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "workflow parse: "+err.Error())
		return
	}
	for _, d := range diags {
		if d.Severity == workflow.Error {
			writeAPIError(w, http.StatusBadRequest, "workflow has Error diagnostics: "+d.String())
			return
		}
	}
	if wf.On.WorkflowDispatch == nil {
		writeAPIError(w, http.StatusBadRequest, "workflow does not declare on.workflow_dispatch")
		return
	}
	inputs, err := dispatch.NormalizeInputs(body.Inputs, wf.On.WorkflowDispatch.Inputs)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}

	requestID, err := randHex(8)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: workflow dispatch rand", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "internal error")
		return
	}
	triggerID := fmt.Sprintf("dispatch:%s:%s:%s", file, headSHA, requestID)
	auth := middleware.PATAuthFromContext(r.Context())

	if _, err := trigger.Enqueue(r.Context(), trigger.Deps{Pool: h.d.Pool, Logger: h.d.Logger}, trigger.EnqueueParams{
		RepoID:         repo.ID,
		WorkflowFile:   file,
		HeadSHA:        headSHA,
		HeadRef:        "refs/heads/" + branch,
		EventKind:      trigger.EventWorkflowDispatch,
		EventPayload:   actionsevent.WorkflowDispatch(inputs),
		ActorUserID:    auth.UserID,
		TriggerEventID: triggerID,
		Workflow:       wf,
	}); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: workflow dispatch enqueue", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "enqueue failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type dispatchAPIRequest struct {
	Ref    string            `json:"ref,omitempty"`
	Inputs map[string]string `json:"inputs,omitempty"`
}

// matchWorkflow finds a discovered file whose basename (or full path)
// matches `param`. `param` may be the basename (`ci.yml`), the full
// path (`.shithub/workflows/ci.yml`), or the FNV-derived numeric id
// from the list response.
func matchWorkflow(files []discoveredWorkflow, param string) (discoveredWorkflow, bool) {
	param = strings.TrimSpace(param)
	if param == "" {
		return discoveredWorkflow{}, false
	}
	// Numeric id path.
	if id, err := parseInt64(param); err == nil {
		for _, f := range files {
			if workflowIDFromPath(f.Path) == id {
				return f, true
			}
		}
	}
	// Path / basename path.
	want := param
	if !strings.HasPrefix(want, dispatch.WorkflowFilesDir) {
		want = dispatch.WorkflowFilesDir + want
	}
	for _, f := range files {
		if f.Path == want {
			return f, true
		}
	}
	return discoveredWorkflow{}, false
}

// resolveWorkflowFile maps a {workflow} URL param (basename, full
// path, or numeric id) to the canonical full path. Numeric ids
// require a discovery walk; this function is the basename/full-path
// fast path used by the dispatch handler (which would otherwise need
// a Discover call before parsing the file we're about to dispatch).
func resolveWorkflowFile(param string) (string, error) {
	// Numeric ids aren't accepted on the dispatch path — clients
	// who want to dispatch by id must hit the list endpoint first to
	// resolve the path. This keeps dispatch a single round-trip when
	// the caller already knows the file name (the common case).
	if _, err := parseInt64(param); err == nil {
		return "", dispatch.ErrInvalidWorkflowName
	}
	return dispatch.NormalizeFilePath(param)
}

func parseInt64(s string) (int64, error) {
	var out int64
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("not a number: %q", s)
		}
		out = out*10 + int64(c-'0')
	}
	if len(s) == 0 {
		return 0, fmt.Errorf("empty")
	}
	return out, nil
}

func presentWorkflow(f discoveredWorkflow) workflowResponse {
	return workflowResponse{
		ID:    workflowIDFromPath(f.Path),
		Name:  f.Name,
		Path:  f.Path,
		File:  strings.TrimPrefix(f.Path, dispatch.WorkflowFilesDir),
		State: "active",
	}
}

type discoveredWorkflow struct {
	Path string
	Name string // workflow.name, or basename without extension if unset
}

// discoverWorkflows walks `.shithub/workflows/` at the given ref's
// HEAD, parsing each found file to extract its `name:` for the
// response. Files that fail to parse are still returned (with their
// basename as the name) so the listing reflects actual ground truth
// rather than silently dropping broken workflows.
func (h *Handlers) discoverWorkflows(r *http.Request, repo *reposdb.Repo, ref string) ([]discoveredWorkflow, []string, error) {
	branch := strings.TrimPrefix(ref, "refs/heads/")
	gitDir, err := h.repoGitDir(r.Context(), repo)
	if err != nil {
		return nil, nil, fmt.Errorf("repo path: %w", err)
	}
	headSHA, err := repogit.ResolveRefOID(r.Context(), gitDir, branch)
	if err != nil {
		return nil, nil, err
	}
	files, skips, err := trigger.Discover(r.Context(), gitDir, headSHA)
	if err != nil {
		return nil, nil, err
	}
	out := make([]discoveredWorkflow, 0, len(files))
	for _, f := range files {
		name := strings.TrimSuffix(strings.TrimSuffix(strings.TrimPrefix(f.Path, dispatch.WorkflowFilesDir), ".yml"), ".yaml")
		if wf, _, err := workflow.Parse(f.Bytes); err == nil && wf.Name != "" {
			name = wf.Name
		}
		out = append(out, discoveredWorkflow{Path: f.Path, Name: name})
	}
	skipPaths := make([]string, 0, len(skips))
	for _, s := range skips {
		skipPaths = append(skipPaths, s.Path)
	}
	return out, skipPaths, nil
}

func randHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
