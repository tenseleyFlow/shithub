// SPDX-License-Identifier: AGPL-3.0-or-later

package jobs_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/infra/storage"
	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
	"github.com/tenseleyFlow/shithub/internal/worker/jobs"
)

// TestGPGBackfill_HappyPath exercises the worker handler end-to-end:
// seed a bare git repo with a signed commit, seed the matching key
// rows in the DB, invoke the handler, and confirm
// commit_verification_cache contains a `valid` row afterwards.
//
// This is an integration test (requires SHITHUB_TEST_DATABASE_URL).
// It exercises the full sqlc + upsert path, not a mock.
func TestGPGBackfill_HappyPath(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	ctx := context.Background()

	// 1. Seed user + user_emails.
	var userID int64
	err := pool.QueryRow(ctx,
		`INSERT INTO users (username, password_hash, email_verified)
		 VALUES ($1, 'x', true) RETURNING id`,
		"alice",
	).Scan(&userID)
	if err != nil {
		t.Fatalf("seed users: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO user_emails (user_id, email, verified) VALUES ($1, $2, true)`,
		userID, "alice@shithub.test",
	); err != nil {
		t.Fatalf("seed user_emails: %v", err)
	}

	// 2. Build a bare repo at the RepoFS-expected path.
	rfsRoot := t.TempDir()
	rfs, err := storage.NewRepoFS(rfsRoot)
	if err != nil {
		t.Fatalf("NewRepoFS: %v", err)
	}
	gitDir, err := rfs.RepoPath("alice", "demo")
	if err != nil {
		t.Fatalf("RepoPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(gitDir), 0o755); err != nil {
		t.Fatalf("mkdir owner: %v", err)
	}
	if err := exec.Command("git", "init", "--quiet", "--bare", gitDir).Run(); err != nil {
		t.Fatalf("git init: %v", err)
	}
	entity := newEd25519(t, "alice@shithub.test")
	commitOID := writeSignedCommit(t, gitDir, entity, "signed commit")

	// 3. Seed a repos row pointing at the bare repo on disk.
	var repoID int64
	err = pool.QueryRow(ctx,
		`INSERT INTO repos (owner_user_id, name, visibility, default_branch)
		 VALUES ($1, $2, 'public', $3) RETURNING id`,
		userID, "demo", "trunk",
	).Scan(&repoID)
	if err != nil {
		t.Fatalf("seed repos: %v", err)
	}

	// 4. Seed user_gpg_keys + user_gpg_subkeys rows so the orchestrator
	//    can resolve the signature.
	primaryFP := hex.EncodeToString(entity.PrimaryKey.Fingerprint)
	primaryKID := fmt.Sprintf("%016x", entity.PrimaryKey.KeyId)
	var gpgKeyID int64
	err = pool.QueryRow(ctx,
		`INSERT INTO user_gpg_keys (
		    user_id, fingerprint, key_id, armored,
		    can_sign, can_encrypt_comms, can_encrypt_storage, can_certify, can_authenticate,
		    primary_algo
		 ) VALUES ($1, $2, $3, $4, true, false, false, true, false, 'ed25519')
		 RETURNING id`,
		userID, primaryFP, primaryKID, armoredPublic(t, entity),
	).Scan(&gpgKeyID)
	if err != nil {
		t.Fatalf("seed user_gpg_keys: %v", err)
	}

	// Register one subkey record per signing subkey; if no subkey
	// is signing (e.g. all subkeys are encryption-only and the
	// primary signs directly), register the primary itself.
	registered := 0
	for i := range entity.Subkeys {
		sk := &entity.Subkeys[i]
		if sk.Sig == nil || !sk.Sig.FlagSign {
			continue
		}
		if _, err := pool.Exec(ctx,
			`INSERT INTO user_gpg_subkeys (
			    gpg_key_id, fingerprint, key_id,
			    can_sign, can_encrypt_comms, can_encrypt_storage, can_certify
			 ) VALUES ($1, $2, $3, true, false, false, false)`,
			gpgKeyID,
			hex.EncodeToString(sk.PublicKey.Fingerprint),
			fmt.Sprintf("%016x", sk.PublicKey.KeyId),
		); err != nil {
			t.Fatalf("seed subkey: %v", err)
		}
		registered++
	}
	if registered == 0 {
		if _, err := pool.Exec(ctx,
			`INSERT INTO user_gpg_subkeys (
			    gpg_key_id, fingerprint, key_id,
			    can_sign, can_encrypt_comms, can_encrypt_storage, can_certify
			 ) VALUES ($1, $2, $3, true, false, false, true)`,
			gpgKeyID, primaryFP, primaryKID,
		); err != nil {
			t.Fatalf("seed primary as subkey: %v", err)
		}
	}

	// 5. Invoke the handler.
	handler := jobs.GPGBackfill(jobs.GPGBackfillDeps{
		Pool:   pool,
		RepoFS: rfs,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	payload, _ := json.Marshal(jobs.GPGBackfillPayload{RepoID: repoID})
	if err := handler(ctx, payload); err != nil {
		t.Fatalf("handler: %v", err)
	}

	// 6. Confirm cache row exists with reason='valid'.
	var reason string
	var verified bool
	err = pool.QueryRow(ctx,
		`SELECT reason, verified FROM commit_verification_cache
		 WHERE repo_id = $1 AND commit_oid = $2`,
		repoID, commitOID,
	).Scan(&reason, &verified)
	if err != nil {
		t.Fatalf("cache row missing: %v", err)
	}
	if reason != "valid" || !verified {
		t.Errorf("cache row: reason=%q verified=%t; want reason=valid verified=true", reason, verified)
	}
}

// TestGPGBackfill_BadPayload returns ErrPoison on malformed input so
// the worker pool doesn't retry forever.
func TestGPGBackfill_BadPayload(t *testing.T) {
	handler := jobs.GPGBackfill(jobs.GPGBackfillDeps{
		Pool:   nil, // not consulted on the poison path
		RepoFS: nil,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	err := handler(context.Background(), json.RawMessage(`{bogus`))
	if err == nil {
		t.Fatal("expected poison error on bad payload; got nil")
	}
	if !isPoisonError(err) {
		t.Errorf("expected wrapped ErrPoison; got %v", err)
	}
}

// TestGPGBackfill_MissingRepoID poisons rather than retries because
// the empty repo_id can never resolve.
func TestGPGBackfill_MissingRepoID(t *testing.T) {
	handler := jobs.GPGBackfill(jobs.GPGBackfillDeps{
		Pool:   nil,
		RepoFS: nil,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	err := handler(context.Background(), json.RawMessage(`{"repo_id": 0}`))
	if err == nil {
		t.Fatal("expected poison error on missing repo_id; got nil")
	}
	if !isPoisonError(err) {
		t.Errorf("expected wrapped ErrPoison; got %v", err)
	}
}

// ─── helpers ────────────────────────────────────────────────────────

// isPoisonError unwraps and matches against worker.ErrPoison without
// importing the worker package here — the test only cares that the
// error chain reaches a poison sentinel.
func isPoisonError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "poison")
}

func newEd25519(t *testing.T, email string) *openpgp.Entity {
	t.Helper()
	e, err := openpgp.NewEntity("backfill-test", "", email, &packet.Config{
		Algorithm: packet.PubKeyAlgoEdDSA,
	})
	if err != nil {
		t.Fatalf("NewEntity: %v", err)
	}
	return e
}

func armoredPublic(t *testing.T, e *openpgp.Entity) string {
	t.Helper()
	var buf bytes.Buffer
	w, err := armor.Encode(&buf, "PGP PUBLIC KEY BLOCK", nil)
	if err != nil {
		t.Fatalf("armor.Encode: %v", err)
	}
	if err := e.Serialize(w); err != nil {
		t.Fatalf("Serialize: %v", err)
	}
	w.Close()
	return buf.String()
}

// writeSignedCommit builds a signed commit body, writes it via
// `git hash-object`, and updates refs/heads/trunk so rev-list finds it.
func writeSignedCommit(t *testing.T, gitDir string, entity *openpgp.Entity, message string) string {
	t.Helper()
	const emptyTree = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"
	now := time.Now().Unix()
	unsignedBody := fmt.Sprintf(
		"tree %s\nauthor Alice <alice@shithub.test> %d +0000\ncommitter Alice <alice@shithub.test> %d +0000\n\n%s\n",
		emptyTree, now, now, message,
	)

	var sigBuf bytes.Buffer
	armorWriter, err := armor.Encode(&sigBuf, "PGP SIGNATURE", nil)
	if err != nil {
		t.Fatalf("armor.Encode: %v", err)
	}
	if err := openpgp.DetachSign(armorWriter, entity, strings.NewReader(unsignedBody), nil); err != nil {
		t.Fatalf("DetachSign: %v", err)
	}
	armorWriter.Close()

	sigStr := strings.TrimRight(sigBuf.String(), "\n")
	sigLines := strings.Split(sigStr, "\n")
	indented := []string{"gpgsig " + sigLines[0]}
	for _, line := range sigLines[1:] {
		indented = append(indented, " "+line)
	}
	gpgsigHeader := strings.Join(indented, "\n")
	signedBody := strings.Replace(
		unsignedBody,
		fmt.Sprintf("committer Alice <alice@shithub.test> %d +0000\n\n", now),
		fmt.Sprintf("committer Alice <alice@shithub.test> %d +0000\n%s\n\n", now, gpgsigHeader),
		1,
	)

	cmd := exec.Command("git", "-C", gitDir, "hash-object", "-w", "-t", "commit", "--stdin")
	cmd.Stdin = bytes.NewReader([]byte(signedBody))
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("hash-object: %v", err)
	}
	oid := strings.TrimSpace(string(out))

	if err := exec.Command("git", "-C", gitDir, "update-ref", "refs/heads/trunk", oid).Run(); err != nil {
		t.Fatalf("update-ref trunk: %v", err)
	}
	return oid
}

var _ = pgxpool.Pool{} // keep the import used; pool type appears in t-Helper signatures elsewhere
