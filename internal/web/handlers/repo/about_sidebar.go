// SPDX-License-Identifier: AGPL-3.0-or-later

package repo

import (
	"context"
	"fmt"
	"html/template"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/tenseleyFlow/shithub/internal/repos/git"
	"github.com/tenseleyFlow/shithub/internal/repos/identity"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/handlers/repo/treecache"
)

type repoAboutData struct {
	Resources    []repoAboutResource
	Contributors []repoAboutContributor
	Languages    []repoAboutLanguage
}

type repoAboutResource struct {
	Icon        string
	Label       string
	Href        string
	Path        string
	OverviewTab string
}

type repoReadmeTab struct {
	Icon   string
	Label  string
	Href   string
	Active bool
}

const (
	repoOverviewReadmeTab        = "readme-ov-file"
	repoOverviewCodeOfConductTab = "coc-ov-file"
	repoOverviewContributingTab  = "contributing-ov-file"
	repoOverviewLicenseTab       = "license-ov-file"
	repoOverviewSecurityTab      = "security-ov-file"
)

type repoAboutContributor struct {
	User          bool
	Username      string
	DisplayName   string
	AvatarURL     string
	IdenticonSeed string
	Label         string
	Count         int
}

type repoAboutLanguage struct {
	Name    string
	Color   template.CSS
	Width   template.CSS
	Percent string
}

type repoLanguageAggregate struct {
	name  string
	color string
	size  int64
}

// repoAbout builds the About sidebar. commitOID is the resolved OID
// of the commit being rendered; it keys the contributor and language
// caches, so a push (new OID) is what invalidates them. Pass "" to
// bypass the caches.
func (h *Handlers) repoAbout(ctx context.Context, cc *codeContext, commitOID string, rootEntries []git.TreeEntry) repoAboutData {
	return repoAboutData{
		Resources:    repoAboutResources(cc.owner, cc.row.Name, cc.ref, cc.row, rootEntries),
		Contributors: h.repoAboutContributors(ctx, cc, commitOID),
		Languages:    h.repoAboutLanguages(ctx, cc, commitOID),
	}
}

func repoAboutResources(owner, repoName, ref string, row reposdb.Repo, entries []git.TreeEntry) []repoAboutResource {
	findRootFile := func(matches func(string) bool) string {
		for _, e := range entries {
			if e.Kind != git.EntryBlob {
				continue
			}
			lower := strings.ToLower(e.Name)
			if matches(lower) {
				return e.Name
			}
		}
		return ""
	}
	overviewHref := func(tab string) string {
		base := "/" + owner + "/" + repoName
		if ref != "" && row.DefaultBranch != "" && ref != row.DefaultBranch {
			base += "/tree/" + ref
		}
		return base + "?tab=" + tab + "#readme"
	}

	resources := []repoAboutResource{}
	if readme := findRootFile(func(name string) bool { return strings.HasPrefix(name, "readme") }); readme != "" {
		resources = append(resources, repoAboutResource{
			Icon:        "book",
			Label:       "Readme",
			Href:        overviewHref(repoOverviewReadmeTab),
			Path:        readme,
			OverviewTab: repoOverviewReadmeTab,
		})
	}
	licensePath := findRootFile(func(name string) bool {
		return name == "license" || strings.HasPrefix(name, "license.") || name == "copying" || strings.HasPrefix(name, "copying.")
	})
	if row.LicenseKey.Valid || licensePath != "" {
		label := "License"
		if row.LicenseKey.Valid {
			label = row.LicenseKey.String + " license"
		}
		href := ""
		if licensePath != "" {
			href = overviewHref(repoOverviewLicenseTab)
		}
		resources = append(resources, repoAboutResource{
			Icon:        "law",
			Label:       label,
			Href:        href,
			Path:        licensePath,
			OverviewTab: repoOverviewLicenseTab,
		})
	}
	if codeOfConduct := findRootFile(func(name string) bool {
		return name == "code_of_conduct.md" || name == "code-of-conduct.md" || name == "code_of_conduct" || name == "code-of-conduct"
	}); codeOfConduct != "" {
		resources = append(resources, repoAboutResource{
			Icon:        "heart",
			Label:       "Code of conduct",
			Href:        overviewHref(repoOverviewCodeOfConductTab),
			Path:        codeOfConduct,
			OverviewTab: repoOverviewCodeOfConductTab,
		})
	}
	if contributing := findRootFile(func(name string) bool {
		return name == "contributing.md" || name == "contributing"
	}); contributing != "" {
		resources = append(resources, repoAboutResource{
			Icon:        "people",
			Label:       "Contributing",
			Href:        overviewHref(repoOverviewContributingTab),
			Path:        contributing,
			OverviewTab: repoOverviewContributingTab,
		})
	}
	if security := findRootFile(func(name string) bool {
		return name == "security.md" || name == "security"
	}); security != "" {
		resources = append(resources, repoAboutResource{
			Icon:        "law",
			Label:       "Security policy",
			Href:        overviewHref(repoOverviewSecurityTab),
			Path:        security,
			OverviewTab: repoOverviewSecurityTab,
		})
	}
	resources = append(resources,
		repoAboutResource{Icon: "pulse", Label: "Activity", Href: "/" + owner + "/" + repoName + "/activity"},
		repoAboutResource{Icon: "note", Label: "Custom properties", Href: "/" + owner + "/" + repoName + "/settings/custom-properties"},
	)
	return resources
}

func repoReadmeTabs(resources []repoAboutResource, activeTab string) []repoReadmeTab {
	tabs := make([]repoReadmeTab, 0, len(resources))
	for _, resource := range resources {
		if resource.Href == "" || resource.Path == "" || resource.OverviewTab == "" {
			continue
		}
		label := resource.Label
		switch lower := strings.ToLower(resource.Label); {
		case lower == "readme":
			label = "README"
		case lower == "code of conduct", lower == "contributing", strings.Contains(lower, "license"):
		case strings.HasPrefix(lower, "security"):
			label = "Security"
		default:
			continue
		}
		tabs = append(tabs, repoReadmeTab{
			Icon:   resource.Icon,
			Label:  label,
			Href:   resource.Href,
			Active: resource.OverviewTab == activeTab,
		})
	}
	return tabs
}

func activeRepoOverviewResource(resources []repoAboutResource, requestedTab string) (repoAboutResource, bool) {
	docs := make([]repoAboutResource, 0, len(resources))
	for _, resource := range resources {
		if resource.Path == "" || resource.OverviewTab == "" {
			continue
		}
		docs = append(docs, resource)
		if requestedTab != "" && resource.OverviewTab == requestedTab {
			return resource, true
		}
	}
	if len(docs) == 0 {
		return repoAboutResource{}, false
	}
	for _, resource := range docs {
		if resource.OverviewTab == repoOverviewReadmeTab {
			return resource, true
		}
	}
	return docs[0], true
}

func repoOverviewDocumentLabel(resource repoAboutResource) string {
	if strings.EqualFold(resource.Label, "Readme") {
		return "README"
	}
	if strings.HasPrefix(strings.ToLower(resource.Label), "security") {
		return "Security"
	}
	return resource.Label
}

// contributorWalkDepth is how far back the About sidebar's
// contributor strip counts. It is a `git log -n 500` on every repo
// home view, so the tally it reduces to is cached per commit OID.
const contributorWalkDepth = 500

// repoAboutContributorTally runs the bounded walk and reduces it to
// one row per distinct author. Only this reduction is cached —
// identity resolution stays per-request because it reads the users
// table and its answer can change without any git ref moving.
func (h *Handlers) repoAboutContributorTally(ctx context.Context, cc *codeContext, commitOID string) ([]treecache.ContributorTally, error) {
	return h.d.TreeCache.Contributors(ctx,
		treecache.RevKey{RepoID: cc.row.ID, CommitOID: commitOID},
		func(ctx context.Context) ([]treecache.ContributorTally, error) {
			commits, err := git.Log(ctx, cc.gitDir, git.LogOptions{Ref: cc.ref, MaxCount: contributorWalkDepth})
			if err != nil {
				return nil, err
			}
			order := make([]string, 0, len(commits))
			byKey := map[string]*treecache.ContributorTally{}
			for _, c := range commits {
				key := strings.ToLower(strings.TrimSpace(c.AuthorEmail))
				if key == "" {
					key = "name:" + strings.ToLower(strings.TrimSpace(c.AuthorName))
				}
				tally, ok := byKey[key]
				if !ok {
					tally = &treecache.ContributorTally{Name: c.AuthorName, Email: c.AuthorEmail}
					byKey[key] = tally
					order = append(order, key)
				}
				tally.Count++
			}
			out := make([]treecache.ContributorTally, 0, len(order))
			for _, key := range order {
				out = append(out, *byKey[key])
			}
			return out, nil
		})
}

func (h *Handlers) repoAboutContributors(ctx context.Context, cc *codeContext, commitOID string) []repoAboutContributor {
	tallies, err := h.repoAboutContributorTally(ctx, cc, commitOID)
	if err != nil {
		if h.d.Logger != nil {
			h.d.Logger.WarnContext(ctx, "repo about: contributors", "error", err)
		}
		return nil
	}
	type aggregate struct {
		contributor repoAboutContributor
	}
	byAuthor := map[string]*aggregate{}
	resolver := identity.New(h.d.Pool)
	for _, c := range tallies {
		resolved := resolver.Resolve(ctx, c.Email)
		key := ""
		contributor := repoAboutContributor{}
		if resolved.User {
			key = "user:" + strconv.FormatInt(resolved.UserID, 10)
			contributor.User = true
			contributor.Username = resolved.Username
			contributor.DisplayName = resolved.DisplayName
			contributor.AvatarURL = resolved.AvatarURL
			contributor.Label = resolved.DisplayName
			if contributor.Label == "" {
				contributor.Label = resolved.Username
			}
		} else {
			email := strings.ToLower(strings.TrimSpace(c.Email))
			name := strings.ToLower(strings.TrimSpace(c.Name))
			if email != "" {
				key = "email:" + email
			} else if name != "" {
				key = "name:" + name
			}
			contributor.Label = strings.TrimSpace(c.Name)
			if contributor.Label == "" {
				contributor.Label = strings.TrimSpace(c.Email)
			}
			contributor.IdenticonSeed = resolved.IdenticonSeed
		}
		if key == "" {
			continue
		}
		agg, ok := byAuthor[key]
		if !ok {
			agg = &aggregate{contributor: contributor}
			byAuthor[key] = agg
		}
		agg.contributor.Count += c.Count
	}

	contributors := make([]repoAboutContributor, 0, len(byAuthor))
	for _, agg := range byAuthor {
		c := agg.contributor
		if c.Label == "" {
			c.Label = "Unknown author"
		}
		contributors = append(contributors, c)
	}
	sort.Slice(contributors, func(i, j int) bool {
		if contributors[i].Count != contributors[j].Count {
			return contributors[i].Count > contributors[j].Count
		}
		return strings.ToLower(contributors[i].Label) < strings.ToLower(contributors[j].Label)
	})
	if len(contributors) > 11 {
		contributors = contributors[:11]
	}
	return contributors
}

// repoAboutLanguageBytes is the `git ls-tree -r`-derived language
// aggregate: one entry per language, in bytes. The recursive walk
// itself is never cached (it can be megabytes of path text on a big
// repo); its reduction to a handful of numbers is, keyed per commit
// OID.
func (h *Handlers) repoAboutLanguageBytes(ctx context.Context, cc *codeContext, commitOID string) (map[string]int64, error) {
	return h.d.TreeCache.Languages(ctx,
		treecache.RevKey{RepoID: cc.row.ID, CommitOID: commitOID},
		func(ctx context.Context) (map[string]int64, error) {
			blobs, err := git.ListBlobs(ctx, cc.gitDir, cc.ref)
			if err != nil {
				return nil, err
			}
			byName := map[string]int64{}
			for _, blob := range blobs {
				if blob.Size <= 0 {
					continue
				}
				name, _, ok := repoLanguageForPath(blob.Path)
				if !ok {
					continue
				}
				byName[name] += blob.Size
			}
			return byName, nil
		})
}

func (h *Handlers) repoAboutLanguages(ctx context.Context, cc *codeContext, commitOID string) []repoAboutLanguage {
	byName, err := h.repoAboutLanguageBytes(ctx, cc, commitOID)
	if err != nil {
		if h.d.Logger != nil {
			h.d.Logger.WarnContext(ctx, "repo about: languages", "error", err)
		}
		return fallbackPrimaryLanguage(cc.row)
	}

	var total int64
	for _, size := range byName {
		total += size
	}
	if total == 0 {
		return fallbackPrimaryLanguage(cc.row)
	}

	ordered := make([]repoLanguageAggregate, 0, len(byName))
	for name, size := range byName {
		ordered = append(ordered, repoLanguageAggregate{name: name, color: repoLanguageColor(name), size: size})
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].size != ordered[j].size {
			return ordered[i].size > ordered[j].size
		}
		return ordered[i].name < ordered[j].name
	})
	ordered = mergeSmallLanguages(ordered)

	out := make([]repoAboutLanguage, 0, len(ordered))
	for _, agg := range ordered {
		percent := float64(agg.size) / float64(total) * 100
		out = append(out, repoAboutLanguage{
			Name:    agg.name,
			Color:   template.CSS(agg.color),                      //nolint:gosec // values come from repoLanguageColor constants
			Width:   template.CSS(fmt.Sprintf("%.1f%%", percent)), //nolint:gosec // computed numeric percent
			Percent: fmt.Sprintf("%.1f%%", percent),
		})
	}
	return out
}

func mergeSmallLanguages(ordered []repoLanguageAggregate) []repoLanguageAggregate {
	const maxLanguages = 7
	if len(ordered) <= maxLanguages {
		return ordered
	}
	kept := append([]repoLanguageAggregate(nil), ordered[:maxLanguages-1]...)
	var rest int64
	for _, agg := range ordered[maxLanguages-1:] {
		rest += agg.size
	}
	for i := range kept {
		if kept[i].name == "Other" {
			kept[i].size += rest
			sort.Slice(kept, func(a, b int) bool {
				if kept[a].size != kept[b].size {
					return kept[a].size > kept[b].size
				}
				return kept[a].name < kept[b].name
			})
			return kept
		}
	}
	return append(kept, repoLanguageAggregate{name: "Other", color: repoLanguageColor("Other"), size: rest})
}

func fallbackPrimaryLanguage(row reposdb.Repo) []repoAboutLanguage {
	if !row.PrimaryLanguage.Valid || row.PrimaryLanguage.String == "" {
		return nil
	}
	color := repoLanguageColor(row.PrimaryLanguage.String)
	return []repoAboutLanguage{{
		Name:    row.PrimaryLanguage.String,
		Color:   template.CSS(color),  //nolint:gosec // values come from repoLanguageColor constants
		Width:   template.CSS("100%"), //nolint:gosec // constant CSS percentage
		Percent: "100.0%",
	}}
}

func repoLanguageForPath(p string) (name, color string, ok bool) {
	base := strings.ToLower(filepath.Base(p))
	ext := strings.ToLower(filepath.Ext(p))
	switch base {
	case "makefile", "gnumakefile":
		return "Makefile", repoLanguageColor("Makefile"), true
	case "dockerfile":
		return "Dockerfile", repoLanguageColor("Dockerfile"), true
	}
	if isLinguistIgnored(ext, base) {
		return "", "", false
	}
	switch ext {
	case ".go":
		name = "Go"
	case ".html", ".htm":
		name = "HTML"
	case ".css":
		name = "CSS"
	case ".sh", ".bash", ".zsh", ".fish":
		name = "Shell"
	case ".sql":
		name = "PLpgSQL"
	case ".jinja", ".j2":
		name = "Jinja"
	case ".js", ".mjs", ".cjs":
		name = "JavaScript"
	case ".ts", ".tsx":
		name = "TypeScript"
	case ".py":
		name = "Python"
	case ".java":
		name = "Java"
	case ".rs":
		name = "Rust"
	case ".rb":
		name = "Ruby"
	case ".php":
		name = "PHP"
	case ".c", ".h":
		name = "C"
	case ".cc", ".cpp", ".cxx", ".hpp", ".hh":
		name = "C++"
	default:
		name = "Other"
	}
	return name, repoLanguageColor(name), true
}

func isLinguistIgnored(ext, base string) bool {
	switch ext {
	case ".md", ".markdown", ".txt", ".rst", ".adoc", ".png", ".jpg", ".jpeg", ".gif", ".webp", ".ico", ".pdf", ".lock":
		return true
	}
	switch base {
	case "license", "copying", "notice", "changelog":
		return true
	}
	return false
}

func repoLanguageColor(name string) string {
	switch name {
	case "Go":
		return "#00add8"
	case "HTML":
		return "#e34c26"
	case "CSS":
		return "#663399"
	case "Shell":
		return "#89e051"
	case "PLpgSQL":
		return "#336790"
	case "Jinja":
		return "#a52a22"
	case "JavaScript":
		return "#f1e05a"
	case "TypeScript":
		return "#3178c6"
	case "Python":
		return "#3572a5"
	case "Java":
		return "#b07219"
	case "Rust":
		return "#dea584"
	case "Ruby":
		return "#701516"
	case "PHP":
		return "#4f5d95"
	case "C":
		return "#555555"
	case "C++":
		return "#f34b7d"
	case "Makefile":
		return "#427819"
	case "Dockerfile":
		return "#384d54"
	default:
		return "#ededed"
	}
}
