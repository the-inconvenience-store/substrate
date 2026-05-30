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

func TestPolicyDeniedExcludedFromHistoryAndAsOf(t *testing.T) {
	pool := store.NewTestPool(t)
	ctx := context.Background()
	q := db.New(pool)
	ws, _ := q.CreateWorkspace(ctx, db.CreateWorkspaceParams{ID: uuid.New(), Name: "w", PolicyMode: "allow"})
	col, _ := q.CreateCollection(ctx, db.CreateCollectionParams{ID: uuid.New(), WorkspaceID: ws.ID, Name: "c", Level: "flexible"})
	svc := record.New(pool, nil).WithEvaluator(policy.NewEngine(pool))

	rec, err := svc.Create(ctx, record.CreateCmd{Workspace: ws.ID, Collection: col.ID, Actor: "alice", Data: map[string]any{"v": 1}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := svc.Update(ctx, record.UpdateCmd{
		Workspace: ws.ID, Collection: col.ID, ID: rec.ID, ExpectedRevision: 1,
		Actor: "alice", Data: map[string]any{"v": 2},
	}); err != nil {
		t.Fatalf("update: %v", err)
	}

	if _, err := q.InsertPolicy(ctx, db.InsertPolicyParams{
		ID: uuid.New(), WorkspaceID: ws.ID, Actor: "mallory",
		CollectionID: pgtype.UUID{Bytes: col.ID, Valid: true}, Operation: policy.OpUpdate, Effect: "deny",
	}); err != nil {
		t.Fatalf("rule: %v", err)
	}

	_, derr := svc.Update(ctx, record.UpdateCmd{
		Workspace: ws.ID, Collection: col.ID, ID: rec.ID, ExpectedRevision: 2,
		Actor: "mallory", Data: map[string]any{"v": 3},
	})
	var ae *apierr.Error
	if !errors.As(derr, &ae) || ae.Code != apierr.Forbidden {
		t.Fatalf("want Forbidden update for mallory, got %v", derr)
	}

	hist, err := svc.History(ctx, ws.ID, col.ID, rec.ID, "alice")
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	for _, h := range hist {
		if h.Type == "policy_denied" {
			t.Fatalf("history must not contain policy_denied entries, got %+v", hist)
		}
	}
	if len(hist) == 0 {
		t.Fatalf("expected history entries")
	}
	if last := hist[len(hist)-1]; last.Revision != 2 {
		t.Fatalf("expected last history revision 2, got %d (%+v)", last.Revision, hist)
	}

	got, err := svc.GetAsOf(ctx, ws.ID, col.ID, rec.ID, record.AsOf{Revision: 99}, "alice")
	if err != nil {
		t.Fatalf("get as-of: %v", err)
	}
	if got.Revision != 2 {
		t.Fatalf("expected as-of revision 2, got %d", got.Revision)
	}
	if v, ok := got.Data["v"].(float64); !ok || v != float64(2) {
		t.Fatalf("expected as-of data v=2, got %#v", got.Data["v"])
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
