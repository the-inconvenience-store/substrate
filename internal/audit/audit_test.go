//go:build integration

package audit_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/substrate/substrate/internal/audit"
	"github.com/substrate/substrate/internal/db"
	"github.com/substrate/substrate/internal/store"
)

func TestAuditListAndPaginate(t *testing.T) {
	pool := store.NewTestPool(t)
	ctx := context.Background()
	q := db.New(pool)
	ws, _ := q.CreateWorkspace(ctx, db.CreateWorkspaceParams{ID: uuid.New(), Name: "w", PolicyMode: "allow"})
	col, _ := q.CreateCollection(ctx, db.CreateCollectionParams{ID: uuid.New(), WorkspaceID: ws.ID, Name: "c", Level: "flexible"})

	rid := uuid.New()
	for i := 0; i < 3; i++ {
		if err := q.AppendEvent(ctx, db.AppendEventParams{
			ID: uuid.New(), WorkspaceID: ws.ID, CollectionID: col.ID, RecordID: rid,
			Type: "create", Revision: int64(i + 1),
		}); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	svc := audit.New(pool)

	page1, next, err := svc.List(ctx, ws.ID, audit.Filter{Limit: 2})
	if err != nil || len(page1) != 2 || next == "" {
		t.Fatalf("page1 len=%d next=%q err=%v", len(page1), next, err)
	}
	page2, next2, err := svc.List(ctx, ws.ID, audit.Filter{Limit: 2, Cursor: next})
	if err != nil || len(page2) != 1 || next2 != "" {
		t.Fatalf("page2 len=%d next=%q err=%v", len(page2), next2, err)
	}
	none, _, err := svc.List(ctx, ws.ID, audit.Filter{Type: "nope"})
	if err != nil || len(none) != 0 {
		t.Fatalf("filtered len=%d err=%v", len(none), err)
	}
}
