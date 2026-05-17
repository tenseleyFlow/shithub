// SPDX-License-Identifier: AGPL-3.0-or-later

package webhookrelay_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/tenseleyFlow/shithub/internal/webhookrelay"
)

func TestIngest_CreatesDeliveryPerDestination(t *testing.T) {
	t.Parallel()
	f := setup(t)
	res, err := f.deps.Create(context.Background(), webhookrelay.CreateInput{
		UserID: f.userID, Name: "n", HMACSecret: []byte("k"),
		Destinations: []webhookrelay.Destination{
			{URL: "http://127.0.0.1:8001/a"},
			{URL: "http://127.0.0.1:8002/b"},
			{URL: "http://127.0.0.1:8003/c"},
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ingest, err := f.deps.Ingest(context.Background(), logger, res.Relay, []byte(`{"x":1}`))
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if ingest.DeliveryRows != 3 {
		t.Errorf("DeliveryRows: got %d, want 3", ingest.DeliveryRows)
	}
	if ingest.RequestID == "" {
		t.Error("RequestID should be set")
	}

	// Confirm DB has 3 delivery rows + 3 enqueued worker jobs.
	var rows int
	if err := f.pool.QueryRow(
		context.Background(),
		`SELECT count(*) FROM webhook_relay_deliveries WHERE relay_id = $1`, res.ID,
	).Scan(&rows); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if rows != 3 {
		t.Errorf("delivery rows in DB: got %d, want 3", rows)
	}
	var jobs int
	if err := f.pool.QueryRow(
		context.Background(),
		`SELECT count(*) FROM jobs WHERE kind = 'webhook_relay:deliver'`,
	).Scan(&jobs); err != nil {
		t.Fatalf("count jobs: %v", err)
	}
	if jobs != 3 {
		t.Errorf("enqueued jobs: got %d, want 3", jobs)
	}
}

func TestIngest_NoDestinationsIsNoOp(t *testing.T) {
	t.Parallel()
	f := setup(t)
	res, err := f.deps.Create(context.Background(), webhookrelay.CreateInput{
		UserID: f.userID, Name: "n", HMACSecret: []byte("k"),
		// Empty destinations slice — the user configured upstream
		// before they wired any downstreams.
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ingest, err := f.deps.Ingest(context.Background(), logger, res.Relay, []byte(`{}`))
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if ingest.DeliveryRows != 0 {
		t.Errorf("DeliveryRows: got %d, want 0", ingest.DeliveryRows)
	}
	if ingest.RequestID == "" {
		t.Error("RequestID should be set even for no-op")
	}
}

func TestIngest_DisabledRelayRejected(t *testing.T) {
	t.Parallel()
	f := setup(t)
	res, err := f.deps.Create(context.Background(), webhookrelay.CreateInput{
		UserID: f.userID, Name: "n", HMACSecret: []byte("k"),
		Destinations: []webhookrelay.Destination{{URL: "http://127.0.0.1:8001/a"}},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := f.deps.Disable(context.Background(), res.ID); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	// Simulate a stale read: caller has the pre-disable Relay; the
	// race between LookupByToken and Ingest is what we're testing.
	relay := res.Relay
	relay.Disabled = true
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	_, err = f.deps.Ingest(context.Background(), logger, relay, []byte(`{}`))
	if !errors.Is(err, webhookrelay.ErrDisabled) {
		t.Errorf("got %v, want ErrDisabled", err)
	}
}

func TestIngest_ClampsTooManyDestinations(t *testing.T) {
	t.Parallel()
	f := setup(t)
	// Build a Relay by hand with MaxDestinations+2 — defense-in-depth
	// since Create rejects this at construction time. Mirrors what an
	// out-of-band SQL insert could produce.
	res, err := f.deps.Create(context.Background(), webhookrelay.CreateInput{
		UserID: f.userID, Name: "n", HMACSecret: []byte("k"),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	tooMany := make([]webhookrelay.Destination, webhookrelay.MaxDestinations+2)
	for i := range tooMany {
		tooMany[i].URL = "http://127.0.0.1:8001/a"
	}
	relay := res.Relay
	relay.Destinations = tooMany
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ingest, err := f.deps.Ingest(context.Background(), logger, relay, []byte(`{}`))
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if ingest.DeliveryRows != webhookrelay.MaxDestinations {
		t.Errorf("DeliveryRows: got %d, want %d (truncated at cap)",
			ingest.DeliveryRows, webhookrelay.MaxDestinations)
	}
}
