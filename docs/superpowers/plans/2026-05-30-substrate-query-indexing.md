# Substrate Plan 3 — Query & Indexing Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement the `GET /v1/collections/{c}/records` list endpoint with filtering, sorting, and opaque keyset pagination, plus auto-created JSONB expression indexes consuming each schema's `indexed_fields`.

**Architecture:** A pure `internal/query` package parses request params into a `ListQuery`, builds a parameterized SQL statement (the one place we hand-build SQL, since sqlc handles only static queries), and encodes/decodes opaque keyset cursors. The record service gains a `List` method that runs the built query. Schema activation gains an optional `Indexer` seam that creates partial, per-collection expression indexes post-commit.

**Tech Stack:** Go 1.26, pgx/v5 (`pool.Query`), net/http `ServeMux`, testcontainers for integration tests. Builds on Plan 1 (record core) + Plan 2 (schema registry).

---

## Design reference

Spec: `docs/superpowers/specs/2026-05-30-substrate-query-indexing-design.md`. Read it before starting.

Relevant existing code to match patterns:
- `internal/record/record.go` — `Record` struct, `Service{pool,q}`, `Validator` seam, `mustJSON`/`textOrNull` helpers.
- `internal/schema/registry.go` — `activateTx`, `Register`, `Activate`; the `Validator`-style optional dependency pattern.
- `internal/api/handlers.go` + `router.go` — handler shape, `resolveCollection`, `writeErr`, route registration (note: Go ServeMux can't do `{x}:literal` segments).
- `internal/apierr/apierr.go` — `BadRequest` (400) code already exists; reuse it.
- `internal/migrations/00001_init.sql` — `records` columns: `id, collection_id, workspace_id, schema_version, data, revision, status, actor, created_at, updated_at`; PK `(collection_id, id)`.

Field/value conventions (from spec §4):
- field regex: `^[a-zA-Z_][a-zA-Z0-9_]*$`; system columns: `created_at, updated_at, revision, id`.
- ops: `eq, neq, gt, gte, lt, lte, in, exists`.
- default sort `-created_at`; `id` appended as same-direction tiebreaker; default limit 50, max 200.

---

## Task 1: Filter/sort/limit parser

**Files:**
- Create: `internal/query/filter.go`
- Test: `internal/query/filter_test.go`

- [ ] **Step 1: Write the failing test**

```go
package query

import "testing"

func TestParse_Defaults(t *testing.T) {
	q, err := Parse(nil, "", "", "")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if q.Limit != 50 {
		t.Fatalf("default limit = %d, want 50", q.Limit)
	}
	if len(q.Sort) != 1 || q.Sort[0].Field != "created_at" || !q.Sort[0].Desc {
		t.Fatalf("default sort = %+v, want [-created_at]", q.Sort)
	}
	if len(q.Filters) != 0 {
		t.Fatalf("filters = %+v, want none", q.Filters)
	}
}

func TestParse_Filters(t *testing.T) {
	q, err := Parse([]string{"status:eq:open", "age:gt:21", "tags:in:a,b", "note:exists:false"}, "", "100", "")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if q.Limit != 100 {
		t.Fatalf("limit = %d, want 100", q.Limit)
	}
	if len(q.Filters) != 4 {
		t.Fatalf("filters = %d, want 4", len(q.Filters))
	}
	if q.Filters[0].Field != "status" || q.Filters[0].Op != "eq" || q.Filters[0].Value != "open" {
		t.Fatalf("filter0 = %+v", q.Filters[0])
	}
	if q.Filters[2].Op != "in" || len(q.Filters[2].List) != 2 {
		t.Fatalf("in filter = %+v", q.Filters[2])
	}
	if q.Filters[3].Op != "exists" || q.Filters[3].Value != "false" {
		t.Fatalf("exists filter = %+v", q.Filters[3])
	}
}

func TestParse_SortAscDesc(t *testing.T) {
	q, err := Parse(nil, "price", "", "")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if q.Sort[0].Field != "price" || q.Sort[0].Desc {
		t.Fatalf("sort = %+v, want price asc", q.Sort)
	}
}

func TestParse_LimitClamp(t *testing.T) {
	q, err := Parse(nil, "", "9999", "")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if q.Limit != 200 {
		t.Fatalf("limit = %d, want clamp to 200", q.Limit)
	}
}

func TestParse_Errors(t *testing.T) {
	cases := [][]string{
		{"bad-syntax"},        // no colons
		{"f:bogus:v"},         // unknown op
		{"1bad:eq:v"},         // bad identifier
		{"note:exists:maybe"}, // exists needs true/false
	}
	for _, f := range cases {
		if _, err := Parse(f, "", "", ""); err == nil {
			t.Fatalf("expected error for %v", f)
		}
	}
	if _, err := Parse(nil, "", "abc", ""); err == nil {
		t.Fatal("expected error for non-numeric limit")
	}
	if _, err := Parse(nil, "1bad", "", ""); err == nil {
		t.Fatal("expected error for bad sort identifier")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/query/ -run TestParse -v`
Expected: FAIL — `undefined: Parse` / package has no Go files.

- [ ] **Step 3: Write minimal implementation**

```go
// Package query parses, builds, and paginates record list queries, and manages
// the JSONB expression indexes that back them.
package query

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/substrate/substrate/internal/apierr"
)

const (
	defaultLimit = 50
	maxLimit     = 200
)

var (
	fieldRe    = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)
	validOps   = map[string]bool{"eq": true, "neq": true, "gt": true, "gte": true, "lt": true, "lte": true, "in": true, "exists": true}
	systemCols = map[string]bool{"created_at": true, "updated_at": true, "revision": true, "id": true}
)

// Filter is one parsed predicate.
type Filter struct {
	Field string
	Op    string
	Value string   // raw value for scalar ops; "true"/"false" for exists
	List  []string // populated for the "in" op
}

// SortKey is one ORDER BY term.
type SortKey struct {
	Field string
	Desc  bool
}

// ListQuery is the parsed, validated representation of a list request.
type ListQuery struct {
	Filters []Filter
	Sort    []SortKey
	Limit   int
	Cursor  string // raw opaque cursor token (decoded by the builder)
}

func badRequest(msg string) error { return apierr.New(apierr.BadRequest, msg) }

// Parse validates raw query params into a ListQuery. filters is the repeated
// "filter" param; sort/limit/cursor are their single string forms ("" = unset).
func Parse(filters []string, sort, limit, cursor string) (ListQuery, error) {
	q := ListQuery{Limit: defaultLimit, Cursor: cursor}

	for _, raw := range filters {
		parts := strings.SplitN(raw, ":", 3)
		if len(parts) != 3 {
			return ListQuery{}, badRequest(fmt.Sprintf("filter %q must be field:op:value", raw))
		}
		field, op, val := parts[0], parts[1], parts[2]
		if err := checkField(field); err != nil {
			return ListQuery{}, err
		}
		if !validOps[op] {
			return ListQuery{}, badRequest(fmt.Sprintf("unknown filter op %q", op))
		}
		f := Filter{Field: field, Op: op, Value: val}
		switch op {
		case "in":
			f.List = strings.Split(val, ",")
		case "exists":
			if val != "true" && val != "false" {
				return ListQuery{}, badRequest("exists value must be true or false")
			}
		}
		q.Filters = append(q.Filters, f)
	}

	if sort == "" {
		q.Sort = []SortKey{{Field: "created_at", Desc: true}}
	} else {
		field, desc := sort, false
		if strings.HasPrefix(sort, "-") {
			field, desc = sort[1:], true
		}
		if err := checkField(field); err != nil {
			return ListQuery{}, err
		}
		q.Sort = []SortKey{{Field: field, Desc: desc}}
	}

	if limit != "" {
		n, err := strconv.Atoi(limit)
		if err != nil || n <= 0 {
			return ListQuery{}, badRequest("limit must be a positive integer")
		}
		if n > maxLimit {
			n = maxLimit
		}
		q.Limit = n
	}

	return q, nil
}

func checkField(field string) error {
	if systemCols[field] || fieldRe.MatchString(field) {
		return nil
	}
	return badRequest(fmt.Sprintf("invalid field name %q", field))
}

// isSystemCol reports whether a field maps to a real records column rather than
// a JSON path. Used by the builder.
func isSystemCol(field string) bool { return systemCols[field] }
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/query/ -run TestParse -v`
Expected: PASS (all subtests).

- [ ] **Step 5: Commit**

```bash
git add internal/query/filter.go internal/query/filter_test.go
git commit -m "feat: list-query param parser (filter/sort/limit)"
```

---

## Task 2: Opaque keyset cursor codec

**Files:**
- Create: `internal/query/cursor.go`
- Test: `internal/query/cursor_test.go`

- [ ] **Step 1: Write the failing test**

```go
package query

import "testing"

func TestCursor_RoundTrip(t *testing.T) {
	c := cursorData{Sort: "-created_at", Value: "2026-05-30T12:00:00Z", ID: "11111111-1111-1111-1111-111111111111"}
	tok := encodeCursor(c)
	if tok == "" {
		t.Fatal("empty token")
	}
	got, err := decodeCursor(tok)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got != c {
		t.Fatalf("round-trip mismatch: %+v != %+v", got, c)
	}
}

func TestCursor_Garbled(t *testing.T) {
	if _, err := decodeCursor("not-base64-!!!"); err == nil {
		t.Fatal("expected error for garbled cursor")
	}
	if _, err := decodeCursor("YWJj"); err == nil { // "abc", not JSON
		t.Fatal("expected error for non-JSON cursor")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/query/ -run TestCursor -v`
Expected: FAIL — `undefined: cursorData/encodeCursor/decodeCursor`.

- [ ] **Step 3: Write minimal implementation**

```go
package query

import (
	"encoding/base64"
	"encoding/json"
)

// cursorData is the decoded payload of an opaque keyset cursor.
type cursorData struct {
	Sort  string `json:"s"`  // normalized sort spec the cursor was produced under
	Value string `json:"v"`  // last row's sort-key value, as a string
	ID    string `json:"id"` // last row's id (final tiebreaker)
}

func encodeCursor(c cursorData) string {
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeCursor(tok string) (cursorData, error) {
	raw, err := base64.RawURLEncoding.DecodeString(tok)
	if err != nil {
		return cursorData{}, badRequest("invalid cursor encoding")
	}
	var c cursorData
	if err := json.Unmarshal(raw, &c); err != nil {
		return cursorData{}, badRequest("invalid cursor payload")
	}
	return c, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/query/ -run TestCursor -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/query/cursor.go internal/query/cursor_test.go
git commit -m "feat: opaque keyset cursor codec"
```

---

## Task 3: SQL builder

**Files:**
- Create: `internal/query/builder.go`
- Test: `internal/query/builder_test.go`

Design notes for the implementer:
- Output: `(sqlText string, args []any, err error)`.
- `args[0]` = workspace uuid, `args[1]` = collection uuid; subsequent args are filter/keyset values, numbered `$3`, `$4`, … in order appended.
- Column expression: system col → bare column name; data field → `data->>'<field>'` (the field is a validated identifier, safe to embed).
- Predicates by op:
  - `eq`  → `<expr> = $n`
  - `neq` → `<expr> IS DISTINCT FROM $n`
  - `in`  → `<expr> = ANY($n)` with arg a `[]string`
  - `exists` → `(data ? '<field>') = $n` with arg a `bool` (skip for system cols → error)
  - range (`gt/gte/lt/lte`) → cast inferred from value via `castExpr`: numeric → `(<expr>)::numeric`, bool → `::boolean`, else text; operator from a map (`gt`→`>`, etc.). System cols cast the param to the column type instead.
- Sort: `ORDER BY <sortexpr> <dir>, id <dir>` where `<dir>` is `DESC`/`ASC`. For a data-field sort, `<sortexpr>` = `data->>'field'` (text). System col sorts use the column.
- Keyset (when `Cursor != ""`): decode, verify `Sort` matches the normalized sort string (else `badRequest`), then add a row-value predicate:
  - DESC: `(<sortexpr>, id) < ($k::<type>, $kid::uuid)`
  - ASC:  `(<sortexpr>, id) > ($k::<type>, $kid::uuid)`
  - `<type>`: `timestamptz` for created_at/updated_at, `bigint` for revision, `uuid` for id, else `text` (data field).
- `LIMIT` is `q.Limit + 1` (caller drops the extra row). Emit the literal number (it is an int, not user text) or bind it as an arg — either is acceptable; binding is cleaner.
- Provide `normalizeSort(q.Sort) string` returning e.g. `-created_at` / `price`; used both in the keyset check and when producing the next cursor (Task 4).

- [ ] **Step 1: Write the failing test**

```go
package query

import (
	"strings"
	"testing"
)

func mustBuild(t *testing.T, q ListQuery) (string, []any) {
	t.Helper()
	sql, args, err := Build(q)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	return sql, args
}

func TestBuild_BaseFilterSortLimit(t *testing.T) {
	q, _ := Parse([]string{"status:eq:open"}, "-created_at", "10", "")
	sql, args := mustBuild(t, q)
	if !strings.Contains(sql, "workspace_id = $1 AND collection_id = $2") {
		t.Fatalf("missing scope predicate:\n%s", sql)
	}
	if !strings.Contains(sql, "status = 'active'") {
		t.Fatalf("missing active filter:\n%s", sql)
	}
	if !strings.Contains(sql, "data->>'status' = $3") {
		t.Fatalf("missing eq predicate:\n%s", sql)
	}
	if !strings.Contains(sql, "ORDER BY created_at DESC, id DESC") {
		t.Fatalf("bad order by:\n%s", sql)
	}
	// args: ws placeholder + col placeholder are filled by caller; builder appends value args.
	if len(args) < 1 || args[len(args)-1] != "open" {
		t.Fatalf("args = %v, want last 'open'", args)
	}
}

func TestBuild_RangeNumericCast(t *testing.T) {
	q, _ := Parse([]string{"age:gt:21"}, "", "", "")
	sql, _ := mustBuild(t, q)
	if !strings.Contains(sql, "(data->>'age')::numeric > $3") {
		t.Fatalf("missing numeric cast:\n%s", sql)
	}
}

func TestBuild_InAndExists(t *testing.T) {
	q, _ := Parse([]string{"tags:in:a,b", "note:exists:false"}, "", "", "")
	sql, args := mustBuild(t, q)
	if !strings.Contains(sql, "data->>'tags' = ANY($3)") {
		t.Fatalf("missing in predicate:\n%s", sql)
	}
	if !strings.Contains(sql, "(data ? 'note') = $4") {
		t.Fatalf("missing exists predicate:\n%s", sql)
	}
	// in arg is a []string; exists arg is a bool.
	if _, ok := args[len(args)-2].([]string); !ok {
		t.Fatalf("in arg type = %T, want []string", args[len(args)-2])
	}
	if b, ok := args[len(args)-1].(bool); !ok || b != false {
		t.Fatalf("exists arg = %v, want bool false", args[len(args)-1])
	}
}

func TestBuild_KeysetFromCursor(t *testing.T) {
	tok := encodeCursor(cursorData{Sort: "-created_at", Value: "2026-05-30T12:00:00Z", ID: "11111111-1111-1111-1111-111111111111"})
	q, _ := Parse(nil, "-created_at", "", tok)
	sql, _ := mustBuild(t, q)
	if !strings.Contains(sql, "(created_at, id) < (") {
		t.Fatalf("missing keyset predicate:\n%s", sql)
	}
}

func TestBuild_CursorSortMismatch(t *testing.T) {
	tok := encodeCursor(cursorData{Sort: "price", Value: "9", ID: "11111111-1111-1111-1111-111111111111"})
	q, _ := Parse(nil, "-created_at", "", tok) // request sort != cursor sort
	if _, _, err := Build(q); err == nil {
		t.Fatal("expected cursor/sort mismatch error")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/query/ -run TestBuild -v`
Expected: FAIL — `undefined: Build`.

- [ ] **Step 3: Write minimal implementation**

```go
package query

import (
	"fmt"
	"strconv"
	"strings"
)

var rangeOps = map[string]string{"gt": ">", "gte": ">=", "lt": "<", "lte": "<="}

// colExpr returns the SQL expression for a field: a bare column for system
// fields, else a JSONB text extraction.
func colExpr(field string) string {
	if isSystemCol(field) {
		return field
	}
	return "data->>'" + field + "'"
}

// colType returns the SQL type a system column / data field sorts and keysets as.
func colType(field string) string {
	switch field {
	case "created_at", "updated_at":
		return "timestamptz"
	case "revision":
		return "bigint"
	case "id":
		return "uuid"
	default:
		return "text"
	}
}

// inferCast picks the cast for a range comparison from the raw value.
func inferCast(expr, value string) string {
	if _, err := strconv.ParseFloat(value, 64); err == nil {
		return "(" + expr + ")::numeric"
	}
	if value == "true" || value == "false" {
		return "(" + expr + ")::boolean"
	}
	return expr
}

// normalizeSort renders the sort spec back to its canonical "-field"/"field" string.
func normalizeSort(keys []SortKey) string {
	if len(keys) == 0 {
		return ""
	}
	if keys[0].Desc {
		return "-" + keys[0].Field
	}
	return keys[0].Field
}

// Build renders a ListQuery into a SQL statement. $1 and $2 are reserved for
// workspace_id and collection_id (the caller supplies them as args[0], args[1]);
// value args follow in $3.. order.
func Build(q ListQuery) (string, []any, error) {
	var b strings.Builder
	args := []any{}            // value args, starting logically at $3
	ph := func(v any) string { // append an arg, return its placeholder
		args = append(args, v)
		return "$" + strconv.Itoa(len(args)+2)
	}

	b.WriteString("SELECT id, collection_id, data, revision, status, actor, created_at\n")
	b.WriteString("FROM records\n")
	b.WriteString("WHERE workspace_id = $1 AND collection_id = $2 AND status = 'active'")

	for _, f := range q.Filters {
		expr := colExpr(f.Field)
		switch f.Op {
		case "eq":
			b.WriteString("\n  AND " + expr + " = " + ph(f.Value))
		case "neq":
			b.WriteString("\n  AND " + expr + " IS DISTINCT FROM " + ph(f.Value))
		case "in":
			b.WriteString("\n  AND " + expr + " = ANY(" + ph(f.List) + ")")
		case "exists":
			if isSystemCol(f.Field) {
				return "", nil, badRequest("exists is not supported on system fields")
			}
			b.WriteString("\n  AND (data ? '" + f.Field + "') = " + ph(f.Value == "true"))
		default: // range
			op := rangeOps[f.Op]
			if isSystemCol(f.Field) {
				b.WriteString("\n  AND " + expr + " " + op + " " + ph(f.Value) + "::" + colType(f.Field))
			} else {
				b.WriteString("\n  AND " + inferCast(expr, f.Value) + " " + op + " " + ph(f.Value))
			}
		}
	}

	sortField := q.Sort[0].Field
	sortExpr := colExpr(sortField)
	dir := "ASC"
	cmp := ">"
	if q.Sort[0].Desc {
		dir, cmp = "DESC", "<"
	}

	if q.Cursor != "" {
		c, err := decodeCursor(q.Cursor)
		if err != nil {
			return "", nil, err
		}
		if c.Sort != normalizeSort(q.Sort) {
			return "", nil, badRequest("cursor does not match sort")
		}
		valPH := ph(c.Value)
		idPH := ph(c.ID)
		b.WriteString(fmt.Sprintf("\n  AND (%s, id) %s (%s::%s, %s::uuid)",
			sortExpr, cmp, valPH, colType(sortField), idPH))
	}

	b.WriteString(fmt.Sprintf("\nORDER BY %s %s, id %s", sortExpr, dir, dir))
	b.WriteString("\nLIMIT " + ph(q.Limit+1))

	return b.String(), args, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/query/ -run TestBuild -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/query/builder.go internal/query/builder_test.go
git commit -m "feat: parameterized list-query SQL builder with keyset predicate"
```

---

## Task 4: Index manager + name derivation

**Files:**
- Create: `internal/query/index.go`
- Test: `internal/query/index_test.go` (unit: name derivation only — DDL execution is covered in Task 7 integration)

- [ ] **Step 1: Write the failing test**

```go
package query

import (
	"testing"

	"github.com/google/uuid"
)

func TestIndexName_Deterministic(t *testing.T) {
	col := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	a := indexName(col, "price")
	b := indexName(col, "price")
	if a != b {
		t.Fatalf("non-deterministic: %s != %s", a, b)
	}
	if indexName(col, "price") == indexName(col, "color") {
		t.Fatal("different fields must yield different names")
	}
}

func TestIndexName_LengthBound(t *testing.T) {
	col := uuid.New()
	long := "this_is_an_extremely_long_field_name_that_would_blow_past_the_postgres_identifier_limit"
	if n := indexName(col, long); len(n) > 63 {
		t.Fatalf("index name %q len %d > 63", n, len(n))
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/query/ -run TestIndexName -v`
Expected: FAIL — `undefined: indexName`.

- [ ] **Step 3: Write minimal implementation**

```go
package query

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Indexer creates the expression indexes that back declared indexed_fields.
// It implements the schema package's Indexer seam.
type Indexer struct {
	pool *pgxpool.Pool
}

func NewIndexer(pool *pgxpool.Pool) *Indexer { return &Indexer{pool: pool} }

// indexName derives a deterministic, <=63-char index name from a collection id
// and field. Postgres truncates identifiers at 63 bytes, so we hash to stay
// unique and bounded.
func indexName(col uuid.UUID, field string) string {
	h := sha1.Sum([]byte(col.String() + ":" + field))
	return "idx_rec_" + hex.EncodeToString(h[:])[:24]
}

// EnsureCollectionIndexes idempotently creates a partial text expression index
// per field, scoped to the collection's active records.
func (ix *Indexer) EnsureCollectionIndexes(ctx context.Context, col uuid.UUID, fields []string) error {
	for _, f := range fields {
		if !fieldRe.MatchString(f) {
			// indexed_fields entries are validated at registration; skip anything odd.
			continue
		}
		stmt := fmt.Sprintf(
			`CREATE INDEX IF NOT EXISTS %s ON records ((data->>'%s')) WHERE collection_id = '%s' AND status = 'active'`,
			indexName(col, f), f, col.String(),
		)
		if _, err := ix.pool.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("ensure index on %q: %w", f, err)
		}
	}
	return nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/query/ -run TestIndexName -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/query/index.go internal/query/index_test.go
git commit -m "feat: idempotent JSONB expression index manager"
```

---

## Task 5: Record service List method

**Files:**
- Modify: `internal/record/record.go`
- Test: `internal/record/list_test.go` (integration-tagged)

Design notes:
- Add `func (s *Service) List(ctx, ws, col uuid.UUID, q query.ListQuery) ([]Record, string, error)`.
- Build SQL via `query.Build(q)`; prepend `ws, col` to the args; run `s.pool.Query`.
- Scan rows into `Record` (reuse the existing struct; `created_at` is read into a local `time.Time` used only for cursor production, not stored on `Record`).
- Fetch is `limit+1`: if more than `limit` rows return, drop the extra and produce a `next_cursor` from the last *kept* row via `query.NextCursor`.
- Add a `query.NextCursor(q ListQuery, lastSortValue, lastID string) string` helper in `internal/query/builder.go` that wraps `encodeCursor` using `normalizeSort(q.Sort)`. The sort value string for `created_at` is the RFC3339Nano timestamp; for a data field it is `data->>'field'` (already text). To keep the record service simple, expose a way to read the raw sort key per row — see implementation.

To avoid leaking SQL-shape decisions into the record service, add a small helper in the query package that the record service calls to get the sort-key string for the row. Simplest approach: have `Build` also select the sort key as a trailing column aliased `sort_key`, so the record service reads it generically. Update Task 3's `Build` SELECT accordingly.

**Implementer:** update `Build` (Task 3 file) to append the sort key to the SELECT:

```go
// in Build(), change the SELECT line to also project the sort key as text:
b.WriteString("SELECT id, collection_id, data, revision, status, actor, created_at, (")
b.WriteString(sortKeyProjection(q.Sort[0].Field))
b.WriteString(")::text AS sort_key\n")
```

…but `sortExpr`/`sortField` are computed lower down. Resolve by computing `sortField`/`sortExpr` at the **top** of `Build` (move those four lines up before writing SELECT), then write SELECT using `sortExpr`. Add:

```go
func sortKeyProjection(field string) string { return colExpr(field) }
```

Add `query.NextCursor`:

```go
// NextCursor builds the opaque cursor for the row that ended a page.
func NextCursor(q ListQuery, sortValue, id string) string {
	return encodeCursor(cursorData{Sort: normalizeSort(q.Sort), Value: sortValue, ID: id})
}
```

- [ ] **Step 1: Write the failing test** (integration)

```go
//go:build integration

package record_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/substrate/substrate/internal/collection"
	"github.com/substrate/substrate/internal/query"
	"github.com/substrate/substrate/internal/record"
	"github.com/substrate/substrate/internal/store"
	"github.com/substrate/substrate/internal/workspace"
)

func seedList(t *testing.T) (*record.Service, uuid.UUID, uuid.UUID) {
	t.Helper()
	pool := store.NewTestPool(t)
	ctx := context.Background()
	ws, err := workspace.New(pool).CreateWorkspace(ctx, "acme")
	if err != nil {
		t.Fatalf("ws: %v", err)
	}
	col, err := collection.New(pool).Create(ctx, ws.ID, "items")
	if err != nil {
		t.Fatalf("col: %v", err)
	}
	svc := record.New(pool, nil)
	for _, n := range []struct {
		status string
		age    int
	}{{"open", 30}, {"open", 20}, {"closed", 40}} {
		if _, err := svc.Create(ctx, record.CreateCmd{
			Workspace: ws.ID, Collection: col.ID,
			Data: map[string]any{"status": n.status, "age": n.age},
		}); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	return svc, ws.ID, col.ID
}

func TestList_FilterEq(t *testing.T) {
	svc, ws, col := seedList(t)
	q, _ := query.Parse([]string{"status:eq:open"}, "", "", "")
	items, _, err := svc.List(context.Background(), ws, col, q)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("got %d items, want 2", len(items))
	}
}

func TestList_RangeNumeric(t *testing.T) {
	svc, ws, col := seedList(t)
	q, _ := query.Parse([]string{"age:gte:30"}, "", "", "")
	items, _, err := svc.List(context.Background(), ws, col, q)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("got %d items, want 2 (age>=30)", len(items))
	}
}

func TestList_Pagination(t *testing.T) {
	svc, ws, col := seedList(t)
	q, _ := query.Parse(nil, "-created_at", "2", "")
	page1, cur, err := svc.List(context.Background(), ws, col, q)
	if err != nil {
		t.Fatalf("page1: %v", err)
	}
	if len(page1) != 2 || cur == "" {
		t.Fatalf("page1 len=%d cursor=%q, want 2 + cursor", len(page1), cur)
	}
	q2, _ := query.Parse(nil, "-created_at", "2", cur)
	page2, cur2, err := svc.List(context.Background(), ws, col, q2)
	if err != nil {
		t.Fatalf("page2: %v", err)
	}
	if len(page2) != 1 || cur2 != "" {
		t.Fatalf("page2 len=%d cursor=%q, want 1 + empty", len(page2), cur2)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -tags=integration ./internal/record/ -run TestList -v`
Expected: FAIL — `svc.List undefined`.

- [ ] **Step 3: Write minimal implementation**

Add imports `"time"` and `"github.com/substrate/substrate/internal/query"` to `record.go` (keep existing imports). Then add:

```go
// List runs a parsed list query within a workspace+collection and returns the
// page of records plus an opaque next_cursor ("" when the page is the last one).
func (s *Service) List(ctx context.Context, ws, col uuid.UUID, q query.ListQuery) ([]Record, string, error) {
	sqlText, valueArgs, err := query.Build(q)
	if err != nil {
		return nil, "", err
	}
	args := append([]any{ws, col}, valueArgs...)
	rows, err := s.pool.Query(ctx, sqlText, args...)
	if err != nil {
		return nil, "", fmt.Errorf("list query: %w", err)
	}
	defer rows.Close()

	type scanned struct {
		rec     Record
		created time.Time
		sortKey string
	}
	var all []scanned
	for rows.Next() {
		var (
			sc      scanned
			rawData []byte
			actor   pgtype.Text
		)
		if err := rows.Scan(&sc.rec.ID, &sc.rec.Collection, &rawData, &sc.rec.Revision,
			&sc.rec.Status, &actor, &sc.created, &sc.sortKey); err != nil {
			return nil, "", fmt.Errorf("scan row: %w", err)
		}
		sc.rec.Actor = actor.String
		if err := json.Unmarshal(rawData, &sc.rec.Data); err != nil {
			return nil, "", fmt.Errorf("decode data: %w", err)
		}
		all = append(all, sc)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("iterate rows: %w", err)
	}

	next := ""
	if len(all) > q.Limit {
		last := all[q.Limit-1]
		all = all[:q.Limit]
		next = query.NextCursor(q, last.sortKey, last.rec.ID.String())
	}

	items := make([]Record, len(all))
	for i, sc := range all {
		items[i] = sc.rec
	}
	return items, next, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test -tags=integration ./internal/record/ -run TestList -v`
Expected: PASS (all three subtests).

- [ ] **Step 5: Commit**

```bash
git add internal/record/record.go internal/record/list_test.go internal/query/builder.go
git commit -m "feat: record List with keyset pagination over the query builder"
```

---

## Task 6: HTTP list endpoint

**Files:**
- Modify: `internal/api/handlers.go`
- Modify: `internal/api/router.go`
- Test: `internal/api/list_api_test.go` (integration-tagged)

- [ ] **Step 1: Write the failing test**

```go
//go:build integration

package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// NOTE: reuse this package's existing integration harness for building an
// authed server + workspace key (see schema_api_test.go for the pattern:
// newTestServer / createCollection / authed request helpers). The helper names
// below mirror those; adapt to the actual helpers in this package.

func TestListRecordsHTTP(t *testing.T) {
	env := newTestServer(t)        // existing helper: server + workspace + api key
	col := env.createCollection(t, "items")

	for _, s := range []string{"open", "open", "closed"} {
		env.createRecord(t, col, map[string]any{"status": s})
	}

	req := env.authedGET(t, "/v1/collections/"+col+"/records?filter=status:eq:open&limit=10")
	resp := httptest.NewRecorder()
	env.handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", resp.Code, resp.Body.String())
	}
	var out struct {
		Items      []map[string]any `json:"items"`
		NextCursor string           `json:"next_cursor"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(out.Items))
	}
}

func TestListRecordsBadFilterHTTP(t *testing.T) {
	env := newTestServer(t)
	col := env.createCollection(t, "items")
	req := env.authedGET(t, "/v1/collections/"+col+"/records?filter=bad-syntax")
	resp := httptest.NewRecorder()
	env.handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.Code)
	}
}
```

> **Implementer:** before writing the handler, open `internal/api/schema_api_test.go` and `internal/api/api_test.go` to find the actual test-harness helper names/signatures in this package, and adapt the test above to them (server construction, workspace/key creation, authed request builder, create-collection, create-record). Do not invent helpers that don't exist.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -tags=integration ./internal/api/ -run TestListRecords -v`
Expected: FAIL — route not registered / `listRecords` undefined (404 or compile error).

- [ ] **Step 3: Write minimal implementation**

In `internal/api/handlers.go`, add the import `"github.com/substrate/substrate/internal/query"` and the handler:

```go
func (h *handlers) listRecords(w http.ResponseWriter, r *http.Request) {
	c, err := h.resolveCollection(r, r.PathValue("collection"))
	if err != nil {
		writeErr(w, err)
		return
	}
	v := r.URL.Query()
	q, err := query.Parse(v["filter"], v.Get("sort"), v.Get("limit"), v.Get("cursor"))
	if err != nil {
		writeErr(w, err)
		return
	}
	items, next, err := h.records.List(r.Context(), c.WorkspaceID, c.ID, q)
	if err != nil {
		writeErr(w, err)
		return
	}
	if items == nil {
		items = []record.Record{}
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"items": items, "next_cursor": next})
}
```

In `internal/api/router.go`, register the route **before** the `{id}` routes is not required (paths differ), but add it alongside the other record routes:

```go
	api.HandleFunc("GET /v1/collections/{collection}/records", h.listRecords)
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test -tags=integration ./internal/api/ -run TestListRecords -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/api/handlers.go internal/api/router.go internal/api/list_api_test.go
git commit -m "feat: GET list records endpoint with filter/sort/pagination"
```

---

## Task 7: Wire index creation into schema activation

**Files:**
- Modify: `internal/schema/registry.go`
- Modify: `cmd/substrate` server wiring (wherever `schema.New` is constructed — find it; likely `cmd/substrate/main.go` and/or `internal/api` test setup)
- Test: `internal/schema/index_wiring_test.go` (integration-tagged)

Design notes:
- Add an `Indexer` interface to the schema package and an optional field on `Service`:

```go
// Indexer ensures the expression indexes backing a version's indexed_fields.
type Indexer interface {
	EnsureCollectionIndexes(ctx context.Context, collectionID uuid.UUID, fields []string) error
}
```

- Change `New` to accept an indexer, keeping it optional (nil = skip). To avoid breaking existing callers/tests, add a second constructor rather than changing `New`'s signature:

```go
func New(pool *pgxpool.Pool) *Service { return &Service{pool: pool, q: db.New(pool)} }

// NewWithIndexer wires an optional index manager invoked after activations.
func NewWithIndexer(pool *pgxpool.Pool, ix Indexer) *Service {
	s := New(pool)
	s.indexer = ix
	return s
}
```

- Add `indexer Indexer` to the `Service` struct.
- After a successful activation commits, ensure indexes for the just-activated version's `indexed_fields`. Both the `Register`(activate) path and `Activate` end by committing the txn; ensure **post-commit**. Implement a small helper that, given a collection id, loads the active version's indexed_fields and calls the indexer:

```go
// ensureActiveIndexes runs post-commit; loads the active version's indexed_fields
// and asks the indexer to create them. No-op when no indexer is configured.
func (s *Service) ensureActiveIndexes(ctx context.Context, col uuid.UUID) error {
	if s.indexer == nil {
		return nil
	}
	row, err := s.q.GetActiveSchema(ctx, col)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("load active for indexing: %w", err)
	}
	var fields []string
	_ = json.Unmarshal(row.IndexedFields, &fields)
	if len(fields) == 0 {
		return nil
	}
	return s.indexer.EnsureCollectionIndexes(ctx, col, fields)
}
```

> **Implementer:** confirm `GetActiveSchema` returns `IndexedFields` in its row; if not, either add it to the `GetActiveSchema` query in `internal/queries/schemas.sql` and regenerate sqlc (`mise run sqlc:generate`), or load the fields via `GetSchema` for the active version. Adjust accordingly.

- Call `ensureActiveIndexes` at the end of `Register` (only when the result lifecycle is `active`) and at the end of `Activate`, **after** `store.WithTx` returns nil. Wrap its error and return it (activation already committed; the ensure is idempotent and re-runnable).
- Update server wiring so production uses `NewWithIndexer(pool, query.NewIndexer(pool))`.

- [ ] **Step 1: Write the failing test** (integration)

```go
//go:build integration

package schema_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/substrate/substrate/internal/collection"
	"github.com/substrate/substrate/internal/query"
	"github.com/substrate/substrate/internal/schema"
	"github.com/substrate/substrate/internal/store"
	"github.com/substrate/substrate/internal/workspace"
)

func TestActivationCreatesIndexes(t *testing.T) {
	pool := store.NewTestPool(t)
	ctx := context.Background()
	ws, err := workspace.New(pool).CreateWorkspace(ctx, "acme")
	if err != nil {
		t.Fatalf("ws: %v", err)
	}
	col, err := collection.New(pool).Create(ctx, ws.ID, "items")
	if err != nil {
		t.Fatalf("col: %v", err)
	}

	svc := schema.NewWithIndexer(pool, query.NewIndexer(pool))
	_, err = svc.Register(ctx, schema.RegisterCmd{
		Workspace: ws.ID, Collection: col.ID,
		JSONSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"price": map[string]any{"type": "number"}},
		},
		IndexedFields: []string{"price"},
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	var count int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM pg_indexes WHERE tablename='records' AND indexdef LIKE '%price%' AND indexdef LIKE '%'||$1||'%'`,
		col.ID.String(),
	).Scan(&count); err != nil {
		t.Fatalf("pg_indexes: %v", err)
	}
	if count == 0 {
		t.Fatal("expected an expression index on price for the collection")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -tags=integration ./internal/schema/ -run TestActivationCreatesIndexes -v`
Expected: FAIL — `schema.NewWithIndexer undefined`.

- [ ] **Step 3: Write minimal implementation**

Apply the struct field, `Indexer` interface, `NewWithIndexer`, `ensureActiveIndexes`, and the two call sites described in the Design notes above. Wire `NewWithIndexer` into the server bootstrap.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test -tags=integration ./internal/schema/ -run TestActivationCreatesIndexes -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/schema/registry.go internal/schema/index_wiring_test.go cmd/substrate
git commit -m "feat: create JSONB expression indexes on schema activation"
```

---

## Task 8: Full verification + final review

**Files:** none (verification only)

- [ ] **Step 1: Build and vet**

Run: `go build ./... && go vet ./...`
Expected: clean.

- [ ] **Step 2: Unit tests**

Run: `mise run test`  (i.e. `go test ./...`)
Expected: PASS, including all `internal/query` unit tests.

- [ ] **Step 3: Integration tests**

Run: `mise run test:integration`  (i.e. `go test -tags=integration ./...`)
Expected: PASS across all packages (Docker required for testcontainers).

- [ ] **Step 4: Manual sanity (optional)**

If a local server can run, exercise:
`GET /v1/collections/{c}/records?filter=status:eq:open&sort=-created_at&limit=2`, then follow `next_cursor`. Confirm ordering, membership, and that the final page returns an empty `next_cursor`.

- [ ] **Step 5: Commit any fixups**

```bash
git add -A
git commit -m "test: Plan 3 query & indexing verification"
```

---

## Self-review checklist (completed during planning)

- **Spec coverage:** filter/sort/limit parse (T1), keyset cursor (T2, T3), all ops incl. `in`/`exists`/range casts (T3), any-field policy (T1 accepts unknown fields; T3 builds JSON path), text-index DDL on activation (T4, T7), list endpoint + response shape (T6), deleted-excluded + active-scoped (T3 `status='active'`), error→400 (T1/T6). Covered.
- **Type consistency:** `ListQuery`/`Filter`/`SortKey`/`cursorData` defined in T1–T2 and used unchanged in T3–T6; `Build` signature `(string, []any, error)` consistent across T3/T5; `Indexer` interface identical in `internal/query` (T4) and `internal/schema` (T7); `EnsureCollectionIndexes(ctx, uuid.UUID, []string)` matches in both.
- **Placeholder scan:** none — every code step is complete. The two "find the real helper" notes (T6 harness, T7 `GetActiveSchema` shape) are explicit verification instructions, not placeholders.
- **Known cross-task edit:** T5 modifies `Build`'s SELECT (added in T3) to project `sort_key`; called out explicitly so T3 and T5 stay consistent.
