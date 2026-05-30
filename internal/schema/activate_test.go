//go:build integration

package schema

import (
	"context"
	"testing"

	"github.com/substrate/substrate/internal/apierr"
)

func TestActivateBlocksBreakingUnlessForced(t *testing.T) {
	svc, ws, col := setup(t)
	ctx := context.Background()

	// v1 active: {name required}
	_, err := svc.Register(ctx, RegisterCmd{Workspace: ws, Collection: col, JSONSchema: personSchema()})
	if err != nil {
		t.Fatalf("v1: %v", err)
	}
	// v2 draft: adds a new required field "age" (breaking).
	breaking := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name": map[string]any{"type": "string"},
			"age":  map[string]any{"type": "integer"},
		},
		"required": []any{"name", "age"},
	}
	v2, err := svc.Register(ctx, RegisterCmd{Workspace: ws, Collection: col, JSONSchema: breaking})
	if err != nil {
		t.Fatalf("v2 register: %v", err)
	}

	// Activate without force -> schema_incompatible.
	err = svc.Activate(ctx, ws, col, v2.Version, "agent-1", false, "")
	e, ok := apierr.As(err)
	if !ok || e.Code != apierr.SchemaIncompatible {
		t.Fatalf("expected schema_incompatible, got %v", err)
	}

	// Activate with force -> succeeds, becomes active.
	if err := svc.Activate(ctx, ws, col, v2.Version, "agent-1", true, "intentional breaking change"); err != nil {
		t.Fatalf("forced activate: %v", err)
	}
	active, _ := svc.GetActive(ctx, col)
	if active.Version != v2.Version {
		t.Fatalf("active = %d, want %d", active.Version, v2.Version)
	}
}

func TestDeprecate(t *testing.T) {
	svc, ws, col := setup(t)
	ctx := context.Background()
	_, _ = svc.Register(ctx, RegisterCmd{Workspace: ws, Collection: col, JSONSchema: personSchema()})
	// register a second compatible version (adds optional field), activate it
	v2doc := map[string]any{
		"type":       "object",
		"properties": map[string]any{"name": map[string]any{"type": "string"}, "note": map[string]any{"type": "string"}},
		"required":   []any{"name"},
	}
	v2, _ := svc.Register(ctx, RegisterCmd{Workspace: ws, Collection: col, JSONSchema: v2doc, Activate: true})
	// Now deprecate v1.
	if err := svc.Deprecate(ctx, col, 1); err != nil {
		t.Fatalf("deprecate: %v", err)
	}
	got, _ := svc.Get(ctx, col, 1)
	if got.Lifecycle != "deprecated" {
		t.Fatalf("v1 lifecycle = %s, want deprecated", got.Lifecycle)
	}
	if v2.Lifecycle != "active" {
		t.Fatalf("v2 should be active")
	}
}
