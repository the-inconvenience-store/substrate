//go:build integration

package record

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/substrate/substrate/internal/apierr"
	"github.com/substrate/substrate/internal/collection"
	"github.com/substrate/substrate/internal/store"
	"github.com/substrate/substrate/internal/workspace"
)

// setup creates a workspace + flexible collection and returns ids and a record service.
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

func TestCreateAndGet(t *testing.T) {
	svc, ws, col := setup(t)
	ctx := context.Background()

	rec, err := svc.Create(ctx, CreateCmd{
		Workspace: ws, Collection: col, Actor: "agent-1",
		Data: map[string]any{"destination": "Tokyo"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if rec.Revision != 1 {
		t.Fatalf("revision = %d, want 1", rec.Revision)
	}

	got, err := svc.Get(ctx, ws, col, rec.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Data["destination"] != "Tokyo" {
		t.Fatalf("data = %v", got.Data)
	}
}

func TestUpdateRevisionConflict(t *testing.T) {
	svc, ws, col := setup(t)
	ctx := context.Background()
	rec, _ := svc.Create(ctx, CreateCmd{Workspace: ws, Collection: col, Data: map[string]any{"n": 1}})

	// Correct revision succeeds and bumps to 2.
	updated, err := svc.Update(ctx, UpdateCmd{
		Workspace: ws, Collection: col, ID: rec.ID,
		ExpectedRevision: 1, Data: map[string]any{"n": 2},
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.Revision != 2 {
		t.Fatalf("revision = %d, want 2", updated.Revision)
	}

	// Stale revision fails with Conflict.
	_, err = svc.Update(ctx, UpdateCmd{
		Workspace: ws, Collection: col, ID: rec.ID,
		ExpectedRevision: 1, Data: map[string]any{"n": 3},
	})
	e, ok := apierr.As(err)
	if !ok || e.Code != apierr.Conflict {
		t.Fatalf("err = %v, want revision_conflict", err)
	}
}

func TestIdempotentCreate(t *testing.T) {
	svc, ws, col := setup(t)
	ctx := context.Background()
	cmd := CreateCmd{
		Workspace: ws, Collection: col, IdempotencyKey: "k1",
		Data: map[string]any{"n": 1},
	}
	first, err := svc.Create(ctx, cmd)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, err := svc.Create(ctx, cmd)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if first.ID != second.ID || second.Revision != 1 {
		t.Fatalf("replay produced %+v, want same id and revision 1", second)
	}
}

func TestSoftDelete(t *testing.T) {
	svc, ws, col := setup(t)
	ctx := context.Background()
	rec, _ := svc.Create(ctx, CreateCmd{Workspace: ws, Collection: col, Data: map[string]any{"n": 1}})
	if err := svc.Delete(ctx, ws, col, rec.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	_, err := svc.Get(ctx, ws, col, rec.ID)
	e, ok := apierr.As(err)
	if !ok || e.Code != apierr.NotFound {
		t.Fatalf("get after delete: %v, want not_found", err)
	}
}
