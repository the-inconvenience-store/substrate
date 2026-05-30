//go:build integration

package collection_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/substrate/substrate/internal/collection"
	"github.com/substrate/substrate/internal/db"
	"github.com/substrate/substrate/internal/store"
)

func TestSetAutoBackfill(t *testing.T) {
	pool := store.NewTestPool(t)
	ctx := context.Background()
	q := db.New(pool)
	ws, _ := q.CreateWorkspace(ctx, db.CreateWorkspaceParams{ID: uuid.New(), Name: "w", PolicyMode: "allow"})
	svc := collection.New(pool)
	c, err := svc.Create(ctx, ws.ID, "things")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if c.AutoBackfill {
		t.Fatal("default auto_backfill should be false")
	}
	if err := svc.SetAutoBackfill(ctx, ws.ID, c.ID, true); err != nil {
		t.Fatalf("set: %v", err)
	}
	got, err := svc.GetByName(ctx, ws.ID, "things")
	if err != nil || !got.AutoBackfill {
		t.Fatalf("GetByName auto_backfill=%v err=%v", got.AutoBackfill, err)
	}
}
