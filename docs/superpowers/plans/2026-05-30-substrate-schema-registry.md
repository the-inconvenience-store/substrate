# Substrate Schema Registry & Validation Implementation Plan (Plan 2)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add the **typed** level to Substrate: a versioned JSON Schema registry with a `draft`/`active`/`deprecated` lifecycle, a deterministic recursive compatibility classifier, and validation of record writes against a collection's active schema.

**Architecture:** New `internal/schema` package (registry service, pure compatibility classifier, active-schema validator) over the existing goose+sqlc+pgx data layer. Validation is injected into the existing `record.Service` via a small `Validator` interface so `record` does not depend on `schema`. Schema lifecycle changes are recorded as events on the existing `events` table. Builds on the merged Plan 1 foundation.

**Tech Stack:** Go 1.26 · PostgreSQL (goose migrations + sqlc, `sql_package: pgx/v5`) · `github.com/santhosh-tekuri/jsonschema/v6` (JSON Schema draft 2020-12) · `net/http` · testcontainers integration tests. Task runner is **mise** (`mise run test`, `mise run test:integration`, `mise run sqlc:generate`).

**Spec:** [docs/superpowers/specs/2026-05-30-substrate-schema-registry-design.md](../specs/2026-05-30-substrate-schema-registry-design.md)

---

## Conventions (carried from Plan 1)

- **Module path:** `github.com/substrate/substrate`.
- **Data access:** services hold `*pgxpool.Pool` + `*db.Queries` (`db.New(pool)`); multi-statement mutations run in `store.WithTx(ctx, pool, func(tx pgx.Tx) error { qtx := s.q.WithTx(tx); ... })`. No inline SQL in services — add a query to `internal/queries/*.sql` and run `go tool sqlc generate` (or `mise run sqlc:generate`).
- **Errors:** typed `*apierr.Error` from services; transport mapping only in `internal/api`.
- **jsonb** columns are `[]byte` in generated code; nullable `int`/`text` are `pgtype.Int4`/`pgtype.Text`; `uuid` is `github.com/google/uuid.UUID`.
- **Integration tests** are `//go:build integration`, use `store.NewTestPool(t)`, run with `go test -tags=integration -timeout 300s ./...`. Generated `internal/db/*` files are committed; gopls may show stale "undefined method" errors after regeneration — rely on `go build`/`go test`.

## File structure (created/modified across this plan)

```
internal/migrations/00002_schemas.sql        # CREATE TABLE schemas (goose)
internal/queries/schemas.sql                 # sqlc queries for the registry
internal/db/schemas.sql.go                   # GENERATED
internal/apierr/apierr.go                    # + SchemaInvalid, SchemaIncompatible codes  [MODIFY]
internal/schema/classifier.go                # pure recursive compatibility classifier
internal/schema/registry.go                  # Service: register/get/list/activate/deprecate
internal/schema/validator.go                 # active-schema resolver + compiled-validator cache (record.Validator impl)
internal/api/schema_handlers.go              # HTTP handlers for schema endpoints
internal/queries/records.sql                 # InsertRecord/UpdateRecordData gain schema_version  [MODIFY]
internal/record/record.go                    # Validator dep; validate + stamp schema_version    [MODIFY]
internal/api/router.go                       # wire schema routes + Validator into record service [MODIFY]
cmd/substrate/main.go                         # wire the Validator                                 [MODIFY]
```

Tests live beside the code as `*_test.go`.

---

## Task 1: Schema data layer — migration, queries, error codes

**Files:**
- Create: `internal/migrations/00002_schemas.sql`, `internal/queries/schemas.sql`
- Modify: `internal/apierr/apierr.go`
- Generated: `internal/db/schemas.sql.go`
- Test: `internal/store/schema_migrate_test.go`

- [ ] **Step 1: Write the goose migration**

Create `internal/migrations/00002_schemas.sql`:
```sql
-- +goose Up
CREATE TABLE schemas (
    id             uuid PRIMARY KEY,
    collection_id  uuid NOT NULL REFERENCES collections(id) ON DELETE CASCADE,
    workspace_id   uuid NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    version        int NOT NULL,
    json_schema    jsonb NOT NULL,
    lifecycle      text NOT NULL DEFAULT 'draft',
    indexed_fields jsonb NOT NULL DEFAULT '[]',
    rationale      text,
    created_by     text,
    created_at     timestamptz NOT NULL DEFAULT now(),
    UNIQUE (collection_id, version)
);

-- +goose Down
DROP TABLE IF EXISTS schemas;
```

- [ ] **Step 2: Write the schema queries**

Create `internal/queries/schemas.sql`:
```sql
-- name: LockCollection :one
SELECT id, level, active_schema_version
FROM collections
WHERE id = $1
FOR UPDATE;

-- name: NextSchemaVersion :one
SELECT COALESCE(MAX(version), 0) + 1 AS next
FROM schemas
WHERE collection_id = $1;

-- name: InsertSchema :one
INSERT INTO schemas (id, collection_id, workspace_id, version, json_schema, lifecycle, indexed_fields, rationale, created_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
RETURNING id, collection_id, version, lifecycle, indexed_fields, created_at, created_by;

-- name: GetSchema :one
SELECT id, collection_id, version, json_schema, lifecycle, indexed_fields, rationale, created_at, created_by
FROM schemas
WHERE collection_id = $1 AND version = $2;

-- name: ListSchemas :many
SELECT version, lifecycle, indexed_fields, created_at, created_by
FROM schemas
WHERE collection_id = $1
ORDER BY version ASC;

-- name: GetActiveSchema :one
SELECT s.version, s.json_schema
FROM schemas s
JOIN collections c ON c.id = s.collection_id AND c.active_schema_version = s.version
WHERE c.id = $1;

-- name: SetSchemaLifecycle :exec
UPDATE schemas SET lifecycle = $3
WHERE collection_id = $1 AND version = $2;

-- name: SetCollectionActiveVersion :exec
UPDATE collections SET active_schema_version = $2, level = 'typed', updated_at = now()
WHERE id = $1;
```

- [ ] **Step 3: Add the new error codes**

In `internal/apierr/apierr.go`, add two constants to the `const (...)` block (after `Internal`):
```go
	SchemaInvalid      Code = "schema_invalid"
	SchemaIncompatible Code = "schema_incompatible"
```
And add cases to `HTTPStatus()` before the `default`:
```go
	case SchemaInvalid:
		return http.StatusUnprocessableEntity
	case SchemaIncompatible:
		return http.StatusConflict
```

- [ ] **Step 4: Generate and write the failing migration test**

Run: `go tool sqlc generate` then `go build ./...`
Create `internal/store/schema_migrate_test.go`:
```go
//go:build integration

package store

import (
	"context"
	"testing"
)

func TestMigrate_CreatesSchemasTable(t *testing.T) {
	pool := NewTestPool(t)
	var n int
	err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.tables
		 WHERE table_schema='public' AND table_name='schemas'`).Scan(&n)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if n != 1 {
		t.Fatalf("schemas table missing (n=%d)", n)
	}
}
```

- [ ] **Step 5: Run the test**

Run: `go test -tags=integration -timeout 300s ./internal/store/...`
Expected: PASS (existing store tests + `TestMigrate_CreatesSchemasTable`).

- [ ] **Step 6: Confirm unit build/tests and commit**

Run: `go build ./... && go test ./...`
Expected: clean.
```bash
git add internal/migrations internal/queries/schemas.sql internal/db internal/apierr internal/store
git commit -m "feat: schemas table, registry queries, and schema error codes"
```

---

## Task 2: Compatibility classifier (pure, unit-tested)

**Files:**
- Create: `internal/schema/classifier.go`
- Test: `internal/schema/classifier_test.go`

A dependency-free recursive diff between the current-active schema and a candidate. No DB.

- [ ] **Step 1: Write the failing tests**

Create `internal/schema/classifier_test.go`:
```go
package schema

import "testing"

func obj(props map[string]any, required ...string) map[string]any {
	m := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		r := make([]any, len(required))
		for i, s := range required {
			r[i] = s
		}
		m["required"] = r
	}
	return m
}

func hasBreaking(cs []Change) bool {
	for _, c := range cs {
		if c.Breaking {
			return true
		}
	}
	return false
}

func TestClassify_AddOptionalField_NotBreaking(t *testing.T) {
	cur := obj(map[string]any{"a": map[string]any{"type": "string"}}, "a")
	cand := obj(map[string]any{
		"a": map[string]any{"type": "string"},
		"b": map[string]any{"type": "string"},
	}, "a")
	if hasBreaking(Classify(cur, cand)) {
		t.Fatal("adding an optional field must not be breaking")
	}
}

func TestClassify_RemoveRequiredField_Breaking(t *testing.T) {
	cur := obj(map[string]any{
		"a": map[string]any{"type": "string"},
		"b": map[string]any{"type": "string"},
	}, "a", "b")
	cand := obj(map[string]any{"a": map[string]any{"type": "string"}}, "a")
	if !hasBreaking(Classify(cur, cand)) {
		t.Fatal("removing a required field must be breaking")
	}
}

func TestClassify_AddRequired_Breaking(t *testing.T) {
	cur := obj(map[string]any{"a": map[string]any{"type": "string"}}, "a")
	cand := obj(map[string]any{
		"a": map[string]any{"type": "string"},
		"b": map[string]any{"type": "string"},
	}, "a", "b")
	if !hasBreaking(Classify(cur, cand)) {
		t.Fatal("promoting a field to required must be breaking")
	}
}

func TestClassify_NarrowType_Breaking(t *testing.T) {
	cur := obj(map[string]any{"a": map[string]any{"type": "number"}}, "a")
	cand := obj(map[string]any{"a": map[string]any{"type": "integer"}}, "a")
	if !hasBreaking(Classify(cur, cand)) {
		t.Fatal("number->integer must be breaking")
	}
}

func TestClassify_WidenType_NotBreaking(t *testing.T) {
	cur := obj(map[string]any{"a": map[string]any{"type": "integer"}}, "a")
	cand := obj(map[string]any{"a": map[string]any{"type": "number"}}, "a")
	if hasBreaking(Classify(cur, cand)) {
		t.Fatal("integer->number must not be breaking")
	}
}

func TestClassify_AddEnumValue_NotBreaking(t *testing.T) {
	cur := obj(map[string]any{"a": map[string]any{"enum": []any{"x"}}}, "a")
	cand := obj(map[string]any{"a": map[string]any{"enum": []any{"x", "y"}}}, "a")
	if hasBreaking(Classify(cur, cand)) {
		t.Fatal("adding an enum value must not be breaking")
	}
}

func TestClassify_RemoveEnumValue_Breaking(t *testing.T) {
	cur := obj(map[string]any{"a": map[string]any{"enum": []any{"x", "y"}}}, "a")
	cand := obj(map[string]any{"a": map[string]any{"enum": []any{"x"}}}, "a")
	if !hasBreaking(Classify(cur, cand)) {
		t.Fatal("removing an enum value must be breaking")
	}
}

func TestClassify_NestedRemoveRequired_Breaking(t *testing.T) {
	cur := obj(map[string]any{
		"a": obj(map[string]any{"x": map[string]any{"type": "string"}}, "x"),
	}, "a")
	cand := obj(map[string]any{
		"a": obj(map[string]any{"x": map[string]any{"type": "string"}}),
	}, "a")
	if !hasBreaking(Classify(cur, cand)) {
		t.Fatal("nested required removal must be breaking")
	}
}

func TestClassify_AmbiguousConstruct_Breaking(t *testing.T) {
	cur := obj(map[string]any{"a": map[string]any{"type": "string"}}, "a")
	cand := obj(map[string]any{"a": map[string]any{"$ref": "#/$defs/Foo"}}, "a")
	if !hasBreaking(Classify(cur, cand)) {
		t.Fatal("switching to an unanalyzable construct must be conservatively breaking")
	}
}
```

- [ ] **Step 2: Run the tests to confirm they fail**

Run: `go test ./internal/schema/...`
Expected: FAIL — `undefined: Classify`, `undefined: Change`.

- [ ] **Step 3: Implement the classifier**

Create `internal/schema/classifier.go`:
```go
// Package schema provides the schema registry, JSON Schema validation, and the
// compatibility classifier for Substrate's typed collections.
package schema

import "fmt"

// Change is one structural difference between two schemas.
type Change struct {
	Path     string `json:"path"`
	Kind     string `json:"kind"`
	Breaking bool   `json:"breaking"`
}

// Classify returns the structural changes from current to candidate. A change is
// Breaking if migrating existing data validated by current could fail candidate.
// Constructs it cannot confidently analyze are reported as breaking (conservative).
func Classify(current, candidate map[string]any) []Change {
	var out []Change
	classifyNode(current, candidate, "$", &out)
	return out
}

func classifyNode(cur, cand map[string]any, path string, out *[]Change) {
	// Unanalyzable combinators / refs on either side -> conservative breaking.
	for _, k := range []string{"$ref", "anyOf", "oneOf", "allOf", "not", "patternProperties"} {
		if _, ok := cur[k]; ok {
			if !equalJSON(cur[k], cand[k]) {
				*out = append(*out, Change{Path: path, Kind: "unanalyzable:" + k, Breaking: true})
				return
			}
		}
		if _, ok := cand[k]; ok {
			if !equalJSON(cur[k], cand[k]) {
				*out = append(*out, Change{Path: path, Kind: "unanalyzable:" + k, Breaking: true})
				return
			}
		}
	}

	classifyType(cur["type"], cand["type"], path, out)
	classifyEnum(cur["enum"], cand["enum"], path, out)
	classifyBounds(cur, cand, path, out)
	classifyRequired(cur["required"], cand["required"], path, out)
	classifyProperties(cur, cand, path, out)
	classifyItems(cur["items"], cand["items"], path, out)
}

func classifyType(cur, cand any, path string, out *[]Change) {
	if cur == nil || cand == nil || equalJSON(cur, cand) {
		return
	}
	curSet := typeSet(cur)
	candSet := typeSet(cand)
	// Narrowing: some accepted type is no longer accepted (and not a safe widen).
	for t := range curSet {
		if !candSet[t] && !(t == "integer" && candSet["number"]) {
			*out = append(*out, Change{Path: path + ".type", Kind: "narrow-type", Breaking: true})
			return
		}
	}
	*out = append(*out, Change{Path: path + ".type", Kind: "widen-type", Breaking: false})
}

func classifyEnum(cur, cand any, path string, out *[]Change) {
	if cur == nil {
		return // no prior constraint -> candidate adding enum is a tighten only if cur unconstrained
	}
	curVals, ok1 := cur.([]any)
	candVals, ok2 := cand.([]any)
	if !ok1 {
		return
	}
	if !ok2 {
		// enum removed entirely (relaxed) -> not breaking
		*out = append(*out, Change{Path: path + ".enum", Kind: "remove-enum-constraint", Breaking: false})
		return
	}
	candSet := map[string]bool{}
	for _, v := range candVals {
		candSet[fmt.Sprintf("%v", v)] = true
	}
	for _, v := range curVals {
		if !candSet[fmt.Sprintf("%v", v)] {
			*out = append(*out, Change{Path: path + ".enum", Kind: "remove-enum-value", Breaking: true})
			return
		}
	}
	if len(candVals) > len(curVals) {
		*out = append(*out, Change{Path: path + ".enum", Kind: "add-enum-value", Breaking: false})
	}
}

func classifyBounds(cur, cand map[string]any, path string, out *[]Change) {
	// Tightening numeric/length bounds is breaking; loosening or removing is not.
	type bound struct {
		key      string
		tighten  func(c, n float64) bool // true if n is stricter than c
	}
	bounds := []bound{
		{"minimum", func(c, n float64) bool { return n > c }},
		{"minLength", func(c, n float64) bool { return n > c }},
		{"minItems", func(c, n float64) bool { return n > c }},
		{"maximum", func(c, n float64) bool { return n < c }},
		{"maxLength", func(c, n float64) bool { return n < c }},
		{"maxItems", func(c, n float64) bool { return n < c }},
	}
	for _, b := range bounds {
		cv, cok := asFloat(cur[b.key])
		nv, nok := asFloat(cand[b.key])
		switch {
		case nok && !cok:
			*out = append(*out, Change{Path: path + "." + b.key, Kind: "add-bound", Breaking: true})
		case cok && nok && b.tighten(cv, nv):
			*out = append(*out, Change{Path: path + "." + b.key, Kind: "tighten-bound", Breaking: true})
		}
	}
	// pattern / format: any change is conservatively breaking.
	for _, k := range []string{"pattern", "format"} {
		if _, ok := cand[k]; ok && !equalJSON(cur[k], cand[k]) {
			*out = append(*out, Change{Path: path + "." + k, Kind: "change-" + k, Breaking: true})
		}
	}
}

func classifyRequired(cur, cand any, path string, out *[]Change) {
	curSet := strSet(cur)
	candSet := strSet(cand)
	for f := range candSet {
		if !curSet[f] {
			*out = append(*out, Change{Path: path + ".required." + f, Kind: "add-required", Breaking: true})
		}
	}
	for f := range curSet {
		if !candSet[f] {
			*out = append(*out, Change{Path: path + ".required." + f, Kind: "remove-required", Breaking: false})
		}
	}
}

func classifyProperties(cur, cand map[string]any, path string, out *[]Change) {
	curProps, _ := cur["properties"].(map[string]any)
	candProps, _ := cand["properties"].(map[string]any)
	candRequired := strSet(cand["required"])
	for name, cp := range curProps {
		np, ok := candProps[name]
		if !ok {
			// property dropped from schema: only breaking if it was required (caught by required diff);
			// dropping an optional property definition is not breaking.
			continue
		}
		cpm, _ := cp.(map[string]any)
		npm, _ := np.(map[string]any)
		if cpm != nil && npm != nil {
			classifyNode(cpm, npm, path+"."+name, out)
		}
	}
	for name := range candProps {
		if _, ok := curProps[name]; !ok && candRequired[name] {
			*out = append(*out, Change{Path: path + "." + name, Kind: "add-required-property", Breaking: true})
		}
	}
}

func classifyItems(cur, cand any, path string, out *[]Change) {
	cm, ok1 := cur.(map[string]any)
	nm, ok2 := cand.(map[string]any)
	if ok1 && ok2 {
		classifyNode(cm, nm, path+".items", out)
	}
}

// --- small helpers ---

func typeSet(v any) map[string]bool {
	s := map[string]bool{}
	switch t := v.(type) {
	case string:
		s[t] = true
	case []any:
		for _, e := range t {
			if es, ok := e.(string); ok {
				s[es] = true
			}
		}
	}
	return s
}

func strSet(v any) map[string]bool {
	s := map[string]bool{}
	if arr, ok := v.([]any); ok {
		for _, e := range arr {
			if es, ok := e.(string); ok {
				s[es] = true
			}
		}
	}
	return s
}

func asFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	}
	return 0, false
}

func equalJSON(a, b any) bool {
	return fmt.Sprintf("%v", a) == fmt.Sprintf("%v", b)
}
```

- [ ] **Step 4: Run the tests to confirm they pass**

Run: `go test ./internal/schema/...`
Expected: PASS (all classifier tests).

- [ ] **Step 5: Commit**

```bash
git add internal/schema/classifier.go internal/schema/classifier_test.go
git commit -m "feat: deterministic recursive schema compatibility classifier"
```

---

## Task 3: Registry service — register, get, list

**Files:**
- Create: `internal/schema/registry.go`
- Test: `internal/schema/registry_test.go`

Handles registration (draft / first-auto-activate / `activate=true`), reads, and version allocation. Activation logic (compatibility check, deprecate prior, force) lives in Task 4; this task implements register + the activate call it delegates to.

- [ ] **Step 1: Write the failing integration test**

Create `internal/schema/registry_test.go`:
```go
//go:build integration

package schema

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/substrate/substrate/internal/collection"
	"github.com/substrate/substrate/internal/store"
	"github.com/substrate/substrate/internal/workspace"
)

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
```

- [ ] **Step 2: Run it to confirm it fails**

Run: `go test -tags=integration ./internal/schema/...`
Expected: FAIL — `undefined: New`, `RegisterCmd`, etc.

- [ ] **Step 3: Implement the registry service**

Create `internal/schema/registry.go`:
```go
package schema

import (
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
	"github.com/substrate/substrate/internal/store"
)

// SchemaVersion is a registered, immutable schema version.
type SchemaVersion struct {
	Version       int            `json:"version"`
	Lifecycle     string         `json:"lifecycle"`
	IndexedFields []string       `json:"indexed_fields"`
	JSONSchema    map[string]any `json:"json_schema,omitempty"`
}

// RegisterCmd registers a new schema version.
type RegisterCmd struct {
	Workspace     uuid.UUID
	Collection    uuid.UUID
	Actor         string
	JSONSchema    map[string]any
	IndexedFields []string
	Activate      bool   // register + activate in one call
	Force         bool   // allow a breaking change on activation
	Rationale     string // recorded when Force is used
}

// ActiveSchema is the active version's number plus its raw document.
type ActiveSchema struct {
	Version int
	Raw     []byte
}

// Service manages the schema registry.
type Service struct {
	pool *pgxpool.Pool
	q    *db.Queries
}

func New(pool *pgxpool.Pool) *Service { return &Service{pool: pool, q: db.New(pool)} }

// compileSchema validates that doc is a usable JSON Schema (draft 2020-12).
func compileSchema(doc map[string]any) error {
	raw, err := json.Marshal(doc)
	if err != nil {
		return apierr.New(apierr.SchemaInvalid, "schema is not JSON-encodable")
	}
	c := jsonschema.NewCompiler()
	parsed, err := jsonschema.UnmarshalJSON(bytesReader(raw))
	if err != nil {
		return apierr.New(apierr.SchemaInvalid, "schema is not valid JSON")
	}
	const resID = "substrate://schema.json"
	if err := c.AddResource(resID, parsed); err != nil {
		return apierr.New(apierr.SchemaInvalid, fmt.Sprintf("invalid schema: %v", err))
	}
	if _, err := c.Compile(resID); err != nil {
		return apierr.New(apierr.SchemaInvalid, fmt.Sprintf("invalid schema: %v", err))
	}
	return nil
}

// Register inserts a new immutable version. First schema auto-activates; otherwise
// it is a draft unless Activate is set.
func (s *Service) Register(ctx context.Context, cmd RegisterCmd) (SchemaVersion, error) {
	if err := compileSchema(cmd.JSONSchema); err != nil {
		return SchemaVersion{}, err
	}
	rawSchema, _ := json.Marshal(cmd.JSONSchema)
	idxRaw, _ := json.Marshal(normStrings(cmd.IndexedFields))

	var result SchemaVersion
	err := store.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		qtx := s.q.WithTx(tx)
		col, err := qtx.LockCollection(ctx, cmd.Collection)
		if errors.Is(err, pgx.ErrNoRows) {
			return apierr.New(apierr.NotFound, "collection not found")
		}
		if err != nil {
			return err
		}
		isFirst := !col.ActiveSchemaVersion.Valid
		next, err := qtx.NextSchemaVersion(ctx, cmd.Collection)
		if err != nil {
			return err
		}
		lifecycle := "draft"
		if isFirst || cmd.Activate {
			lifecycle = "active"
		}
		// Compatibility gate when activating over an existing active version.
		if !isFirst && cmd.Activate {
			if err := s.checkCompatibleTx(ctx, qtx, cmd.Collection, col.ActiveSchemaVersion.Int32, cmd.JSONSchema, cmd.Force); err != nil {
				return err
			}
		}
		row, err := qtx.InsertSchema(ctx, db.InsertSchemaParams{
			ID: uuid.New(), CollectionID: cmd.Collection, WorkspaceID: cmd.Workspace,
			Version: next, JsonSchema: rawSchema, Lifecycle: lifecycle,
			IndexedFields: idxRaw, Rationale: textOrNull(cmd.Rationale),
			CreatedBy: textOrNull(cmd.Actor),
		})
		if err != nil {
			return err
		}
		if lifecycle == "active" {
			if err := s.activateTx(ctx, qtx, cmd.Collection, col.ActiveSchemaVersion, next, cmd.Actor); err != nil {
				return err
			}
		}
		result = SchemaVersion{Version: int(row.Version), Lifecycle: lifecycle, IndexedFields: normStrings(cmd.IndexedFields)}
		return nil
	})
	if err != nil {
		return SchemaVersion{}, err
	}
	return result, nil
}

// GetActive returns the collection's active schema version + raw document.
func (s *Service) GetActive(ctx context.Context, col uuid.UUID) (ActiveSchema, error) {
	row, err := s.q.GetActiveSchema(ctx, col)
	if errors.Is(err, pgx.ErrNoRows) {
		return ActiveSchema{}, apierr.New(apierr.NotFound, "no active schema")
	}
	if err != nil {
		return ActiveSchema{}, fmt.Errorf("get active schema: %w", err)
	}
	return ActiveSchema{Version: int(row.Version), Raw: row.JsonSchema}, nil
}

// Get returns one version including its full document.
func (s *Service) Get(ctx context.Context, col uuid.UUID, version int) (SchemaVersion, error) {
	row, err := s.q.GetSchema(ctx, db.GetSchemaParams{CollectionID: col, Version: int32(version)})
	if errors.Is(err, pgx.ErrNoRows) {
		return SchemaVersion{}, apierr.New(apierr.NotFound, "schema version not found")
	}
	if err != nil {
		return SchemaVersion{}, fmt.Errorf("get schema: %w", err)
	}
	var doc map[string]any
	_ = json.Unmarshal(row.JsonSchema, &doc)
	var idx []string
	_ = json.Unmarshal(row.IndexedFields, &idx)
	return SchemaVersion{Version: int(row.Version), Lifecycle: row.Lifecycle, IndexedFields: idx, JSONSchema: doc}, nil
}

// List returns all versions (without full documents).
func (s *Service) List(ctx context.Context, col uuid.UUID) ([]SchemaVersion, error) {
	rows, err := s.q.ListSchemas(ctx, col)
	if err != nil {
		return nil, fmt.Errorf("list schemas: %w", err)
	}
	out := make([]SchemaVersion, 0, len(rows))
	for _, r := range rows {
		var idx []string
		_ = json.Unmarshal(r.IndexedFields, &idx)
		out = append(out, SchemaVersion{Version: int(r.Version), Lifecycle: r.Lifecycle, IndexedFields: idx})
	}
	return out, nil
}

// --- helpers ---

func textOrNull(s string) pgtype.Text { return pgtype.Text{String: s, Valid: s != ""} }

func normStrings(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}
```

> Note: `bytesReader` is a tiny adapter so the jsonschema v6 API (which reads from an
> `io.Reader`) can consume a byte slice — define it in `validator.go` (Task 5) as
> `func bytesReader(b []byte) *bytes.Reader { return bytes.NewReader(b) }`. If you implement
> Task 5 after this, add a temporary copy in `registry.go` and remove the duplicate when
> `validator.go` lands. `checkCompatibleTx` and `activateTx` are implemented in Task 4.

- [ ] **Step 4: Add the activate/compat helpers as stubs to compile, then run**

Because Task 4 implements `activateTx`/`checkCompatibleTx`, add minimal working versions now in `registry.go` so this task compiles and its tests pass (Task 4 replaces them with the full versions + deprecate-prior + events):
```go
func (s *Service) activateTx(ctx context.Context, q *db.Queries, col uuid.UUID, prior pgtype.Int4, version int32, actor string) error {
	return q.SetCollectionActiveVersion(ctx, db.SetCollectionActiveVersionParams{ID: col, ActiveSchemaVersion: pgtype.Int4{Int32: version, Valid: true}})
}

func (s *Service) checkCompatibleTx(ctx context.Context, q *db.Queries, col uuid.UUID, priorVersion int32, candidate map[string]any, force bool) error {
	return nil // Task 4 implements the real compatibility gate
}
```
Add `"github.com/jackc/pgx/v5/pgtype"` import if not already present (it is, via textOrNull).

Run: `go build ./... && go test -tags=integration -timeout 300s ./internal/schema/...`
Expected: PASS (`TestRegisterFirstSchemaAutoActivates`, `TestRegisterDraftThenList`, `TestRegisterInvalidSchemaRejected`).

- [ ] **Step 5: Commit**

```bash
git add internal/schema/registry.go internal/schema/registry_test.go
git commit -m "feat: schema registry register/get/list with version allocation and auto-activate"
```

---

## Task 4: Activation, deprecation, compatibility gate, schema events

**Files:**
- Modify: `internal/schema/registry.go`
- Test: `internal/schema/activate_test.go`

Replaces the Task 3 stubs with the real `activateTx` (deprecate prior + write a `schema_activated` event) and `checkCompatibleTx` (load prior active doc, run `Classify`, block breaking unless forced), and adds public `Activate`/`Deprecate`.

- [ ] **Step 1: Write the failing integration test**

Create `internal/schema/activate_test.go`:
```go
//go:build integration

package schema

import (
	"context"
	"testing"

	"github.com/substrate/substrate/internal/apierr"
)

func TestActivateBlocksBreakingUnlessForced(t *testing.T) {
	svc, ws, col := setup(t)
	ctx := context.Background()

	// v1 active: {name required}
	_, err := svc.Register(ctx, RegisterCmd{Workspace: ws, Collection: col, JSONSchema: personSchema()})
	if err != nil {
		t.Fatalf("v1: %v", err)
	}
	// v2 draft: adds a new required field "age" (breaking).
	breaking := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name": map[string]any{"type": "string"},
			"age":  map[string]any{"type": "integer"},
		},
		"required": []any{"name", "age"},
	}
	v2, err := svc.Register(ctx, RegisterCmd{Workspace: ws, Collection: col, JSONSchema: breaking})
	if err != nil {
		t.Fatalf("v2 register: %v", err)
	}

	// Activate without force -> schema_incompatible.
	err = svc.Activate(ctx, ws, col, v2.Version, "agent-1", false, "")
	e, ok := apierr.As(err)
	if !ok || e.Code != apierr.SchemaIncompatible {
		t.Fatalf("expected schema_incompatible, got %v", err)
	}

	// Activate with force -> succeeds, becomes active.
	if err := svc.Activate(ctx, ws, col, v2.Version, "agent-1", true, "intentional breaking change"); err != nil {
		t.Fatalf("forced activate: %v", err)
	}
	active, _ := svc.GetActive(ctx, col)
	if active.Version != v2.Version {
		t.Fatalf("active = %d, want %d", active.Version, v2.Version)
	}
}

func TestDeprecate(t *testing.T) {
	svc, ws, col := setup(t)
	ctx := context.Background()
	_, _ = svc.Register(ctx, RegisterCmd{Workspace: ws, Collection: col, JSONSchema: personSchema()})
	// register a second compatible version (adds optional field), activate it
	v2doc := map[string]any{
		"type":       "object",
		"properties": map[string]any{"name": map[string]any{"type": "string"}, "note": map[string]any{"type": "string"}},
		"required":   []any{"name"},
	}
	v2, _ := svc.Register(ctx, RegisterCmd{Workspace: ws, Collection: col, JSONSchema: v2doc, Activate: true})
	// Now deprecate v1.
	if err := svc.Deprecate(ctx, col, 1); err != nil {
		t.Fatalf("deprecate: %v", err)
	}
	got, _ := svc.Get(ctx, col, 1)
	if got.Lifecycle != "deprecated" {
		t.Fatalf("v1 lifecycle = %s, want deprecated", got.Lifecycle)
	}
	if v2.Lifecycle != "active" {
		t.Fatalf("v2 should be active")
	}
}
```

- [ ] **Step 2: Run it to confirm it fails**

Run: `go test -tags=integration ./internal/schema/...`
Expected: FAIL — `undefined: (*Service).Activate`, `Deprecate`.

- [ ] **Step 3: Replace the stubs and add public methods**

In `internal/schema/registry.go`, replace the two stub functions from Task 3 with these, and add `Activate`/`Deprecate`. Add imports `"github.com/substrate/substrate/internal/record"`? No — do NOT import record (avoid cycle). Use `appendSchemaEvent` writing directly via a query (below).

Replace `activateTx` and `checkCompatibleTx`:
```go
func (s *Service) checkCompatibleTx(ctx context.Context, q *db.Queries, col uuid.UUID, priorVersion int32, candidate map[string]any, force bool) error {
	if force {
		return nil
	}
	prior, err := q.GetSchema(ctx, db.GetSchemaParams{CollectionID: col, Version: priorVersion})
	if err != nil {
		return fmt.Errorf("load prior schema: %w", err)
	}
	var priorDoc map[string]any
	if err := json.Unmarshal(prior.JsonSchema, &priorDoc); err != nil {
		return fmt.Errorf("decode prior schema: %w", err)
	}
	changes := Classify(priorDoc, candidate)
	var breaking []Change
	for _, c := range changes {
		if c.Breaking {
			breaking = append(breaking, c)
		}
	}
	if len(breaking) > 0 {
		return apierr.New(apierr.SchemaIncompatible, "breaking schema change requires force").
			WithDetails(map[string]any{"breaking_changes": breaking})
	}
	return nil
}

func (s *Service) activateTx(ctx context.Context, q *db.Queries, col uuid.UUID, prior pgtype.Int4, version int32, actor string) error {
	// Deprecate the previously-active version, if any and different.
	if prior.Valid && prior.Int32 != version {
		if err := q.SetSchemaLifecycle(ctx, db.SetSchemaLifecycleParams{
			CollectionID: col, Version: prior.Int32, Lifecycle: "deprecated",
		}); err != nil {
			return err
		}
	}
	if err := q.SetSchemaLifecycle(ctx, db.SetSchemaLifecycleParams{
		CollectionID: col, Version: version, Lifecycle: "active",
	}); err != nil {
		return err
	}
	if err := q.SetCollectionActiveVersion(ctx, db.SetCollectionActiveVersionParams{
		ID: col, ActiveSchemaVersion: pgtype.Int4{Int32: version, Valid: true},
	}); err != nil {
		return err
	}
	return appendSchemaEvent(ctx, q, col, "schema_activated", int64(version), actor)
}
```

Add the public methods and the event helper:
```go
// Activate makes an existing draft/deprecated version the active one.
func (s *Service) Activate(ctx context.Context, ws, col uuid.UUID, version int, actor string, force bool, rationale string) error {
	return store.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		qtx := s.q.WithTx(tx)
		c, err := qtx.LockCollection(ctx, col)
		if errors.Is(err, pgx.ErrNoRows) {
			return apierr.New(apierr.NotFound, "collection not found")
		}
		if err != nil {
			return err
		}
		target, err := qtx.GetSchema(ctx, db.GetSchemaParams{CollectionID: col, Version: int32(version)})
		if errors.Is(err, pgx.ErrNoRows) {
			return apierr.New(apierr.NotFound, "schema version not found")
		}
		if err != nil {
			return err
		}
		if c.ActiveSchemaVersion.Valid && c.ActiveSchemaVersion.Int32 != int32(version) {
			var candidate map[string]any
			if err := json.Unmarshal(target.JsonSchema, &candidate); err != nil {
				return fmt.Errorf("decode candidate: %w", err)
			}
			if err := s.checkCompatibleTx(ctx, qtx, col, c.ActiveSchemaVersion.Int32, candidate, force); err != nil {
				return err
			}
		}
		if force && rationale != "" {
			if err := qtx.SetSchemaLifecycle(ctx, db.SetSchemaLifecycleParams{CollectionID: col, Version: int32(version), Lifecycle: "draft"}); err != nil {
				return err
			}
		}
		return s.activateTx(ctx, qtx, col, c.ActiveSchemaVersion, int32(version), actor)
	})
}

// Deprecate marks a non-active version deprecated.
func (s *Service) Deprecate(ctx context.Context, col uuid.UUID, version int) error {
	return store.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		qtx := s.q.WithTx(tx)
		c, err := qtx.LockCollection(ctx, col)
		if errors.Is(err, pgx.ErrNoRows) {
			return apierr.New(apierr.NotFound, "collection not found")
		}
		if err != nil {
			return err
		}
		if c.ActiveSchemaVersion.Valid && c.ActiveSchemaVersion.Int32 == int32(version) {
			return apierr.New(apierr.Conflict, "cannot deprecate the active version; activate another first")
		}
		return qtx.SetSchemaLifecycle(ctx, db.SetSchemaLifecycleParams{
			CollectionID: col, Version: int32(version), Lifecycle: "deprecated",
		})
	})
}

// appendSchemaEvent records a schema lifecycle change on the events timeline. The
// event is collection-scoped: record_id is the collection id; revision is the schema version.
func appendSchemaEvent(ctx context.Context, q *db.Queries, col uuid.UUID, typ string, version int64, actor string) error {
	return q.AppendEvent(ctx, db.AppendEventParams{
		ID: uuid.New(), WorkspaceID: uuid.Nil, CollectionID: col, RecordID: col,
		Type: typ, Revision: version, StateAfter: []byte("{}"),
		Actor: textOrNull(actor), IdempotencyKey: textOrNull(""),
	})
}
```

> WorkspaceID on the schema event: `AppendEvent` requires a workspace id (NOT NULL FK). Pass
> the collection's workspace. Add `WorkspaceID` to `RegisterCmd`/`Activate` flows by reading it
> from the locked collection — `LockCollection` returns the collection row; if it doesn't expose
> `workspace_id`, extend the `LockCollection` query to `SELECT id, workspace_id, level,
> active_schema_version ... FOR UPDATE` and regenerate. Then pass `c.WorkspaceID` into
> `appendSchemaEvent`. **Do this:** update `internal/queries/schemas.sql` `LockCollection` to
> select `workspace_id`, regenerate, and thread it through (the `uuid.Nil` above is a
> placeholder you must replace with the real workspace id).

- [ ] **Step 4: Thread workspace id and run**

Update `LockCollection` in `internal/queries/schemas.sql`:
```sql
-- name: LockCollection :one
SELECT id, workspace_id, level, active_schema_version
FROM collections
WHERE id = $1
FOR UPDATE;
```
Run `go tool sqlc generate`. Update `appendSchemaEvent` to take a `ws uuid.UUID` parameter and pass `c.WorkspaceID` from callers (`activateTx` gains a `ws` param sourced from the locked collection). Adjust `Register`'s `activateTx` call site likewise.

Run: `go build ./... && go test -tags=integration -timeout 300s ./internal/schema/...`
Expected: PASS (register tests + `TestActivateBlocksBreakingUnlessForced` + `TestDeprecate`).

- [ ] **Step 5: Commit**

```bash
git add internal/schema internal/queries/schemas.sql internal/db
git commit -m "feat: schema activation/deprecation with compatibility gate and lifecycle events"
```

---

## Task 5: Validator — active-schema resolver + compiled cache

**Files:**
- Create: `internal/schema/validator.go`
- Test: `internal/schema/validator_test.go`

Implements the `record.Validator` contract: resolve a collection's active schema, validate a record body, return the version to stamp. Flexible collections (no active schema) validate trivially and return version 0.

- [ ] **Step 1: Write the failing integration test**

Create `internal/schema/validator_test.go`:
```go
//go:build integration

package schema

import (
	"context"
	"testing"

	"github.com/substrate/substrate/internal/apierr"
)

func TestValidator_FlexibleReturnsZero(t *testing.T) {
	svc, _, col := setup(t)
	v := NewValidator(svc)
	ver, err := v.ValidateWrite(context.Background(), col, map[string]any{"anything": true})
	if err != nil || ver != 0 {
		t.Fatalf("flexible collection should validate trivially as version 0, got v%d err=%v", ver, err)
	}
}

func TestValidator_TypedValidatesAndStamps(t *testing.T) {
	svc, ws, col := setup(t)
	ctx := context.Background()
	_, _ = svc.Register(ctx, RegisterCmd{Workspace: ws, Collection: col, JSONSchema: personSchema()})
	v := NewValidator(svc)

	ver, err := v.ValidateWrite(ctx, col, map[string]any{"name": "Ada"})
	if err != nil || ver != 1 {
		t.Fatalf("valid write should pass as v1, got v%d err=%v", ver, err)
	}

	_, err = v.ValidateWrite(ctx, col, map[string]any{"name": 123}) // wrong type
	e, ok := apierr.As(err)
	if !ok || e.Code != apierr.Validation {
		t.Fatalf("invalid write should be validation_failed, got %v", err)
	}
}
```

- [ ] **Step 2: Run it to confirm it fails**

Run: `go test -tags=integration ./internal/schema/...`
Expected: FAIL — `undefined: NewValidator`.

- [ ] **Step 3: Implement the validator**

Create `internal/schema/validator.go`:
```go
package schema

import (
	"bytes"
	"context"
	"fmt"
	"sync"

	"github.com/google/uuid"
	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/substrate/substrate/internal/apierr"
)

// bytesReader adapts a byte slice to the io.Reader the jsonschema API expects.
func bytesReader(b []byte) *bytes.Reader { return bytes.NewReader(b) }

// activeResolver is the subset of the registry the validator needs.
type activeResolver interface {
	GetActive(ctx context.Context, col uuid.UUID) (ActiveSchema, error)
}

// Validator resolves a collection's active schema and validates record bodies.
// It satisfies record.Validator.
type Validator struct {
	reg   activeResolver
	mu    sync.RWMutex
	cache map[string]*jsonschema.Schema // key: "<collection>:<version>"
}

func NewValidator(reg activeResolver) *Validator {
	return &Validator{reg: reg, cache: map[string]*jsonschema.Schema{}}
}

// ValidateWrite validates data against the collection's active schema. Returns the
// active version to stamp (0 for flexible collections with no active schema).
func (v *Validator) ValidateWrite(ctx context.Context, col uuid.UUID, data map[string]any) (int, error) {
	active, err := v.reg.GetActive(ctx, col)
	if err != nil {
		if e, ok := apierr.As(err); ok && e.Code == apierr.NotFound {
			return 0, nil // flexible: no active schema
		}
		return 0, err
	}
	compiled, err := v.compiled(col, active)
	if err != nil {
		return 0, err
	}
	if err := compiled.Validate(data); err != nil {
		return 0, apierr.New(apierr.Validation, "record does not match schema").
			WithDetails(map[string]any{"errors": fmt.Sprintf("%v", err)})
	}
	return active.Version, nil
}

func (v *Validator) compiled(col uuid.UUID, active ActiveSchema) (*jsonschema.Schema, error) {
	key := fmt.Sprintf("%s:%d", col, active.Version)
	v.mu.RLock()
	c := v.cache[key]
	v.mu.RUnlock()
	if c != nil {
		return c, nil
	}
	parsed, err := jsonschema.UnmarshalJSON(bytesReader(active.Raw))
	if err != nil {
		return nil, fmt.Errorf("parse active schema: %w", err)
	}
	comp := jsonschema.NewCompiler()
	const resID = "substrate://active.json"
	if err := comp.AddResource(resID, parsed); err != nil {
		return nil, fmt.Errorf("add active schema: %w", err)
	}
	sch, err := comp.Compile(resID)
	if err != nil {
		return nil, fmt.Errorf("compile active schema: %w", err)
	}
	v.mu.Lock()
	v.cache[key] = sch
	v.mu.Unlock()
	return sch, nil
}
```

> Now remove any temporary `bytesReader` copy you added in `registry.go` (Task 3 note) — the
> canonical one lives here in the same package.

- [ ] **Step 4: Run it to confirm it passes**

Run: `go build ./... && go test -tags=integration -timeout 300s ./internal/schema/...`
Expected: PASS (validator tests + all prior schema tests).

- [ ] **Step 5: Commit**

```bash
git add internal/schema/validator.go internal/schema/validator_test.go internal/schema/registry.go
git commit -m "feat: active-schema validator with compiled-schema cache"
```

---

## Task 6: Wire validation into the record write pipeline

**Files:**
- Modify: `internal/queries/records.sql`, `internal/record/record.go`, `internal/record/record_test.go`, `internal/api/api_test.go`, `cmd/substrate/main.go`
- Generated: `internal/db/records.sql.go`
- Test: `internal/record/typed_test.go`

Records gain a `schema_version` stamp on typed writes, and `record.Service` calls an injected `Validator` on create/update.

- [ ] **Step 1: Add schema_version to the record write queries**

In `internal/queries/records.sql`, change `InsertRecord` and `UpdateRecordData` to set `schema_version` (add a parameter). Replace those two queries with:
```sql
-- name: InsertRecord :exec
INSERT INTO records (id, collection_id, workspace_id, data, revision, status, actor, schema_version)
VALUES ($1, $2, $3, $4, $5, 'active', $6, $7);

-- name: UpdateRecordData :exec
UPDATE records SET data = $4, revision = $5, actor = $6, schema_version = $7, updated_at = now()
WHERE workspace_id = $1 AND collection_id = $2 AND id = $3;
```
Run `go tool sqlc generate`. `InsertRecordParams`/`UpdateRecordDataParams` gain a
`SchemaVersion pgtype.Int4` field.

- [ ] **Step 2: Define the Validator interface and inject it (write the failing typed test)**

Create `internal/record/typed_test.go`:
```go
//go:build integration

package record

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/substrate/substrate/internal/apierr"
	"github.com/substrate/substrate/internal/collection"
	"github.com/substrate/substrate/internal/schema"
	"github.com/substrate/substrate/internal/store"
	"github.com/substrate/substrate/internal/workspace"
)

func TestTypedCreateValidatesAndStamps(t *testing.T) {
	pool := store.NewTestPool(t)
	ctx := context.Background()
	ws, _ := workspace.New(pool).CreateWorkspace(ctx, "acme")
	col, _ := collection.New(pool).Create(ctx, ws.ID, "people")
	reg := schema.New(pool)
	_, err := reg.Register(ctx, schema.RegisterCmd{
		Workspace: ws.ID, Collection: col.ID,
		JSONSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"name": map[string]any{"type": "string"}},
			"required":   []any{"name"},
		},
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	svc := New(pool, schema.NewValidator(reg))

	// Valid create stamps schema_version=1.
	rec, err := svc.Create(ctx, CreateCmd{Workspace: ws.ID, Collection: col.ID, Data: map[string]any{"name": "Ada"}})
	if err != nil {
		t.Fatalf("valid create: %v", err)
	}
	var sv int
	if e := pool.QueryRow(ctx, `SELECT schema_version FROM records WHERE id=$1`, rec.ID).Scan(&sv); e != nil {
		t.Fatalf("read schema_version: %v", e)
	}
	if sv != 1 {
		t.Fatalf("schema_version = %d, want 1", sv)
	}

	// Invalid create -> validation_failed.
	_, err = svc.Create(ctx, CreateCmd{Workspace: ws.ID, Collection: col.ID, Data: map[string]any{"name": 123}})
	e, ok := apierr.As(err)
	if !ok || e.Code != apierr.Validation {
		t.Fatalf("invalid create: %v, want validation_failed", err)
	}
}

var _ = uuid.Nil
```

- [ ] **Step 3: Run it to confirm it fails**

Run: `go test -tags=integration ./internal/record/...`
Expected: FAIL — `New` takes 1 arg, not 2; `Validator` undefined.

- [ ] **Step 4: Add the Validator interface, change `New`, validate + stamp**

In `internal/record/record.go`:

(a) Add the interface and a field, change `New`:
```go
// Validator validates a record body against a collection's active schema and
// returns the schema version to stamp (0 for flexible collections).
type Validator interface {
	ValidateWrite(ctx context.Context, collectionID uuid.UUID, data map[string]any) (int, error)
}

// Service performs record mutations and reads.
type Service struct {
	pool      *pgxpool.Pool
	q         *db.Queries
	validator Validator
}

// New builds a record service. validator may be nil (flexible-only; no validation).
func New(pool *pgxpool.Pool, validator Validator) *Service {
	return &Service{pool: pool, q: db.New(pool), validator: validator}
}

// schemaVersionParam validates (if a validator is configured) and returns the
// pgtype.Int4 to stamp on the record. version 0 -> NULL (flexible / grandfathered).
func (s *Service) schemaVersionParam(ctx context.Context, col uuid.UUID, data map[string]any) (pgtype.Int4, error) {
	if s.validator == nil {
		return pgtype.Int4{}, nil
	}
	ver, err := s.validator.ValidateWrite(ctx, col, data)
	if err != nil {
		return pgtype.Int4{}, err
	}
	if ver == 0 {
		return pgtype.Int4{}, nil
	}
	return pgtype.Int4{Int32: int32(ver), Valid: true}, nil
}
```
Ensure `"github.com/jackc/pgx/v5/pgtype"` is imported (it already is).

(b) In `Create`, validate BEFORE opening the transaction (validation is a read; failing early avoids an empty tx), then pass the stamp to `InsertRecord`:
```go
func (s *Service) Create(ctx context.Context, cmd CreateCmd) (Record, error) {
	rec := Record{
		ID: uuid.New(), Collection: cmd.Collection, Data: cmd.Data,
		Revision: 1, Status: "active", Actor: cmd.Actor,
	}
	if rec.Data == nil {
		rec.Data = map[string]any{}
	}
	sv, err := s.schemaVersionParam(ctx, cmd.Collection, rec.Data)
	if err != nil {
		return Record{}, err
	}
	err = store.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		qtx := s.q.WithTx(tx)
		if cmd.IdempotencyKey != "" {
			if replayed, ok, err := lookupReplay(ctx, qtx, cmd.Workspace, cmd.IdempotencyKey); err != nil {
				return err
			} else if ok {
				rec = replayed
				return nil
			}
		}
		if err := appendEvent(ctx, qtx, eventRow{
			Workspace: cmd.Workspace, Collection: cmd.Collection, RecordID: rec.ID,
			Type: "create", Revision: 1, State: rec.Data, Actor: cmd.Actor,
			IdempotencyKey: cmd.IdempotencyKey,
		}); err != nil {
			return err
		}
		return qtx.InsertRecord(ctx, db.InsertRecordParams{
			ID: rec.ID, CollectionID: cmd.Collection, WorkspaceID: cmd.Workspace,
			Data: mustJSON(rec.Data), Revision: 1, Actor: textOrNull(cmd.Actor),
			SchemaVersion: sv,
		})
	})
	if err != nil {
		if errors.Is(err, errIdempotencyConflict) && cmd.IdempotencyKey != "" {
			replayed, ok, rerr := lookupReplay(ctx, s.q, cmd.Workspace, cmd.IdempotencyKey)
			if rerr != nil {
				return Record{}, fmt.Errorf("replay after conflict: %w", rerr)
			}
			if ok {
				return replayed, nil
			}
		}
		return Record{}, err
	}
	return rec, nil
}
```

(c) In `Update`, after the revision check passes, compute the stamp and pass it to
`UpdateRecordData`. Add, just before the `appendEvent` call inside the tx:
```go
		sv, verr := s.schemaVersionParam(ctx, cmd.Collection, cmd.Data)
		if verr != nil {
			return verr
		}
```
and change the `UpdateRecordData` call to include `SchemaVersion: sv`:
```go
		if err := qtx.UpdateRecordData(ctx, db.UpdateRecordDataParams{
			WorkspaceID: cmd.Workspace, CollectionID: cmd.Collection, ID: cmd.ID,
			Data: mustJSON(cmd.Data), Revision: next, Actor: textOrNull(cmd.Actor),
			SchemaVersion: sv,
		}); err != nil {
			return err
		}
```
(`cmd.Data` is already defaulted to `map[string]any{}` just above in the existing code; keep that line. Validation inside the tx is acceptable here since update already holds a row lock.)

- [ ] **Step 5: Update the other `record.New` call sites**

These now must pass a validator (use `nil` where flexible behavior is intended):
- `internal/record/record_test.go` — the `setup` helper: `return New(pool, nil), ws.ID, c.ID`.
- `internal/api/api_test.go` — `Records: record.New(pool, nil)` in the test server (Plan 1 HTTP tests are flexible).
- `cmd/substrate/main.go` — `record.New(pool, schema.NewValidator(schema.New(pool)))` (import `internal/schema`).

- [ ] **Step 6: Run the record tests**

Run: `go test -tags=integration -timeout 300s ./internal/record/...`
Expected: PASS (Plan 1 record tests with `nil` validator + `TestTypedCreateValidatesAndStamps`).

- [ ] **Step 7: Build, full unit tests, commit**

Run: `go build ./... && go test ./...`
Expected: clean.
```bash
git add internal/queries/records.sql internal/db internal/record cmd/substrate internal/api/api_test.go
git commit -m "feat: validate typed record writes against active schema and stamp schema_version"
```

---

## Task 7: HTTP schema endpoints + router wiring

**Files:**
- Create: `internal/api/schema_handlers.go`
- Modify: `internal/api/router.go`
- Test: `internal/api/schema_api_test.go`

- [ ] **Step 1: Write the failing end-to-end test**

Create `internal/api/schema_api_test.go`:
```go
//go:build integration

package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/substrate/substrate/internal/collection"
	"github.com/substrate/substrate/internal/record"
	"github.com/substrate/substrate/internal/schema"
	"github.com/substrate/substrate/internal/store"
	"github.com/substrate/substrate/internal/workspace"
)

func newSchemaServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	pool := store.NewTestPool(t)
	ws := workspace.New(pool)
	w, _ := ws.CreateWorkspace(t.Context(), "acme")
	key, _, _ := ws.CreateAPIKey(t.Context(), w.ID, "test")
	reg := schema.New(pool)
	srv := httptest.NewServer(NewRouter(Deps{
		Workspaces:  ws,
		Collections: collection.New(pool),
		Records:     record.New(pool, schema.NewValidator(reg)),
		Schemas:     reg,
	}))
	t.Cleanup(srv.Close)
	return srv, key
}

func sreq(t *testing.T, method, url, key, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(method, url, bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("X-Substrate-Actor", "agent-1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	return resp
}

func TestSchemaLifecycleOverHTTP(t *testing.T) {
	srv, key := newSchemaServer(t)
	base := srv.URL + "/v1/collections/people"

	// Create collection (flexible).
	sreq(t, "POST", srv.URL+"/v1/collections", key, `{"name":"people"}`).Body.Close()

	// Register + activate first schema (auto-activates anyway).
	resp := sreq(t, "POST", base+"/schemas", key,
		`{"json_schema":{"type":"object","properties":{"name":{"type":"string"}},"required":["name"]}}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("register status = %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Valid typed record create -> 201.
	resp = sreq(t, "POST", base+"/records", key, `{"data":{"name":"Ada"}}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("valid create status = %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Invalid typed record create -> 422.
	resp = sreq(t, "POST", base+"/records", key, `{"data":{"name":123}}`)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("invalid create status = %d, want 422", resp.StatusCode)
	}
	resp.Body.Close()

	// Register v2 breaking (adds required age), then activate -> 409.
	resp = sreq(t, "POST", base+"/schemas", key,
		`{"json_schema":{"type":"object","properties":{"name":{"type":"string"},"age":{"type":"integer"}},"required":["name","age"]}}`)
	var reg struct{ Version int `json:"version"` }
	_ = json.NewDecoder(resp.Body).Decode(&reg)
	resp.Body.Close()

	resp = sreq(t, "POST", base+"/schemas/2:activate", key, `{}`)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("breaking activate status = %d, want 409", resp.StatusCode)
	}
	resp.Body.Close()

	// Force activate -> 200/204.
	resp = sreq(t, "POST", base+"/schemas/2:activate", key, `{"force":true,"rationale":"intended"}`)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		t.Fatalf("forced activate status = %d", resp.StatusCode)
	}
	resp.Body.Close()
}
```

- [ ] **Step 2: Run it to confirm it fails**

Run: `go test -tags=integration ./internal/api/...`
Expected: FAIL — `Deps` has no `Schemas`.

- [ ] **Step 3: Implement the schema handlers**

Create `internal/api/schema_handlers.go`:
```go
package api

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/substrate/substrate/internal/apierr"
	"github.com/substrate/substrate/internal/auth"
	"github.com/substrate/substrate/internal/httpx"
	"github.com/substrate/substrate/internal/schema"
)

type schemaHandlers struct {
	collections *collectionResolver
	schemas     *schema.Service
}

// collectionResolver is the subset of collection lookup the schema handlers need.
type collectionResolver struct{ h *handlers }

func (sh *schemaHandlers) register(w http.ResponseWriter, r *http.Request) {
	c, err := sh.h().resolveCollection(r, r.PathValue("collection"))
	if err != nil {
		writeErr(w, err)
		return
	}
	var body struct {
		JSONSchema    map[string]any `json:"json_schema"`
		IndexedFields []string       `json:"indexed_fields"`
		Activate      bool           `json:"activate"`
		Force         bool           `json:"force"`
		Rationale     string         `json:"rationale"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, apierr.New(apierr.BadRequest, "invalid json"))
		return
	}
	out, err := sh.schemas.Register(r.Context(), schema.RegisterCmd{
		Workspace: c.WorkspaceID, Collection: c.ID, Actor: auth.ActorFrom(r.Context()),
		JSONSchema: body.JSONSchema, IndexedFields: body.IndexedFields,
		Activate: body.Activate, Force: body.Force, Rationale: body.Rationale,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, out)
}

func (sh *schemaHandlers) list(w http.ResponseWriter, r *http.Request) {
	c, err := sh.h().resolveCollection(r, r.PathValue("collection"))
	if err != nil {
		writeErr(w, err)
		return
	}
	out, err := sh.schemas.List(r.Context(), c.ID)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

func (sh *schemaHandlers) get(w http.ResponseWriter, r *http.Request) {
	c, err := sh.h().resolveCollection(r, r.PathValue("collection"))
	if err != nil {
		writeErr(w, err)
		return
	}
	ver, err := strconv.Atoi(r.PathValue("version"))
	if err != nil {
		writeErr(w, apierr.New(apierr.BadRequest, "invalid version"))
		return
	}
	out, err := sh.schemas.Get(r.Context(), c.ID, ver)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

func (sh *schemaHandlers) activate(w http.ResponseWriter, r *http.Request) {
	c, err := sh.h().resolveCollection(r, r.PathValue("collection"))
	if err != nil {
		writeErr(w, err)
		return
	}
	ver, err := strconv.Atoi(r.PathValue("version"))
	if err != nil {
		writeErr(w, apierr.New(apierr.BadRequest, "invalid version"))
		return
	}
	var body struct {
		Force     bool   `json:"force"`
		Rationale string `json:"rationale"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if err := sh.schemas.Activate(r.Context(), c.WorkspaceID, c.ID, ver, auth.ActorFrom(r.Context()), body.Force, body.Rationale); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (sh *schemaHandlers) deprecate(w http.ResponseWriter, r *http.Request) {
	c, err := sh.h().resolveCollection(r, r.PathValue("collection"))
	if err != nil {
		writeErr(w, err)
		return
	}
	ver, err := strconv.Atoi(r.PathValue("version"))
	if err != nil {
		writeErr(w, apierr.New(apierr.BadRequest, "invalid version"))
		return
	}
	if err := sh.schemas.Deprecate(r.Context(), c.ID, ver); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// h returns the underlying record/collection handlers (set during wiring).
func (sh *schemaHandlers) h() *handlers { return sh.collections.h }
```

> The handler reuses the existing `handlers.resolveCollection(r, name)` (from Plan 1's
> `handlers.go`) which looks up a collection by name within the request's workspace and returns
> a `collection.Collection` (with `ID` and `WorkspaceID`). The `collectionResolver` wrapper just
> carries a pointer to that `*handlers`. If simpler, give `schemaHandlers` a direct
> `*handlers` field instead of the wrapper — match whatever compiles cleanly against the
> existing `handlers` type.

- [ ] **Step 4: Wire routes and the Deps field**

In `internal/api/router.go`: add `Schemas *schema.Service` to `Deps` (import `internal/schema`); in the non-health branch, after building `h`, add:
```go
	sh := &schemaHandlers{collections: &collectionResolver{h: h}, schemas: d.Schemas}
	api.HandleFunc("POST /v1/collections/{collection}/schemas", sh.register)
	api.HandleFunc("GET /v1/collections/{collection}/schemas", sh.list)
	api.HandleFunc("GET /v1/collections/{collection}/schemas/{version}", sh.get)
	api.HandleFunc("POST /v1/collections/{collection}/schemas/{version}:activate", sh.activate)
	api.HandleFunc("POST /v1/collections/{collection}/schemas/{version}:deprecate", sh.deprecate)
```
(These mount on the same auth-protected `api` mux as the record routes.)

> Go 1.22+ `ServeMux` treats `{version}` as a path segment; the `:activate` suffix is part of
> the literal segment pattern `{version}:activate`. If the mux rejects a wildcard-plus-literal
> in one segment, fall back to a subpath: `POST /v1/collections/{collection}/schemas/{version}/activate`
> and update the test URLs to `/schemas/2/activate`. Pick whichever the router accepts; keep the
> test and routes consistent.

- [ ] **Step 5: Run the end-to-end test**

Run: `go test -tags=integration -timeout 300s ./internal/api/...`
Expected: PASS (`TestSchemaLifecycleOverHTTP` + existing api tests).

- [ ] **Step 6: Full suite, build, commit**

Run: `go build ./... && go test ./... && go test -tags=integration -timeout 600s ./...`
Expected: all pass.
```bash
git add internal/api
git commit -m "feat: HTTP schema registry endpoints (register/list/get/activate/deprecate)"
```

---

## Self-review notes (for the implementer)

- **Spec coverage:** Task 1 = schemas table + queries + error codes (spec §3, §7). Task 2 = compatibility classifier (spec §6, decision #3). Task 3 = register/get/list + version allocation + first-auto-activate + `activate=true` (spec §4, decision #1). Task 4 = activate/deprecate + compatibility gate + `force` + lifecycle events (spec §3, §4, §6). Task 5 = validator + compiled cache (spec §5, decision #5). Task 6 = validation in the record pipeline + `schema_version` stamp + grandfather-on-next-write (spec §5, decision #2). Task 7 = HTTP endpoints (spec §4). Deferred: index DDL (Plan 3), backfill (Plan 5) — correctly absent.
- **`record.New` signature change** ripples to: `record_test.go` setup, `api/api_test.go`, `cmd/substrate/main.go`, and the new typed tests. Task 6 Step 5 enumerates them — the build fails loudly if one is missed.
- **sqlc generated names:** verify `InsertSchemaParams.JsonSchema`, `GetActiveSchemaRow.{Version,JsonSchema}`, `LockCollectionRow.{WorkspaceID,Level,ActiveSchemaVersion}`, and `SchemaVersion pgtype.Int4` after each `go tool sqlc generate`; match the service code to the emitted identifiers (gopls may lag — trust `go build`).
- **jsonschema/v6 API:** the exact compiler entrypoints (`UnmarshalJSON`, `AddResource`, `Compile`, `Schema.Validate`) should be confirmed against the installed version during Task 2/5; if names differ, adapt — the contract is "compile a schema doc, validate a `map[string]any`, get a non-nil error on failure." Run `go doc github.com/santhosh-tekuri/jsonschema/v6` first.
- **Run** `go test ./... && go test -tags=integration ./...` at the end.
```
