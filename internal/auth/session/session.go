// SPDX-License-Identifier: AGPL-3.0-or-later

// Package session owns the cookie-based session pipeline. S02 ships the
// AEAD-encrypted cookie store; the Store interface lets later sprints add
// a server-side store for revocation without touching handlers.
package session

import (
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"golang.org/x/crypto/chacha20poly1305"
)

// CookieName is the canonical session cookie name. Prefixed with an
// underscore so it doesn't collide with any future user-controllable name.
const CookieName = "_shithub_session"

// DefaultMaxAge is the default cookie lifetime.
const DefaultMaxAge = 30 * 24 * time.Hour

// Session is the data carried in a cookie. The shape is intentionally
// small; anything that doesn't fit a few hundred bytes belongs server-side.
type Session struct {
	UserID       int64 `json:"uid,omitempty"`
	Pre2FAUserID int64 `json:"p2,omitempty"` // set after password OK, before TOTP step
	Recent2FAAt  int64 `json:"r2,omitempty"` // unix-seconds of last successful 2FA challenge
	// Epoch is the users.session_epoch value at issue time. The session
	// loader compares it against the current DB value on every request;
	// "log out everywhere" bumps the column, invalidating every cookie
	// that still carries the old epoch. Zero is the unbumped baseline
	// and matches the migration's column default.
	Epoch     int32             `json:"e,omitempty"`
	CSRFToken string            `json:"csrf,omitempty"`
	Theme     string            `json:"theme,omitempty"`
	Flashes   []string          `json:"flashes,omitempty"`
	Extras    map[string]string `json:"extras,omitempty"`
	IssuedAt  int64             `json:"iat,omitempty"`
}

// IsAnonymous returns true when no user is bound to the session.
func (s *Session) IsAnonymous() bool { return s == nil || s.UserID == 0 }

// AddFlash appends a flash message; it'll be rendered + cleared on the next
// page load.
func (s *Session) AddFlash(msg string) {
	if s == nil {
		return
	}
	s.Flashes = append(s.Flashes, msg)
}

// PopFlashes returns and clears any pending flash messages.
func (s *Session) PopFlashes() []string {
	if s == nil {
		return nil
	}
	out := s.Flashes
	s.Flashes = nil
	return out
}

// Store is the abstraction every handler uses. The cookie implementation
// stores everything client-side; future implementations may persist
// server-side rows for revocation.
type Store interface {
	Load(r *http.Request) (*Session, error)
	Save(w http.ResponseWriter, r *http.Request, s *Session) error
	Clear(w http.ResponseWriter)
}

// CookieStore is the AEAD-encrypted-cookie implementation.
type CookieStore struct {
	aead     cipher.AEAD
	maxAge   time.Duration
	secure   bool   // set Secure attribute on the cookie
	domain   string // optional cookie domain
	path     string // cookie path; defaults to "/"
	sameSite http.SameSite
	clock    func() time.Time
}

// CookieStoreConfig holds the ergonomic options. Key MUST be 32 bytes.
type CookieStoreConfig struct {
	Key      []byte
	MaxAge   time.Duration
	Secure   bool
	Domain   string
	Path     string
	SameSite http.SameSite
}

// NewCookieStore constructs a store. Returns an error if the key is wrong
// size — this is a configuration error and must surface at startup.
func NewCookieStore(cfg CookieStoreConfig) (*CookieStore, error) {
	if len(cfg.Key) != chacha20poly1305.KeySize {
		return nil, fmt.Errorf("session: key must be %d bytes, got %d", chacha20poly1305.KeySize, len(cfg.Key))
	}
	aead, err := chacha20poly1305.New(cfg.Key)
	if err != nil {
		return nil, fmt.Errorf("session: aead init: %w", err)
	}
	if cfg.MaxAge <= 0 {
		cfg.MaxAge = DefaultMaxAge
	}
	if cfg.Path == "" {
		cfg.Path = "/"
	}
	if cfg.SameSite == 0 {
		cfg.SameSite = http.SameSiteLaxMode
	}
	return &CookieStore{
		aead:     aead,
		maxAge:   cfg.MaxAge,
		secure:   cfg.Secure,
		domain:   cfg.Domain,
		path:     cfg.Path,
		sameSite: cfg.SameSite,
		clock:    time.Now,
	}, nil
}

// Load returns the session bound to the cookie, or a fresh empty session
// when none is present or the cookie is invalid (expired / wrong key /
// tampered). Errors are surfaced for malformed input but a missing or
// invalid cookie yields an empty session, not an error — the caller can
// proceed with anonymous handling.
func (c *CookieStore) Load(r *http.Request) (*Session, error) {
	cookie, err := r.Cookie(CookieName)
	if err != nil {
		return &Session{}, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(cookie.Value)
	if err != nil || len(raw) < c.aead.NonceSize() {
		return &Session{}, nil
	}
	nonce, ct := raw[:c.aead.NonceSize()], raw[c.aead.NonceSize():]
	plain, err := c.aead.Open(nil, nonce, ct, nil)
	if err != nil {
		return &Session{}, nil
	}
	var s Session
	if err := json.Unmarshal(plain, &s); err != nil {
		return &Session{}, nil
	}
	if s.IssuedAt > 0 && c.clock().Unix()-s.IssuedAt > int64(c.maxAge.Seconds()) {
		return &Session{}, nil
	}
	return &s, nil
}

// Save serializes, encrypts, and sets the session cookie.
func (c *CookieStore) Save(w http.ResponseWriter, _ *http.Request, s *Session) error {
	if s == nil {
		s = &Session{}
	}
	s.IssuedAt = c.clock().Unix()
	plain, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("session: marshal: %w", err)
	}
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return fmt.Errorf("session: nonce: %w", err)
	}
	sealed := c.aead.Seal(nonce, nonce, plain, nil)
	encoded := base64.RawURLEncoding.EncodeToString(sealed)
	http.SetCookie(w, c.cookie(encoded, int(c.maxAge.Seconds())))
	return nil
}

// Clear deletes the session cookie.
func (c *CookieStore) Clear(w http.ResponseWriter) {
	http.SetCookie(w, c.cookie("", -1))
}

// cookie builds a *http.Cookie with the store's policy. Concentrating the
// construction here gives us a single point where Secure-flag policy is
// enforced. Secure is operator-controlled per cfg.Secure (S37 enables it
// under TLS).
//
//nolint:gosec // G124: Secure attribute is intentionally configurable via cfg.Secure.
func (c *CookieStore) cookie(value string, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name:     CookieName,
		Value:    value,
		Path:     c.path,
		Domain:   c.domain,
		MaxAge:   maxAge,
		Secure:   c.secure,
		HttpOnly: true,
		SameSite: c.sameSite,
	}
}

// GenerateKey returns a new random 32-byte key suitable for NewCookieStore.
// Used by the operator's first-run setup; production keys come from config.
func GenerateKey() ([]byte, error) {
	key := make([]byte, chacha20poly1305.KeySize)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("session: generate key: %w", err)
	}
	return key, nil
}

// Sentinel errors callers may check for.
var (
	ErrInvalidKey = errors.New("session: invalid key size")
)
