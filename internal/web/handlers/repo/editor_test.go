// SPDX-License-Identifier: AGPL-3.0-or-later

package repo

import (
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/tenseleyFlow/shithub/internal/web/middleware"
	"github.com/tenseleyFlow/shithub/internal/web/render"
)

func TestCodeEditorDataRendersAgainstRealTemplates(t *testing.T) {
	t.Parallel()

	tmplFS := os.DirFS("../../templates")
	renderer, err := render.New(tmplFS, render.Options{})
	if err != nil {
		t.Fatalf("render.New on real templates: %v", err)
	}

	req := httptest.NewRequest("GET", "/octo/demo/edit/trunk/README.md", nil)
	rw := httptest.NewRecorder()
	data := codeEditorData{
		Title:       "Edit · demo",
		CSRFToken:   "test-token",
		Viewer:      middleware.CurrentUser{ID: 1, Username: "octo"},
		Owner:       "octo",
		Repo:        editorTemplateRepo{Name: "demo", Visibility: "public"},
		Ref:         "trunk",
		RefDisplay:  "trunk",
		Path:        "README.md",
		PathValue:   "README.md",
		Mode:        "edit",
		FormAction:  "/octo/demo/edit/trunk/README.md",
		CancelURL:   "/octo/demo/blob/trunk/README.md",
		PreviewURL:  "/octo/demo/markdown-preview",
		Content:     "# Demo\n",
		Message:     "Update README.md",
		Primary:     "Commit changes",
		RepoActions: repoActionView{IsLoggedIn: true, ReturnTo: "/octo/demo/edit/trunk/README.md"},
		RepoCounts:  repoSubnavData{},
		CanSettings: true,
	}

	if err := renderer.RenderPage(rw, req, "repo/editor", data); err != nil {
		t.Fatalf("RenderPage(repo/editor): %v", err)
	}
	body := rw.Body.String()
	if !strings.Contains(body, `data-code-editor`) {
		t.Fatalf("rendered editor body missing editor root: %s", body)
	}
}

type editorTemplateRepo struct {
	Name         string
	Visibility   string
	WatcherCount int64
	ForkCount    int64
	StarCount    int64
}
