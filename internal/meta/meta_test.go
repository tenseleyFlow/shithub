// SPDX-License-Identifier: AGPL-3.0-or-later

package meta_test

import (
	"context"
	"testing"

	metadb "github.com/tenseleyFlow/shithub/internal/meta/sqlc"
	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
)

// TestMetaRoundtrip exercises the dbtest harness end-to-end: clone-from-template,
// migrations applied, sqlc-generated queries against the meta table.
func TestMetaRoundtrip(t *testing.T) {
	t.Parallel()
	pool := dbtest.NewTestDB(t)
	q := metadb.New()
	ctx := context.Background()

	// Migrations populate the schema_version + app rows.
	got, err := q.GetMeta(ctx, pool, "schema_version")
	if err != nil {
		t.Fatalf("GetMeta(schema_version): %v", err)
	}
	if string(got.Value) != `"0001"` {
		t.Errorf("schema_version: got %s, want %q", got.Value, `"0001"`)
	}

	// Set a new key and read it back.
	if err := q.SetMeta(ctx, pool, metadb.SetMetaParams{Key: "test_key", Value: []byte(`"hello"`)}); err != nil {
		t.Fatalf("SetMeta: %v", err)
	}
	got2, err := q.GetMeta(ctx, pool, "test_key")
	if err != nil {
		t.Fatalf("GetMeta(test_key): %v", err)
	}
	if string(got2.Value) != `"hello"` {
		t.Errorf("test_key: got %s, want %q", got2.Value, `"hello"`)
	}

	// updated_at trigger: re-set with a new value, updated_at advances.
	first := got2.UpdatedAt
	if err := q.SetMeta(ctx, pool, metadb.SetMetaParams{Key: "test_key", Value: []byte(`"world"`)}); err != nil {
		t.Fatalf("SetMeta upsert: %v", err)
	}
	got3, err := q.GetMeta(ctx, pool, "test_key")
	if err != nil {
		t.Fatalf("GetMeta upsert: %v", err)
	}
	if !got3.UpdatedAt.Time.After(first.Time) {
		t.Errorf("updated_at trigger did not advance: first=%v, after=%v", first.Time, got3.UpdatedAt.Time)
	}

	// List + delete.
	rows, err := q.ListMeta(ctx, pool)
	if err != nil {
		t.Fatalf("ListMeta: %v", err)
	}
	if len(rows) < 3 { // schema_version, app, test_key
		t.Errorf("ListMeta: got %d rows, want >= 3", len(rows))
	}
	if err := q.DeleteMeta(ctx, pool, "test_key"); err != nil {
		t.Fatalf("DeleteMeta: %v", err)
	}
}

// TestMetaParallelism asserts dbtest gives each parallel test its own DB.
// If two parallel tests shared a database, the writes here would race.
func TestMetaParallelism(t *testing.T) {
	t.Parallel()
	for i := 0; i < 4; i++ {
		i := i
		t.Run("worker", func(t *testing.T) {
			t.Parallel()
			pool := dbtest.NewTestDB(t)
			q := metadb.New()
			key := "parallel_key"
			val := []byte(`"worker"`)
			if err := q.SetMeta(context.Background(), pool, metadb.SetMetaParams{Key: key, Value: val}); err != nil {
				t.Fatalf("SetMeta worker %d: %v", i, err)
			}
		})
	}
}
