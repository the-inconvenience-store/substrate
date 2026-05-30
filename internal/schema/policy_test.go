//go:build integration

package schema_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/substrate/substrate/internal/apierr"
	"github.com/substrate/substrate/internal/db"
	"github.com/substrate/substrate/internal/policy"
	"github.com/substrate/substrate/internal/schema"
	"github.com/substrate/substrate/internal/store"
)

func TestRegisterDeniedByPolicy(t *testing.T) {
	pool := store.NewTestPool(t)
	ctx := context.Background()
	q := db.New(pool)
	ws, _ := q.CreateWorkspace(ctx, db.CreateWorkspaceParams{ID: uuid.New(), Name: "w", PolicyMode: "allow"})
	col, _ := q.CreateCollection(ctx, db.CreateCollectionParams{ID: uuid.New(), WorkspaceID: ws.ID, Name: "c", Level: "flexible"})
	if _, err := q.InsertPolicy(ctx, db.InsertPolicyParams{
		ID: uuid.New(), WorkspaceID: ws.ID, Actor: "*",
		CollectionID: pgtype.UUID{Bytes: col.ID, Valid: true}, Operation: policy.OpRegisterSchema, Effect: "deny",
	}); err != nil {
		t.Fatalf("rule: %v", err)
	}
	svc := schema.New(pool).WithEvaluator(policy.NewEngine(pool))

	_, err := svc.Register(ctx, schema.RegisterCmd{
		Workspace: ws.ID, Collection: col.ID, Actor: "alice",
		JSONSchema: map[string]any{"type": "object"},
	})
	var ae *apierr.Error
	if !errors.As(err, &ae) || ae.Code != apierr.Forbidden {
		t.Fatalf("want Forbidden, got %v", err)
	}
}
