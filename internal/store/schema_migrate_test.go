//go:build integration

package store

import (
	"context"
	"testing"
)

func TestMigrate_CreatesSchemasTable(t *testing.T) {
	pool := NewTestPool(t)
	var n int
	err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.tables
		 WHERE table_schema='public' AND table_name='schemas'`).Scan(&n)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if n != 1 {
		t.Fatalf("schemas table missing (n=%d)", n)
	}
}
