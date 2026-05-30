//go:build integration

package projection_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/substrate/substrate/internal/db"
	"github.com/substrate/substrate/internal/projection"
	"github.com/substrate/substrate/internal/store"
)

func TestReplayRebuildsProjection(t *testing.T) {
	pool := store.NewTestPool(t)
	ctx := context.Background()
	q := db.New(pool)
	ws, _ := q.CreateWorkspace(ctx, db.CreateWorkspaceParams{ID: uuid.New(), Name: "w", PolicyMode: "allow"})
	col, _ := q.CreateCollection(ctx, db.CreateCollectionParams{ID: uuid.New(), WorkspaceID: ws.ID, Name: "c", Level: "flexible"})
	rid := uuid.New()

	appendEv := func(typ string, rev int64, state string) {
		if err := q.AppendEvent(ctx, db.AppendEventParams{
			ID: uuid.New(), WorkspaceID: ws.ID, CollectionID: col.ID, RecordID: rid,
			Type: typ, Revision: rev, StateAfter: []byte(state),
			SchemaVersion: pgtype.Int4{Int32: 1, Valid: true},
		}); err != nil {
			t.Fatalf("append %s: %v", typ, err)
		}
	}
	appendEv("create", 1, `{"a":"1"}`)
	appendEv("update", 2, `{"a":"2"}`)
	if err := q.AppendEvent(ctx, db.AppendEventParams{
		ID: uuid.New(), WorkspaceID: ws.ID, CollectionID: col.ID, RecordID: col.ID,
		Type: "schema_registered", Revision: 1, StateAfter: []byte(`{}`),
	}); err != nil {
		t.Fatalf("lifecycle: %v", err)
	}

	rep := projection.NewReplayer(pool)
	ok, err := rep.RebuildRecord(ctx, ws.ID, col.ID, rid)
	if err != nil || !ok {
		t.Fatalf("rebuild: ok=%v err=%v", ok, err)
	}
	var data []byte
	var rev int64
	var status string
	if err := pool.QueryRow(ctx, `SELECT data, revision, status FROM records WHERE collection_id=$1 AND id=$2`, col.ID, rid).
		Scan(&data, &rev, &status); err != nil {
		t.Fatalf("read projection: %v", err)
	}
	if rev != 2 || status != "active" || string(data) == "" {
		t.Fatalf("projection rev=%d status=%s data=%s", rev, status, data)
	}

	n, err := rep.RebuildCollection(ctx, ws.ID, col.ID)
	if err != nil || n != 1 {
		t.Fatalf("rebuild collection n=%d err=%v", n, err)
	}

	appendEv("delete", 3, `{"a":"2"}`)
	if _, err := rep.RebuildRecord(ctx, ws.ID, col.ID, rid); err != nil {
		t.Fatalf("rebuild2: %v", err)
	}
	_ = pool.QueryRow(ctx, `SELECT status FROM records WHERE collection_id=$1 AND id=$2`, col.ID, rid).Scan(&status)
	if status != "deleted" {
		t.Fatalf("status = %s, want deleted", status)
	}

	if ok, _ := rep.RebuildRecord(ctx, ws.ID, col.ID, uuid.New()); ok {
		t.Fatal("expected ok=false for record with no events")
	}
}
