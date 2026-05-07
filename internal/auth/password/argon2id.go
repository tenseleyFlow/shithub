// SPDX-License-Identifier: AGPL-3.0-or-later

// Package password implements argon2id password hashing with PHC-string
// encoding. argon2id is the OWASP-recommended scheme: memory-hard,
// parallelism-tunable, and the PHC-format winner.
//
// Output format (matches the canonical PHC string):
//
//	$argon2id$v=19$m=65536,t=3,p=2$<saltB64>$<hashB64>
//
// The b64 encoding is RFC4648 standard b64 *without* padding, per the PHC
// spec. Hashes produced here are forward-compatible: parameters travel
// inside the string so callers can rotate Defaults() without losing the
// ability to verify older hashes.
package password

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Algo is the password_algo column value for hashes produced by this
// package. Lets the DB carry an explicit identifier so we can roll
// forward to a different algorithm later without ambiguity.
const Algo = "argon2id-v1"

// Params controls argon2id cost. Defaults() values are tuned to take
// roughly 100–300ms on dev hardware (Apple M-series) and a similar
// envelope on the production droplet (modern x86, 4 vCPU). Operators can
// override via auth.argon2.* config.
type Params struct {
	Memory  uint32 // KiB
	Time    uint32 // iterations
	Threads uint8  // parallelism
	SaltLen uint32 // bytes
	KeyLen  uint32 // bytes
}

// Defaults returns the OWASP-recommended baseline. ~64 MiB memory,
// 3 iterations, 2 lanes — empirically ~100–300 ms on dev hardware.
func Defaults() Params {
	return Params{
		Memory:  64 * 1024,
		Time:    3,
		Threads: 2,
		SaltLen: 16,
		KeyLen:  32,
	}
}

// MinPasswordLength is the absolute floor enforced at the lowest layer.
// Higher layers (signup form) MAY enforce additional rules (common-password
// list, etc.) but MUST NOT relax this minimum.
const MinPasswordLength = 10

// Hash produces a PHC-encoded argon2id string for password using p.
// Returns an error when password is shorter than MinPasswordLength so
// the policy floor cannot be bypassed by a misbehaving caller.
func Hash(password string, p Params) (string, error) {
	if len(password) < MinPasswordLength {
		return "", fmt.Errorf("password: minimum %d characters", MinPasswordLength)
	}
	salt := make([]byte, p.SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("password: salt: %w", err)
	}
	key := argon2.IDKey([]byte(password), salt, p.Time, p.Memory, p.Threads, p.KeyLen)
	return encode(p, salt, key), nil
}

// Verify reports whether password matches the PHC-encoded hash.
// Returns false (without error) for a mismatching password and an error
// for a malformed hash.
func Verify(password, encoded string) (bool, error) {
	p, salt, want, err := decode(encoded)
	if err != nil {
		return false, err
	}
	got := argon2.IDKey([]byte(password), salt, p.Time, p.Memory, p.Threads, p.KeyLen)
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// dummyEncoded is a fixed, valid hash used by login handlers to avoid
// timing leaks: when a username doesn't exist, callers still call Verify
// against this so the request takes the same time as a real failed login.
// The "password" used to derive it is intentionally unguessable.
var dummyEncoded string

// MustGenerateDummy generates the dummy hash on first call. Safe to call
// during init in test code; production wires this from server start so
// the hash matches the configured Params.
func MustGenerateDummy(p Params) {
	enc, err := Hash("dummy-not-a-real-secret-not-a-real-secret", p)
	if err != nil {
		panic(fmt.Errorf("password: dummy: %w", err))
	}
	dummyEncoded = enc
}

// VerifyAgainstDummy runs Verify against the pre-computed dummy hash.
// Used by login handlers when the user lookup fails so the response time
// matches a real Verify call.
func VerifyAgainstDummy(password string) {
	if dummyEncoded == "" {
		MustGenerateDummy(Defaults())
	}
	_, _ = Verify(password, dummyEncoded)
}

func encode(p Params, salt, key []byte) string {
	b := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, p.Memory, p.Time, p.Threads,
		b.EncodeToString(salt), b.EncodeToString(key))
}

// decode parses a PHC argon2id string. Rejects unknown algorithms and
// versions so a future swap is forced through this package.
func decode(s string) (Params, []byte, []byte, error) {
	parts := strings.Split(s, "$")
	if len(parts) != 6 || parts[0] != "" {
		return Params{}, nil, nil, errors.New("password: malformed PHC string")
	}
	if parts[1] != "argon2id" {
		return Params{}, nil, nil, fmt.Errorf("password: unsupported algo %q", parts[1])
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return Params{}, nil, nil, fmt.Errorf("password: version: %w", err)
	}
	if version != argon2.Version {
		return Params{}, nil, nil, fmt.Errorf("password: argon2 version mismatch: got %d, want %d", version, argon2.Version)
	}
	var p Params
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.Memory, &p.Time, &p.Threads); err != nil {
		return Params{}, nil, nil, fmt.Errorf("password: params: %w", err)
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return Params{}, nil, nil, fmt.Errorf("password: salt: %w", err)
	}
	key, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return Params{}, nil, nil, fmt.Errorf("password: key: %w", err)
	}
	p.SaltLen = uint32(len(salt))
	p.KeyLen = uint32(len(key))
	return p, salt, key, nil
}
