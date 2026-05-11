// SPDX-License-Identifier: AGPL-3.0-or-later

package repo

import (
	"context"
	"errors"
	"net/url"
	"os"
	"path"
	"sort"

	"github.com/tenseleyFlow/shithub/internal/actions/trigger"
	"github.com/tenseleyFlow/shithub/internal/actions/workflow"
	"github.com/tenseleyFlow/shithub/internal/auth/policy"
	repogit "github.com/tenseleyFlow/shithub/internal/repos/git"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

type actionsDispatchWorkflowView struct {
	File         string
	Name         string
	DispatchHref string
	Inputs       []actionsDispatchInputView
}

type actionsDispatchInputView struct {
	Name        string
	Description string
	Type        string
	Default     string
	Required    bool
	IsBoolean   bool
	IsChoice    bool
	Options     []actionsDispatchOptionView
}

type actionsDispatchOptionView struct {
	Value    string
	Selected bool
}

func (h *Handlers) actionsDispatchWorkflowViews(ctx context.Context, row reposdb.Repo, owner string) ([]actionsDispatchWorkflowView, error) {
	viewer := middleware.CurrentUserFromContext(ctx)
	if viewer.IsAnonymous() {
		return nil, nil
	}
	dec := policy.Can(ctx, policy.Deps{Pool: h.d.Pool}, viewer.PolicyActor(), policy.ActionRepoWrite, policy.NewRepoRefFromRepo(row))
	if !dec.Allow {
		return nil, nil
	}
	if row.DefaultBranch == "" {
		return nil, nil
	}
	gitDir, err := h.d.RepoFS.RepoPath(owner, row.Name)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(gitDir); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	headSHA, err := repogit.ResolveRefOID(ctx, gitDir, row.DefaultBranch)
	if err != nil {
		if errors.Is(err, repogit.ErrRefNotFound) {
			return nil, nil
		}
		return nil, err
	}
	files, _, err := trigger.Discover(ctx, gitDir, headSHA)
	if err != nil {
		return nil, err
	}

	views := make([]actionsDispatchWorkflowView, 0, len(files))
	for _, file := range files {
		wf, diags, err := workflow.Parse(file.Bytes)
		if err != nil || workflowHasErrorDiagnostics(diags) || wf.On.WorkflowDispatch == nil {
			continue
		}
		views = append(views, actionsDispatchWorkflowView{
			File:         file.Path,
			Name:         workflowDisplayName(wf.Name, file.Path),
			DispatchHref: "/" + owner + "/" + row.Name + "/actions/workflows/" + url.PathEscape(path.Base(file.Path)) + "/dispatches",
			Inputs:       actionsDispatchInputViews(wf.On.WorkflowDispatch.Inputs),
		})
	}
	sort.SliceStable(views, func(i, j int) bool {
		if views[i].Name == views[j].Name {
			return views[i].File < views[j].File
		}
		return views[i].Name < views[j].Name
	})
	return views, nil
}

func workflowHasErrorDiagnostics(diags []workflow.Diagnostic) bool {
	for _, d := range diags {
		if d.Severity == workflow.Error {
			return true
		}
	}
	return false
}

func actionsDispatchInputViews(inputs []workflow.DispatchInput) []actionsDispatchInputView {
	views := make([]actionsDispatchInputView, 0, len(inputs))
	for _, input := range inputs {
		view := actionsDispatchInputView{
			Name:        input.Name,
			Description: input.Description,
			Type:        input.Type,
			Default:     input.Default,
			Required:    input.Required,
			IsBoolean:   input.Type == "boolean",
			IsChoice:    input.Type == "choice",
		}
		for _, option := range input.Options {
			view.Options = append(view.Options, actionsDispatchOptionView{
				Value:    option,
				Selected: input.Default == option,
			})
		}
		views = append(views, view)
	}
	return views
}
