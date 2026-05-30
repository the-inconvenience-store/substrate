//go:build integration

package store

import (
	"context"
	"testing"
)

func TestMigrate_CreatesTables(t *testing.T) {
	pool := NewTestPool(t) // NewTestPool already runs Migrate
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
