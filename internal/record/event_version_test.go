//go:build integration

package record_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/substrate/substrate/internal/db"
	"github.com/substrate/substrate/internal/record"
	"github.com/substrate/substrate/internal/schema"
	"github.com/substrate/substrate/internal/store"
)

func TestCreateEventCarriesSchemaVersion(t *testing.T) {
	pool := store.NewTestPool(t)
	ctx := context.Background()
	q := db.New(pool)
	ws, _ := q.CreateWorkspace(ctx, db.CreateWorkspaceParams{ID: uuid.New(), Name: "w", PolicyMode: "allow"})
	col, _ := q.CreateCollection(ctx, db.CreateCollectionParams{ID: uuid.New(), WorkspaceID: ws.ID, Name: "c", Level: "flexible"})

	reg := schema.New(pool)
	if _, err := reg.Register(ctx, schema.RegisterCmd{
		Workspace: ws.ID, Collection: col.ID, Actor: "t",
		JSONSchema: map[string]any{"type": "object", "properties": map[string]any{"a": map[string]any{"type": "string"}}},
		Activate:   true,
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	svc := record.New(pool, schema.NewValidator(reg))
	rec, err := svc.Create(ctx, record.CreateCmd{Workspace: ws.ID, Collection: col.ID, Actor: "t", Data: map[string]any{"a": "x"}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	var sv *int32
	if err := pool.QueryRow(ctx, `SELECT schema_version FROM events WHERE record_id=$1 AND type='create'`, rec.ID).Scan(&sv); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if sv == nil || *sv != 1 {
		t.Fatalf("event schema_version = %v, want 1", sv)
	}
}
