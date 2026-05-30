//go:build integration

package record

import (
	"context"
	"strconv"
	"testing"
)

func TestHistoryAndAsOfAndRevert(t *testing.T) {
	svc, ws, col := setup(t)
	ctx := context.Background()

	rec, _ := svc.Create(ctx, CreateCmd{Workspace: ws, Collection: col, Data: map[string]any{"n": float64(1)}})
	_, _ = svc.Update(ctx, UpdateCmd{Workspace: ws, Collection: col, ID: rec.ID, ExpectedRevision: 1, Data: map[string]any{"n": float64(2)}})
	_, _ = svc.Update(ctx, UpdateCmd{Workspace: ws, Collection: col, ID: rec.ID, ExpectedRevision: 2, Data: map[string]any{"n": float64(3)}})

	// History has 3 entries in order.
	hist, err := svc.History(ctx, ws, col, rec.ID, "tester")
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(hist) != 3 {
		t.Fatalf("history len = %d, want 3", len(hist))
	}
	if hist[0].Revision != 1 || hist[2].Revision != 3 {
		t.Fatalf("history order wrong: %+v", hist)
	}

	// Point-in-time read at revision 1.
	old, err := svc.GetAsOf(ctx, ws, col, rec.ID, AsOf{Revision: 1}, "tester")
	if err != nil {
		t.Fatalf("as-of: %v", err)
	}
	if old.Data["n"] != float64(1) {
		t.Fatalf("as-of n = %v, want 1", old.Data["n"])
	}

	// Revert back to revision 1 -> new revision 4 with old data.
	reverted, err := svc.Revert(ctx, ws, col, rec.ID, AsOf{Revision: 1}, "test-agent")
	if err != nil {
		t.Fatalf("revert: %v", err)
	}
	if reverted.Revision != 4 || reverted.Data["n"] != float64(1) {
		t.Fatalf("reverted = %+v, want revision 4 n=1", reverted)
	}

	// Current read now reflects the revert.
	cur, _ := svc.Get(ctx, ws, col, rec.ID, "tester")
	if cur.Data["n"] != float64(1) {
		t.Fatalf("current n = %v, want 1", cur.Data["n"])
	}

	_ = strconv.Itoa // keep import if unused after edits
}
