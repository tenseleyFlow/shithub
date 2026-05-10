// SPDX-License-Identifier: AGPL-3.0-or-later

package repo

import (
	"bytes"
	"io"
	"net/url"
	"path"
	"strings"

	"golang.org/x/net/html"
)

func codeRouteBase(owner, repoName, route, ref, dir string) string {
	base := "/" + url.PathEscape(owner) + "/" + url.PathEscape(repoName) + "/" + route + "/" + escapePathSegments(ref)
	if dir != "" {
		base += "/" + escapePathSegments(dir)
	}
	return base
}

func rewriteMarkdownRelativeURLs(fragment, linkBase, linkRoot, imageBase, imageRoot string) string {
	if fragment == "" {
		return ""
	}
	z := html.NewTokenizer(strings.NewReader(fragment))
	var out bytes.Buffer
	for {
		tt := z.Next()
		if tt == html.ErrorToken {
			if z.Err() == io.EOF {
				return out.String()
			}
			return fragment
		}
		tok := z.Token()
		switch tt {
		case html.StartTagToken, html.SelfClosingTagToken:
			rewriteMarkdownTokenURLs(&tok, linkBase, linkRoot, imageBase, imageRoot)
		}
		out.WriteString(tok.String())
	}
}

func rewriteMarkdownTokenURLs(tok *html.Token, linkBase, linkRoot, imageBase, imageRoot string) {
	switch tok.Data {
	case "a":
		rewriteAttr(tok, "href", linkBase, linkRoot)
	case "img":
		rewriteAttr(tok, "src", imageBase, imageRoot)
	}
}

func rewriteAttr(tok *html.Token, key, base, root string) {
	for i := range tok.Attr {
		if tok.Attr[i].Key == key {
			tok.Attr[i].Val = rewriteRelativeMarkdownURL(tok.Attr[i].Val, base, root)
		}
	}
}

func rewriteRelativeMarkdownURL(raw, base, root string) string {
	if raw == "" || base == "" || root == "" || strings.TrimSpace(raw) != raw {
		return raw
	}
	if strings.HasPrefix(raw, "#") || strings.HasPrefix(raw, "//") {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.IsAbs() || u.Host != "" || strings.HasPrefix(u.Path, "/") || u.Path == "" {
		return raw
	}
	next := path.Clean(path.Clean(base) + "/" + u.Path)
	if next != root && !strings.HasPrefix(next, root+"/") {
		return raw
	}
	u.Path = next
	u.RawPath = ""
	return u.String()
}
