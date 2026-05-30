//go:build integration

package store_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/substrate/substrate/internal/db"
	"github.com/substrate/substrate/internal/store"
)

func TestPoliciesTableCRUD(t *testing.T) {
	pool := store.NewTestPool(t)
	ctx := context.Background()
	q := db.New(pool)

	ws, err := q.CreateWorkspace(ctx, db.CreateWorkspaceParams{ID: uuid.New(), Name: "w", PolicyMode: "allow"})
	if err != nil {
		t.Fatalf("workspace: %v", err)
	}
	row, err := q.InsertPolicy(ctx, db.InsertPolicyParams{
		ID: uuid.New(), WorkspaceID: ws.ID, Actor: "*",
		CollectionID: pgtype.UUID{}, Operation: "create", Effect: "deny",
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if row.Effect != "deny" {
		t.Fatalf("effect = %q", row.Effect)
	}
	rules, err := q.ListPolicies(ctx, ws.ID)
	if err != nil || len(rules) != 1 {
		t.Fatalf("list: %v len=%d", err, len(rules))
	}
	if rules[0].CollectionName.Valid {
		t.Fatalf("wildcard rule should have NULL collection_name")
	}
	if err := q.SetWorkspacePolicyMode(ctx, db.SetWorkspacePolicyModeParams{ID: ws.ID, PolicyMode: "deny"}); err != nil {
		t.Fatalf("set mode: %v", err)
	}
	mode, err := q.GetWorkspacePolicyMode(ctx, ws.ID)
	if err != nil || mode != "deny" {
		t.Fatalf("mode = %q err=%v", mode, err)
	}
	n, err := q.DeletePolicy(ctx, db.DeletePolicyParams{ID: row.ID, WorkspaceID: ws.ID})
	if err != nil || n != 1 {
		t.Fatalf("delete rows=%d err=%v", n, err)
	}
}
