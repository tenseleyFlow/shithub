// SPDX-License-Identifier: AGPL-3.0-or-later

// Package keyset implements opaque keyset-pagination cursors. The
// cursor wraps the last row's ordering value + id (deterministic
// tiebreaker for rows that share the value) so the next page is a
// simple `WHERE (value, id) < ($cursor_value, $cursor_id) ORDER BY
// value DESC, id DESC LIMIT n` — constant-time per page regardless
// of depth.
//
// Two encoding modes:
//
//   - Encode  — base64url(JSON) of the cursor. Use for pagination
//     surfaces where the leak of the value/id pair is OK
//     (commits list, public issues).
//
//   - Sign    — base64url(JSON || HMAC-SHA256). Use for visibility-
//     sensitive surfaces (audit log, notifications) where
//     a leaked cursor must not let a different user
//     resume someone else's enumeration.
//
// Decoders are strict: a malformed or tampered cursor returns
// ErrInvalidCursor, never a "best-effort" partial decode. Callers
// fall back to the first page on error.
package keyset

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
)

// ErrInvalidCursor is returned by Decode / Verify when the cursor
// can't be parsed, has an HMAC mismatch, or has unsupported fields.
var ErrInvalidCursor = errors.New("keyset: invalid cursor")

// Cursor is the canonical payload. Value is the ordering column's
// last-seen value (timestamp Unix nanos, integer id, etc.); ID is
// the tiebreaker (typically the row's primary key). Both are
// int64s — surface flexibility comes from the caller's choice of
// what to put there.
type Cursor struct {
	Value int64 `json:"v"`
	ID    int64 `json:"i"`
}

// Encode wraps c into an opaque base64url string. Suitable for the
// `?cursor=` URL parameter; URL-safe alphabet, no padding.
func Encode(c Cursor) string {
	raw, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(raw)
}

// Decode reverses Encode. Returns ErrInvalidCursor on malformed
// input.
func Decode(s string) (Cursor, error) {
	if s == "" {
		return Cursor{}, ErrInvalidCursor
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return Cursor{}, ErrInvalidCursor
	}
	var c Cursor
	if err := json.Unmarshal(raw, &c); err != nil {
		return Cursor{}, ErrInvalidCursor
	}
	return c, nil
}

// Sign returns a tamper-resistant cursor. The output is the JSON-
// encoded cursor concatenated with a 32-byte HMAC-SHA256, both
// base64url'd. Use the same key as your other per-user-binding
// HMAC consumers (e.g. notif unsubscribe key) or mint a dedicated
// keyset signing key — whichever your operator-secrets layout
// prefers.
func Sign(c Cursor, key []byte) string {
	if len(key) == 0 {
		// Refusing to sign with an empty key is the operator's bug
		// surfacing as a panic at boot rather than a silently-
		// unsigned cursor making it to production. Callers should
		// pre-check; this is the belt-and-braces guard.
		panic("keyset: empty signing key")
	}
	raw, _ := json.Marshal(c)
	mac := hmac.New(sha256.New, key)
	mac.Write(raw)
	tag := mac.Sum(nil)
	return base64.RawURLEncoding.EncodeToString(append(raw, tag...))
}

// Verify reverses Sign. Constant-time HMAC compare.
func Verify(s string, key []byte) (Cursor, error) {
	if s == "" || len(key) == 0 {
		return Cursor{}, ErrInvalidCursor
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || len(raw) < sha256.Size+1 {
		return Cursor{}, ErrInvalidCursor
	}
	body := raw[:len(raw)-sha256.Size]
	tag := raw[len(raw)-sha256.Size:]
	mac := hmac.New(sha256.New, key)
	mac.Write(body)
	want := mac.Sum(nil)
	if !hmac.Equal(want, tag) {
		return Cursor{}, ErrInvalidCursor
	}
	var c Cursor
	if err := json.Unmarshal(body, &c); err != nil {
		return Cursor{}, ErrInvalidCursor
	}
	return c, nil
}
