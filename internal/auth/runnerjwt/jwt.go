// SPDX-License-Identifier: AGPL-3.0-or-later

// Package runnerjwt signs and verifies the short-lived job tokens used by
// shithub Actions runners.
//
// Registration tokens authenticate a runner to the heartbeat endpoint. A
// successful claim receives one JWT scoped to one workflow_jobs row; job
// endpoints verify the signature, expiry, path/job match, and then consume
// the jti through runner_jwt_used so the token is single-use.
package runnerjwt

import (
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultTTL is the runner job-token lifetime from the S41c contract.
	DefaultTTL = 15 * time.Minute

	signingKeySize = 32
	hkdfInfo       = "actions-runner-jwt-v1"
	jtiBytes       = 32
)

var (
	ErrEmptyKey          = errors.New("runnerjwt: empty key")
	ErrInvalidKey        = errors.New("runnerjwt: key must be 32 bytes")
	ErrMalformed         = errors.New("runnerjwt: malformed token")
	ErrInvalidSignature  = errors.New("runnerjwt: invalid signature")
	ErrExpired           = errors.New("runnerjwt: expired token")
	ErrInvalidClaims     = errors.New("runnerjwt: invalid claims")
	ErrUnsupportedHeader = errors.New("runnerjwt: unsupported header")
)

// Claims are the JWT payload fields accepted by runner job endpoints.
type Claims struct {
	Sub    string `json:"sub"`
	JobID  int64  `json:"job_id"`
	RunID  int64  `json:"run_id"`
	RepoID int64  `json:"repo_id"`
	Exp    int64  `json:"exp"`
	JTI    string `json:"jti"`
}

// RunnerID extracts the runner id encoded in sub="runner:<id>".
func (c Claims) RunnerID() (int64, error) {
	const prefix = "runner:"
	if !strings.HasPrefix(c.Sub, prefix) {
		return 0, ErrInvalidClaims
	}
	id, err := strconv.ParseInt(strings.TrimPrefix(c.Sub, prefix), 10, 64)
	if err != nil || id <= 0 {
		return 0, ErrInvalidClaims
	}
	return id, nil
}

// MintParams describes a job token to issue.
type MintParams struct {
	RunnerID int64
	JobID    int64
	RunID    int64
	RepoID   int64
	TTL      time.Duration
}

// Signer signs and verifies HS256 runner JWTs.
type Signer struct {
	key []byte
	now func() time.Time
	rng io.Reader
}

// Option customizes a Signer. Tests use these for deterministic time/randomness.
type Option func(*Signer)

// WithClock overrides the clock used for exp validation and issuance.
func WithClock(now func() time.Time) Option {
	return func(s *Signer) {
		if now != nil {
			s.now = now
		}
	}
}

// WithRand overrides the random source used for jti generation.
func WithRand(r io.Reader) Option {
	return func(s *Signer) {
		if r != nil {
			s.rng = r
		}
	}
}

// NewFromTOTPKeyB64 decodes cfg.Auth.TOTPKeyB64 and derives an isolated
// runner-JWT signing key via HKDF. The raw TOTP/secretbox key is never used
// directly for JWT signatures.
func NewFromTOTPKeyB64(totpKeyB64 string, opts ...Option) (*Signer, error) {
	key, err := DeriveKeyFromTOTPKeyB64(totpKeyB64)
	if err != nil {
		return nil, err
	}
	return NewFromKey(key, opts...)
}

// DeriveKeyFromTOTPKeyB64 returns the HS256 key derived from the configured
// 32-byte TOTP/secretbox key.
func DeriveKeyFromTOTPKeyB64(totpKeyB64 string) ([]byte, error) {
	if totpKeyB64 == "" {
		return nil, ErrEmptyKey
	}
	raw, err := decodeKey(totpKeyB64)
	if err != nil {
		return nil, fmt.Errorf("runnerjwt: decode key: %w", err)
	}
	if len(raw) != signingKeySize {
		return nil, ErrInvalidKey
	}
	key, err := hkdf.Key(sha256.New, raw, nil, hkdfInfo, signingKeySize)
	if err != nil {
		return nil, fmt.Errorf("runnerjwt: derive key: %w", err)
	}
	return key, nil
}

// NewFromKey constructs a Signer from an already-derived 32-byte HS256 key.
func NewFromKey(key []byte, opts ...Option) (*Signer, error) {
	if len(key) != signingKeySize {
		return nil, ErrInvalidKey
	}
	copied := make([]byte, len(key))
	copy(copied, key)
	s := &Signer{
		key: copied,
		now: time.Now,
		rng: rand.Reader,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s, nil
}

// Mint signs a new job token and returns the token plus the exact claims.
func (s *Signer) Mint(p MintParams) (string, Claims, error) {
	ttl := p.TTL
	if ttl == 0 {
		ttl = DefaultTTL
	}
	if p.RunnerID <= 0 || p.JobID <= 0 || p.RunID <= 0 || p.RepoID <= 0 || ttl <= 0 {
		return "", Claims{}, ErrInvalidClaims
	}
	jti, err := newJTI(s.rng)
	if err != nil {
		return "", Claims{}, err
	}
	claims := Claims{
		Sub:    fmt.Sprintf("runner:%d", p.RunnerID),
		JobID:  p.JobID,
		RunID:  p.RunID,
		RepoID: p.RepoID,
		Exp:    s.now().Add(ttl).Unix(),
		JTI:    jti,
	}
	if err := validateClaims(claims); err != nil {
		return "", Claims{}, err
	}
	token, err := s.sign(claims)
	if err != nil {
		return "", Claims{}, err
	}
	return token, claims, nil
}

// Verify checks token shape, HS256 signature, registered claims, and expiry.
// It does not consume jti; callers perform that DB operation after verifying
// path/job ownership.
func (s *Signer) Verify(token string) (Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return Claims{}, ErrMalformed
	}
	headerBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return Claims{}, ErrMalformed
	}
	var header struct {
		Alg string `json:"alg"`
		Typ string `json:"typ"`
	}
	if err := json.Unmarshal(headerBytes, &header); err != nil {
		return Claims{}, ErrMalformed
	}
	if header.Alg != "HS256" || header.Typ != "JWT" {
		return Claims{}, ErrUnsupportedHeader
	}

	signingInput := parts[0] + "." + parts[1]
	gotSig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return Claims{}, ErrMalformed
	}
	wantSig := signHS256(s.key, signingInput)
	if !hmac.Equal(gotSig, wantSig) {
		return Claims{}, ErrInvalidSignature
	}

	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return Claims{}, ErrMalformed
	}
	var claims Claims
	if err := json.Unmarshal(payloadBytes, &claims); err != nil {
		return Claims{}, ErrMalformed
	}
	if err := validateClaims(claims); err != nil {
		return Claims{}, err
	}
	if !s.now().Before(time.Unix(claims.Exp, 0)) {
		return Claims{}, ErrExpired
	}
	return claims, nil
}

func (s *Signer) sign(claims Claims) (string, error) {
	headerJSON, err := json.Marshal(struct {
		Alg string `json:"alg"`
		Typ string `json:"typ"`
	}{Alg: "HS256", Typ: "JWT"})
	if err != nil {
		return "", err
	}
	payloadJSON, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	header := base64.RawURLEncoding.EncodeToString(headerJSON)
	payload := base64.RawURLEncoding.EncodeToString(payloadJSON)
	signingInput := header + "." + payload
	sig := base64.RawURLEncoding.EncodeToString(signHS256(s.key, signingInput))
	return signingInput + "." + sig, nil
}

func signHS256(key []byte, signingInput string) []byte {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(signingInput))
	return mac.Sum(nil)
}

func newJTI(r io.Reader) (string, error) {
	buf := make([]byte, jtiBytes)
	if _, err := io.ReadFull(r, buf); err != nil {
		return "", fmt.Errorf("runnerjwt: jti: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func validateClaims(c Claims) error {
	if _, err := c.RunnerID(); err != nil {
		return err
	}
	if c.JobID <= 0 || c.RunID <= 0 || c.RepoID <= 0 || c.Exp <= 0 {
		return ErrInvalidClaims
	}
	if len(c.JTI) < 16 || len(c.JTI) > 128 {
		return ErrInvalidClaims
	}
	return nil
}

func decodeKey(s string) ([]byte, error) {
	encodings := []*base64.Encoding{
		base64.StdEncoding,
		base64.RawStdEncoding,
		base64.URLEncoding,
		base64.RawURLEncoding,
	}
	var lastErr error
	for _, enc := range encodings {
		raw, err := enc.DecodeString(s)
		if err == nil {
			return raw, nil
		}
		lastErr = err
	}
	return nil, lastErr
}
