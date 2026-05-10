// SPDX-License-Identifier: AGPL-3.0-or-later

package repo

import (
	"strings"
	"testing"

	repogit "github.com/tenseleyFlow/shithub/internal/repos/git"
)

func TestTreeEntryURL_EscapesPathSegments(t *testing.T) {
	t.Parallel()
	got := treeEntryURL("octo-user", "demo.repo", "feature/x", repogit.EntryBlob, "dir/a file.go")
	want := "/octo-user/demo.repo/blob/feature/x/dir/a%20file.go"
	if got != want {
		t.Fatalf("treeEntryURL = %q, want %q", got, want)
	}
}

func TestTreeEntryURL_NonNavigableEntries(t *testing.T) {
	t.Parallel()
	got := treeEntryURL("octo-user", "demo", "trunk", repogit.EntrySymlink, "vendor/lib")
	if got != "" {
		t.Fatalf("symlink URL = %q, want empty", got)
	}
}

func TestSubmoduleRouteURL_GitHubRemotesBecomeLocalTreeLinks(t *testing.T) {
	t.Parallel()
	cfg := submoduleRouteConfig{
		owner:    "FortranGoingOnForty",
		repoName: "armfortas",
		baseURL:  "https://shithub.sh",
		sshHost:  "git@shithub.sh",
	}
	oid := "56efe6d8fcc64bdc186d9c1a0308db6cc8e03125"

	tests := []struct {
		name   string
		remote string
		want   string
	}{
		{
			name:   "github scp",
			remote: "git@github.com:FortranGoingOnForty/afs-as.git",
			want:   "/FortranGoingOnForty/afs-as/tree/" + oid,
		},
		{
			name:   "github https cross org",
			remote: "https://github.com/tenseleyFlow/bencch.git",
			want:   "/tenseleyFlow/bencch/tree/" + oid,
		},
		{
			name:   "github ssh url",
			remote: "ssh://git@github.com/FortranGoingOnForty/afs-ld.git",
			want:   "/FortranGoingOnForty/afs-ld/tree/" + oid,
		},
		{
			name:   "configured shithub https",
			remote: "https://shithub.sh/tenseleyFlow/bencch.git",
			want:   "/tenseleyFlow/bencch/tree/" + oid,
		},
		{
			name:   "configured shithub scp",
			remote: "git@shithub.sh:tenseleyFlow/bencch.git",
			want:   "/tenseleyFlow/bencch/tree/" + oid,
		},
		{
			name:   "relative sibling",
			remote: "../afs-ld.git",
			want:   "/FortranGoingOnForty/afs-ld/tree/" + oid,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := submoduleRouteURL(cfg, tt.remote, oid)
			if got != tt.want {
				t.Fatalf("submoduleRouteURL(%q) = %q, want %q", tt.remote, got, tt.want)
			}
			route, ok := submoduleRouteForRemote(cfg, tt.remote, oid)
			if !ok {
				t.Fatalf("submoduleRouteForRemote(%q) ok = false", tt.remote)
			}
			wantRepoURL := strings.TrimSuffix(tt.want, "/tree/"+oid)
			if route.RepoURL != wantRepoURL {
				t.Fatalf("RepoURL = %q, want %q", route.RepoURL, wantRepoURL)
			}
		})
	}
}

func TestSubmoduleRouteURL_UnsupportedRemotesStayPlain(t *testing.T) {
	t.Parallel()
	cfg := submoduleRouteConfig{owner: "octo", repoName: "super", baseURL: "https://shithub.sh", sshHost: "git@shithub.sh"}
	oid := "56efe6d8fcc64bdc186d9c1a0308db6cc8e03125"

	for _, remote := range []string{
		"https://example.com/octo/lib.git",
		"javascript:alert(1)",
		"https://github.com/octo/nested/lib.git",
		"https://github.com/%2F/lib.git",
		"",
	} {
		remote := remote
		t.Run(remote, func(t *testing.T) {
			t.Parallel()
			if got := submoduleRouteURL(cfg, remote, oid); got != "" {
				t.Fatalf("submoduleRouteURL(%q) = %q, want empty", remote, got)
			}
		})
	}
}
