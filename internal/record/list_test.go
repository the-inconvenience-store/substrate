//go:build integration

package record_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/substrate/substrate/internal/collection"
	"github.com/substrate/substrate/internal/query"
	"github.com/substrate/substrate/internal/record"
	"github.com/substrate/substrate/internal/store"
	"github.com/substrate/substrate/internal/workspace"
)

func seedList(t *testing.T) (*record.Service, uuid.UUID, uuid.UUID) {
	t.Helper()
	pool := store.NewTestPool(t)
	ctx := context.Background()
	ws, err := workspace.New(pool).CreateWorkspace(ctx, "acme")
	if err != nil {
		t.Fatalf("ws: %v", err)
	}
	col, err := collection.New(pool).Create(ctx, ws.ID, "items")
	if err != nil {
		t.Fatalf("col: %v", err)
	}
	svc := record.New(pool, nil)
	for _, n := range []struct {
		status string
		age    int
	}{{"open", 30}, {"open", 20}, {"closed", 40}} {
		if _, err := svc.Create(ctx, record.CreateCmd{
			Workspace: ws.ID, Collection: col.ID,
			Data: map[string]any{"status": n.status, "age": n.age},
		}); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	return svc, ws.ID, col.ID
}

func TestList_FilterEq(t *testing.T) {
	svc, ws, col := seedList(t)
	q, _ := query.Parse([]string{"status:eq:open"}, "", "", "")
	items, _, err := svc.List(context.Background(), ws, col, q)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("got %d items, want 2", len(items))
	}
}

func TestList_RangeNumeric(t *testing.T) {
	svc, ws, col := seedList(t)
	q, _ := query.Parse([]string{"age:gte:30"}, "", "", "")
	items, _, err := svc.List(context.Background(), ws, col, q)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("got %d items, want 2 (age>=30)", len(items))
	}
}

func TestList_Pagination(t *testing.T) {
	svc, ws, col := seedList(t)
	q, _ := query.Parse(nil, "-created_at", "2", "")
	page1, cur, err := svc.List(context.Background(), ws, col, q)
	if err != nil {
		t.Fatalf("page1: %v", err)
	}
	if len(page1) != 2 || cur == "" {
		t.Fatalf("page1 len=%d cursor=%q, want 2 + cursor", len(page1), cur)
	}
	q2, _ := query.Parse(nil, "-created_at", "2", cur)
	page2, cur2, err := svc.List(context.Background(), ws, col, q2)
	if err != nil {
		t.Fatalf("page2: %v", err)
	}
	if len(page2) != 1 || cur2 != "" {
		t.Fatalf("page2 len=%d cursor=%q, want 1 + empty", len(page2), cur2)
	}
}
