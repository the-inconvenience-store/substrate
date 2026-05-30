//go:build integration

package store

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/substrate/substrate/internal/db"
)

func TestMigrate_CreatesTables(t *testing.T) {
	pool := NewTestPool(t) // NewTestPool already runs Migrate (goose)
	ctx := context.Background()

	var n int
	err := pool.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.tables
		 WHERE table_schema='public'
		   AND table_name IN ('workspaces','api_keys','collections','records','events')`,
	).Scan(&n)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if n != 5 {
		t.Fatalf("found %d of 5 expected tables", n)
	}
}

func TestGeneratedQueries_WorkspaceRoundTrip(t *testing.T) {
	pool := NewTestPool(t)
	ctx := context.Background()
	q := db.New(pool)

	id := uuid.New()
	created, err := q.CreateWorkspace(ctx, db.CreateWorkspaceParams{
		ID: id, Name: "acme", PolicyMode: "allow",
	})
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	if created.Name != "acme" {
		t.Fatalf("name = %q, want acme", created.Name)
	}

	got, err := q.GetWorkspace(ctx, id)
	if err != nil {
		t.Fatalf("get workspace: %v", err)
	}
	if got.ID != id {
		t.Fatalf("id = %s, want %s", got.ID, id)
	}
}
