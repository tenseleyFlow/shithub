// SPDX-License-Identifier: AGPL-3.0-or-later

package repo

import (
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/render"
)

func TestRepoPackagesPageRendersAgainstRealTemplates(t *testing.T) {
	t.Parallel()

	renderer, err := render.New(os.DirFS("../../templates"), render.Options{})
	if err != nil {
		t.Fatalf("render.New on real templates: %v", err)
	}

	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	req := httptest.NewRequest("GET", "/octo/demo/packages", nil)
	rw := httptest.NewRecorder()
	data := map[string]any{
		"Title":        "Packages · demo",
		"CSRFToken":    "test-token",
		"Owner":        "octo",
		"Repo":         reposdb.Repo{ID: 1, Name: "demo", Visibility: reposdb.RepoVisibilityPublic},
		"RepoActions":  repoActionView{IsLoggedIn: true, ReturnTo: "/octo/demo/packages"},
		"RepoCounts":   repoSubnavData{},
		"CanSettings":  true,
		"ActiveSubnav": "packages",
		"Packages": []repoPackageView{
			{
				ID:            1,
				Name:          "demo-package",
				PackageType:   "generic",
				Description:   "Fixture package",
				LatestVersion: "v1.0.0",
				PackageBytes:  "1.0 KB",
				VersionCount:  1,
				FileCount:     1,
				Files: []repoPackageFileView{
					{
						ID:          7,
						Version:     "v1.0.0",
						Filename:    "demo.tar.gz",
						SizeLabel:   "1.0 KB",
						DownloadURL: "/octo/demo/packages/files/7/download",
						CreatedAt:   now,
					},
				},
				DeleteURL: "/octo/demo/packages/1/delete",
				UpdatedAt: now,
				CreatedAt: now,
			},
		},
		"CanPublishPackages": true,
		"UploadForm":         repoPackageUploadForm{},
		"MaxPackageFileSize": "512.0 MB",
	}

	if err := renderer.RenderPage(rw, req, "repo/packages", data); err != nil {
		t.Fatalf("RenderPage(repo/packages): %v", err)
	}
	body := rw.Body.String()
	if !strings.Contains(body, "demo-package") || !strings.Contains(body, "/octo/demo/packages/files/7/download") {
		t.Fatalf("rendered packages page missing package content: %s", body)
	}
}
