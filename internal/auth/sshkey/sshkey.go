// SPDX-License-Identifier: AGPL-3.0-or-later

// Package sshkey wraps the parsing, validation, and fingerprinting of
// user-supplied SSH public keys. Every entry that takes key material
// from outside (the settings handlers, future imports, the AKC binary's
// equality checks) goes through this package so policy lives in exactly
// one place.
package sshkey

import (
	"crypto/dsa" //nolint:staticcheck // DSA detection only — never accepted.
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/ssh"
)

// MinRSABits is the smallest accepted modulus length for ssh-rsa keys.
const MinRSABits = 2048

// MaxKeysPerUser is the cap enforced at the handler layer. Keeps DB rows
// per-user bounded and limits AKC lookup amplification if an attacker
// registered an enormous keyring on a single account.
const MaxKeysPerUser = 100

// Parsed is the validated, ready-to-store representation of a user-supplied
// SSH public key. Title is whatever name the user typed; everything else
// is derived from the key blob.
type Parsed struct {
	Title       string
	Type        string // OpenSSH algorithm name, e.g. "ssh-ed25519"
	Bits        int    // 0 when not meaningful (e.g. ed25519)
	Fingerprint string // canonical SHA256:<base64-no-padding>
	// PublicKey is the canonical authorized_keys line (no comment; no
	// trailing newline). It is what the AKC binary re-emits on a hit.
	PublicKey string
}

// Errors. Keep them precise enough for the settings handler to render a
// useful UI string.
var (
	ErrUnsupportedAlgo = errors.New("sshkey: unsupported key algorithm")
	ErrRSATooShort     = fmt.Errorf("sshkey: RSA keys must be at least %d bits", MinRSABits)
	ErrUnparseable     = errors.New("sshkey: could not parse key — paste the contents of your .pub file")
	ErrTitleEmpty      = errors.New("sshkey: title required")
	ErrTitleTooLong    = errors.New("sshkey: title may be at most 80 characters")
	ErrTitleControl    = errors.New("sshkey: title contains control characters")
)

// Parse validates and canonicalizes input. Title goes through a small
// validator (length + control-char rejection); the key blob through
// ssh.ParseAuthorizedKey + an algorithm whitelist + an RSA-bits check.
func Parse(title, blob string) (*Parsed, error) {
	t := strings.TrimSpace(title)
	if t == "" {
		return nil, ErrTitleEmpty
	}
	if len(t) > 80 {
		return nil, ErrTitleTooLong
	}
	if hasControlChars(t) {
		return nil, ErrTitleControl
	}

	pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(blob))
	if err != nil {
		return nil, ErrUnparseable
	}

	algo := pub.Type()
	if !algorithmAllowed(algo) {
		return nil, ErrUnsupportedAlgo
	}

	bits, err := keyBits(pub)
	if err != nil {
		return nil, err
	}
	if algo == "ssh-rsa" && bits < MinRSABits {
		return nil, ErrRSATooShort
	}

	canonical := canonicalAuthorizedKey(pub)
	return &Parsed{
		Title:       t,
		Type:        algo,
		Bits:        bits,
		Fingerprint: ssh.FingerprintSHA256(pub),
		PublicKey:   canonical,
	}, nil
}

// FingerprintOf returns the canonical SHA256 fingerprint for an
// already-parsed key. Used by the AKC binary's equality checks.
func FingerprintOf(pub ssh.PublicKey) string {
	return ssh.FingerprintSHA256(pub)
}

// canonicalAuthorizedKey re-emits the parsed key as a single
// `<algo> <base64>` line so storage/lookup don't accidentally depend on
// whitespace or comment fields the user supplied.
func canonicalAuthorizedKey(pub ssh.PublicKey) string {
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub)))
	return line
}

// algorithmAllowed gates the OpenSSH algorithm names we accept.
func algorithmAllowed(algo string) bool {
	switch algo {
	case "ssh-ed25519",
		"sk-ssh-ed25519@openssh.com",
		"ecdsa-sha2-nistp256",
		"ecdsa-sha2-nistp384",
		"ecdsa-sha2-nistp521",
		"sk-ecdsa-sha2-nistp256@openssh.com",
		"ssh-rsa":
		return true
	}
	return false
}

// keyBits extracts the modulus / curve size from a parsed key. Returns 0
// for ed25519 (where the concept doesn't apply meaningfully).
func keyBits(pub ssh.PublicKey) (int, error) {
	cp, ok := pub.(ssh.CryptoPublicKey)
	if !ok {
		return 0, nil
	}
	switch k := cp.CryptoPublicKey().(type) {
	case ed25519.PublicKey:
		return 0, nil
	case *rsa.PublicKey:
		return k.N.BitLen(), nil
	case *ecdsa.PublicKey:
		return k.Params().BitSize, nil
	case *dsa.PublicKey:
		return 0, ErrUnsupportedAlgo
	default:
		return 0, nil
	}
}

func hasControlChars(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}
