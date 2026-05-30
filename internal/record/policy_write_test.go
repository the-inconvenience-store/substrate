//go:build integration

package record_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/substrate/substrate/internal/apierr"
	"github.com/substrate/substrate/internal/db"
	"github.com/substrate/substrate/internal/policy"
	"github.com/substrate/substrate/internal/record"
	"github.com/substrate/substrate/internal/store"
)

func TestCreateDeniedByPolicy(t *testing.T) {
	pool := store.NewTestPool(t)
	ctx := context.Background()
	q := db.New(pool)
	ws, _ := q.CreateWorkspace(ctx, db.CreateWorkspaceParams{ID: uuid.New(), Name: "w", PolicyMode: "allow"})
	col, _ := q.CreateCollection(ctx, db.CreateCollectionParams{ID: uuid.New(), WorkspaceID: ws.ID, Name: "c", Level: "flexible"})
	if _, err := q.InsertPolicy(ctx, db.InsertPolicyParams{
		ID: uuid.New(), WorkspaceID: ws.ID, Actor: "*",
		CollectionID: pgtype.UUID{Bytes: col.ID, Valid: true}, Operation: policy.OpCreate, Effect: "deny",
	}); err != nil {
		t.Fatalf("rule: %v", err)
	}
	svc := record.New(pool, nil).WithEvaluator(policy.NewEngine(pool))

	_, err := svc.Create(ctx, record.CreateCmd{Workspace: ws.ID, Collection: col.ID, Actor: "alice", Data: map[string]any{"x": 1}})
	var ae *apierr.Error
	if !errors.As(err, &ae) || ae.Code != apierr.Forbidden {
		t.Fatalf("want Forbidden, got %v", err)
	}
}

func TestCreateAllowedStampsTrace(t *testing.T) {
	pool := store.NewTestPool(t)
	ctx := context.Background()
	q := db.New(pool)
	ws, _ := q.CreateWorkspace(ctx, db.CreateWorkspaceParams{ID: uuid.New(), Name: "w", PolicyMode: "allow"})
	col, _ := q.CreateCollection(ctx, db.CreateCollectionParams{ID: uuid.New(), WorkspaceID: ws.ID, Name: "c", Level: "flexible"})
	svc := record.New(pool, nil).WithEvaluator(policy.NewEngine(pool))

	rec, err := svc.Create(ctx, record.CreateCmd{Workspace: ws.ID, Collection: col.ID, Actor: "alice", Data: map[string]any{"x": 1}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	var trace []byte
	if err := pool.QueryRow(ctx,
		`SELECT trace FROM events WHERE record_id=$1 AND type='create'`, rec.ID).Scan(&trace); err != nil {
		t.Fatalf("scan trace: %v", err)
	}
	if len(trace) == 0 {
		t.Fatalf("expected non-empty trace on allowed create")
	}
}

func TestCreateNoEvaluatorUnchanged(t *testing.T) {
	pool := store.NewTestPool(t)
	ctx := context.Background()
	q := db.New(pool)
	ws, _ := q.CreateWorkspace(ctx, db.CreateWorkspaceParams{ID: uuid.New(), Name: "w", PolicyMode: "deny"})
	col, _ := q.CreateCollection(ctx, db.CreateCollectionParams{ID: uuid.New(), WorkspaceID: ws.ID, Name: "c", Level: "flexible"})
	svc := record.New(pool, nil) // no evaluator: deny mode must NOT apply

	if _, err := svc.Create(ctx, record.CreateCmd{Workspace: ws.ID, Collection: col.ID, Actor: "alice"}); err != nil {
		t.Fatalf("create without evaluator should succeed, got %v", err)
	}
}
