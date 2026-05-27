// SPDX-License-Identifier: AGPL-3.0-or-later

package attestations

import (
	"strings"
	"testing"
)

func TestNormalizeStatement(t *testing.T) {
	t.Parallel()
	stmt, err := NormalizeStatement([]byte(sampleStatement()))
	if err != nil {
		t.Fatalf("NormalizeStatement: %v", err)
	}
	if stmt.SubjectName != "dist/app.tar.gz" {
		t.Fatalf("subject name=%q", stmt.SubjectName)
	}
	if stmt.SubjectDigest != "sha256:abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890" {
		t.Fatalf("subject digest=%q", stmt.SubjectDigest)
	}
	if stmt.PredicateType != "https://slsa.dev/provenance/v1" {
		t.Fatalf("predicateType=%q", stmt.PredicateType)
	}
	if !strings.Contains(string(stmt.Statement), `"predicateType":"https://slsa.dev/provenance/v1"`) {
		t.Fatalf("statement not compact JSON: %s", stmt.Statement)
	}
}

func TestNormalizeStatementRejectsInvalidSubject(t *testing.T) {
	t.Parallel()
	_, err := NormalizeStatement([]byte(`{
	  "_type": "https://in-toto.io/Statement/v1",
	  "subject": [{"name": "dist/app.tar.gz", "digest": {"sha256": "not-a-digest"}}],
	  "predicateType": "https://slsa.dev/provenance/v1",
	  "predicate": {}
	}`))
	if err == nil {
		t.Fatal("expected invalid digest error")
	}
}

func sampleStatement() string {
	return `{
	  "_type": "https://in-toto.io/Statement/v1",
	  "subject": [{
	    "name": "dist/app.tar.gz",
	    "digest": {"sha256": "ABCDEF1234567890ABCDEF1234567890ABCDEF1234567890ABCDEF1234567890"}
	  }],
	  "predicateType": "https://slsa.dev/provenance/v1",
	  "predicate": {"buildType": "https://shithub.sh/actions"}
	}`
}
