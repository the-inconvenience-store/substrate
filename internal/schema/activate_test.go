//go:build integration

package schema

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/substrate/substrate/internal/apierr"
	"github.com/substrate/substrate/internal/db"
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

func TestActivateForceRationalePersistedOnSchemaRow(t *testing.T) {
	svc, ws, col := setup(t)
	ctx := context.Background()

	// v1 active
	_, err := svc.Register(ctx, RegisterCmd{Workspace: ws, Collection: col, JSONSchema: personSchema()})
	if err != nil {
		t.Fatalf("v1 register: %v", err)
	}
	// v2 draft: breaking change
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

	const wantRationale = "intentional breaking activation"
	if err := svc.Activate(ctx, ws, col, v2.Version, "agent-1", true, wantRationale); err != nil {
		t.Fatalf("forced activate: %v", err)
	}

	got, err := svc.Get(ctx, col, v2.Version)
	if err != nil {
		t.Fatalf("get schema: %v", err)
	}
	if got.Lifecycle != "active" {
		t.Fatalf("v2 lifecycle = %q, want active", got.Lifecycle)
	}

	// Verify rationale is persisted on the schemas row via the internal pool (white-box, same package).
	q := db.New(svc.pool)
	row, err := q.GetSchema(ctx, db.GetSchemaParams{CollectionID: col, Version: int32(v2.Version)})
	if err != nil {
		t.Fatalf("GetSchema: %v", err)
	}
	if !row.Rationale.Valid || row.Rationale.String != wantRationale {
		t.Fatalf("rationale = %q (valid=%v), want %q", row.Rationale.String, row.Rationale.Valid, wantRationale)
	}
}

func TestSchemaLifecycleEventsEmitted(t *testing.T) {
	svc, ws, col := setup(t)
	ctx := context.Background()

	// register v1 (auto-activates) → schema_registered + schema_activated
	_, err := svc.Register(ctx, RegisterCmd{Workspace: ws, Collection: col, JSONSchema: personSchema(), Actor: "agent-1"})
	if err != nil {
		t.Fatalf("v1 register: %v", err)
	}

	// register v2 compatible (draft) → schema_registered
	v2doc := map[string]any{
		"type":       "object",
		"properties": map[string]any{"name": map[string]any{"type": "string"}, "note": map[string]any{"type": "string"}},
		"required":   []any{"name"},
	}
	v2, err := svc.Register(ctx, RegisterCmd{Workspace: ws, Collection: col, JSONSchema: v2doc, Actor: "agent-1"})
	if err != nil {
		t.Fatalf("v2 register: %v", err)
	}

	// activate v2 → schema_activated (and internally deprecates v1 in activateTx, no separate deprecated event from activateTx)
	if err := svc.Activate(ctx, ws, col, v2.Version, "agent-1", false, ""); err != nil {
		t.Fatalf("activate v2: %v", err)
	}

	// deprecate v1 explicitly → schema_deprecated
	if err := svc.Deprecate(ctx, col, 1); err != nil {
		t.Fatalf("deprecate v1: %v", err)
	}

	// Assert event counts via direct SQL on the pool.
	countEvent := func(typ string) int {
		t.Helper()
		var n int
		err := svc.pool.QueryRow(ctx,
			`SELECT count(*) FROM events WHERE collection_id = $1 AND type = $2`,
			col, typ,
		).Scan(&n)
		if err != nil {
			t.Fatalf("count %s events: %v", typ, err)
		}
		return n
	}

	// v1 register + v2 register = 2
	if n := countEvent("schema_registered"); n != 2 {
		t.Errorf("schema_registered count = %d, want 2", n)
	}
	// v1 auto-activate + v2 activate = 2
	if n := countEvent("schema_activated"); n != 2 {
		t.Errorf("schema_activated count = %d, want 2", n)
	}
	// explicit deprecate of v1 = 1
	if n := countEvent("schema_deprecated"); n != 1 {
		t.Errorf("schema_deprecated count = %d, want 1", n)
	}

	// Assert state_after JSON payload on one activated event.
	var stateAfter []byte
	err = svc.pool.QueryRow(ctx,
		`SELECT state_after FROM events WHERE collection_id = $1 AND type = 'schema_activated' ORDER BY seq DESC LIMIT 1`,
		col,
	).Scan(&stateAfter)
	if err != nil {
		t.Fatalf("fetch state_after: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(stateAfter, &payload); err != nil {
		t.Fatalf("decode state_after: %v", err)
	}
	if payload["lifecycle"] != "active" {
		t.Errorf("state_after.lifecycle = %q, want active", payload["lifecycle"])
	}
	if payload["version"] == nil {
		t.Errorf("state_after missing version field")
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
