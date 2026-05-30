//go:build integration

package policy_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/substrate/substrate/internal/apierr"
	"github.com/substrate/substrate/internal/db"
	"github.com/substrate/substrate/internal/policy"
	"github.com/substrate/substrate/internal/store"
)

func seedWS(t *testing.T, q *db.Queries, mode string) (uuid.UUID, uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	ws, err := q.CreateWorkspace(ctx, db.CreateWorkspaceParams{ID: uuid.New(), Name: "w", PolicyMode: mode})
	if err != nil {
		t.Fatalf("ws: %v", err)
	}
	col, err := q.CreateCollection(ctx, db.CreateCollectionParams{
		ID: uuid.New(), WorkspaceID: ws.ID, Name: "things", Level: "flexible",
	})
	if err != nil {
		t.Fatalf("col: %v", err)
	}
	return ws.ID, col.ID
}

func TestAuthorizeDefaultAllow(t *testing.T) {
	pool := store.NewTestPool(t)
	q := db.New(pool)
	ws, col := seedWS(t, q, "allow")
	eng := policy.NewEngine(pool)

	dec, err := eng.Authorize(context.Background(), policy.Request{
		Workspace: ws, Actor: "alice", Collection: col, Operation: policy.OpCreate,
	})
	if err != nil || !dec.Allowed() || dec.Reason != "default_allow" {
		t.Fatalf("dec=%+v err=%v", dec, err)
	}
}

func TestAuthorizeDenyRuleWritesEvent(t *testing.T) {
	pool := store.NewTestPool(t)
	ctx := context.Background()
	q := db.New(pool)
	ws, col := seedWS(t, q, "allow")
	if _, err := q.InsertPolicy(ctx, db.InsertPolicyParams{
		ID: uuid.New(), WorkspaceID: ws, Actor: "alice",
		CollectionID: pgtype.UUID{Bytes: col, Valid: true}, Operation: policy.OpCreate, Effect: "deny",
	}); err != nil {
		t.Fatalf("rule: %v", err)
	}
	eng := policy.NewEngine(pool)

	dec, err := eng.Authorize(ctx, policy.Request{
		Workspace: ws, Actor: "alice", Collection: col, Target: uuid.Nil, Operation: policy.OpCreate,
	})
	if dec.Allowed() {
		t.Fatalf("expected deny, got %+v", dec)
	}
	var ae *apierr.Error
	if !errors.As(err, &ae) || ae.Code != apierr.Forbidden {
		t.Fatalf("expected Forbidden, got %v", err)
	}
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM events WHERE workspace_id=$1 AND type='policy_denied'`, ws).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("denial events = %d, want 1", n)
	}
}

func TestAuthorizeDefaultDeny(t *testing.T) {
	pool := store.NewTestPool(t)
	q := db.New(pool)
	ws, col := seedWS(t, q, "deny")
	eng := policy.NewEngine(pool)

	dec, err := eng.Authorize(context.Background(), policy.Request{
		Workspace: ws, Actor: "alice", Collection: col, Operation: policy.OpRead,
	})
	if dec.Allowed() || dec.Reason != "default_deny" {
		t.Fatalf("dec=%+v err=%v", dec, err)
	}
}
