// SPDX-License-Identifier: AGPL-3.0-or-later

package api

// PRO-EXT_SR-08: benchmark resolveVisibleSecretsFromDB across user-scope
// secret counts. Before this sprint mergeUserSecrets ran 1 + N queries
// (List + GetUserSecret per name). The bulk-fetch variant runs a single
// query, so allocs/op and Postgres round-trip count should both stay
// flat as N grows.
//
// Run:
//
//	SHITHUB_TEST_DATABASE_URL=... go test ./internal/web/handlers/api \
//	    -run=^$ -bench=BenchmarkResolveVisibleSecrets_UserScope -benchmem

import (
	"context"
	"io"
	"log/slog"
	"strconv"
	"testing"

	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
)

func benchResolveVisibleSecretsUserScope(b *testing.B, n int) {
	b.Helper()
	pool := dbtest.NewTestDB(b)
	box := mustBox(b)

	h := &Handlers{d: Deps{
		Pool:      pool,
		SecretBox: box,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}}

	userID := mustUser(b, pool, "bench-user")
	repoID := mustRepo(b, pool, userID, "bench-repo")

	for i := 0; i < n; i++ {
		mustUserSecret(b, pool, box, userID, "S"+strconv.Itoa(i), []byte("value"))
	}

	ctx := context.Background()
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := h.resolveVisibleSecretsFromDB(ctx, pool, repoID, ""); err != nil {
			b.Fatalf("resolve: %v", err)
		}
	}
}

func BenchmarkResolveVisibleSecrets_UserScope_N1(b *testing.B) {
	benchResolveVisibleSecretsUserScope(b, 1)
}

func BenchmarkResolveVisibleSecrets_UserScope_N10(b *testing.B) {
	benchResolveVisibleSecretsUserScope(b, 10)
}

func BenchmarkResolveVisibleSecrets_UserScope_N100(b *testing.B) {
	benchResolveVisibleSecretsUserScope(b, 100)
}
