//go:build integration

// Package benchfix seeds deterministic fixtures for Substrate's DB-backed and
// HTTP benchmarks. It is integration-tagged because it requires a live pool.
package benchfix

import (
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/substrate/substrate/internal/collection"
	"github.com/substrate/substrate/internal/record"
	"github.com/substrate/substrate/internal/workspace"
)

// Fixture is a seeded workspace + flexible collection plus a fast (nil-validator)
// seeding service. Build the service-under-test separately in the benchmark.
type Fixture struct {
	Pool       *pgxpool.Pool
	Workspace  uuid.UUID
	Collection uuid.UUID
	seedSvc    *record.Service
}

// Setup creates a workspace and a flexible collection named "bench" and returns a
// Fixture. The seeding service uses a nil validator so seeding does no schema work.
func Setup(tb testing.TB, pool *pgxpool.Pool) *Fixture {
	tb.Helper()
	ctx := tb.Context()
	w, err := workspace.New(pool).CreateWorkspace(ctx, "bench")
	if err != nil {
		tb.Fatalf("create workspace: %v", err)
	}
	col, err := collection.New(pool).Create(ctx, w.ID, "bench")
	if err != nil {
		tb.Fatalf("create collection: %v", err)
	}
	return &Fixture{
		Pool: pool, Workspace: w.ID, Collection: col.ID,
		seedSvc: record.New(pool, nil),
	}
}

// Payload returns a deterministic ~1 KB / 6-field record body. The "filler" field
// pads the body to a realistic size; pass a unique marker to vary one field.
func Payload(marker string) map[string]any {
	return map[string]any{
		"title":    "benchmark record " + marker,
		"status":   "active",
		"priority": 3,
		"score":    42.5,
		"tags":     []any{"alpha", "beta", "gamma"},
		"filler":   strings.Repeat("x", 900),
	}
}

// SeedRecords creates n records and returns their ids in creation order.
func (f *Fixture) SeedRecords(tb testing.TB, n int) []uuid.UUID {
	tb.Helper()
	ctx := tb.Context()
	ids := make([]uuid.UUID, 0, n)
	for i := 0; i < n; i++ {
		rec, err := f.seedSvc.Create(ctx, record.CreateCmd{
			Workspace:  f.Workspace,
			Collection: f.Collection,
			Actor:      "seed",
			Data:       Payload(strconv.Itoa(i)),
		})
		if err != nil {
			tb.Fatalf("seed record %d: %v", i, err)
		}
		ids = append(ids, rec.ID)
	}
	return ids
}

// SeedHistory creates one record and advances it through `revisions` total
// revisions (1 create + revisions-1 updates), returning its id.
func (f *Fixture) SeedHistory(tb testing.TB, revisions int) uuid.UUID {
	tb.Helper()
	ctx := tb.Context()
	rec, err := f.seedSvc.Create(ctx, record.CreateCmd{
		Workspace: f.Workspace, Collection: f.Collection, Actor: "seed",
		Data: Payload("v1"),
	})
	if err != nil {
		tb.Fatalf("seed history create: %v", err)
	}
	for r := 2; r <= revisions; r++ {
		_, err := f.seedSvc.Update(ctx, record.UpdateCmd{
			Workspace: f.Workspace, Collection: f.Collection, ID: rec.ID,
			ExpectedRevision: int64(r - 1), Actor: "seed",
			Data: Payload("v" + strconv.Itoa(r)),
		})
		if err != nil {
			tb.Fatalf("seed history update rev %d: %v", r, err)
		}
	}
	return rec.ID
}
