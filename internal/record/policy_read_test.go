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
	"github.com/substrate/substrate/internal/query"
	"github.com/substrate/substrate/internal/record"
	"github.com/substrate/substrate/internal/store"
)

func TestReadDeniedByPolicy(t *testing.T) {
	pool := store.NewTestPool(t)
	ctx := context.Background()
	q := db.New(pool)
	ws, _ := q.CreateWorkspace(ctx, db.CreateWorkspaceParams{ID: uuid.New(), Name: "w", PolicyMode: "allow"})
	col, _ := q.CreateCollection(ctx, db.CreateCollectionParams{ID: uuid.New(), WorkspaceID: ws.ID, Name: "c", Level: "flexible"})
	if _, err := q.InsertPolicy(ctx, db.InsertPolicyParams{
		ID: uuid.New(), WorkspaceID: ws.ID, Actor: "mallory",
		CollectionID: pgtype.UUID{Bytes: col.ID, Valid: true}, Operation: policy.OpRead, Effect: "deny",
	}); err != nil {
		t.Fatalf("rule: %v", err)
	}
	svc := record.New(pool, nil).WithEvaluator(policy.NewEngine(pool))

	if _, _, err := svc.List(ctx, ws.ID, col.ID, "alice", mustParse(t)); err != nil {
		t.Fatalf("alice list: %v", err)
	}
	_, _, err := svc.List(ctx, ws.ID, col.ID, "mallory", mustParse(t))
	var ae *apierr.Error
	if !errors.As(err, &ae) || ae.Code != apierr.Forbidden {
		t.Fatalf("mallory list want Forbidden, got %v", err)
	}
}

func mustParse(t *testing.T) query.ListQuery {
	t.Helper()
	q, err := query.Parse(nil, "", "", "")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return q
}
