//go:build integration

package schema_test

import (
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/substrate/substrate/internal/db"
	"github.com/substrate/substrate/internal/schema"
	"github.com/substrate/substrate/internal/store"
)

type fakeEnqueuer struct {
	mu    sync.Mutex
	calls []uuid.UUID
}

func (f *fakeEnqueuer) Enqueue(ws, col uuid.UUID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, col)
}

func TestEnqueueOnActivationWhenOptedIn(t *testing.T) {
	pool := store.NewTestPool(t)
	ctx := context.Background()
	q := db.New(pool)
	ws, _ := q.CreateWorkspace(ctx, db.CreateWorkspaceParams{ID: uuid.New(), Name: "w", PolicyMode: "allow"})
	col, _ := q.CreateCollection(ctx, db.CreateCollectionParams{ID: uuid.New(), WorkspaceID: ws.ID, Name: "c", Level: "flexible"})

	fe := &fakeEnqueuer{}
	reg := schema.New(pool).WithBackfillEnqueuer(fe)
	obj := map[string]any{"type": "object", "properties": map[string]any{"a": map[string]any{"type": "string"}}}

	// auto_backfill OFF -> no enqueue on first activation.
	if _, err := reg.Register(ctx, schema.RegisterCmd{Workspace: ws.ID, Collection: col.ID, Actor: "t", JSONSchema: obj, Activate: true}); err != nil {
		t.Fatalf("v1: %v", err)
	}
	if len(fe.calls) != 0 {
		t.Fatalf("unexpected enqueue while opted out: %v", fe.calls)
	}

	// Opt in, activate a v2 -> enqueue once.
	if err := q.SetAutoBackfill(ctx, db.SetAutoBackfillParams{WorkspaceID: ws.ID, ID: col.ID, AutoBackfill: true}); err != nil {
		t.Fatalf("opt-in: %v", err)
	}
	v2 := map[string]any{"type": "object", "properties": map[string]any{
		"a": map[string]any{"type": "string"}, "b": map[string]any{"type": "string", "default": "x"},
	}}
	if _, err := reg.Register(ctx, schema.RegisterCmd{Workspace: ws.ID, Collection: col.ID, Actor: "t", JSONSchema: v2, Activate: true}); err != nil {
		t.Fatalf("v2: %v", err)
	}
	if len(fe.calls) != 1 || fe.calls[0] != col.ID {
		t.Fatalf("expected one enqueue for col, got %v", fe.calls)
	}
}
