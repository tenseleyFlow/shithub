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

func TestCodeRefDisplayShortensSHAs(t *testing.T) {
	t.Parallel()
	sha := "cea2569cfb80705f3eaeceadd8968aa031c22813"
	if got := codeRefDisplay(sha); got != "cea2569" {
		t.Fatalf("codeRefDisplay(sha) = %q, want cea2569", got)
	}
	if got := codeRefDisplay("release/v1"); got != "release/v1" {
		t.Fatalf("codeRefDisplay(branch) = %q, want release/v1", got)
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

func TestGitHubSubmoduleFetchURL_CanonicalizesSupportedRemotes(t *testing.T) {
	t.Parallel()
	cfg := submoduleRouteConfig{
		owner:    "FortranGoingOnForty",
		repoName: "lib-modules",
		baseURL:  "https://shithub.sh",
		sshHost:  "git@shithub.sh",
	}

	for _, tt := range []struct {
		name   string
		remote string
		want   string
	}{
		{
			name:   "scp",
			remote: "git@github.com:FortranGoingOnForty/afs-as.git",
			want:   "https://github.com/FortranGoingOnForty/afs-as.git",
		},
		{
			name:   "https",
			remote: "https://github.com/tenseleyFlow/bencch.git",
			want:   "https://github.com/tenseleyFlow/bencch.git",
		},
		{
			name:   "ssh url",
			remote: "ssh://git@github.com/FortranGoingOnForty/afs-ld.git",
			want:   "https://github.com/FortranGoingOnForty/afs-ld.git",
		},
		{
			name:   "relative sibling without suffix",
			remote: "../fgof-process",
			want:   "https://github.com/FortranGoingOnForty/fgof-process.git",
		},
		{
			name:   "relative sibling with suffix",
			remote: "../fgof-fs.git",
			want:   "https://github.com/FortranGoingOnForty/fgof-fs.git",
		},
	} {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, ok := githubSubmoduleFetchURL(cfg, tt.remote)
			if !ok {
				t.Fatalf("githubSubmoduleFetchURL(%q) ok = false", tt.remote)
			}
			if got != tt.want {
				t.Fatalf("githubSubmoduleFetchURL(%q) = %q, want %q", tt.remote, got, tt.want)
			}
		})
	}
}

func TestGitHubSubmoduleFetchURL_RejectsUnsupportedRemotes(t *testing.T) {
	t.Parallel()
	cfg := submoduleRouteConfig{owner: "octo", repoName: "super", baseURL: "https://shithub.sh", sshHost: "git@shithub.sh"}

	for _, remote := range []string{
		"https://shithub.sh/tenseleyFlow/bencch.git",
		"../../../afs-ld.git",
		"https://example.com/octo/lib.git",
		"https://github.com/octo/nested/lib.git",
		"https://github.com/%2F/lib.git",
		"javascript:alert(1)",
		"",
	} {
		remote := remote
		t.Run(remote, func(t *testing.T) {
			t.Parallel()
			if got, ok := githubSubmoduleFetchURL(cfg, remote); ok || got != "" {
				t.Fatalf("githubSubmoduleFetchURL(%q) = %q, %v; want empty, false", remote, got, ok)
			}
		})
	}
}
