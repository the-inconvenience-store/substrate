//go:build integration

package store_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/substrate/substrate/internal/db"
	"github.com/substrate/substrate/internal/store"
)

func TestProjectionSchemaAndQueries(t *testing.T) {
	pool := store.NewTestPool(t)
	ctx := context.Background()
	q := db.New(pool)

	ws, _ := q.CreateWorkspace(ctx, db.CreateWorkspaceParams{ID: uuid.New(), Name: "w", PolicyMode: "allow"})
	col, _ := q.CreateCollection(ctx, db.CreateCollectionParams{ID: uuid.New(), WorkspaceID: ws.ID, Name: "c", Level: "flexible"})

	on, err := q.GetCollectionAutoBackfill(ctx, col.ID)
	if err != nil || on {
		t.Fatalf("default auto_backfill=%v err=%v", on, err)
	}
	if err := q.SetAutoBackfill(ctx, db.SetAutoBackfillParams{WorkspaceID: ws.ID, ID: col.ID, AutoBackfill: true}); err != nil {
		t.Fatalf("set: %v", err)
	}
	if on, _ := q.GetCollectionAutoBackfill(ctx, col.ID); !on {
		t.Fatalf("auto_backfill should be true")
	}

	rid := uuid.New()
	if err := q.AppendEvent(ctx, db.AppendEventParams{
		ID: uuid.New(), WorkspaceID: ws.ID, CollectionID: col.ID, RecordID: rid,
		Type: "create", Revision: 1, StateAfter: []byte(`{"a":1}`),
		SchemaVersion: pgtype.Int4{Int32: 2, Valid: true},
	}); err != nil {
		t.Fatalf("append: %v", err)
	}
	ev, err := q.GetLatestRecordEvent(ctx, db.GetLatestRecordEventParams{WorkspaceID: ws.ID, CollectionID: col.ID, RecordID: rid})
	if err != nil || !ev.SchemaVersion.Valid || ev.SchemaVersion.Int32 != 2 {
		t.Fatalf("latest event sv=%+v err=%v", ev.SchemaVersion, err)
	}

	if err := q.UpsertRecordProjection(ctx, db.UpsertRecordProjectionParams{
		ID: rid, CollectionID: col.ID, WorkspaceID: ws.ID, Data: []byte(`{"a":1}`),
		Revision: 1, Status: "active", SchemaVersion: pgtype.Int4{},
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	rows, err := q.ListRecordsBelowVersion(ctx, db.ListRecordsBelowVersionParams{
		CollectionID: col.ID, SchemaVersion: pgtype.Int4{Int32: 2, Valid: true}, ID: uuid.UUID{}, Limit: 10,
	})
	if err != nil || len(rows) != 1 {
		t.Fatalf("below-version len=%d err=%v", len(rows), err)
	}
}
