//go:build integration

package schema

import (
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/substrate/substrate/internal/collection"
	"github.com/substrate/substrate/internal/store"
	"github.com/substrate/substrate/internal/workspace"
)

// TestRegisterConcurrentVersionAllocation fires many concurrent Register calls at the
// same collection and asserts every one succeeds with a distinct, gap-free version
// 1..N. This exercises the FOR UPDATE lock in version allocation: without it,
// concurrent registers would race on NextSchemaVersion and collide on the
// UNIQUE(collection_id, version) constraint.
func TestRegisterConcurrentVersionAllocation(t *testing.T) {
	svc, ws, col := setup(t)
	ctx := context.Background()

	const n = 8
	var wg sync.WaitGroup
	versions := make([]int, n)
	errs := make([]error, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			reg, err := svc.Register(ctx, RegisterCmd{
				Workspace: ws, Collection: col, JSONSchema: personSchema(),
			})
			errs[i] = err
			if err == nil {
				versions[i] = reg.Version
			}
		}(i)
	}
	wg.Wait()

	seen := map[int]bool{}
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("register %d failed: %v", i, errs[i])
		}
		if versions[i] < 1 || versions[i] > n {
			t.Fatalf("version %d out of range: %d", i, versions[i])
		}
		if seen[versions[i]] {
			t.Fatalf("duplicate version allocated: %d", versions[i])
		}
		seen[versions[i]] = true
	}
	if len(seen) != n {
		t.Fatalf("expected %d distinct versions 1..%d, got %d", n, n, len(seen))
	}
}

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
	return New(pool), ws.ID, c.ID
}

func personSchema() map[string]any {
	return map[string]any{
		"type":       "object",
		"properties": map[string]any{"name": map[string]any{"type": "string"}},
		"required":   []any{"name"},
	}
}

func TestRegisterFirstSchemaAutoActivates(t *testing.T) {
	svc, ws, col := setup(t)
	ctx := context.Background()

	reg, err := svc.Register(ctx, RegisterCmd{
		Workspace: ws, Collection: col, Actor: "agent-1",
		JSONSchema: personSchema(),
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if reg.Version != 1 || reg.Lifecycle != "active" {
		t.Fatalf("first schema should be v1 active, got v%d %s", reg.Version, reg.Lifecycle)
	}

	got, err := svc.GetActive(ctx, col)
	if err != nil {
		t.Fatalf("get active: %v", err)
	}
	if got.Version != 1 {
		t.Fatalf("active version = %d, want 1", got.Version)
	}
}

func TestRegisterDraftThenList(t *testing.T) {
	svc, ws, col := setup(t)
	ctx := context.Background()
	// First schema auto-activates (v1).
	_, _ = svc.Register(ctx, RegisterCmd{Workspace: ws, Collection: col, JSONSchema: personSchema()})
	// Second registers as draft by default.
	d, err := svc.Register(ctx, RegisterCmd{Workspace: ws, Collection: col, JSONSchema: personSchema()})
	if err != nil {
		t.Fatalf("register draft: %v", err)
	}
	if d.Version != 2 || d.Lifecycle != "draft" {
		t.Fatalf("second schema should be v2 draft, got v%d %s", d.Version, d.Lifecycle)
	}
	list, err := svc.List(ctx, col)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("list len = %d, want 2", len(list))
	}
}

func TestRegisterInvalidSchemaRejected(t *testing.T) {
	svc, ws, col := setup(t)
	ctx := context.Background()
	_, err := svc.Register(ctx, RegisterCmd{
		Workspace: ws, Collection: col,
		JSONSchema: map[string]any{"type": 123}, // invalid: type must be string/array
	})
	if err == nil {
		t.Fatal("expected schema_invalid error")
	}
}
