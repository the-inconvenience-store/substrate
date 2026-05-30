# Substrate Plan 5 — Backfill & Replay Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build the `internal/projection` subsystem — event-stream **replay** (rebuild the records projection for disaster recovery) and lazy **backfill** (advance records toward the active schema version), with an opt-in background worker, a manual trigger, and admin replay endpoints.

**Architecture:** A new `internal/projection` package holds a pure defaults applier, a `Replayer` (rebuild projection from `events`), a `Backfiller` (apply defaults + re-validate + re-stamp records below the active version, one txn per record, idempotent), and an async `Worker`. Backfill is triggered both by an opt-in `collections.auto_backfill` flag (enqueued on schema activation via a `BackfillEnqueuer` seam on `schema.Service`, mirroring the Plan 4 evaluator) and a manual endpoint. A new nullable `events.schema_version` column makes events self-describing so replay restores the version stamp faithfully.

**Tech Stack:** Go 1.26, PostgreSQL via pgx/v5 (pgxpool), goose migrations, sqlc, `santhosh-tekuri/jsonschema/v6`, testcontainers, `mise` task runner.

---

## Background the implementer needs

- **Events** are the authoritative log; `records` is a synchronously-maintained projection. The `events` table has `seq bigserial`, `id`, `workspace_id`, `collection_id`, `record_id`, `type`, `revision`, `state_after jsonb`, `actor text`, `trace jsonb`, `idempotency_key text`, `created_at`. Plan 5 adds `schema_version int` (nullable).
- **`db.AppendEvent`** is generated from `internal/queries/events.sql`. Adding a column to its INSERT makes existing struct-literal callers default the new field to nil/zero (build stays green — the Plan 4 `trace` precedent).
- **Records** carry `schema_version pgtype.Int4` (NULL = flexible/grandfathered). `record.Service` (`internal/record/record.go`, `timetravel.go`) computes the version via the `Validator` (`sv`) and writes it to `records`, but currently NOT to `events`.
- **Schema registry** (`internal/schema/registry.go`): `GetActive(ctx, col) (ActiveSchema{Version int; Raw []byte}, error)` returns `apierr.NotFound` for flexible collections. `Register`/`Activate` call `s.ensureActiveIndexes(ctx, col)` **post-commit** — the right spot to also enqueue backfill. Optional dependencies are wired chainably (`WithIndexer`, `WithEvaluator`).
- **Compatibility**: only non-breaking changes activate without `force`, so old data already validates against a newly-active schema — backfill is default-filling + re-stamping, not transformation.
- **API**: `internal/api/router.go` `Deps` + `NewRouter`; inner `api` mux for `/v1/*` (auth-wrapped); outer `mux` for admin routes; `h := &handlers{collections, records}`; `(h *handlers) resolveCollection(r, name)`; `writeErr(w, err)`; `adminHandlers{workspaces, token}` with `authed(r)` checking `X-Admin-Token`; `httpx.JSON`. Go ServeMux can't do `{x}:literal`, so use subpaths (e.g. `/backfill`).
- **Tests**: unit `go test ./...`; integration `//go:build integration` + `store.NewTestPool(t)`; api integration tests are `package api` and reuse `doAs(t, method, url, key, actor, body)` from `internal/api/policy_api_test.go`. `mise run test`, `mise run test:integration` (runs the full suite regardless of path args), `mise run sqlc:generate`, `go build ./...`, `go vet ./...`.
- Migrations are 5-digit zero-padded (`00001`–`00003`); `internal/migrations/embed.go` uses `//go:embed *.sql` (new files auto-embed).

---

## Task 1: DB foundation & codegen

**Files:** create `internal/migrations/00004_event_schema_version.sql`, `internal/migrations/00005_collection_auto_backfill.sql`; modify `internal/queries/events.sql`, `internal/queries/records.sql`, `internal/queries/collections.sql`; regenerate `internal/db/`; test `internal/store/projection_migrate_test.go`.

- [ ] **Step 1: Write the migrations**

`internal/migrations/00004_event_schema_version.sql`:
```sql
-- +goose Up
ALTER TABLE events ADD COLUMN schema_version int;

-- +goose Down
ALTER TABLE events DROP COLUMN IF EXISTS schema_version;
```

`internal/migrations/00005_collection_auto_backfill.sql`:
```sql
-- +goose Up
ALTER TABLE collections ADD COLUMN auto_backfill boolean NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE collections DROP COLUMN IF EXISTS auto_backfill;
```

- [ ] **Step 2: Edit `internal/queries/events.sql`**

Replace `AppendEvent` and add `GetLatestRecordEvent`:
```sql
-- name: AppendEvent :exec
INSERT INTO events (id, workspace_id, collection_id, record_id, type, revision, state_after, actor, idempotency_key, trace, schema_version)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11);

-- name: GetLatestRecordEvent :one
SELECT state_after, revision, type, actor, schema_version
FROM events
WHERE workspace_id = $1 AND collection_id = $2 AND record_id = $3 AND type <> 'policy_denied'
ORDER BY seq DESC
LIMIT 1;
```

- [ ] **Step 3: Edit `internal/queries/records.sql`**

Modify `GetRecordForUpdate` to also return `schema_version`, and add four queries:
```sql
-- name: GetRecordForUpdate :one
SELECT revision, data, schema_version
FROM records
WHERE workspace_id = $1 AND collection_id = $2 AND id = $3 AND status = 'active'
FOR UPDATE;

-- name: ListRecordsBelowVersion :many
SELECT id, data, revision, schema_version
FROM records
WHERE collection_id = $1 AND status = 'active'
  AND (schema_version IS NULL OR schema_version < $2)
  AND id > $3
ORDER BY id
LIMIT $4;

-- name: CountRecordsBelowVersion :one
SELECT count(*)
FROM records
WHERE collection_id = $1 AND status = 'active'
  AND (schema_version IS NULL OR schema_version < $2);

-- name: UpsertRecordProjection :exec
INSERT INTO records (id, collection_id, workspace_id, data, revision, status, actor, schema_version)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (collection_id, id) DO UPDATE
SET data = EXCLUDED.data, revision = EXCLUDED.revision, status = EXCLUDED.status,
    actor = EXCLUDED.actor, schema_version = EXCLUDED.schema_version, updated_at = now();

-- name: ListRecordIDsInCollection :many
SELECT DISTINCT record_id
FROM events
WHERE workspace_id = $1 AND collection_id = $2 AND record_id <> collection_id AND type <> 'policy_denied';
```

- [ ] **Step 4: Edit `internal/queries/collections.sql`**

Add two queries and add `auto_backfill` to `GetCollectionByName`:
```sql
-- name: GetCollectionByName :one
SELECT id, workspace_id, name, level, auto_backfill
FROM collections
WHERE workspace_id = $1 AND name = $2;

-- name: SetAutoBackfill :exec
UPDATE collections SET auto_backfill = $3, updated_at = now()
WHERE workspace_id = $1 AND id = $2;

-- name: GetCollectionAutoBackfill :one
SELECT auto_backfill FROM collections WHERE id = $1;
```
(Leave `CreateCollection` unchanged — the column defaults false.)

- [ ] **Step 5: Regenerate + build**

Run `mise run sqlc:generate` then `go build ./...`.
Expected PASS. Note: `db.GetRecordForUpdateRow` gains `SchemaVersion pgtype.Int4` and `db.GetCollectionByNameRow` gains `AutoBackfill bool` — existing callers (`record.Delete`, `collection.GetByName`) read other fields, so the build stays green. `db.AppendEventParams` gains `SchemaVersion pgtype.Int4`; existing callers omit it (→ NULL).

VERIFY generated types: `db.GetLatestRecordEventRow{StateAfter []byte, Revision int64, Type string, Actor pgtype.Text, SchemaVersion pgtype.Int4}`; `db.ListRecordsBelowVersionParams{CollectionID uuid.UUID, SchemaVersion int32, ID uuid.UUID, Limit int32}` with row `{ID uuid.UUID, Data []byte, Revision int64, SchemaVersion pgtype.Int4}`; `db.UpsertRecordProjectionParams{ID, CollectionID, WorkspaceID uuid.UUID, Data []byte, Revision int64, Status string, Actor pgtype.Text, SchemaVersion pgtype.Int4}`; `db.SetAutoBackfillParams{WorkspaceID, ID uuid.UUID, AutoBackfill bool}`. If a generated field/param name differs, note it — downstream tasks reference these names.

- [ ] **Step 6: Write the smoke test `internal/store/projection_migrate_test.go`**
```go
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

func TestProjectionSchemaAndQueries(t *testing.T) {
	pool := store.NewTestPool(t)
	ctx := context.Background()
	q := db.New(pool)

	ws, _ := q.CreateWorkspace(ctx, db.CreateWorkspaceParams{ID: uuid.New(), Name: "w", PolicyMode: "allow"})
	col, _ := q.CreateCollection(ctx, db.CreateCollectionParams{ID: uuid.New(), WorkspaceID: ws.ID, Name: "c", Level: "flexible"})

	// auto_backfill defaults false; toggle to true.
	on, err := q.GetCollectionAutoBackfill(ctx, col.ID)
	if err != nil || on {
		t.Fatalf("default auto_backfill=%v err=%v", on, err)
	}
	if err := q.SetAutoBackfill(ctx, db.SetAutoBackfillParams{WorkspaceID: ws.ID, ID: col.ID, AutoBackfill: true}); err != nil {
		t.Fatalf("set: %v", err)
	}
	if on, _ := q.GetCollectionAutoBackfill(ctx, col.ID); !on {
		t.Fatalf("auto_backfill should be true")
	}

	// Event with schema_version + latest lookup.
	rid := uuid.New()
	if err := q.AppendEvent(ctx, db.AppendEventParams{
		ID: uuid.New(), WorkspaceID: ws.ID, CollectionID: col.ID, RecordID: rid,
		Type: "create", Revision: 1, StateAfter: []byte(`{"a":1}`),
		SchemaVersion: pgtype.Int4{Int32: 2, Valid: true},
	}); err != nil {
		t.Fatalf("append: %v", err)
	}
	ev, err := q.GetLatestRecordEvent(ctx, db.GetLatestRecordEventParams{WorkspaceID: ws.ID, CollectionID: col.ID, RecordID: rid})
	if err != nil || !ev.SchemaVersion.Valid || ev.SchemaVersion.Int32 != 2 {
		t.Fatalf("latest event sv=%+v err=%v", ev.SchemaVersion, err)
	}

	// Upsert projection + list-below-version.
	if err := q.UpsertRecordProjection(ctx, db.UpsertRecordProjectionParams{
		ID: rid, CollectionID: col.ID, WorkspaceID: ws.ID, Data: []byte(`{"a":1}`),
		Revision: 1, Status: "active", SchemaVersion: pgtype.Int4{}, // NULL -> below v2
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	rows, err := q.ListRecordsBelowVersion(ctx, db.ListRecordsBelowVersionParams{
		CollectionID: col.ID, SchemaVersion: 2, ID: uuid.UUID{}, Limit: 10,
	})
	if err != nil || len(rows) != 1 {
		t.Fatalf("below-version len=%d err=%v", len(rows), err)
	}
}
```

- [ ] **Step 7: Run + commit**

`mise run test:integration -- -run TestProjectionSchemaAndQueries ./internal/store/` (PASS); `mise run test` (PASS).
```bash
git add internal/migrations/00004_event_schema_version.sql internal/migrations/00005_collection_auto_backfill.sql internal/queries/ internal/db/ internal/store/projection_migrate_test.go
git commit -m "feat: events.schema_version + collections.auto_backfill + projection queries"
```

---

## Task 2: schema_version on record events

**Files:** modify `internal/record/record.go` (eventRow + appendEvent + Create/Update/Delete), `internal/record/timetravel.go` (Revert); test `internal/record/event_version_test.go`.

- [ ] **Step 1: Thread schema_version through the event row**

In `record.go`, add `SchemaVersion pgtype.Int4` to the `eventRow` struct, and in `appendEvent` add `SchemaVersion: e.SchemaVersion,` to the `db.AppendEventParams{...}` literal.

- [ ] **Step 2: Populate on Create / Update / Delete**

- `Create`: the `appendEvent(ctx, qtx, eventRow{...})` call inside the txn → add `SchemaVersion: sv,` (`sv` is the `pgtype.Int4` already computed before the txn).
- `Update`: its `appendEvent` call → add `SchemaVersion: sv,` (`sv` is computed in the closure before the append).
- `Delete`: `GetRecordForUpdate` now returns `SchemaVersion` (Task 1). In `Delete`'s `appendEvent` call → add `SchemaVersion: row.SchemaVersion,`.

- [ ] **Step 3: Revert (timetravel.go)**

In `Revert`'s `appendEvent` call, add `SchemaVersion: pgtype.Int4{},` (NULL — best-effort per the spec; the restored shape predates version tracking). Confirm `pgtype` is imported in `timetravel.go` (it is).

- [ ] **Step 4: Write the failing test `internal/record/event_version_test.go`**
```go
//go:build integration

package record_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/substrate/substrate/internal/db"
	"github.com/substrate/substrate/internal/record"
	"github.com/substrate/substrate/internal/schema"
	"github.com/substrate/substrate/internal/store"
)

func TestCreateEventCarriesSchemaVersion(t *testing.T) {
	pool := store.NewTestPool(t)
	ctx := context.Background()
	q := db.New(pool)
	ws, _ := q.CreateWorkspace(ctx, db.CreateWorkspaceParams{ID: uuid.New(), Name: "w", PolicyMode: "allow"})
	col, _ := q.CreateCollection(ctx, db.CreateCollectionParams{ID: uuid.New(), WorkspaceID: ws.ID, Name: "c", Level: "flexible"})

	reg := schema.New(pool)
	if _, err := reg.Register(ctx, schema.RegisterCmd{
		Workspace: ws.ID, Collection: col.ID, Actor: "t",
		JSONSchema: map[string]any{"type": "object", "properties": map[string]any{"a": map[string]any{"type": "string"}}},
		Activate:  true,
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	svc := record.New(pool, schema.NewValidator(reg))
	rec, err := svc.Create(ctx, record.CreateCmd{Workspace: ws.ID, Collection: col.ID, Actor: "t", Data: map[string]any{"a": "x"}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	var sv *int32
	if err := pool.QueryRow(ctx, `SELECT schema_version FROM events WHERE record_id=$1 AND type='create'`, rec.ID).Scan(&sv); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if sv == nil || *sv != 1 {
		t.Fatalf("event schema_version = %v, want 1", sv)
	}
}
```

- [ ] **Step 5: Build, run, commit**

`go build ./...`; `mise run test:integration -- -run TestCreateEventCarriesSchemaVersion ./internal/record/` (PASS); `mise run test:integration -- ./internal/record/` (full record suite, no regression).
```bash
git add internal/record/record.go internal/record/timetravel.go internal/record/event_version_test.go
git commit -m "feat: record events carry schema_version"
```

---

## Task 3: Pure defaults applier

**Files:** create `internal/projection/defaults.go`; test `internal/projection/defaults_test.go`.

- [ ] **Step 1: Write the failing test `internal/projection/defaults_test.go`**
```go
package projection

import "testing"

func TestApplyDefaults(t *testing.T) {
	schema := []byte(`{"type":"object","properties":{"a":{"type":"string"},"b":{"type":"string","default":"x"},"c":{"type":"integer","default":7}}}`)

	out, changed := applyDefaults(schema, map[string]any{"a": "hi"})
	if !changed {
		t.Fatal("expected changed=true")
	}
	if out["b"] != "x" {
		t.Fatalf("b = %v, want x", out["b"])
	}
	if got, ok := out["c"].(float64); !ok || got != 7 {
		t.Fatalf("c = %v, want 7", out["c"])
	}
	if out["a"] != "hi" {
		t.Fatalf("a mutated: %v", out["a"])
	}

	// Present field is not overwritten by a default.
	out2, _ := applyDefaults(schema, map[string]any{"b": "keep"})
	if out2["b"] != "keep" {
		t.Fatalf("b overwritten: %v", out2["b"])
	}

	// No defaults to apply -> unchanged.
	if _, changed := applyDefaults([]byte(`{"properties":{"a":{"type":"string"}}}`), map[string]any{"a": "x"}); changed {
		t.Fatal("expected changed=false")
	}

	// Garbage schema -> unchanged, no panic.
	if _, changed := applyDefaults([]byte(`not json`), map[string]any{"a": 1}); changed {
		t.Fatal("expected changed=false for bad schema")
	}
}
```

- [ ] **Step 2: Run (FAIL: undefined applyDefaults)**

`go test ./internal/projection/`

- [ ] **Step 3: Write `internal/projection/defaults.go`**
```go
// Package projection rebuilds and advances the records current-state projection.
package projection

import "encoding/json"

// applyDefaults returns data with the schema's top-level defaults filled in for any
// missing keys, plus whether anything changed. It copies-on-write: the input map is
// never mutated. Only top-level `properties[*].default` are applied (v0 scope).
func applyDefaults(schemaRaw []byte, data map[string]any) (map[string]any, bool) {
	var doc struct {
		Properties map[string]struct {
			Default json.RawMessage `json:"default"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(schemaRaw, &doc); err != nil || len(doc.Properties) == 0 {
		return data, false
	}
	out := data
	changed := false
	for name, prop := range doc.Properties {
		if len(prop.Default) == 0 {
			continue
		}
		if _, present := data[name]; present {
			continue
		}
		var v any
		if err := json.Unmarshal(prop.Default, &v); err != nil {
			continue
		}
		if !changed {
			out = make(map[string]any, len(data)+1)
			for k, val := range data {
				out[k] = val
			}
			changed = true
		}
		out[name] = v
	}
	return out, changed
}
```

- [ ] **Step 4: Run (PASS) + commit**

`go test ./internal/projection/` (PASS); `go build ./...`.
```bash
git add internal/projection/defaults.go internal/projection/defaults_test.go
git commit -m "feat: pure JSON Schema top-level defaults applier"
```

---

## Task 4: Replayer

**Files:** create `internal/projection/replay.go`; test `internal/projection/replay_test.go`.

- [ ] **Step 1: Write `internal/projection/replay.go`**
```go
package projection

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/substrate/substrate/internal/db"
)

// Replayer rebuilds the records projection from the authoritative events stream.
type Replayer struct {
	pool *pgxpool.Pool
	q    *db.Queries
}

func NewReplayer(pool *pgxpool.Pool) *Replayer { return &Replayer{pool: pool, q: db.New(pool)} }

// RebuildRecord reconstructs a single record's projection row from its latest event.
// Returns false when the record has no events. Idempotent.
func (r *Replayer) RebuildRecord(ctx context.Context, ws, col, id uuid.UUID) (bool, error) {
	ev, err := r.q.GetLatestRecordEvent(ctx, db.GetLatestRecordEventParams{
		WorkspaceID: ws, CollectionID: col, RecordID: id,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("latest event: %w", err)
	}
	status := "active"
	if ev.Type == "delete" {
		status = "deleted"
	}
	data := ev.StateAfter
	if len(data) == 0 {
		data = []byte("{}")
	}
	if err := r.q.UpsertRecordProjection(ctx, db.UpsertRecordProjectionParams{
		ID: id, CollectionID: col, WorkspaceID: ws,
		Data: data, Revision: ev.Revision, Status: status,
		Actor: ev.Actor, SchemaVersion: ev.SchemaVersion,
	}); err != nil {
		return false, fmt.Errorf("upsert projection: %w", err)
	}
	return true, nil
}

// RebuildCollection rebuilds every record in a collection from its events and returns
// the number rebuilt. Lifecycle pseudo-events (record_id = collection_id) are excluded
// by the underlying query.
func (r *Replayer) RebuildCollection(ctx context.Context, ws, col uuid.UUID) (int, error) {
	ids, err := r.q.ListRecordIDsInCollection(ctx, db.ListRecordIDsInCollectionParams{
		WorkspaceID: ws, CollectionID: col,
	})
	if err != nil {
		return 0, fmt.Errorf("list record ids: %w", err)
	}
	n := 0
	for _, id := range ids {
		ok, err := r.RebuildRecord(ctx, ws, col, id)
		if err != nil {
			return n, err
		}
		if ok {
			n++
		}
	}
	return n, nil
}
```

- [ ] **Step 2: Write the failing test `internal/projection/replay_test.go`**
```go
//go:build integration

package projection_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/substrate/substrate/internal/db"
	"github.com/substrate/substrate/internal/projection"
	"github.com/substrate/substrate/internal/store"
)

func TestReplayRebuildsProjection(t *testing.T) {
	pool := store.NewTestPool(t)
	ctx := context.Background()
	q := db.New(pool)
	ws, _ := q.CreateWorkspace(ctx, db.CreateWorkspaceParams{ID: uuid.New(), Name: "w", PolicyMode: "allow"})
	col, _ := q.CreateCollection(ctx, db.CreateCollectionParams{ID: uuid.New(), WorkspaceID: ws.ID, Name: "c", Level: "flexible"})
	rid := uuid.New()

	appendEv := func(typ string, rev int64, state string) {
		if err := q.AppendEvent(ctx, db.AppendEventParams{
			ID: uuid.New(), WorkspaceID: ws.ID, CollectionID: col.ID, RecordID: rid,
			Type: typ, Revision: rev, StateAfter: []byte(state),
			SchemaVersion: pgtype.Int4{Int32: 1, Valid: true},
		}); err != nil {
			t.Fatalf("append %s: %v", typ, err)
		}
	}
	appendEv("create", 1, `{"a":"1"}`)
	appendEv("update", 2, `{"a":"2"}`)
	// A lifecycle pseudo-event on the collection id must be ignored by RebuildCollection.
	if err := q.AppendEvent(ctx, db.AppendEventParams{
		ID: uuid.New(), WorkspaceID: ws.ID, CollectionID: col.ID, RecordID: col.ID,
		Type: "schema_registered", Revision: 1, StateAfter: []byte(`{}`),
	}); err != nil {
		t.Fatalf("lifecycle: %v", err)
	}

	rep := projection.NewReplayer(pool)
	ok, err := rep.RebuildRecord(ctx, ws.ID, col.ID, rid)
	if err != nil || !ok {
		t.Fatalf("rebuild: ok=%v err=%v", ok, err)
	}
	var data []byte
	var rev int64
	var status string
	if err := pool.QueryRow(ctx, `SELECT data, revision, status FROM records WHERE collection_id=$1 AND id=$2`, col.ID, rid).
		Scan(&data, &rev, &status); err != nil {
		t.Fatalf("read projection: %v", err)
	}
	if rev != 2 || status != "active" || string(data) == "" {
		t.Fatalf("projection rev=%d status=%s data=%s", rev, status, data)
	}

	// Collection rebuild counts exactly the one real record (not the lifecycle row).
	n, err := rep.RebuildCollection(ctx, ws.ID, col.ID)
	if err != nil || n != 1 {
		t.Fatalf("rebuild collection n=%d err=%v", n, err)
	}

	// Idempotent: a delete event flips status to deleted on re-run.
	appendEv("delete", 3, `{"a":"2"}`)
	if _, err := rep.RebuildRecord(ctx, ws.ID, col.ID, rid); err != nil {
		t.Fatalf("rebuild2: %v", err)
	}
	_ = pool.QueryRow(ctx, `SELECT status FROM records WHERE collection_id=$1 AND id=$2`, col.ID, rid).Scan(&status)
	if status != "deleted" {
		t.Fatalf("status = %s, want deleted", status)
	}

	// Unknown record -> not rebuilt.
	if ok, _ := rep.RebuildRecord(ctx, ws.ID, col.ID, uuid.New()); ok {
		t.Fatal("expected ok=false for record with no events")
	}
}
```

- [ ] **Step 3: Build, run, commit**

`go build ./...`; `mise run test:integration -- -run TestReplay ./internal/projection/` (PASS).
```bash
git add internal/projection/replay.go internal/projection/replay_test.go
git commit -m "feat: event-stream replay to rebuild the records projection"
```

---

## Task 5: Backfiller

**Files:** create `internal/projection/backfill.go`; test `internal/projection/backfill_test.go`.

- [ ] **Step 1: Write `internal/projection/backfill.go`**
```go
package projection

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/substrate/substrate/internal/apierr"
	"github.com/substrate/substrate/internal/db"
	"github.com/substrate/substrate/internal/schema"
	"github.com/substrate/substrate/internal/store"
)

const defaultBatch = 200

// SchemaResolver is the slice of the schema registry the backfiller needs.
type SchemaResolver interface {
	GetActive(ctx context.Context, col uuid.UUID) (schema.ActiveSchema, error)
}

// Report summarizes a backfill run.
type Report struct {
	Scanned   int `json:"scanned"`
	Migrated  int `json:"migrated"`
	Skipped   int `json:"skipped"`
	Remaining int `json:"remaining"`
}

// Backfiller advances active records toward a collection's active schema version.
type Backfiller struct {
	pool   *pgxpool.Pool
	q      *db.Queries
	schema SchemaResolver
}

func NewBackfiller(pool *pgxpool.Pool, sr SchemaResolver) *Backfiller {
	return &Backfiller{pool: pool, q: db.New(pool), schema: sr}
}

// Run advances every active record below the active version, in bounded keyset batches,
// applying schema defaults and re-validating. Invalid records are skipped (never altered).
func (b *Backfiller) Run(ctx context.Context, ws, col uuid.UUID, batch int) (Report, error) {
	var rep Report
	active, err := b.schema.GetActive(ctx, col)
	if err != nil {
		if e, ok := apierr.As(err); ok && e.Code == apierr.NotFound {
			return rep, nil // flexible collection: nothing to advance
		}
		return rep, fmt.Errorf("get active schema: %w", err)
	}
	compiled, err := compileSchema(active.Raw)
	if err != nil {
		return rep, err
	}
	if batch <= 0 || batch > defaultBatch {
		batch = defaultBatch
	}
	activeVer := int32(active.Version)

	after := uuid.UUID{} // keyset cursor by id; uuid.Nil starts before all ids
	for {
		rows, err := b.q.ListRecordsBelowVersion(ctx, db.ListRecordsBelowVersionParams{
			CollectionID: col, SchemaVersion: activeVer, ID: after, Limit: int32(batch),
		})
		if err != nil {
			return rep, fmt.Errorf("list below version: %w", err)
		}
		if len(rows) == 0 {
			break
		}
		for _, row := range rows {
			rep.Scanned++
			migrated, err := b.migrateOne(ctx, ws, col, row.ID, activeVer, active.Raw, compiled)
			if err != nil {
				return rep, err
			}
			if migrated {
				rep.Migrated++
			} else {
				rep.Skipped++
			}
			after = row.ID
		}
		if len(rows) < batch {
			break
		}
	}

	remaining, err := b.q.CountRecordsBelowVersion(ctx, db.CountRecordsBelowVersionParams{
		CollectionID: col, SchemaVersion: activeVer,
	})
	if err != nil {
		return rep, fmt.Errorf("count remaining: %w", err)
	}
	rep.Remaining = int(remaining)
	return rep, nil
}

func (b *Backfiller) migrateOne(ctx context.Context, ws, col, id uuid.UUID, activeVer int32, schemaRaw []byte, compiled *jsonschema.Schema) (bool, error) {
	var migrated bool
	err := store.WithTx(ctx, b.pool, func(tx pgx.Tx) error {
		qtx := b.q.WithTx(tx)
		row, err := qtx.GetRecordForUpdate(ctx, db.GetRecordForUpdateParams{WorkspaceID: ws, CollectionID: col, ID: id})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // deleted/gone since the scan
		}
		if err != nil {
			return err
		}
		if row.SchemaVersion.Valid && row.SchemaVersion.Int32 >= activeVer {
			return nil // advanced by a concurrent write
		}
		var data map[string]any
		if err := json.Unmarshal(row.Data, &data); err != nil {
			return fmt.Errorf("decode data: %w", err)
		}
		if data == nil {
			data = map[string]any{}
		}
		migratedData, _ := applyDefaults(schemaRaw, data)
		if err := compiled.Validate(migratedData); err != nil {
			return nil // invalid under active schema: skip, leave untouched
		}
		next := row.Revision + 1
		raw, err := json.Marshal(migratedData)
		if err != nil {
			return fmt.Errorf("encode migrated: %w", err)
		}
		sysActor := pgtype.Text{String: "system:backfill", Valid: true}
		sv := pgtype.Int4{Int32: activeVer, Valid: true}
		if err := qtx.AppendEvent(ctx, db.AppendEventParams{
			ID: uuid.New(), WorkspaceID: ws, CollectionID: col, RecordID: id,
			Type: "migration", Revision: next, StateAfter: raw,
			Actor: sysActor, IdempotencyKey: pgtype.Text{}, Trace: nil, SchemaVersion: sv,
		}); err != nil {
			return fmt.Errorf("append migration event: %w", err)
		}
		if err := qtx.UpdateRecordData(ctx, db.UpdateRecordDataParams{
			WorkspaceID: ws, CollectionID: col, ID: id,
			Data: raw, Revision: next, Actor: sysActor, SchemaVersion: sv,
		}); err != nil {
			return fmt.Errorf("update record: %w", err)
		}
		migrated = true
		return nil
	})
	return migrated, err
}

func compileSchema(raw []byte) (*jsonschema.Schema, error) {
	parsed, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("parse active schema: %w", err)
	}
	c := jsonschema.NewCompiler()
	const id = "substrate://backfill.json"
	if err := c.AddResource(id, parsed); err != nil {
		return nil, fmt.Errorf("add active schema: %w", err)
	}
	sch, err := c.Compile(id)
	if err != nil {
		return nil, fmt.Errorf("compile active schema: %w", err)
	}
	return sch, nil
}
```

- [ ] **Step 2: Write the failing test `internal/projection/backfill_test.go`**
```go
//go:build integration

package projection_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/substrate/substrate/internal/db"
	"github.com/substrate/substrate/internal/projection"
	"github.com/substrate/substrate/internal/record"
	"github.com/substrate/substrate/internal/schema"
	"github.com/substrate/substrate/internal/store"
)

func TestBackfillAppliesDefaultsAndReStamps(t *testing.T) {
	pool := store.NewTestPool(t)
	ctx := context.Background()
	q := db.New(pool)
	ws, _ := q.CreateWorkspace(ctx, db.CreateWorkspaceParams{ID: uuid.New(), Name: "w", PolicyMode: "allow"})
	col, _ := q.CreateCollection(ctx, db.CreateCollectionParams{ID: uuid.New(), WorkspaceID: ws.ID, Name: "c", Level: "flexible"})

	reg := schema.New(pool)
	mustObj := map[string]any{"type": "object", "properties": map[string]any{"a": map[string]any{"type": "string"}}}
	if _, err := reg.Register(ctx, schema.RegisterCmd{Workspace: ws.ID, Collection: col.ID, Actor: "t", JSONSchema: mustObj, Activate: true}); err != nil {
		t.Fatalf("v1: %v", err)
	}
	recs := record.New(pool, schema.NewValidator(reg))
	r1, _ := recs.Create(ctx, record.CreateCmd{Workspace: ws.ID, Collection: col.ID, Actor: "t", Data: map[string]any{"a": "x"}})
	recs.Create(ctx, record.CreateCmd{Workspace: ws.ID, Collection: col.ID, Actor: "t", Data: map[string]any{"a": "y"}})

	// Non-breaking v2: adds optional field b with a default.
	v2 := map[string]any{"type": "object", "properties": map[string]any{
		"a": map[string]any{"type": "string"},
		"b": map[string]any{"type": "string", "default": "filled"},
	}}
	if _, err := reg.Register(ctx, schema.RegisterCmd{Workspace: ws.ID, Collection: col.ID, Actor: "t", JSONSchema: v2, Activate: true}); err != nil {
		t.Fatalf("v2: %v", err)
	}

	bf := projection.NewBackfiller(pool, reg)
	rep, err := bf.Run(ctx, ws.ID, col.ID, 0)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if rep.Migrated != 2 || rep.Skipped != 0 || rep.Remaining != 0 {
		t.Fatalf("report = %+v, want migrated=2 skipped=0 remaining=0", rep)
	}

	var data []byte
	var rev int64
	var sv *int32
	if err := pool.QueryRow(ctx, `SELECT data, revision, schema_version FROM records WHERE id=$1`, r1.ID).Scan(&data, &rev, &sv); err != nil {
		t.Fatalf("read: %v", err)
	}
	if rev != 2 || sv == nil || *sv != 2 {
		t.Fatalf("rev=%d sv=%v, want rev=2 sv=2", rev, sv)
	}
	if !contains(string(data), `"b":"filled"`) {
		t.Fatalf("data missing default: %s", data)
	}
	// A migration event was recorded.
	var n int
	pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE record_id=$1 AND type='migration'`, r1.ID).Scan(&n)
	if n != 1 {
		t.Fatalf("migration events = %d, want 1", n)
	}

	// Second run is a no-op.
	rep2, _ := bf.Run(ctx, ws.ID, col.ID, 0)
	if rep2.Migrated != 0 {
		t.Fatalf("second run migrated = %d, want 0", rep2.Migrated)
	}

	// A record invalid under v2 (a must be a string) is skipped, not corrupted.
	bad := uuid.New()
	if err := q.UpsertRecordProjection(ctx, db.UpsertRecordProjectionParams{
		ID: bad, CollectionID: col.ID, WorkspaceID: ws.ID, Data: []byte(`{"a":123}`),
		Revision: 1, Status: "active", SchemaVersion: pgtype.Int4{Int32: 1, Valid: true},
	}); err != nil {
		t.Fatalf("insert bad: %v", err)
	}
	rep3, _ := bf.Run(ctx, ws.ID, col.ID, 0)
	if rep3.Skipped != 1 || rep3.Remaining != 1 {
		t.Fatalf("report3 = %+v, want skipped=1 remaining=1", rep3)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	}())
}

func TestBackfillFlexibleNoop(t *testing.T) {
	pool := store.NewTestPool(t)
	ctx := context.Background()
	q := db.New(pool)
	ws, _ := q.CreateWorkspace(ctx, db.CreateWorkspaceParams{ID: uuid.New(), Name: "w", PolicyMode: "allow"})
	col, _ := q.CreateCollection(ctx, db.CreateCollectionParams{ID: uuid.New(), WorkspaceID: ws.ID, Name: "c", Level: "flexible"})
	rep, err := projection.NewBackfiller(pool, schema.New(pool)).Run(ctx, ws.ID, col.ID, 0)
	if err != nil || rep.Migrated != 0 || rep.Scanned != 0 {
		t.Fatalf("flexible run = %+v err=%v", rep, err)
	}
}
```
(If a `contains`/`strings.Contains` style helper already exists in the package's test files, use `strings.Contains` instead and drop the local helper.)

- [ ] **Step 3: Build, run, commit**

`go build ./...`; `mise run test:integration -- -run TestBackfill ./internal/projection/` (PASS).
```bash
git add internal/projection/backfill.go internal/projection/backfill_test.go
git commit -m "feat: backfiller advances records to active schema (defaults + re-validate)"
```

---

## Task 6: Collection auto_backfill (service + API)

**Files:** modify `internal/collection/collection.go` (Collection.AutoBackfill, GetByName, SetAutoBackfill), `internal/api/handlers.go` (createCollection accepts auto_backfill); test `internal/collection/auto_backfill_test.go`.

- [ ] **Step 1: Collection service**

In `internal/collection/collection.go`:
- Add `AutoBackfill bool `json:"auto_backfill"`` to the `Collection` struct.
- In `GetByName`, set `AutoBackfill: row.AutoBackfill` on the returned `Collection` (the row now has it from Task 1).
- Add:
```go
// SetAutoBackfill toggles a collection's opt-in auto-backfill flag.
func (s *Service) SetAutoBackfill(ctx context.Context, ws, col uuid.UUID, enabled bool) error {
	if err := s.q.SetAutoBackfill(ctx, db.SetAutoBackfillParams{WorkspaceID: ws, ID: col, AutoBackfill: enabled}); err != nil {
		return fmt.Errorf("set auto_backfill: %w", err)
	}
	return nil
}
```
Ensure `fmt` is imported (it is).

- [ ] **Step 2: createCollection handler accepts auto_backfill**

In `internal/api/handlers.go` `createCollection`, extend the body and toggle after create:
```go
	var body struct {
		Name         string `json:"name"`
		Level        string `json:"level"`
		AutoBackfill bool   `json:"auto_backfill"`
	}
	// ... decode + Create as today ...
	if body.AutoBackfill {
		if err := h.collections.SetAutoBackfill(r.Context(), c.WorkspaceID, c.ID, true); err != nil {
			writeErr(w, err)
			return
		}
		c.AutoBackfill = true
	}
	httpx.JSON(w, http.StatusCreated, c)
```

- [ ] **Step 3: Write the failing test `internal/collection/auto_backfill_test.go`**
```go
//go:build integration

package collection_test

import (
	"context"
	"testing"

	"github.com/substrate/substrate/internal/collection"
	"github.com/substrate/substrate/internal/db"
	"github.com/substrate/substrate/internal/store"

	"github.com/google/uuid"
)

func TestSetAutoBackfill(t *testing.T) {
	pool := store.NewTestPool(t)
	ctx := context.Background()
	q := db.New(pool)
	ws, _ := q.CreateWorkspace(ctx, db.CreateWorkspaceParams{ID: uuid.New(), Name: "w", PolicyMode: "allow"})
	svc := collection.New(pool)
	c, err := svc.Create(ctx, ws.ID, "things")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if c.AutoBackfill {
		t.Fatal("default auto_backfill should be false")
	}
	if err := svc.SetAutoBackfill(ctx, ws.ID, c.ID, true); err != nil {
		t.Fatalf("set: %v", err)
	}
	got, err := svc.GetByName(ctx, ws.ID, "things")
	if err != nil || !got.AutoBackfill {
		t.Fatalf("GetByName auto_backfill=%v err=%v", got.AutoBackfill, err)
	}
}
```

- [ ] **Step 4: Build, run, commit**

`go build ./...`; `mise run test:integration -- -run TestSetAutoBackfill ./internal/collection/` (PASS); `mise run test:integration -- ./internal/api/` (no regression in collection create).
```bash
git add internal/collection/collection.go internal/api/handlers.go internal/collection/auto_backfill_test.go
git commit -m "feat: collection auto_backfill flag (service + create body)"
```

---

## Task 7: Backfill worker + schema enqueuer seam

**Files:** create `internal/projection/worker.go`; modify `internal/schema/registry.go` (BackfillEnqueuer seam + enqueue on activation); test `internal/schema/auto_backfill_test.go`.

- [ ] **Step 1: Write `internal/projection/worker.go`**
```go
package projection

import (
	"context"

	"github.com/google/uuid"
)

type backfillJob struct{ ws, col uuid.UUID }

// Worker runs backfills asynchronously off a buffered queue. It satisfies
// schema.BackfillEnqueuer.
type Worker struct {
	bf *Backfiller
	ch chan backfillJob
}

func NewWorker(bf *Backfiller, buffer int) *Worker {
	if buffer <= 0 {
		buffer = 256
	}
	return &Worker{bf: bf, ch: make(chan backfillJob, buffer)}
}

// Enqueue is non-blocking. A full buffer drops the signal; the next activation or a
// manual run still converges (backfill is idempotent).
func (w *Worker) Enqueue(ws, col uuid.UUID) {
	select {
	case w.ch <- backfillJob{ws: ws, col: col}:
	default:
	}
}

// Run drains the queue until ctx is cancelled, backfilling each collection to completion.
func (w *Worker) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case job := <-w.ch:
			_, _ = w.bf.Run(ctx, job.ws, job.col, defaultBatch)
		}
	}
}
```

- [ ] **Step 2: Add the enqueuer seam to `internal/schema/registry.go`**

- Add the interface + field + chainable setter:
```go
// BackfillEnqueuer is notified when a collection's active version changes and the
// collection has opted into auto-backfill.
type BackfillEnqueuer interface {
	Enqueue(workspace, collection uuid.UUID)
}

func (s *Service) WithBackfillEnqueuer(e BackfillEnqueuer) *Service { s.backfill = e; return s }
```
Add `backfill BackfillEnqueuer` to the `Service` struct.
- Add the helper:
```go
// enqueueBackfill signals the worker after an activation if the collection opted in.
func (s *Service) enqueueBackfill(ctx context.Context, ws, col uuid.UUID) {
	if s.backfill == nil {
		return
	}
	on, err := s.q.GetCollectionAutoBackfill(ctx, col)
	if err != nil || !on {
		return
	}
	s.backfill.Enqueue(ws, col)
}
```
- Call it post-commit, right after the existing `ensureActiveIndexes` calls:
  - In `Register`, the block that runs when `result.Lifecycle == "active"` (after `s.ensureActiveIndexes(ctx, cmd.Collection)`): add `s.enqueueBackfill(ctx, cmd.Workspace, cmd.Collection)`.
  - In `Activate`, after `return s.ensureActiveIndexes(ctx, col)` — restructure so both run: capture the index error, then enqueue, then return. e.g.:
    ```go
    if err := s.ensureActiveIndexes(ctx, col); err != nil {
        return err
    }
    s.enqueueBackfill(ctx, ws, col)
    return nil
    ```
  Read the current `Register`/`Activate` post-commit tails and adapt precisely (keep the existing error handling intact).

- [ ] **Step 3: Write the failing test `internal/schema/auto_backfill_test.go`**
```go
//go:build integration

package schema_test

import (
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/substrate/substrate/internal/db"
	"github.com/substrate/substrate/internal/schema"
	"github.com/substrate/substrate/internal/store"
)

type fakeEnqueuer struct {
	mu    sync.Mutex
	calls []uuid.UUID
}

func (f *fakeEnqueuer) Enqueue(ws, col uuid.UUID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, col)
}

func TestEnqueueOnActivationWhenOptedIn(t *testing.T) {
	pool := store.NewTestPool(t)
	ctx := context.Background()
	q := db.New(pool)
	ws, _ := q.CreateWorkspace(ctx, db.CreateWorkspaceParams{ID: uuid.New(), Name: "w", PolicyMode: "allow"})
	col, _ := q.CreateCollection(ctx, db.CreateCollectionParams{ID: uuid.New(), WorkspaceID: ws.ID, Name: "c", Level: "flexible"})

	fe := &fakeEnqueuer{}
	reg := schema.New(pool).WithBackfillEnqueuer(fe)
	obj := map[string]any{"type": "object", "properties": map[string]any{"a": map[string]any{"type": "string"}}}

	// auto_backfill OFF -> no enqueue on activation.
	if _, err := reg.Register(ctx, schema.RegisterCmd{Workspace: ws.ID, Collection: col.ID, Actor: "t", JSONSchema: obj, Activate: true}); err != nil {
		t.Fatalf("v1: %v", err)
	}
	if len(fe.calls) != 0 {
		t.Fatalf("unexpected enqueue while opted out: %v", fe.calls)
	}

	// Opt in, activate a v2 -> enqueue once.
	if err := q.SetAutoBackfill(ctx, db.SetAutoBackfillParams{WorkspaceID: ws.ID, ID: col.ID, AutoBackfill: true}); err != nil {
		t.Fatalf("opt-in: %v", err)
	}
	v2 := map[string]any{"type": "object", "properties": map[string]any{
		"a": map[string]any{"type": "string"}, "b": map[string]any{"type": "string", "default": "x"},
	}}
	if _, err := reg.Register(ctx, schema.RegisterCmd{Workspace: ws.ID, Collection: col.ID, Actor: "t", JSONSchema: v2, Activate: true}); err != nil {
		t.Fatalf("v2: %v", err)
	}
	if len(fe.calls) != 1 || fe.calls[0] != col.ID {
		t.Fatalf("expected one enqueue for col, got %v", fe.calls)
	}
}
```

- [ ] **Step 4: Build, run, commit**

`go build ./...`; `mise run test:integration -- -run TestEnqueueOnActivation ./internal/schema/` (PASS); `mise run test:integration -- ./internal/schema/` (no regression).
```bash
git add internal/projection/worker.go internal/schema/registry.go internal/schema/auto_backfill_test.go
git commit -m "feat: async backfill worker + schema activation enqueue seam"
```

---

## Task 8: API endpoints (manual backfill, auto-backfill toggle, admin replay)

**Files:** create `internal/api/projection_handlers.go`; modify `internal/api/router.go` (Deps + routes), `internal/api/handlers.go` (adminHandlers.replayer field); test `internal/api/projection_api_test.go`.

- [ ] **Step 1: Add the replayer field to adminHandlers**

In `internal/api/handlers.go`, add `replayer *projection.Replayer` to the `adminHandlers` struct (import `"github.com/substrate/substrate/internal/projection"`).

- [ ] **Step 2: Write `internal/api/projection_handlers.go`**
```go
package api

import (
	"encoding/json"
	"net/http"

	"github.com/google/uuid"

	"github.com/substrate/substrate/internal/apierr"
	"github.com/substrate/substrate/internal/httpx"
	"github.com/substrate/substrate/internal/projection"
)

type projectionHandlers struct {
	h          *handlers
	backfiller *projection.Backfiller
}

func (p *projectionHandlers) backfill(w http.ResponseWriter, r *http.Request) {
	c, err := p.h.resolveCollection(r, r.PathValue("collection"))
	if err != nil {
		writeErr(w, err)
		return
	}
	rep, err := p.backfiller.Run(r.Context(), c.WorkspaceID, c.ID, 0)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, rep)
}

func (p *projectionHandlers) setAutoBackfill(w http.ResponseWriter, r *http.Request) {
	c, err := p.h.resolveCollection(r, r.PathValue("collection"))
	if err != nil {
		writeErr(w, err)
		return
	}
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, apierr.New(apierr.BadRequest, "invalid json"))
		return
	}
	if err := p.h.collections.SetAutoBackfill(r.Context(), c.WorkspaceID, c.ID, body.Enabled); err != nil {
		writeErr(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"collection": c.Name, "auto_backfill": body.Enabled})
}

// replay is admin-token gated (method on adminHandlers).
func (a *adminHandlers) replay(w http.ResponseWriter, r *http.Request) {
	if !a.authed(r) {
		writeErr(w, apierr.New(apierr.Unauthorized, "invalid admin token"))
		return
	}
	var body struct {
		WorkspaceID  string `json:"workspace_id"`
		CollectionID string `json:"collection_id"`
		RecordID     string `json:"record_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, apierr.New(apierr.BadRequest, "invalid json"))
		return
	}
	ws, err := uuid.Parse(body.WorkspaceID)
	if err != nil {
		writeErr(w, apierr.New(apierr.BadRequest, "invalid workspace_id"))
		return
	}
	col, err := uuid.Parse(body.CollectionID)
	if err != nil {
		writeErr(w, apierr.New(apierr.BadRequest, "invalid collection_id"))
		return
	}
	if body.RecordID != "" {
		id, err := uuid.Parse(body.RecordID)
		if err != nil {
			writeErr(w, apierr.New(apierr.BadRequest, "invalid record_id"))
			return
		}
		ok, err := a.replayer.RebuildRecord(r.Context(), ws, col, id)
		if err != nil {
			writeErr(w, err)
			return
		}
		n := 0
		if ok {
			n = 1
		}
		httpx.JSON(w, http.StatusOK, map[string]any{"rebuilt": n})
		return
	}
	n, err := a.replayer.RebuildCollection(r.Context(), ws, col)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"rebuilt": n})
}
```

- [ ] **Step 3: Wire `internal/api/router.go`**

- Add to `Deps`: `Backfiller *projection.Backfiller` and `Replayer *projection.Replayer` (import the projection package).
- In the `adminHandlers` literal, add `replayer: d.Replayer`.
- After the policy/audit routes on the inner `api` mux:
```go
	pjh := &projectionHandlers{h: h, backfiller: d.Backfiller}
	api.HandleFunc("POST /v1/collections/{collection}/backfill", pjh.backfill)
	api.HandleFunc("POST /v1/collections/{collection}/auto-backfill", pjh.setAutoBackfill)
```
- Alongside the admin routes on the outer `mux`:
```go
	mux.HandleFunc("POST /admin/replay", admin.replay)
```

- [ ] **Step 4: Write the HTTP test `internal/api/projection_api_test.go`**

Defines a `newProjServer` harness (the existing `newGovServer` doesn't wire Schemas/Backfiller/Replayer). Reuses `doAs` from `policy_api_test.go`.
```go
//go:build integration

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/substrate/substrate/internal/collection"
	"github.com/substrate/substrate/internal/projection"
	"github.com/substrate/substrate/internal/query"
	"github.com/substrate/substrate/internal/record"
	"github.com/substrate/substrate/internal/schema"
	"github.com/substrate/substrate/internal/store"
	"github.com/substrate/substrate/internal/workspace"
)

func newProjServer(t *testing.T) (*httptest.Server, string, string, uuid.UUID) {
	t.Helper()
	pool := store.NewTestPool(t)
	wsSvc := workspace.New(pool)
	w, err := wsSvc.CreateWorkspace(t.Context(), "acme")
	if err != nil {
		t.Fatalf("ws: %v", err)
	}
	key, _, err := wsSvc.CreateAPIKey(t.Context(), w.ID, "test")
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	const adminToken = "admin-secret"
	reg := schema.NewWithIndexer(pool, query.NewIndexer(pool))
	srv := httptest.NewServer(NewRouter(Deps{
		Workspaces:  wsSvc,
		Collections: collection.New(pool),
		Records:     record.New(pool, schema.NewValidator(reg)),
		Schemas:     reg,
		Backfiller:  projection.NewBackfiller(pool, reg),
		Replayer:    projection.NewReplayer(pool),
		AdminToken:  adminToken,
	}))
	t.Cleanup(srv.Close)
	return srv, key, adminToken, w.ID
}

func TestBackfillAndReplayOverHTTP(t *testing.T) {
	srv, key, adminToken, ws := newProjServer(t)

	// Collection.
	resp := doAs(t, "POST", srv.URL+"/v1/collections", key, "agent-1", map[string]any{"name": "orders"})
	var coll struct {
		ID string `json:"id"`
	}
	json.NewDecoder(resp.Body).Decode(&coll)
	resp.Body.Close()

	// v1 schema (active).
	resp = doAs(t, "POST", srv.URL+"/v1/collections/orders/schemas", key, "agent-1", map[string]any{
		"json_schema": map[string]any{"type": "object", "properties": map[string]any{"a": map[string]any{"type": "string"}}},
		"activate":    true,
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("v1 = %d", resp.StatusCode)
	}
	resp.Body.Close()

	// A record on v1.
	resp = doAs(t, "POST", srv.URL+"/v1/collections/orders/records", key, "agent-1", map[string]any{"data": map[string]any{"a": "x"}})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("record = %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Non-breaking v2 with a defaulted field (active).
	resp = doAs(t, "POST", srv.URL+"/v1/collections/orders/schemas", key, "agent-1", map[string]any{
		"json_schema": map[string]any{"type": "object", "properties": map[string]any{
			"a": map[string]any{"type": "string"}, "b": map[string]any{"type": "string", "default": "filled"},
		}},
		"activate": true,
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("v2 = %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Manual backfill.
	resp = doAs(t, "POST", srv.URL+"/v1/collections/orders/backfill", key, "agent-1", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("backfill = %d", resp.StatusCode)
	}
	var rep struct{ Migrated, Skipped, Remaining int }
	json.NewDecoder(resp.Body).Decode(&rep)
	resp.Body.Close()
	if rep.Migrated != 1 || rep.Remaining != 0 {
		t.Fatalf("report = %+v", rep)
	}

	// migration event visible in audit? (audit not wired here) -> check via list: defaulted field present.
	resp = doAs(t, "GET", srv.URL+"/v1/collections/orders/records", key, "agent-1", nil)
	var page struct {
		Items []struct {
			Data map[string]any `json:"data"`
		} `json:"items"`
	}
	json.NewDecoder(resp.Body).Decode(&page)
	resp.Body.Close()
	if len(page.Items) != 1 || page.Items[0].Data["b"] != "filled" {
		t.Fatalf("list after backfill = %+v", page.Items)
	}

	// Admin replay of the whole collection.
	req := mustJSONReq(t, "POST", srv.URL+"/admin/replay", map[string]any{"workspace_id": ws.String(), "collection_id": coll.ID})
	req.Header.Set("X-Admin-Token", adminToken)
	rresp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	var rr struct{ Rebuilt int }
	json.NewDecoder(rresp.Body).Decode(&rr)
	rresp.Body.Close()
	if rresp.StatusCode != http.StatusOK || rr.Rebuilt != 1 {
		t.Fatalf("replay status=%d rebuilt=%d", rresp.StatusCode, rr.Rebuilt)
	}

	// Auto-backfill toggle.
	resp = doAs(t, "POST", srv.URL+"/v1/collections/orders/auto-backfill", key, "agent-1", map[string]any{"enabled": true})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("toggle = %d", resp.StatusCode)
	}
	resp.Body.Close()
}
```
Add a small request helper to this file (the existing `doAs` doesn't set the admin header). Import `"bytes"`:
```go
func mustJSONReq(t *testing.T, method, url string, body any) *http.Request {
	t.Helper()
	b, _ := json.Marshal(body)
	req, err := http.NewRequest(method, url, bytes.NewReader(b))
	if err != nil {
		t.Fatalf("req: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	return req
}
```

- [ ] **Step 5: Build, run, commit**

`go build ./...`; `mise run test:integration -- ./internal/api/` (PASS — new test + no regression).
```bash
git add internal/api/projection_handlers.go internal/api/router.go internal/api/handlers.go internal/api/projection_api_test.go
git commit -m "feat: manual backfill + auto-backfill toggle + admin replay endpoints"
```

---

## Task 9: Production wiring + end-to-end test

**Files:** modify `cmd/substrate/main.go`; test `internal/api/projection_e2e_test.go`.

- [ ] **Step 1: Wire `cmd/substrate/main.go`**

Add import `"github.com/substrate/substrate/internal/projection"`. After the existing `engine`/`schemaReg` wiring:
```go
	backfiller := projection.NewBackfiller(pool, schemaReg)
	worker := projection.NewWorker(backfiller, 256)
	go worker.Run(ctx)
	schemaReg.WithBackfillEnqueuer(worker)

	router := api.NewRouter(api.Deps{
		Workspaces:  workspace.New(pool),
		Collections: collection.New(pool),
		Records:     record.New(pool, schema.NewValidator(schemaReg)).WithEvaluator(engine),
		Schemas:     schemaReg,
		Policies:    policy.NewService(pool),
		Audit:       audit.New(pool),
		Backfiller:  backfiller,
		Replayer:    projection.NewReplayer(pool),
		AdminToken:  cfg.AdminToken,
	})
```
(`ctx` is the `context.Background()` already created in `main`; the worker runs for the process lifetime.)

- [ ] **Step 2: Write the e2e test `internal/api/projection_e2e_test.go`**

Exercises the full HTTP flow: create a collection with `auto_backfill` on, register v1, write a record, activate a defaulted v2, then drive the migration via the manual endpoint (the same `Backfiller` the worker uses). Async-worker behavior is covered by `internal/schema/auto_backfill_test.go` (enqueue) + `internal/projection/backfill_test.go` (advance), so this e2e does not depend on worker timing.
```go
//go:build integration

package api

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestProjectionSurfaceEndToEnd(t *testing.T) {
	srv, key, _, _ := newProjServer(t)

	resp := doAs(t, "POST", srv.URL+"/v1/collections", key, "agent-1", map[string]any{"name": "orders", "auto_backfill": true})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("collection = %d", resp.StatusCode)
	}
	var coll struct {
		AutoBackfill bool `json:"auto_backfill"`
	}
	json.NewDecoder(resp.Body).Decode(&coll)
	resp.Body.Close()
	if !coll.AutoBackfill {
		t.Fatalf("auto_backfill not set at create: %+v", coll)
	}

	resp = doAs(t, "POST", srv.URL+"/v1/collections/orders/schemas", key, "agent-1", map[string]any{
		"json_schema": map[string]any{"type": "object", "properties": map[string]any{"a": map[string]any{"type": "string"}}},
		"activate":    true,
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("schema = %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = doAs(t, "POST", srv.URL+"/v1/collections/orders/records", key, "agent-1", map[string]any{"data": map[string]any{"a": "x"}})
	resp.Body.Close()

	// Activate a defaulted v2; with auto_backfill on this would enqueue in production.
	resp = doAs(t, "POST", srv.URL+"/v1/collections/orders/schemas", key, "agent-1", map[string]any{
		"json_schema": map[string]any{"type": "object", "properties": map[string]any{
			"a": map[string]any{"type": "string"}, "b": map[string]any{"type": "string", "default": "filled"},
		}},
		"activate": true,
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("v2 = %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Manual backfill drives the same Backfiller the worker would.
	resp = doAs(t, "POST", srv.URL+"/v1/collections/orders/backfill", key, "agent-1", nil)
	var rep struct{ Migrated int }
	json.NewDecoder(resp.Body).Decode(&rep)
	resp.Body.Close()
	if rep.Migrated != 1 {
		t.Fatalf("migrated = %d, want 1", rep.Migrated)
	}
}
```

- [ ] **Step 3: Build, vet, full test, commit**

`go build ./...`; `go vet ./...`; `mise run test`; `mise run test:integration` (ALL PASS).
```bash
git add cmd/substrate/main.go internal/api/projection_e2e_test.go
git commit -m "feat: wire backfiller/replayer/worker into the server; projection e2e"
```

---

## Final review (controller, after all tasks)

- Dispatch a final code-review subagent over `git diff main...HEAD`: check the backfill keyset loop terminates and never re-scans skipped records; per-record txn re-check + FOR UPDATE correctness under concurrency; that invalid records are skipped (never written); that `migration` events are state events (appear in history/audit) while `policy_denied` stays excluded; replay idempotency + lifecycle-row exclusion; the `nil`-enqueuer no-op; and that all Plan 1–4 tests still pass.
- Then use **superpowers:finishing-a-development-branch**.

---

## Self-review notes (spec coverage)

- Spec §2 #1 trigger (both) → Task 7 (worker + enqueue) + Task 8 (manual endpoint) + Task 9 (wiring).
- Spec §2 #2 transform (defaults + re-validate) → Task 3 (applier) + Task 5 (Backfiller).
- Spec §2 #3 replay admin endpoints → Task 4 (Replayer) + Task 8 (admin route).
- Spec §2 #4 inherent resumability → Task 5 (keyset loop, idempotent re-stamp, Report counts).
- Spec §2 #5 events.schema_version → Task 1 (column) + Task 2 (population) + Task 4 (replay consumes it).
- Spec §4 population matrix → Task 2 (create/update from `sv`, delete from row, revert NULL) + Task 5 (migration = active version).
- Spec §5 replay (record + collection, exclude lifecycle/denied) → Task 4.
- Spec §6 backfill (batched, per-record txn, skip-invalid, migration events) → Task 5.
- Spec §7 auto-backfill flag + worker → Tasks 6–7.
- Spec §8 API surface → Task 8.
- Spec §9 error handling → Tasks 5/8 (flexible no-op, skip-invalid, 400/401/404).
- Spec §11 testing → unit (Task 3), per-package integration (Tasks 1,2,4,5,6,7), HTTP/e2e (Tasks 8,9).
