//go:build integration

package collection

import (
	"context"
	"testing"

	"github.com/substrate/substrate/internal/store"
	"github.com/substrate/substrate/internal/workspace"
)

func TestCreateAndGetCollection(t *testing.T) {
	pool := store.NewTestPool(t)
	ctx := context.Background()
	ws, err := workspace.New(pool).CreateWorkspace(ctx, "acme")
	if err != nil {
		t.Fatalf("workspace: %v", err)
	}

	svc := New(pool)
	c, err := svc.Create(ctx, ws.ID, "trips")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if c.Level != "flexible" {
		t.Fatalf("level = %q, want flexible", c.Level)
	}

	got, err := svc.GetByName(ctx, ws.ID, "trips")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ID != c.ID {
		t.Fatalf("get returned %s, want %s", got.ID, c.ID)
	}

	if _, err := svc.Create(ctx, ws.ID, "trips"); err == nil {
		t.Fatal("expected duplicate-name error")
	}
}
