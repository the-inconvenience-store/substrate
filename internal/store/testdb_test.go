//go:build integration

package store

import (
	"context"
	"testing"
)

func TestNewTestPool_Connects(t *testing.T) {
	pool := NewTestPool(t)
	var one int
	if err := pool.QueryRow(context.Background(), "SELECT 1").Scan(&one); err != nil {
		t.Fatalf("query: %v", err)
	}
	if one != 1 {
		t.Fatalf("got %d, want 1", one)
	}
}
