// SPDX-License-Identifier: AGPL-3.0-or-later

package repo

import (
	"net/http"
	"net/url"
	"strings"

	actionsdispatch "github.com/tenseleyFlow/shithub/internal/actions/dispatch"
	actionsdb "github.com/tenseleyFlow/shithub/internal/actions/sqlc"
	"github.com/tenseleyFlow/shithub/internal/actions/workflow"
	"github.com/tenseleyFlow/shithub/internal/actions/workflowtemplates"
	"github.com/tenseleyFlow/shithub/internal/auth/policy"
	repogit "github.com/tenseleyFlow/shithub/internal/repos/git"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/repos/webedit"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
)

type actionsNewWorkflowTemplateView struct {
	Key         string
	Name        string
	Description string
	Filename    string
	Href        string
	Unsupported bool
	Reason      string
}

type workflowDiagnosticView struct {
	Path          string
	Message       string
	SeverityLabel string
	SeverityClass string
	Icon          string
}

func (h *Handlers) repoActionsNewWorkflow(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoWrite)
	if !ok {
		return
	}
	key := strings.TrimSpace(r.URL.Query().Get("template"))
	if key == "" {
		h.renderActionsNewWorkflowPicker(w, r, row, owner, "", http.StatusOK)
		return
	}
	tmpl, ok := workflowtemplates.Find(key)
	if !ok {
		h.renderActionsNewWorkflowPicker(w, r, row, owner, "Unknown workflow template.", http.StatusBadRequest)
		return
	}
	cc, ok := h.actionsNewWorkflowCodeContext(w, r, row, owner.Username, row.DefaultBranch)
	if !ok {
		return
	}
	target := actionsTemplatePath(tmpl)
	data := h.actionsNewWorkflowEditorData(r, cc, target, tmpl.Body)
	h.renderEditor(w, r, data, http.StatusOK)
}

func (h *Handlers) repoActionsCreateWorkflow(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoWrite)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, webedit.MaxTextBytes+128*1024)
	if err := r.ParseForm(); err != nil {
		cc, ok := h.actionsNewWorkflowCodeContext(w, r, row, owner.Username, row.DefaultBranch)
		if !ok {
			return
		}
		data := h.actionsNewWorkflowEditorData(r, cc, actionsdispatch.WorkflowFilesDir+"workflow.yml", "")
		data.Error = "The submitted workflow is too large or could not be read."
		h.renderEditor(w, r, data, http.StatusRequestEntityTooLarge)
		return
	}
	cc, ok := h.actionsNewWorkflowCodeContext(w, r, row, owner.Username, strings.TrimSpace(r.PostFormValue("branch")))
	if !ok {
		return
	}

	rawTarget := r.PostFormValue("path")
	target, ok := cleanActionsWorkflowAuthorPath(rawTarget)
	content := []byte(r.PostFormValue("content"))
	if !ok {
		data := h.actionsNewWorkflowEditorData(r, cc, cleanEditorPath(rawTarget), string(content))
		data.Message = r.PostFormValue("commit_message")
		data.Description = r.PostFormValue("commit_description")
		data.Error = "Enter a workflow file path under .shithub/workflows/ ending in .yml or .yaml."
		h.renderEditor(w, r, data, http.StatusBadRequest)
		return
	}
	if len(content) > webedit.MaxTextBytes {
		data := h.actionsNewWorkflowEditorData(r, cc, target, string(content))
		data.Message = r.PostFormValue("commit_message")
		data.Description = r.PostFormValue("commit_description")
		data.Error = "Workflow files edited in the browser must be 1 MiB or smaller."
		h.renderEditor(w, r, data, http.StatusRequestEntityTooLarge)
		return
	}
	diagnostics, blocked := workflowDiagnosticsForAuthoring(content)
	if blocked {
		data := h.actionsNewWorkflowEditorData(r, cc, target, string(content))
		data.Message = r.PostFormValue("commit_message")
		data.Description = r.PostFormValue("commit_description")
		data.Error = "Fix the workflow syntax before committing."
		data.WorkflowDiagnostics = diagnostics
		h.renderEditor(w, r, data, http.StatusUnprocessableEntity)
		return
	}

	if _, err := h.commitWebEdit(r, cc, webedit.Params{
		Op:          webedit.OpCreate,
		TargetPath:  target,
		Content:     content,
		BaseOID:     r.PostFormValue("base_oid"),
		Message:     submittedCommitMessage(r, webedit.OpCreate, cc, target, nil),
		Description: r.PostFormValue("commit_description"),
	}); err != nil {
		data := h.actionsNewWorkflowEditorData(r, cc, target, string(content))
		data.Message = r.PostFormValue("commit_message")
		data.Description = r.PostFormValue("commit_description")
		h.renderWebEditError(w, r, data, err)
		return
	}

	basePath := "/" + cc.owner + "/" + cc.row.Name + "/actions"
	if href := actionsWorkflowRoutePath(basePath, target); href != "" {
		http.Redirect(w, r, href, http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, codeURL(cc.owner, cc.row.Name, "blob", cc.ref, target), http.StatusSeeOther)
}

func (h *Handlers) actionsNewWorkflowCodeContext(w http.ResponseWriter, r *http.Request, row reposdb.Repo, ownerUsername, branch string) (*codeContext, bool) {
	gitDir, err := h.d.RepoFS.RepoPath(ownerUsername, row.Name)
	if err != nil {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		return nil, false
	}
	refs, err := repogit.ListRefs(r.Context(), gitDir)
	if err != nil {
		h.d.Logger.WarnContext(r.Context(), "repo actions new workflow: ListRefs", "repo_id", row.ID, "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return nil, false
	}
	if branch == "" {
		branch = row.DefaultBranch
	}
	if !actionsBranchExists(refs, branch) {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "Choose an existing branch before creating a workflow.")
		return nil, false
	}
	return &codeContext{
		owner:   ownerUsername,
		row:     row,
		gitDir:  gitDir,
		refs:    refs,
		allRefs: refNames(refs),
		ref:     branch,
		subpath: "",
	}, true
}

func actionsBranchExists(refs repogit.RefListing, branch string) bool {
	for _, ref := range refs.Branches {
		if ref.Name == branch {
			return true
		}
	}
	return false
}

func (h *Handlers) actionsNewWorkflowEditorData(r *http.Request, cc *codeContext, target, content string) codeEditorData {
	data := h.editorData(r, cc, "new", target, content)
	data.Title = "New workflow · " + cc.row.Name
	data.FormAction = "/" + cc.owner + "/" + cc.row.Name + "/actions/new"
	data.CancelURL = data.FormAction
	data.Primary = "Commit new workflow"
	data.Message = webedit.DefaultMessage(webedit.OpCreate, "", target, nil)
	data.ActiveSubnav = "actions"
	return data
}

func (h *Handlers) renderActionsNewWorkflowPicker(w http.ResponseWriter, r *http.Request, row reposdb.Repo, owner usersdb.User, formError string, status int) {
	basePath := "/" + owner.Username + "/" + row.Name + "/actions"
	q := actionsdb.New()
	workflowRows, err := q.ListWorkflowRunWorkflowsForRepo(r.Context(), h.d.Pool, row.ID)
	if err != nil {
		h.d.Logger.WarnContext(r.Context(), "repo actions new workflow: list workflows", "repo_id", row.ID, "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	workflows, allRunCount, _ := actionsWorkflowViews(workflowRows, actionsListFilters{}, basePath)
	data := h.repoHeaderData(r, row, owner.Username, "actions")
	data["Title"] = "New workflow · " + row.Name
	data["ActionsSidebar"] = actionsSidebar(basePath, workflows, allRunCount, false, "", true)
	data["NewWorkflowHref"] = basePath + "/new"
	data["SupportedTemplates"] = actionsTemplateViews(basePath+"/new", workflowtemplates.Supported())
	data["UnsupportedTemplates"] = actionsTemplateViews("", workflowtemplates.Unsupported())
	data["Error"] = formError
	if status != http.StatusOK {
		w.WriteHeader(status)
	}
	if err := h.d.Render.RenderPage(w, r, "repo/actions_new_workflow", data); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "repo actions new workflow render", "error", err)
	}
}

func actionsTemplateViews(newWorkflowHref string, templates []workflowtemplates.Template) []actionsNewWorkflowTemplateView {
	out := make([]actionsNewWorkflowTemplateView, 0, len(templates))
	for _, tmpl := range templates {
		view := actionsNewWorkflowTemplateView{
			Key:         tmpl.Key,
			Name:        tmpl.Name,
			Description: tmpl.Description,
			Filename:    tmpl.Filename,
			Unsupported: tmpl.Unsupported,
			Reason:      tmpl.Reason,
		}
		if newWorkflowHref != "" && !tmpl.Unsupported {
			q := url.Values{}
			q.Set("template", tmpl.Key)
			view.Href = newWorkflowHref + "?" + q.Encode()
		}
		out = append(out, view)
	}
	return out
}

func actionsTemplatePath(tmpl workflowtemplates.Template) string {
	return actionsdispatch.WorkflowFilesDir + tmpl.Filename
}

func cleanActionsWorkflowAuthorPath(raw string) (string, bool) {
	target := cleanEditorPath(raw)
	if err := webedit.ValidateFilePath(target); err != nil {
		return target, false
	}
	if !strings.HasPrefix(target, actionsdispatch.WorkflowFilesDir) {
		return target, false
	}
	if normalized, ok := normalizeActionsWorkflowFile(target); !ok || normalized != target {
		return target, false
	}
	if !actionsdispatch.ValidWorkflowName(target) {
		return target, false
	}
	return target, true
}

func workflowDiagnosticsForAuthoring(content []byte) ([]workflowDiagnosticView, bool) {
	_, diags, err := workflow.Parse(content)
	blocked := err != nil
	if len(diags) == 0 && err != nil {
		diags = append(diags, workflow.Diagnostic{
			Message:  err.Error(),
			Severity: workflow.Error,
		})
	}
	views := make([]workflowDiagnosticView, 0, len(diags))
	for _, diag := range diags {
		view := workflowDiagnosticView{
			Path:    diag.Path,
			Message: diag.Message,
		}
		if diag.Severity == workflow.Warning {
			view.SeverityLabel = "Warning"
			view.SeverityClass = "pending"
			view.Icon = "alert"
		} else {
			blocked = true
			view.SeverityLabel = "Error"
			view.SeverityClass = "failure"
			view.Icon = "x-circle-fill"
		}
		views = append(views, view)
	}
	return views, blocked
}
