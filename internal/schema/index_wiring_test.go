//go:build integration

package schema_test

import (
	"context"
	"testing"

	"github.com/substrate/substrate/internal/collection"
	"github.com/substrate/substrate/internal/query"
	"github.com/substrate/substrate/internal/schema"
	"github.com/substrate/substrate/internal/store"
	"github.com/substrate/substrate/internal/workspace"
)

func TestActivationCreatesIndexes(t *testing.T) {
	pool := store.NewTestPool(t)
	ctx := context.Background()
	ws, err := workspace.New(pool).CreateWorkspace(ctx, "acme")
	if err != nil {
		t.Fatalf("ws: %v", err)
	}
	col, err := collection.New(pool).Create(ctx, ws.ID, "items")
	if err != nil {
		t.Fatalf("col: %v", err)
	}

	svc := schema.NewWithIndexer(pool, query.NewIndexer(pool))
	_, err = svc.Register(ctx, schema.RegisterCmd{
		Workspace: ws.ID, Collection: col.ID,
		JSONSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"price": map[string]any{"type": "number"}},
		},
		IndexedFields: []string{"price"},
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	var count int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM pg_indexes WHERE tablename='records' AND indexdef LIKE '%price%' AND indexdef LIKE '%'||$1||'%'`,
		col.ID.String(),
	).Scan(&count); err != nil {
		t.Fatalf("pg_indexes: %v", err)
	}
	if count == 0 {
		t.Fatal("expected an expression index on price for the collection")
	}
}
