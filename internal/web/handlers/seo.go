// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/tenseleyFlow/shithub/internal/web/middleware"
	"github.com/tenseleyFlow/shithub/internal/web/render"
)

const defaultMetaDescription = "shithub is an AGPL self-hosted GitHub alternative: Git repositories, pull requests, issues, Actions-style CI, organizations, and code search without Copilot."

type marketingHandler struct {
	render  *render.Renderer
	baseURL string
	logger  *slog.Logger
}

type crawlerHandler struct {
	baseURL string
}

var sitemapPaths = []string{
	"/",
	"/about",
	"/explore",
	"/trending",
}

func (h marketingHandler) serveAbout(w http.ResponseWriter, r *http.Request) {
	data := map[string]any{
		"Title":             "GitHub alternative for self-hosted Git teams",
		"MetaDescription":   "shithub is a self-hosted git forge and GitHub alternative for teams that want familiar pull requests, issues, Actions-style CI, and open-source control without Copilot.",
		"CanonicalURL":      canonicalURL(h.baseURL, r, "/about"),
		"OGTitle":           "shithub: self-hosted GitHub alternative",
		"OGDescription":     "An AGPL git forge with GitHub-style repositories, pull requests, issues, organizations, code search, and Actions-style CI without Copilot.",
		"StructuredData":    organizationStructuredData(publicBaseURL(h.baseURL, r)),
		"GlobalSearchQuery": "",
		"Viewer":            middleware.CurrentUserFromContext(r.Context()),
		"CSRFToken":         middleware.CSRFTokenForRequest(r),
		"Repo":              nil,
		"Org":               nil,
	}
	if err := h.render.RenderPage(w, r, "about", data); err != nil {
		h.logger.Error("render about", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
	}
}

func (h crawlerHandler) serveRobots(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300, must-revalidate")

	sitemap := canonicalURL(h.baseURL, r, "/sitemap.xml")
	fmt.Fprintln(w, "User-agent: *")
	fmt.Fprintln(w, "Allow: /")
	fmt.Fprintln(w, "Disallow: /admin")
	fmt.Fprintln(w, "Disallow: /api/")
	fmt.Fprintln(w, "Disallow: /internal/")
	fmt.Fprintln(w, "Disallow: /notifications")
	fmt.Fprintln(w, "Disallow: /search")
	fmt.Fprintln(w, "Disallow: /settings")
	fmt.Fprintln(w, "Disallow: /*.git/")
	if sitemap != "" {
		fmt.Fprintln(w, "Sitemap: "+sitemap) // #nosec G705 -- text/plain robots body; request-host fallback rejects control characters.
	}
}

func (h crawlerHandler) serveSitemap(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300, must-revalidate")

	fmt.Fprintln(w, xml.Header+`<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">`)
	for _, p := range sitemapPaths {
		loc := canonicalURL(h.baseURL, r, p)
		if loc == "" {
			continue
		}
		fmt.Fprint(w, "  <url><loc>")
		_ = xml.EscapeText(w, []byte(loc))
		fmt.Fprintln(w, "</loc></url>")
	}
	fmt.Fprintln(w, "</urlset>")
}

func canonicalURL(configuredBase string, r *http.Request, p string) string {
	base := publicBaseURL(configuredBase, r)
	if base == "" {
		return ""
	}
	if p == "" {
		p = "/"
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return base + p
}

func publicBaseURL(configuredBase string, r *http.Request) string {
	if base := strings.TrimRight(strings.TrimSpace(configuredBase), "/"); base != "" {
		return base
	}
	if r == nil || r.Host == "" {
		return ""
	}
	host := strings.TrimSpace(r.Host)
	if host == "" || strings.ContainsAny(host, "\r\n\t /\\") {
		return ""
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return (&url.URL{Scheme: scheme, Host: host}).String()
}

func organizationStructuredData(baseURL string) template.JS {
	if baseURL == "" {
		return ""
	}
	payload := map[string]any{
		"@context":    "https://schema.org",
		"@type":       "Organization",
		"name":        "shithub",
		"url":         baseURL,
		"logo":        baseURL + "/static/logo/shithub-mark.svg",
		"description": defaultMetaDescription,
		"sameAs": []string{
			"https://github.com/tenseleyFlow/shithub",
		},
	}
	raw, _ := json.Marshal(payload)
	return template.JS(raw) // #nosec G203 -- payload is marshaled from server-owned constants and URLs.
}
