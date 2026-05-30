//go:build integration

package schema

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/substrate/substrate/internal/collection"
	"github.com/substrate/substrate/internal/store"
	"github.com/substrate/substrate/internal/workspace"
)

func setup(t *testing.T) (*Service, uuid.UUID, uuid.UUID) {
	t.Helper()
	pool := store.NewTestPool(t)
	ctx := context.Background()
	ws, err := workspace.New(pool).CreateWorkspace(ctx, "acme")
	if err != nil {
		t.Fatalf("workspace: %v", err)
	}
	c, err := collection.New(pool).Create(ctx, ws.ID, "trips")
	if err != nil {
		t.Fatalf("collection: %v", err)
	}
	return New(pool), ws.ID, c.ID
}

func personSchema() map[string]any {
	return map[string]any{
		"type":       "object",
		"properties": map[string]any{"name": map[string]any{"type": "string"}},
		"required":   []any{"name"},
	}
}

func TestRegisterFirstSchemaAutoActivates(t *testing.T) {
	svc, ws, col := setup(t)
	ctx := context.Background()

	reg, err := svc.Register(ctx, RegisterCmd{
		Workspace: ws, Collection: col, Actor: "agent-1",
		JSONSchema: personSchema(),
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if reg.Version != 1 || reg.Lifecycle != "active" {
		t.Fatalf("first schema should be v1 active, got v%d %s", reg.Version, reg.Lifecycle)
	}

	got, err := svc.GetActive(ctx, col)
	if err != nil {
		t.Fatalf("get active: %v", err)
	}
	if got.Version != 1 {
		t.Fatalf("active version = %d, want 1", got.Version)
	}
}

func TestRegisterDraftThenList(t *testing.T) {
	svc, ws, col := setup(t)
	ctx := context.Background()
	// First schema auto-activates (v1).
	_, _ = svc.Register(ctx, RegisterCmd{Workspace: ws, Collection: col, JSONSchema: personSchema()})
	// Second registers as draft by default.
	d, err := svc.Register(ctx, RegisterCmd{Workspace: ws, Collection: col, JSONSchema: personSchema()})
	if err != nil {
		t.Fatalf("register draft: %v", err)
	}
	if d.Version != 2 || d.Lifecycle != "draft" {
		t.Fatalf("second schema should be v2 draft, got v%d %s", d.Version, d.Lifecycle)
	}
	list, err := svc.List(ctx, col)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("list len = %d, want 2", len(list))
	}
}

func TestRegisterInvalidSchemaRejected(t *testing.T) {
	svc, ws, col := setup(t)
	ctx := context.Background()
	_, err := svc.Register(ctx, RegisterCmd{
		Workspace: ws, Collection: col,
		JSONSchema: map[string]any{"type": 123}, // invalid: type must be string/array
	})
	if err == nil {
		t.Fatal("expected schema_invalid error")
	}
}
