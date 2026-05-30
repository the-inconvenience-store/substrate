//go:build integration

package projection_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/substrate/substrate/internal/db"
	"github.com/substrate/substrate/internal/projection"
	"github.com/substrate/substrate/internal/record"
	"github.com/substrate/substrate/internal/schema"
	"github.com/substrate/substrate/internal/store"
)

func TestBackfillAppliesDefaultsAndReStamps(t *testing.T) {
	pool := store.NewTestPool(t)
	ctx := context.Background()
	q := db.New(pool)
	ws, _ := q.CreateWorkspace(ctx, db.CreateWorkspaceParams{ID: uuid.New(), Name: "w", PolicyMode: "allow"})
	col, _ := q.CreateCollection(ctx, db.CreateCollectionParams{ID: uuid.New(), WorkspaceID: ws.ID, Name: "c", Level: "flexible"})

	reg := schema.New(pool)
	v1 := map[string]any{"type": "object", "properties": map[string]any{"a": map[string]any{"type": "string"}}}
	if _, err := reg.Register(ctx, schema.RegisterCmd{Workspace: ws.ID, Collection: col.ID, Actor: "t", JSONSchema: v1, Activate: true}); err != nil {
		t.Fatalf("v1: %v", err)
	}
	recs := record.New(pool, schema.NewValidator(reg))
	r1, _ := recs.Create(ctx, record.CreateCmd{Workspace: ws.ID, Collection: col.ID, Actor: "t", Data: map[string]any{"a": "x"}})
	recs.Create(ctx, record.CreateCmd{Workspace: ws.ID, Collection: col.ID, Actor: "t", Data: map[string]any{"a": "y"}})

	v2 := map[string]any{"type": "object", "properties": map[string]any{
		"a": map[string]any{"type": "string"},
		"b": map[string]any{"type": "string", "default": "filled"},
	}}
	if _, err := reg.Register(ctx, schema.RegisterCmd{Workspace: ws.ID, Collection: col.ID, Actor: "t", JSONSchema: v2, Activate: true}); err != nil {
		t.Fatalf("v2: %v", err)
	}

	bf := projection.NewBackfiller(pool, reg)
	rep, err := bf.Run(ctx, ws.ID, col.ID, 0)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if rep.Migrated != 2 || rep.Skipped != 0 || rep.Remaining != 0 {
		t.Fatalf("report = %+v, want migrated=2 skipped=0 remaining=0", rep)
	}

	var data []byte
	var rev int64
	var sv *int32
	if err := pool.QueryRow(ctx, `SELECT data, revision, schema_version FROM records WHERE id=$1`, r1.ID).Scan(&data, &rev, &sv); err != nil {
		t.Fatalf("read: %v", err)
	}
	if rev != 2 || sv == nil || *sv != 2 {
		t.Fatalf("rev=%d sv=%v, want rev=2 sv=2", rev, sv)
	}
	if !strings.Contains(strings.ReplaceAll(string(data), " ", ""), `"b":"filled"`) {
		t.Fatalf("data missing default: %s", data)
	}
	var n int
	pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE record_id=$1 AND type='migration'`, r1.ID).Scan(&n)
	if n != 1 {
		t.Fatalf("migration events = %d, want 1", n)
	}

	rep2, _ := bf.Run(ctx, ws.ID, col.ID, 0)
	if rep2.Migrated != 0 {
		t.Fatalf("second run migrated = %d, want 0", rep2.Migrated)
	}

	bad := uuid.New()
	if err := q.UpsertRecordProjection(ctx, db.UpsertRecordProjectionParams{
		ID: bad, CollectionID: col.ID, WorkspaceID: ws.ID, Data: []byte(`{"a":123}`),
		Revision: 1, Status: "active", SchemaVersion: pgtype.Int4{Int32: 1, Valid: true},
	}); err != nil {
		t.Fatalf("insert bad: %v", err)
	}
	rep3, _ := bf.Run(ctx, ws.ID, col.ID, 0)
	if rep3.Skipped != 1 || rep3.Remaining != 1 {
		t.Fatalf("report3 = %+v, want skipped=1 remaining=1", rep3)
	}
}

func TestBackfillFlexibleNoop(t *testing.T) {
	pool := store.NewTestPool(t)
	ctx := context.Background()
	q := db.New(pool)
	ws, _ := q.CreateWorkspace(ctx, db.CreateWorkspaceParams{ID: uuid.New(), Name: "w", PolicyMode: "allow"})
	col, _ := q.CreateCollection(ctx, db.CreateCollectionParams{ID: uuid.New(), WorkspaceID: ws.ID, Name: "c", Level: "flexible"})
	rep, err := projection.NewBackfiller(pool, schema.New(pool)).Run(ctx, ws.ID, col.ID, 0)
	if err != nil || rep.Migrated != 0 || rep.Scanned != 0 {
		t.Fatalf("flexible run = %+v err=%v", rep, err)
	}
}
