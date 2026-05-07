// SPDX-License-Identifier: AGPL-3.0-or-later

// Package protocol carries the smart-HTTP and (later) smart-SSH bits of
// the git wire protocol that we generate ourselves. The bulk of the
// protocol — capability negotiation, want/have, multi-ack, side-band —
// stays inside canonical `git`'s `upload-pack` and `receive-pack`. We
// only emit the framing we need to wrap their advertise-refs output for
// HTTP transport.
package protocol

import (
	"fmt"
	"io"
)

// MaxPktLine is git's hard limit for a single packet: 65520 bytes of
// payload + 4 bytes of length prefix. Anything longer is malformed.
const MaxPktLine = 65520

// FlushPkt is the literal pkt-line sequence that says "no more
// packets." git uses it to terminate sub-streams.
const FlushPkt = "0000"

// WritePkt writes one pkt-line packet — a 4-hex-digit length prefix
// (covering payload + 4) followed by the payload. Exposed so the smart-
// HTTP info/refs handler can prepend its `# service=...` advertisement.
func WritePkt(w io.Writer, payload string) error {
	if len(payload)+4 > MaxPktLine {
		return fmt.Errorf("pkt-line: payload too long (%d > %d)", len(payload), MaxPktLine-4)
	}
	if _, err := fmt.Fprintf(w, "%04x%s", len(payload)+4, payload); err != nil {
		return err
	}
	return nil
}

// WriteFlush emits the flush packet (`0000`). Marks the end of a
// sub-stream like the service advertisement preamble.
func WriteFlush(w io.Writer) error {
	_, err := io.WriteString(w, FlushPkt)
	return err
}

// WriteServiceAdvertisement writes the standard preamble that the smart-
// HTTP info/refs response uses to tell git which service is being
// advertised. The wire format is:
//
//	001e# service=git-upload-pack\n0000
//	└┬┘└──────────┬─────────────┘└─┬─┘
//	 │            │                └ flush
//	 │            └ payload (trailing newline included)
//	 └ length prefix (hex)
//
// Then `git upload-pack --advertise-refs --stateless-rpc <repo>` writes
// its actual ref advertisement after this.
func WriteServiceAdvertisement(w io.Writer, service string) error {
	if err := WritePkt(w, "# service="+service+"\n"); err != nil {
		return err
	}
	return WriteFlush(w)
}
