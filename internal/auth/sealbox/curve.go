// SPDX-License-Identifier: AGPL-3.0-or-later

package sealbox

import (
	"golang.org/x/crypto/curve25519"
)

// derivePublic computes the X25519 public key from a 32-byte private
// key. NaCl's box package doesn't expose this directly when loading a
// pre-existing private key (only `GenerateKey` returns both halves);
// we use curve25519.X25519 against the basepoint to derive.
func derivePublic(pub, priv *[32]byte) error {
	out, err := curve25519.X25519(priv[:], curve25519.Basepoint)
	if err != nil {
		return err
	}
	copy(pub[:], out)
	return nil
}
