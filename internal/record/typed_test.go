//go:build integration

package record

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/substrate/substrate/internal/apierr"
	"github.com/substrate/substrate/internal/collection"
	"github.com/substrate/substrate/internal/schema"
	"github.com/substrate/substrate/internal/store"
	"github.com/substrate/substrate/internal/workspace"
)

func TestTypedCreateValidatesAndStamps(t *testing.T) {
	pool := store.NewTestPool(t)
	ctx := context.Background()
	ws, _ := workspace.New(pool).CreateWorkspace(ctx, "acme")
	col, _ := collection.New(pool).Create(ctx, ws.ID, "people")
	reg := schema.New(pool)
	_, err := reg.Register(ctx, schema.RegisterCmd{
		Workspace: ws.ID, Collection: col.ID,
		JSONSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"name": map[string]any{"type": "string"}},
			"required":   []any{"name"},
		},
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	svc := New(pool, schema.NewValidator(reg))

	// Valid create stamps schema_version=1.
	rec, err := svc.Create(ctx, CreateCmd{Workspace: ws.ID, Collection: col.ID, Data: map[string]any{"name": "Ada"}})
	if err != nil {
		t.Fatalf("valid create: %v", err)
	}
	var sv int
	if e := pool.QueryRow(ctx, `SELECT schema_version FROM records WHERE id=$1`, rec.ID).Scan(&sv); e != nil {
		t.Fatalf("read schema_version: %v", e)
	}
	if sv != 1 {
		t.Fatalf("schema_version = %d, want 1", sv)
	}

	// Invalid create -> validation_failed.
	_, err = svc.Create(ctx, CreateCmd{Workspace: ws.ID, Collection: col.ID, Data: map[string]any{"name": 123}})
	e, ok := apierr.As(err)
	if !ok || e.Code != apierr.Validation {
		t.Fatalf("invalid create: %v, want validation_failed", err)
	}
}

var _ = uuid.Nil
