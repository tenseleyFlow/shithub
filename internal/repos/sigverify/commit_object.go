// SPDX-License-Identifier: AGPL-3.0-or-later

package sigverify

import (
	"bytes"
	"strings"
)

// splitSignedObject takes the raw bytes of a git commit or annotated
// tag object (as returned by `git cat-file -p <oid>`) and splits it
// into the signature-payload (the bytes the signature was computed
// over) and the armored signature block.
//
// Git stores the signature as a header line whose value is a multi-
// line PGP armor block; the lines after the first are continuation
// lines starting with a single space. For example:
//
//	tree abc123...
//	parent def456...
//	author Alice <alice@example.com> 1700000000 +0000
//	committer Alice <alice@example.com> 1700000000 +0000
//	gpgsig -----BEGIN PGP SIGNATURE-----
//	 <body>
//	 -----END PGP SIGNATURE-----
//
//	commit message body
//
// The canonical payload is the same object with the gpgsig header
// (and its continuation lines) removed entirely. The signature is
// the concatenation of the gpgsig-value first line + the un-indented
// continuation lines.
//
// Returns (payload, armoredSig, true) when a gpgsig header was found,
// or (rawBody, "", false) when the object carries no signature.
//
// Tag objects can store the signature either inline at the end of the
// body (the classic format, with `-----BEGIN PGP SIGNATURE-----`
// appearing in the message body) OR via a header (the newer SSH-sig
// convention). For OpenPGP tags we look at both forms — the legacy
// trailing-block form is the dominant one in the wild.
func splitSignedObject(body []byte) (payload []byte, armoredSig string, signed bool) {
	// Find the end of the header block (the first blank line).
	headerEnd := bytes.Index(body, []byte("\n\n"))
	if headerEnd < 0 {
		return body, "", false
	}
	header := body[:headerEnd]
	rest := body[headerEnd:]

	// Walk the header lines looking for a gpgsig header. There's
	// exactly zero or one per object in practice.
	lines := bytes.Split(header, []byte("\n"))
	var (
		sigBuilder strings.Builder
		newHeader  bytes.Buffer
		inSig      bool
		foundSig   bool
	)
	for i, line := range lines {
		if inSig {
			if bytes.HasPrefix(line, []byte(" ")) {
				// Continuation line — strip one leading space.
				sigBuilder.Write(line[1:])
				sigBuilder.WriteByte('\n')
				continue
			}
			// End of signature continuation — fall through to
			// header handling for this line.
			inSig = false
		}
		if bytes.HasPrefix(line, []byte("gpgsig ")) {
			foundSig = true
			inSig = true
			sigBuilder.Write(line[len("gpgsig "):])
			sigBuilder.WriteByte('\n')
			continue
		}
		// Tag-object signature header is named differently in some
		// gitformats but the modern convention also uses 'gpgsig'.
		// Treat any other line as a regular header to preserve.
		if i > 0 {
			newHeader.WriteByte('\n')
		}
		newHeader.Write(line)
	}

	if !foundSig {
		// Check tag-object inline trailing-signature form: the body
		// (rest, after the blank line) may end with a PGP signature
		// block. This form has no `gpgsig` header.
		return splitTagInlineSignature(body)
	}

	payload = append(newHeader.Bytes(), rest...)
	return payload, sigBuilder.String(), true
}

// splitTagInlineSignature handles annotated tags that embed the
// signature at the end of the message body (the legacy git-tag
// signing convention) rather than as a header. The signature block
// runs from `-----BEGIN PGP SIGNATURE-----` to `-----END PGP
// SIGNATURE-----` inclusive at the tail of the body.
//
// Returns (payload, armoredSig, true) when an inline block is
// detected, or (body, "", false) otherwise.
func splitTagInlineSignature(body []byte) (payload []byte, armoredSig string, signed bool) {
	const begin = "-----BEGIN PGP SIGNATURE-----"
	const end = "-----END PGP SIGNATURE-----"

	beginIdx := bytes.Index(body, []byte(begin))
	if beginIdx < 0 {
		return body, "", false
	}
	endIdx := bytes.Index(body[beginIdx:], []byte(end))
	if endIdx < 0 {
		return body, "", false
	}
	endIdx += beginIdx + len(end)
	// Include trailing newline if present.
	if endIdx < len(body) && body[endIdx] == '\n' {
		endIdx++
	}
	armoredSig = string(body[beginIdx:endIdx])
	// Payload is everything before the signature block. git tag -s
	// appends the signature directly to the tag body — the signed
	// payload IS the body up to (and including the trailing newline
	// before) the BEGIN marker. No further trimming.
	payload = body[:beginIdx]
	return payload, armoredSig, true
}
