//go:build integration

package record

import (
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/substrate/substrate/internal/apierr"
	"github.com/substrate/substrate/internal/collection"
	"github.com/substrate/substrate/internal/db"
	"github.com/substrate/substrate/internal/store"
	"github.com/substrate/substrate/internal/workspace"
)

// setup creates a workspace + flexible collection and returns ids and a record service.
func setup(t *testing.T) (*Service, uuid.UUID, uuid.UUID) {
	t.Helper()
	pool := store.NewTestPool(t)
	ctx := context.Background()
	ws, err := workspace.New(pool).CreateWorkspace(ctx, "acme")
	if err != nil {
		t.Fatalf("workspace: %v", err)
	}
	c, err := collection.New(pool).Create(ctx, ws.ID, "trips")
	if err != nil {
		t.Fatalf("collection: %v", err)
	}
	return New(pool, nil), ws.ID, c.ID
}

func TestCreateAndGet(t *testing.T) {
	svc, ws, col := setup(t)
	ctx := context.Background()

	rec, err := svc.Create(ctx, CreateCmd{
		Workspace: ws, Collection: col, Actor: "agent-1",
		Data: map[string]any{"destination": "Tokyo"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if rec.Revision != 1 {
		t.Fatalf("revision = %d, want 1", rec.Revision)
	}

	got, err := svc.Get(ctx, ws, col, rec.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Data["destination"] != "Tokyo" {
		t.Fatalf("data = %v", got.Data)
	}
}

func TestUpdateRevisionConflict(t *testing.T) {
	svc, ws, col := setup(t)
	ctx := context.Background()
	rec, _ := svc.Create(ctx, CreateCmd{Workspace: ws, Collection: col, Data: map[string]any{"n": 1}})

	// Correct revision succeeds and bumps to 2.
	updated, err := svc.Update(ctx, UpdateCmd{
		Workspace: ws, Collection: col, ID: rec.ID,
		ExpectedRevision: 1, Data: map[string]any{"n": 2},
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.Revision != 2 {
		t.Fatalf("revision = %d, want 2", updated.Revision)
	}

	// Stale revision fails with Conflict.
	_, err = svc.Update(ctx, UpdateCmd{
		Workspace: ws, Collection: col, ID: rec.ID,
		ExpectedRevision: 1, Data: map[string]any{"n": 3},
	})
	e, ok := apierr.As(err)
	if !ok || e.Code != apierr.Conflict {
		t.Fatalf("err = %v, want revision_conflict", err)
	}
}

func TestIdempotentCreate(t *testing.T) {
	svc, ws, col := setup(t)
	ctx := context.Background()
	cmd := CreateCmd{
		Workspace: ws, Collection: col, IdempotencyKey: "k1",
		Data: map[string]any{"n": 1},
	}
	first, err := svc.Create(ctx, cmd)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, err := svc.Create(ctx, cmd)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if first.ID != second.ID || second.Revision != 1 {
		t.Fatalf("replay produced %+v, want same id and revision 1", second)
	}
}

func TestSoftDelete(t *testing.T) {
	svc, ws, col := setup(t)
	ctx := context.Background()
	rec, _ := svc.Create(ctx, CreateCmd{Workspace: ws, Collection: col, Data: map[string]any{"n": 1}})
	if err := svc.Delete(ctx, ws, col, rec.ID, "agent-1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	_, err := svc.Get(ctx, ws, col, rec.ID)
	e, ok := apierr.As(err)
	if !ok || e.Code != apierr.NotFound {
		t.Fatalf("get after delete: %v, want not_found", err)
	}
}

// TestIdempotentCreateConcurrentConflict exercises the conflict replay path that
// is distinct from the sequential replay path.
//
// The sequential path (tested by TestIdempotentCreate) fires when the FIRST
// call within the transaction's lookupReplay already finds the committed row.
// The CONFLICT path fires when two goroutines race: both pass lookupReplay
// (finding nothing), but only one wins the INSERT; the loser's tx sees SQLSTATE
// 23505 on the idempotency index and must re-query *outside* the aborted tx.
//
// This test launches N concurrent Creates with the same key. Exactly one should
// succeed the INSERT path; the rest will hit the unique-index conflict and must
// replay — NOT return an error. All N results must have the same record ID and
// revision 1.
func TestIdempotentCreateConcurrentConflict(t *testing.T) {
	svc, ws, col := setup(t)
	ctx := context.Background()

	const n = 8
	key := "concurrent-key-" + uuid.New().String()

	type result struct {
		rec Record
		err error
	}
	results := make([]result, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := range n {
		i := i
		go func() {
			defer wg.Done()
			rec, err := svc.Create(ctx, CreateCmd{
				Workspace:      ws,
				Collection:     col,
				Actor:          "agent-1",
				Data:           map[string]any{"n": i},
				IdempotencyKey: key,
			})
			results[i] = result{rec, err}
		}()
	}
	wg.Wait()

	// All goroutines must succeed (no 409 or other error).
	var first uuid.UUID
	for i, r := range results {
		if r.err != nil {
			t.Errorf("goroutine %d: unexpected error: %v", i, r.err)
			continue
		}
		if r.rec.Revision != 1 {
			t.Errorf("goroutine %d: revision = %d, want 1", i, r.rec.Revision)
		}
		if first == uuid.Nil {
			first = r.rec.ID
		} else if r.rec.ID != first {
			t.Errorf("goroutine %d: id = %v, want %v", i, r.rec.ID, first)
		}
	}
}

// TestDeleteEventCarriesActor verifies that the event written by Delete includes
// the actor, which is surfaced via History.
func TestDeleteEventCarriesActor(t *testing.T) {
	svc, ws, col := setup(t)
	ctx := context.Background()

	rec, err := svc.Create(ctx, CreateCmd{
		Workspace: ws, Collection: col, Actor: "creator", Data: map[string]any{"x": 1},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	const deleteActor = "deleter-agent"
	if err := svc.Delete(ctx, ws, col, rec.ID, deleteActor); err != nil {
		t.Fatalf("delete: %v", err)
	}

	hist, err := svc.History(ctx, ws, col, rec.ID)
	if err != nil {
		t.Fatalf("history: %v", err)
	}

	// Find the delete event.
	var found bool
	for _, h := range hist {
		if h.Type == "delete" {
			found = true
			if h.Actor != deleteActor {
				t.Fatalf("delete event actor = %q, want %q", h.Actor, deleteActor)
			}
		}
	}
	if !found {
		t.Fatal("no delete event found in history")
	}
}

// TestRevertEventCarriesActor verifies that the event written by Revert includes
// the actor, which is surfaced via History.
func TestRevertEventCarriesActor(t *testing.T) {
	svc, ws, col := setup(t)
	ctx := context.Background()

	// Create a record, then update it so we have revision 2 to revert from.
	rec, err := svc.Create(ctx, CreateCmd{
		Workspace: ws, Collection: col, Actor: "creator", Data: map[string]any{"x": 1},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	_, err = svc.Update(ctx, UpdateCmd{
		Workspace: ws, Collection: col, ID: rec.ID,
		ExpectedRevision: 1, Actor: "updater", Data: map[string]any{"x": 2},
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}

	const revertActor = "reverter-agent"
	reverted, err := svc.Revert(ctx, ws, col, rec.ID, AsOf{Revision: 1}, revertActor)
	if err != nil {
		t.Fatalf("revert: %v", err)
	}
	if reverted.Actor != revertActor {
		t.Fatalf("reverted record actor = %q, want %q", reverted.Actor, revertActor)
	}

	hist, err := svc.History(ctx, ws, col, rec.ID)
	if err != nil {
		t.Fatalf("history: %v", err)
	}

	// Find the revert event.
	var found bool
	for _, h := range hist {
		if h.Type == "revert" {
			found = true
			if h.Actor != revertActor {
				t.Fatalf("revert event actor = %q, want %q", h.Actor, revertActor)
			}
		}
	}
	if !found {
		t.Fatal("no revert event found in history")
	}
}

// TestIdempotentCreateConflictPath exercises the specific errIdempotencyConflict
// sentinel by directly inserting an event row for the idempotency key (bypassing
// lookupReplay), then calling Create with that key. Because the event is already
// committed when Create runs, lookupReplay at the START of the tx finds the row
// (sequential path). To exercise the conflict path without goroutines we use the
// internal db layer directly: insert the event row within an explicit tx that
// commits, then observe that Create replays correctly.
//
// The goroutine-based test above is the canonical concurrent-conflict test.
// This test documents that the sentinel plumbing round-trips correctly when
// triggered via the internal appendEvent helper.
func TestIdempotentConflictSentinelReplay(t *testing.T) {
	pool := store.NewTestPool(t)
	ctx := context.Background()
	wsSvc := workspace.New(pool)
	colSvc := collection.New(pool)
	ws, err := wsSvc.CreateWorkspace(ctx, "acme2")
	if err != nil {
		t.Fatalf("workspace: %v", err)
	}
	c, err := colSvc.Create(ctx, ws.ID, "things")
	if err != nil {
		t.Fatalf("collection: %v", err)
	}
	svc := New(pool, nil)

	// First Create establishes the winning event row.
	key := "sentinel-key-" + uuid.New().String()
	first, err := svc.Create(ctx, CreateCmd{
		Workspace: ws.ID, Collection: c.ID, Actor: "agent-1",
		Data:           map[string]any{"v": 1},
		IdempotencyKey: key,
	})
	if err != nil {
		t.Fatalf("first create: %v", err)
	}

	// Now call errIdempotencyConflict path directly: the errIdempotencyConflict
	// sentinel is only reachable inside appendEvent when an idempotency key is set
	// and the unique index fires. We test this internal path directly.
	q := db.New(pool)
	err = appendEvent(ctx, q, eventRow{
		Workspace:      ws.ID,
		Collection:     c.ID,
		RecordID:       uuid.New(),
		Type:           "create",
		Revision:       1,
		State:          map[string]any{"v": 99},
		Actor:          "adversary",
		IdempotencyKey: key, // duplicate key — will hit the unique index
	})
	if err != errIdempotencyConflict {
		t.Fatalf("appendEvent with dup key: got %v, want errIdempotencyConflict", err)
	}

	// The service's Create, when it hits this sentinel, must replay via s.q.
	// Simulate that by calling lookupReplay directly outside any tx.
	replayed, ok, err := lookupReplay(ctx, q, ws.ID, key)
	if err != nil {
		t.Fatalf("lookupReplay after conflict: %v", err)
	}
	if !ok {
		t.Fatal("lookupReplay: expected to find replayed row")
	}
	if replayed.ID != first.ID || replayed.Revision != 1 {
		t.Fatalf("replay = %+v, want id=%v rev=1", replayed, first.ID)
	}
}
