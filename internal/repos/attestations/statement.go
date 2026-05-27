// SPDX-License-Identifier: AGPL-3.0-or-later

// Package attestations validates the repository artifact attestation JSON
// shithub stores for download. The v1 format is an in-toto statement JSON
// object; runner-side automatic production is intentionally separate work.
package attestations

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

const MaxStatementBytes = 1 << 20

var subjectDigestRE = regexp.MustCompile(`^[A-Za-z0-9._+-]+:[A-Fa-f0-9]{16,}$`)

type NormalizedStatement struct {
	Statement     []byte
	SubjectName   string
	SubjectDigest string
	PredicateType string
	ByteCount     int64
}

type statementEnvelope struct {
	Type          string             `json:"_type"`
	Subject       []statementSubject `json:"subject"`
	PredicateType string             `json:"predicateType"`
	Predicate     json.RawMessage    `json:"predicate"`
}

type statementSubject struct {
	Name   string            `json:"name"`
	Digest map[string]string `json:"digest"`
}

func NormalizeStatement(body []byte) (NormalizedStatement, error) {
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return NormalizedStatement{}, errors.New("attestation statement is required")
	}
	if len(body) > MaxStatementBytes {
		return NormalizedStatement{}, fmt.Errorf("attestation statement exceeds %d bytes", MaxStatementBytes)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return NormalizedStatement{}, fmt.Errorf("attestation statement must be valid JSON: %w", err)
	}
	if len(raw) == 0 {
		return NormalizedStatement{}, errors.New("attestation statement must be a JSON object")
	}

	var stmt statementEnvelope
	if err := json.Unmarshal(body, &stmt); err != nil {
		return NormalizedStatement{}, fmt.Errorf("attestation statement shape is invalid: %w", err)
	}
	if strings.TrimSpace(stmt.Type) == "" {
		return NormalizedStatement{}, errors.New("attestation statement _type is required")
	}
	if !strings.Contains(stmt.Type, "in-toto.io/Statement") {
		return NormalizedStatement{}, errors.New("attestation statement _type must be an in-toto Statement")
	}
	if strings.TrimSpace(stmt.PredicateType) == "" {
		return NormalizedStatement{}, errors.New("attestation statement predicateType is required")
	}
	if len(stmt.Predicate) == 0 || bytes.Equal(bytes.TrimSpace(stmt.Predicate), []byte("null")) {
		return NormalizedStatement{}, errors.New("attestation statement predicate is required")
	}
	if len(stmt.Subject) == 0 {
		return NormalizedStatement{}, errors.New("attestation statement subject is required")
	}

	subject := stmt.Subject[0]
	name := strings.TrimSpace(subject.Name)
	if name == "" {
		return NormalizedStatement{}, errors.New("attestation subject name is required")
	}
	digest := normalizeSubjectDigest(subject.Digest)
	if digest == "" {
		return NormalizedStatement{}, errors.New("attestation subject digest is required")
	}
	if !subjectDigestRE.MatchString(digest) {
		return NormalizedStatement{}, errors.New("attestation subject digest must include an algorithm and hexadecimal digest")
	}

	normalized, err := json.Marshal(raw)
	if err != nil {
		return NormalizedStatement{}, fmt.Errorf("normalize attestation statement: %w", err)
	}
	return NormalizedStatement{
		Statement:     normalized,
		SubjectName:   name,
		SubjectDigest: digest,
		PredicateType: strings.TrimSpace(stmt.PredicateType),
		ByteCount:     int64(len(normalized)),
	}, nil
}

func normalizeSubjectDigest(digests map[string]string) string {
	if len(digests) == 0 {
		return ""
	}
	if v := strings.TrimSpace(digests["sha256"]); v != "" {
		return "sha256:" + strings.ToLower(v)
	}
	keys := make([]string, 0, len(digests))
	for k := range digests {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if v := strings.TrimSpace(digests[k]); v != "" {
			return strings.ToLower(strings.TrimSpace(k)) + ":" + strings.ToLower(v)
		}
	}
	return ""
}
