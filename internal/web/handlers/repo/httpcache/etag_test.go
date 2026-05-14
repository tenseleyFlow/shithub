// SPDX-License-Identifier: AGPL-3.0-or-later

package httpcache

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestETag_FormatAndShape(t *testing.T) {
	t.Parallel()
	got := ETag(42, "deadbeefcafefacefeedfacedeadbeef00112233", 1)
	want := `"42-deadbeefcafefacefeedfacedeadbeef00112233-1"`
	if got != want {
		t.Errorf("ETag = %q, want %q", got, want)
	}
	if !strings.HasPrefix(got, `"`) || !strings.HasSuffix(got, `"`) {
		t.Errorf("ETag not double-quoted: %q", got)
	}
}

func TestETag_DistinctInputsProduceDistinctTags(t *testing.T) {
	t.Parallel()
	base := ETag(1, "aaaa", 1)
	if got := ETag(2, "aaaa", 1); got == base {
		t.Errorf("ETag collided across repo_id: %q == %q", got, base)
	}
	if got := ETag(1, "bbbb", 1); got == base {
		t.Errorf("ETag collided across branch_oid: %q == %q", got, base)
	}
	if got := ETag(1, "aaaa", 2); got == base {
		t.Errorf("ETag collided across page: %q == %q", got, base)
	}
}

func TestETag_StableForSameInputs(t *testing.T) {
	t.Parallel()
	a := ETag(7, "facecafe", 3)
	b := ETag(7, "facecafe", 3)
	if a != b {
		t.Errorf("ETag not stable: %q != %q", a, b)
	}
}

// reqWithINM is a tiny helper that builds an *http.Request carrying
// the given If-None-Match header. The path is irrelevant — the
// IfNoneMatch helper only reads the header.
func reqWithINM(header string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	if header != "" {
		r.Header.Set("If-None-Match", header)
	}
	return r
}

func TestIfNoneMatch_SingleStrongMatch(t *testing.T) {
	t.Parallel()
	etag := ETag(1, "abc", 1)
	if !IfNoneMatch(reqWithINM(etag), etag) {
		t.Errorf("expected match for identical strong tag %q", etag)
	}
}

func TestIfNoneMatch_SingleStrongMiss(t *testing.T) {
	t.Parallel()
	if IfNoneMatch(reqWithINM(`"1-abc-1"`), `"1-abc-2"`) {
		t.Errorf("strong tags differing by page should not match")
	}
}

func TestIfNoneMatch_ListContainsMatch(t *testing.T) {
	t.Parallel()
	etag := `"1-abc-1"`
	header := `"0-stale-1", "1-abc-1", "1-abc-2"`
	if !IfNoneMatch(reqWithINM(header), etag) {
		t.Errorf("expected match: list %q contains %q", header, etag)
	}
}

func TestIfNoneMatch_ListNoMatch(t *testing.T) {
	t.Parallel()
	if IfNoneMatch(reqWithINM(`"a", "b", "c"`), `"d"`) {
		t.Errorf("expected miss when no list entry matches")
	}
}

func TestIfNoneMatch_WildcardAlwaysMatches(t *testing.T) {
	t.Parallel()
	if !IfNoneMatch(reqWithINM("*"), `"anything"`) {
		t.Errorf("wildcard * should match any etag")
	}
}

func TestIfNoneMatch_WildcardInListMatches(t *testing.T) {
	t.Parallel()
	// `*` mixed into a list is unusual but legal per RFC 7232; we
	// honor it so misbehaving CDNs don't strand the cache.
	if !IfNoneMatch(reqWithINM(`"unrelated", *`), `"1-abc-1"`) {
		t.Errorf("wildcard * inside a list should still match")
	}
}

func TestIfNoneMatch_WeakServerMatchesStrongClient(t *testing.T) {
	t.Parallel()
	// RFC 7232 §3.2: If-None-Match uses weak comparison —
	// W/"x" on either side matches "x" on the other.
	if !IfNoneMatch(reqWithINM(`"1-abc-1"`), `W/"1-abc-1"`) {
		t.Errorf("weak server tag should match strong client tag")
	}
}

func TestIfNoneMatch_StrongServerMatchesWeakClient(t *testing.T) {
	t.Parallel()
	if !IfNoneMatch(reqWithINM(`W/"1-abc-1"`), `"1-abc-1"`) {
		t.Errorf("strong server tag should match weak client tag")
	}
}

func TestIfNoneMatch_WhitespaceAroundCommas(t *testing.T) {
	t.Parallel()
	// Real-world headers often have arbitrary surrounding
	// whitespace; the spec allows it. Make sure we don't fail to
	// match on `"x",   "y"` or `"x" ,"y"`.
	cases := []string{
		`"1-abc-1","2-def-1"`,
		`"1-abc-1" , "2-def-1"`,
		`   "1-abc-1"   ,   "2-def-1"   `,
	}
	for _, header := range cases {
		if !IfNoneMatch(reqWithINM(header), `"2-def-1"`) {
			t.Errorf("expected match with header %q", header)
		}
	}
}

func TestIfNoneMatch_EmptyTokensInListSkipped(t *testing.T) {
	t.Parallel()
	// Some intermediaries produce trailing-comma list shapes; treat
	// empty tokens as no-ops rather than letting them collide with
	// an empty server etag (which we already reject below).
	if !IfNoneMatch(reqWithINM(`"x",,"y"`), `"y"`) {
		t.Errorf("empty tokens between commas should be skipped, not fail the match")
	}
}

func TestIfNoneMatch_EmptyHeaderReturnsFalse(t *testing.T) {
	t.Parallel()
	if IfNoneMatch(reqWithINM(""), `"x"`) {
		t.Errorf("empty If-None-Match must not short-circuit")
	}
}

func TestIfNoneMatch_EmptyEtagReturnsFalse(t *testing.T) {
	t.Parallel()
	// Defensive: callers that haven't built their server-side etag
	// yet shouldn't accidentally 304 because the request header is
	// a `*` wildcard.
	if IfNoneMatch(reqWithINM("*"), "") {
		t.Errorf("empty server-side etag must not match even *")
	}
}

func TestIfNoneMatch_QuoteSensitivity(t *testing.T) {
	t.Parallel()
	// The opaque-tag is compared as the full quoted-string
	// (including the surrounding quotes). An unquoted token must
	// not match a quoted server etag.
	if IfNoneMatch(reqWithINM(`1-abc-1`), `"1-abc-1"`) {
		t.Errorf("unquoted client token should not match quoted server etag")
	}
}

func TestIfNoneMatch_BackslashEscapeStaysLiteral(t *testing.T) {
	t.Parallel()
	// RFC 7232 §2.3 forbids backslash inside opaque-tag (etagc
	// rules out DEL + `"` only, but the qdtext escape mechanism is
	// not invoked for entity tags). A misbehaving proxy that
	// injects `\` into the value should NOT collide with our
	// well-formed ETag — string comparison stays literal.
	if IfNoneMatch(reqWithINM(`"1-abc\-1"`), `"1-abc-1"`) {
		t.Errorf(`backslash-escaped client token must not match clean server etag`)
	}
}
