# Substrate Benchmark Suite Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build a co-located benchmark suite (pure-logic micro-benchmarks + DB-backed `record.Service` benchmarks + HTTP end-to-end benchmarks) plus `mise` tasks and committed `benchstat` baselines, so an agent can iteratively reduce latency, memory, and allocations while the existing test suite guards functionality.

**Architecture:** Benchmarks live next to the code they measure. Pure benches are untagged (no Docker, deterministic `allocs/op`); DB/HTTP benches carry `//go:build integration` and reuse the existing white-box test harness (`store.NewTestPool`, `newProjServer`, `do`/`doAs`) plus a new shared seeding helper (`internal/store/benchfix`). Reporting is standard `go test -bench -benchmem` output compared with `benchstat` against committed baseline files.

**Tech Stack:** Go 1.26, `testing.B`, testcontainers Postgres (`store.NewTestPool`), `golang.org/x/perf/cmd/benchstat` (added to the `go.mod` tool directive), `mise` task runner, pprof.

**Spec:** `docs/superpowers/specs/2026-05-30-substrate-benchmark-suite-design.md`

---

## Key facts the implementer must know

- **`testing.TB` widening is required first.** Benchmarks receive `*testing.B`, but the shared helpers take `*testing.T`. Both implement `testing.TB`. Every method the helpers use — `Helper()`, `Fatalf()`, `Cleanup()`, `Context()` — is on `testing.TB` (Go 1.24+ for `Context()`). Widening `*testing.T` → `testing.TB` is source-compatible: existing callers pass `t` (a `*testing.T`, which *is* a `testing.TB`) unchanged.
- **Import-cycle rule.** `benchfix` imports `record`. So any benchmark that imports `benchfix` and lives in the `record` package directory **must use the external test package `record_test`** (not `package record`), or Go reports an import cycle. API benchmarks stay `package api` (api imports record; benchfix does not import api, so no cycle).
- **Pure benches need white-box access** to unexported symbols (`query.decodeCursor`, `projection.applyDefaults`, `schema`'s unexported `activeResolver`), so they use the internal package (`package query`, `package projection`, `package schema`, `package policy`) and import nothing DB-related.
- **Service-under-test wiring mirrors `main.go`:** `record.New(pool, schema.NewValidator(reg)).WithEvaluator(policy.NewEngine(pool))` against a *flexible* collection (no active schema) and an empty policy set. This still exercises the real `GetActive` (→ NotFound → flexible) and `Authorize` (→ load rules → default allow) DB round-trips, which is realistic.
- **Seeding uses a separate fast service:** `benchfix` seeds with `record.New(pool, nil)` (nil validator ⇒ no schema lookup) so fixture build time stays low and out of the timed loop.

Verbatim signatures this plan relies on (already in the codebase):

```go
// internal/store/testdb.go  (//go:build integration)
func NewTestPool(t *testing.T) *pgxpool.Pool        // -> widen to testing.TB

// internal/workspace/workspace.go
func New(pool *pgxpool.Pool) *Service
func (s *Service) CreateWorkspace(ctx context.Context, name string) (Workspace, error)   // Workspace has .ID uuid.UUID

// internal/collection/collection.go
func New(pool *pgxpool.Pool) *Service
func (s *Service) Create(ctx context.Context, ws uuid.UUID, name string) (Collection, error)  // Collection has .ID uuid.UUID

// internal/record/record.go
func New(pool *pgxpool.Pool, validator Validator) *Service
func (s *Service) WithEvaluator(e policy.Evaluator) *Service
func (s *Service) Create(ctx context.Context, cmd CreateCmd) (Record, error)
func (s *Service) Get(ctx context.Context, ws, col, id uuid.UUID, actor string) (Record, error)
func (s *Service) List(ctx context.Context, ws, col uuid.UUID, actor string, q query.ListQuery) ([]Record, string, error)
func (s *Service) Update(ctx context.Context, cmd UpdateCmd) (Record, error)
func (s *Service) Delete(ctx context.Context, ws, col, id uuid.UUID, actor string) error
// CreateCmd{Workspace, Collection uuid.UUID; Actor string; Data map[string]any; IdempotencyKey string}
// UpdateCmd{Workspace, Collection, ID uuid.UUID; ExpectedRevision int64; Actor string; Data map[string]any; IdempotencyKey string}

// internal/record/timetravel.go
func (s *Service) History(ctx context.Context, ws, col, id uuid.UUID, actor string) ([]HistoryEntry, error)
func (s *Service) GetAsOf(ctx context.Context, ws, col, id uuid.UUID, at AsOf, actor string) (Record, error)
// AsOf{Revision int64; EventID uuid.UUID; Timestamp time.Time}

// internal/policy/precedence.go
func Select(rules []Rule, req Request) (Decision, bool)
func NewEngine(pool *pgxpool.Pool) *Engine
// Rule{ID uuid.UUID; Actor string; Collection uuid.UUID; CollectionWildcard bool; Operation, Effect string}
// Request{Workspace uuid.UUID; Actor string; Collection, Target uuid.UUID; Operation string}

// internal/query/filter.go + builder.go + cursor.go
func Parse(filters []string, sort, limit, cursor string) (ListQuery, error)
func Build(q ListQuery) (string, []any, error)
func NextCursor(q ListQuery, sortValue, id string) string
func encodeCursor(c cursorData) string                  // unexported
func decodeCursor(tok string) (cursorData, error)       // unexported
func NewIndexer(pool *pgxpool.Pool) *Indexer

// internal/schema/validator.go + classifier.go + registry.go
func NewValidator(reg activeResolver) *Validator         // activeResolver unexported: GetActive(ctx, uuid.UUID) (ActiveSchema, error)
func (v *Validator) ValidateWrite(ctx context.Context, col uuid.UUID, data map[string]any) (int, error)
func NewWithIndexer(pool *pgxpool.Pool, ix Indexer) *Service
func Classify(current, candidate map[string]any) []Change
// ActiveSchema{Version int; Raw []byte}

// internal/projection/defaults.go
func applyDefaults(schemaRaw []byte, data map[string]any) (map[string]any, bool)   // unexported
```

---

## File structure

| File | Responsibility |
| --- | --- |
| `internal/store/testdb.go` (modify) | Widen `NewTestPool` to `testing.TB`. |
| `internal/api/api_test.go` (modify) | Widen `do` to `testing.TB`. |
| `internal/api/policy_api_test.go` (modify) | Widen `doAs` to `testing.TB`. |
| `internal/api/projection_api_test.go` (modify) | Widen `newProjServer` to `testing.TB`. |
| `internal/policy/precedence_bench_test.go` (create) | Pure `Select` precedence benchmarks. |
| `internal/query/builder_bench_test.go` (create) | Pure `Parse`/`Build`/cursor benchmarks. |
| `internal/schema/validator_bench_test.go` (create) | Pure `ValidateWrite` (cached) + `Classify` benchmarks. |
| `internal/projection/defaults_bench_test.go` (create) | Pure `applyDefaults` benchmark. |
| `internal/store/benchfix/seed.go` (create) | Shared fixture seeder (`Setup`, `SeedRecords`, `SeedHistory`, `Payload`). `//go:build integration`. |
| `internal/record/record_bench_test.go` (create) | DB write/read benches (`Create`/`Update`/`Delete`/`Get`/`List`). `package record_test`, `//go:build integration`. |
| `internal/record/timetravel_bench_test.go` (create) | DB time-travel benches (`History`/`GetAsOf`). `package record_test`, `//go:build integration`. |
| `internal/api/api_bench_test.go` (create) | HTTP e2e benches. `package api`, `//go:build integration`. |
| `mise.toml` (modify) | Add `bench*` tasks. |
| `go.mod` / `go.sum` (modify) | Add `benchstat` to the tool directive. |
| `docs/benchmarks/README.md` (create) | Optimization-loop workflow doc. |
| `docs/benchmarks/baseline/{pure,db,http}.txt` (create) | Committed baselines. |
| `.gitignore` (modify) | Ignore `docs/benchmarks/runs/` and profile outputs. |

---

## Task 1: Widen shared test helpers to `testing.TB`

**Files:**
- Modify: `internal/store/testdb.go:59`
- Modify: `internal/api/api_test.go:39`
- Modify: `internal/api/policy_api_test.go:49`
- Modify: `internal/api/projection_api_test.go:23`

- [ ] **Step 1: Widen `NewTestPool`**

In `internal/store/testdb.go`, change the signature only (body unchanged):

```go
// NewTestPool creates a brand-new database in the shared container, applies all
// migrations, and returns a pool connected to it. Use for per-test isolation.
// Accepts testing.TB so both tests (*testing.T) and benchmarks (*testing.B) can call it.
func NewTestPool(tb testing.TB) *pgxpool.Pool {
	tb.Helper()
	ctx := context.Background()

	adminDSN, err := startShared(ctx)
	if err != nil {
		tb.Fatalf("start shared postgres: %v", err)
	}

	admin, err := pgxpool.New(ctx, adminDSN)
	if err != nil {
		tb.Fatalf("connect admin: %v", err)
	}
	defer admin.Close()

	dbName := fmt.Sprintf("test_%d_%d", time.Now().UnixNano(), dbCounter.next())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		tb.Fatalf("create db %s: %v", dbName, err)
	}

	dsn := replaceDBName(adminDSN, dbName)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		tb.Fatalf("connect %s: %v", dbName, err)
	}
	if err := Migrate(ctx, pool); err != nil {
		tb.Fatalf("migrate: %v", err)
	}
	tb.Cleanup(func() { pool.Close() })
	return pool
}
```

- [ ] **Step 2: Widen `do`, `doAs`, `newProjServer`**

In `internal/api/api_test.go`, change `func do(t *testing.T, ...` to `func do(tb testing.TB, ...` and replace the two `t.` references inside with `tb.`:

```go
func do(tb testing.TB, method, url, key string, body any) *http.Response {
	tb.Helper()
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req, _ := http.NewRequest(method, url, &buf)
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("X-Substrate-Actor", "agent-1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		tb.Fatalf("%s %s: %v", method, url, err)
	}
	return resp
}
```

In `internal/api/policy_api_test.go`, change `func doAs(t *testing.T, ...` to `func doAs(tb testing.TB, ...` and replace its `t.Helper()`/`t.Fatalf(...)` with `tb.`:

```go
func doAs(tb testing.TB, method, url, key, actor string, body any) *http.Response {
	tb.Helper()
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req, _ := http.NewRequest(method, url, &buf)
	req.Header.Set("Authorization", "Bearer "+key)
	if actor != "" {
		req.Header.Set("X-Substrate-Actor", actor)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		tb.Fatalf("%s %s: %v", method, url, err)
	}
	return resp
}
```

In `internal/api/projection_api_test.go`, change `func newProjServer(t *testing.T)` to `func newProjServer(tb testing.TB)` and replace every `t.` inside the function body with `tb.` (the calls are `store.NewTestPool(t)`, `wsSvc.CreateWorkspace(t.Context(), ...)`, three `t.Fatalf`, `wsSvc.CreateAPIKey(t.Context(), ...)`, and `t.Cleanup(srv.Close)`):

```go
func newProjServer(tb testing.TB) (*httptest.Server, string, string, uuid.UUID) {
	tb.Helper()
	pool := store.NewTestPool(tb)
	wsSvc := workspace.New(pool)
	w, err := wsSvc.CreateWorkspace(tb.Context(), "acme")
	if err != nil {
		tb.Fatalf("ws: %v", err)
	}
	key, _, err := wsSvc.CreateAPIKey(tb.Context(), w.ID, "test")
	if err != nil {
		tb.Fatalf("key: %v", err)
	}
	const adminToken = "admin-secret"
	reg := schema.NewWithIndexer(pool, query.NewIndexer(pool))
	engine := policy.NewEngine(pool)
	srv := httptest.NewServer(NewRouter(Deps{
		Workspaces:  wsSvc,
		Collections: collection.New(pool),
		Records:     record.New(pool, schema.NewValidator(reg)),
		Schemas:     reg,
		Policies:    policy.NewService(pool),
		Backfiller:  projection.NewBackfiller(pool, reg),
		Replayer:    projection.NewReplayer(pool),
		Evaluator:   engine,
		AdminToken:  adminToken,
	}))
	tb.Cleanup(srv.Close)
	return srv, key, adminToken, w.ID
}
```

> Note: `newTestServer` and `newGovServer` are intentionally left on `*testing.T` — no benchmark uses them, and `*testing.T` still satisfies the widened `NewTestPool`. `tb.Helper()` adds nothing inside a top-level `newProjServer` call from a benchmark, but it is harmless and keeps the helper uniform.

- [ ] **Step 3: Verify integration test files still compile**

Run: `go build -tags=integration ./...`
Expected: exit 0, no errors. (This compiles the integration-tagged test helpers, catching any signature breakage without needing Docker.)

- [ ] **Step 4: Vet**

Run: `go vet ./... && go vet -tags=integration ./internal/...`
Expected: exit 0.

- [ ] **Step 5: Commit**

```bash
gofmt -w internal/store/testdb.go internal/api/api_test.go internal/api/policy_api_test.go internal/api/projection_api_test.go
git add internal/store/testdb.go internal/api/api_test.go internal/api/policy_api_test.go internal/api/projection_api_test.go
git commit -m "test: widen shared test helpers to testing.TB for benchmark reuse"
```

---

## Task 2: Policy precedence micro-benchmark (pure)

**Files:**
- Create: `internal/policy/precedence_bench_test.go`

- [ ] **Step 1: Write the benchmark**

```go
package policy

import (
	"testing"

	"github.com/google/uuid"
)

// buildRules returns n deterministic rules: a spread of wildcard/specific actor,
// collection, and operation dimensions, ending with one exact-match allow rule
// for the benchmarked request so Select must scan the whole slice.
func buildRules(n int, col uuid.UUID) []Rule {
	rules := make([]Rule, 0, n)
	ops := []string{OpCreate, OpRead, OpUpdate, OpDelete}
	for i := 0; i < n-1; i++ {
		rules = append(rules, Rule{
			ID:                 uuid.New(),
			Actor:              "other-actor",
			Collection:         uuid.New(),
			CollectionWildcard: i%2 == 0,
			Operation:          ops[i%len(ops)],
			Effect:             "deny",
		})
	}
	rules = append(rules, Rule{
		ID: uuid.New(), Actor: "agent-1", Collection: col,
		CollectionWildcard: false, Operation: OpUpdate, Effect: "allow",
	})
	return rules
}

func benchmarkSelect(b *testing.B, n int) {
	b.Helper()
	col := uuid.New()
	rules := buildRules(n, col)
	req := Request{Actor: "agent-1", Collection: col, Operation: OpUpdate}
	b.ReportAllocs()
	b.ResetTimer()
	var sink Decision
	for i := 0; i < b.N; i++ {
		d, _ := Select(rules, req)
		sink = d
	}
	_ = sink
}

func BenchmarkSelect_Small(b *testing.B) { benchmarkSelect(b, 5) }
func BenchmarkSelect_Large(b *testing.B) { benchmarkSelect(b, 50) }
```

- [ ] **Step 2: Smoke-run to verify it compiles and reports allocs**

Run: `go test -run '^$' -bench 'BenchmarkSelect' -benchmem -benchtime=1x ./internal/policy/`
Expected: PASS, output lines for `BenchmarkSelect_Small` and `BenchmarkSelect_Large` each showing `ns/op`, `B/op`, `allocs/op`.

- [ ] **Step 3: Commit**

```bash
gofmt -w internal/policy/precedence_bench_test.go
git add internal/policy/precedence_bench_test.go
git commit -m "bench: add policy precedence micro-benchmarks"
```

---

## Task 3: Query parse/build/cursor micro-benchmark (pure)

**Files:**
- Create: `internal/query/builder_bench_test.go`

- [ ] **Step 1: Write the benchmark**

```go
package query

import "testing"

// filters with one eq + several range/exists predicates to exercise the builder.
var benchFilters = []string{
	"status:eq:active",
	"priority:gte:3",
	"score:lt:100",
	"region:eq:us-east",
	"tags:in:a,b,c",
	"archived:exists:false",
}

func BenchmarkParse(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := Parse(benchFilters, "-created_at", "50", ""); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkBuild(b *testing.B) {
	q, err := Parse(benchFilters, "-created_at", "50", "")
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, err := Build(q); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkCursorRoundTrip(b *testing.B) {
	in := cursorData{Sort: "-created_at", Value: "2026-05-30T12:00:00Z", ID: "11111111-2222-3333-4444-555555555555"}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tok := encodeCursor(in)
		if _, err := decodeCursor(tok); err != nil {
			b.Fatal(err)
		}
	}
}
```

- [ ] **Step 2: Smoke-run**

Run: `go test -run '^$' -bench 'BenchmarkParse|BenchmarkBuild|BenchmarkCursorRoundTrip' -benchmem -benchtime=1x ./internal/query/`
Expected: PASS, three benchmark lines each with `ns/op`, `B/op`, `allocs/op`.

- [ ] **Step 3: Commit**

```bash
gofmt -w internal/query/builder_bench_test.go
git add internal/query/builder_bench_test.go
git commit -m "bench: add query parse/build/cursor micro-benchmarks"
```

---

## Task 4: Schema validation + classifier micro-benchmark (pure)

**Files:**
- Create: `internal/schema/validator_bench_test.go`

- [ ] **Step 1: Write the benchmark**

```go
package schema

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

// stubResolver satisfies the unexported activeResolver interface with a fixed
// in-memory schema, so the validator never touches a database.
type stubResolver struct{ s ActiveSchema }

func (r stubResolver) GetActive(ctx context.Context, col uuid.UUID) (ActiveSchema, error) {
	return r.s, nil
}

var benchSchemaRaw = []byte(`{
  "type": "object",
  "required": ["name", "age"],
  "properties": {
    "name":   {"type": "string", "minLength": 1, "maxLength": 120},
    "age":    {"type": "integer", "minimum": 0, "maximum": 130},
    "email":  {"type": "string", "format": "email"},
    "active": {"type": "boolean"},
    "score":  {"type": "number"},
    "tags":   {"type": "array", "items": {"type": "string"}}
  }
}`)

func benchPayload() map[string]any {
	return map[string]any{
		"name": "Ada Lovelace", "age": 36, "email": "ada@example.com",
		"active": true, "score": 99.5, "tags": []any{"x", "y", "z"},
	}
}

func BenchmarkValidateWrite(b *testing.B) {
	v := NewValidator(stubResolver{s: ActiveSchema{Version: 1, Raw: benchSchemaRaw}})
	col := uuid.New()
	data := benchPayload()
	// Warm the compiled-schema cache so we measure the steady-state validate path.
	if _, err := v.ValidateWrite(context.Background(), col, data); err != nil {
		b.Fatalf("warmup: %v", err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := v.ValidateWrite(context.Background(), col, data); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkClassify(b *testing.B) {
	current := map[string]any{
		"type":     "object",
		"required": []any{"name"},
		"properties": map[string]any{
			"name": map[string]any{"type": "string", "maxLength": float64(120)},
			"age":  map[string]any{"type": "integer", "minimum": float64(0)},
		},
	}
	candidate := map[string]any{
		"type":     "object",
		"required": []any{"name", "age"}, // add-required => breaking
		"properties": map[string]any{
			"name": map[string]any{"type": "string", "maxLength": float64(80)}, // tighten => breaking
			"age":  map[string]any{"type": "integer", "minimum": float64(0)},
			"email": map[string]any{"type": "string"}, // new optional => non-breaking
		},
	}
	b.ReportAllocs()
	b.ResetTimer()
	var sink int
	for i := 0; i < b.N; i++ {
		sink = len(Classify(current, candidate))
	}
	_ = sink
}
```

- [ ] **Step 2: Smoke-run**

Run: `go test -run '^$' -bench 'BenchmarkValidateWrite|BenchmarkClassify' -benchmem -benchtime=1x ./internal/schema/`
Expected: PASS, two benchmark lines each with `ns/op`, `B/op`, `allocs/op`.

- [ ] **Step 3: Commit**

```bash
gofmt -w internal/schema/validator_bench_test.go
git add internal/schema/validator_bench_test.go
git commit -m "bench: add schema validate + classifier micro-benchmarks"
```

---

## Task 5: Projection defaults micro-benchmark (pure)

**Files:**
- Create: `internal/projection/defaults_bench_test.go`

- [ ] **Step 1: Write the benchmark**

```go
package projection

import "testing"

var benchDefaultsSchema = []byte(`{
  "type": "object",
  "properties": {
    "status":   {"type": "string", "default": "active"},
    "priority": {"type": "integer", "default": 0},
    "tags":     {"type": "array", "default": []},
    "meta":     {"type": "object", "default": {"v": 1}},
    "name":     {"type": "string"}
  }
}`)

func BenchmarkApplyDefaults(b *testing.B) {
	// data is missing every defaulted key, forcing the copy-on-write path.
	data := map[string]any{"name": "widget"}
	b.ReportAllocs()
	b.ResetTimer()
	var changed bool
	for i := 0; i < b.N; i++ {
		_, changed = applyDefaults(benchDefaultsSchema, data)
	}
	_ = changed
}
```

- [ ] **Step 2: Smoke-run**

Run: `go test -run '^$' -bench 'BenchmarkApplyDefaults' -benchmem -benchtime=1x ./internal/projection/`
Expected: PASS, one line with `ns/op`, `B/op`, `allocs/op`.

- [ ] **Step 3: Commit**

```bash
gofmt -w internal/projection/defaults_bench_test.go
git add internal/projection/defaults_bench_test.go
git commit -m "bench: add projection defaults-applier micro-benchmark"
```

---

## Task 6: Shared fixture seeder (`internal/store/benchfix`)

**Files:**
- Create: `internal/store/benchfix/seed.go`

**Note on package placement:** `benchfix` is its own package under `internal/store/` (not part of `package store`). It imports `record`, `workspace`, `collection`. None of those import `benchfix`, and `store` does not import `benchfix`, so there is no cycle. It is integration-tagged because it touches a live pool.

- [ ] **Step 1: Write the seeder**

```go
//go:build integration

// Package benchfix seeds deterministic fixtures for Substrate's DB-backed and
// HTTP benchmarks. It is integration-tagged because it requires a live pool.
package benchfix

import (
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/substrate/substrate/internal/collection"
	"github.com/substrate/substrate/internal/record"
	"github.com/substrate/substrate/internal/workspace"
)

// Fixture is a seeded workspace + flexible collection plus a fast (nil-validator)
// seeding service. Build the service-under-test separately in the benchmark.
type Fixture struct {
	Pool       *pgxpool.Pool
	Workspace  uuid.UUID
	Collection uuid.UUID
	seedSvc    *record.Service
}

// Setup creates a workspace and a flexible collection named "bench" and returns a
// Fixture. The seeding service uses a nil validator so seeding does no schema work.
func Setup(tb testing.TB, pool *pgxpool.Pool) *Fixture {
	tb.Helper()
	ctx := tb.Context()
	w, err := workspace.New(pool).CreateWorkspace(ctx, "bench")
	if err != nil {
		tb.Fatalf("create workspace: %v", err)
	}
	col, err := collection.New(pool).Create(ctx, w.ID, "bench")
	if err != nil {
		tb.Fatalf("create collection: %v", err)
	}
	return &Fixture{
		Pool: pool, Workspace: w.ID, Collection: col.ID,
		seedSvc: record.New(pool, nil),
	}
}

// Payload returns a deterministic ~1 KB / 6-field record body. The "filler" field
// pads the body to a realistic size; pass a unique marker to vary one field.
func Payload(marker string) map[string]any {
	return map[string]any{
		"title":    "benchmark record " + marker,
		"status":   "active",
		"priority": 3,
		"score":    42.5,
		"tags":     []any{"alpha", "beta", "gamma"},
		"filler":   strings.Repeat("x", 900),
	}
}

// SeedRecords creates n records and returns their ids in creation order.
func (f *Fixture) SeedRecords(tb testing.TB, n int) []uuid.UUID {
	tb.Helper()
	ctx := tb.Context()
	ids := make([]uuid.UUID, 0, n)
	for i := 0; i < n; i++ {
		rec, err := f.seedSvc.Create(ctx, record.CreateCmd{
			Workspace:  f.Workspace,
			Collection: f.Collection,
			Actor:      "seed",
			Data:       Payload(strconv.Itoa(i)),
		})
		if err != nil {
			tb.Fatalf("seed record %d: %v", i, err)
		}
		ids = append(ids, rec.ID)
	}
	return ids
}

// SeedHistory creates one record and advances it through `revisions` total
// revisions (1 create + revisions-1 updates), returning its id.
func (f *Fixture) SeedHistory(tb testing.TB, revisions int) uuid.UUID {
	tb.Helper()
	ctx := tb.Context()
	rec, err := f.seedSvc.Create(ctx, record.CreateCmd{
		Workspace: f.Workspace, Collection: f.Collection, Actor: "seed",
		Data: Payload("v1"),
	})
	if err != nil {
		tb.Fatalf("seed history create: %v", err)
	}
	for r := 2; r <= revisions; r++ {
		_, err := f.seedSvc.Update(ctx, record.UpdateCmd{
			Workspace: f.Workspace, Collection: f.Collection, ID: rec.ID,
			ExpectedRevision: int64(r - 1), Actor: "seed",
			Data: Payload("v" + strconv.Itoa(r)),
		})
		if err != nil {
			tb.Fatalf("seed history update rev %d: %v", r, err)
		}
	}
	return rec.ID
}
```

- [ ] **Step 2: Verify it compiles**

Run: `go build -tags=integration ./internal/store/benchfix/`
Expected: exit 0.

- [ ] **Step 3: Vet**

Run: `go vet -tags=integration ./internal/store/benchfix/`
Expected: exit 0.

- [ ] **Step 4: Commit**

```bash
gofmt -w internal/store/benchfix/seed.go
git add internal/store/benchfix/seed.go
git commit -m "bench: add shared fixture seeder (benchfix)"
```

---

## Task 7: DB-backed record write/read benchmarks

**Files:**
- Create: `internal/record/record_bench_test.go`

**Requires Docker** (testcontainers). Uses external `package record_test` to avoid the `benchfix → record` import cycle.

- [ ] **Step 1: Write the benchmarks**

```go
//go:build integration

package record_test

import (
	"testing"

	"github.com/substrate/substrate/internal/policy"
	"github.com/substrate/substrate/internal/query"
	"github.com/substrate/substrate/internal/record"
	"github.com/substrate/substrate/internal/schema"
	"github.com/substrate/substrate/internal/store"
	"github.com/substrate/substrate/internal/store/benchfix"
)

// underTest builds a record.Service wired like main.go: real schema validator +
// policy evaluator. The collection is flexible (no active schema) and there are no
// policy rules, so GetActive and Authorize each do one realistic DB round-trip.
func underTest(pool *benchfix.Fixture) *record.Service {
	reg := schema.NewWithIndexer(pool.Pool, query.NewIndexer(pool.Pool))
	return record.New(pool.Pool, schema.NewValidator(reg)).WithEvaluator(policy.NewEngine(pool.Pool))
}

func BenchmarkCreate(b *testing.B) {
	pool := store.NewTestPool(b)
	f := benchfix.Setup(b, pool)
	svc := underTest(f)
	ctx := b.Context()
	data := benchfix.Payload("create")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := svc.Create(ctx, record.CreateCmd{
			Workspace: f.Workspace, Collection: f.Collection, Actor: "bench", Data: data,
		}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkUpdate(b *testing.B) {
	pool := store.NewTestPool(b)
	f := benchfix.Setup(b, pool)
	svc := underTest(f)
	ctx := b.Context()
	id := f.SeedRecords(b, 1)[0] // starts at revision 1
	data := benchfix.Payload("update")
	rev := int64(1)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rec, err := svc.Update(ctx, record.UpdateCmd{
			Workspace: f.Workspace, Collection: f.Collection, ID: id,
			ExpectedRevision: rev, Actor: "bench", Data: data,
		})
		if err != nil {
			b.Fatal(err)
		}
		rev = rec.Revision
	}
}

func BenchmarkDelete(b *testing.B) {
	pool := store.NewTestPool(b)
	f := benchfix.Setup(b, pool)
	svc := underTest(f)
	ctx := b.Context()
	ids := f.SeedRecords(b, b.N) // one distinct record per iteration
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := svc.Delete(ctx, f.Workspace, f.Collection, ids[i], "bench"); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkGet(b *testing.B) {
	pool := store.NewTestPool(b)
	f := benchfix.Setup(b, pool)
	svc := underTest(f)
	ctx := b.Context()
	id := f.SeedRecords(b, 1)[0]
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := svc.Get(ctx, f.Workspace, f.Collection, id, "bench"); err != nil {
			b.Fatal(err)
		}
	}
}

func benchmarkList(b *testing.B, n int) {
	b.Helper()
	pool := store.NewTestPool(b)
	f := benchfix.Setup(b, pool)
	svc := underTest(f)
	ctx := b.Context()
	f.SeedRecords(b, n)
	q, err := query.Parse(nil, "-created_at", "50", "")
	if err != nil {
		b.Fatalf("parse: %v", err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, err := svc.List(ctx, f.Workspace, f.Collection, "bench", q); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkList_Small(b *testing.B) { benchmarkList(b, 100) }
func BenchmarkList_Large(b *testing.B) { benchmarkList(b, 10000) }
```

- [ ] **Step 2: Smoke-run (Docker required)**

Run: `go test -tags=integration -run '^$' -bench 'BenchmarkCreate|BenchmarkUpdate|BenchmarkDelete|BenchmarkGet|BenchmarkList' -benchmem -benchtime=1x ./internal/record/`
Expected: PASS, lines for `BenchmarkCreate`, `BenchmarkUpdate`, `BenchmarkDelete`, `BenchmarkGet`, `BenchmarkList_Small`, `BenchmarkList_Large`, each with `ns/op`, `B/op`, `allocs/op`. (`-benchtime=1x` keeps `BenchmarkDelete`'s seed of `b.N` rows tiny during the smoke run.)

- [ ] **Step 3: Vet**

Run: `go vet -tags=integration ./internal/record/`
Expected: exit 0.

- [ ] **Step 4: Commit**

```bash
gofmt -w internal/record/record_bench_test.go
git add internal/record/record_bench_test.go
git commit -m "bench: add DB-backed record write/read benchmarks"
```

---

## Task 8: DB-backed time-travel benchmarks

**Files:**
- Create: `internal/record/timetravel_bench_test.go`

**Requires Docker.** Same external `package record_test`; reuses `underTest` from Task 7 (same package, same dir).

- [ ] **Step 1: Write the benchmarks**

```go
//go:build integration

package record_test

import (
	"testing"

	"github.com/substrate/substrate/internal/record"
	"github.com/substrate/substrate/internal/store"
	"github.com/substrate/substrate/internal/store/benchfix"
)

func benchmarkHistory(b *testing.B, revisions int) {
	b.Helper()
	pool := store.NewTestPool(b)
	f := benchfix.Setup(b, pool)
	svc := underTest(f)
	ctx := b.Context()
	id := f.SeedHistory(b, revisions)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := svc.History(ctx, f.Workspace, f.Collection, id, "bench"); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkHistory_Short(b *testing.B) { benchmarkHistory(b, 5) }
func BenchmarkHistory_Long(b *testing.B)  { benchmarkHistory(b, 500) }

func BenchmarkGetAsOf_Deep(b *testing.B) {
	pool := store.NewTestPool(b)
	f := benchfix.Setup(b, pool)
	svc := underTest(f)
	ctx := b.Context()
	id := f.SeedHistory(b, 500)
	at := record.AsOf{Revision: 250} // mid-history point against a deep event stream
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := svc.GetAsOf(ctx, f.Workspace, f.Collection, id, at, "bench"); err != nil {
			b.Fatal(err)
		}
	}
}
```

- [ ] **Step 2: Smoke-run (Docker required)**

Run: `go test -tags=integration -run '^$' -bench 'BenchmarkHistory|BenchmarkGetAsOf' -benchmem -benchtime=1x ./internal/record/`
Expected: PASS, lines for `BenchmarkHistory_Short`, `BenchmarkHistory_Long`, `BenchmarkGetAsOf_Deep`, each with `ns/op`, `B/op`, `allocs/op`.

- [ ] **Step 3: Commit**

```bash
gofmt -w internal/record/timetravel_bench_test.go
git add internal/record/timetravel_bench_test.go
git commit -m "bench: add DB-backed time-travel benchmarks"
```

---

## Task 9: HTTP end-to-end benchmarks

**Files:**
- Create: `internal/api/api_bench_test.go`

**Requires Docker.** Stays `package api` to reuse `newProjServer`/`do`/`doAs`. `newProjServer` wires the validator + evaluator + schema + backfill stack (closest to `main.go`). Each iteration drains and closes the response body to avoid FD/alloc leakage skewing numbers.

- [ ] **Step 1: Write the benchmarks**

```go
//go:build integration

package api

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

// drain reads and closes a response body so connections are reused and the body
// allocation is realistic and not leaked across iterations.
func drain(b *testing.B, resp *http.Response, want int) {
	b.Helper()
	if resp.StatusCode != want {
		b.Fatalf("status = %d, want %d", resp.StatusCode, want)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
}

func benchPayload(marker string) map[string]any {
	return map[string]any{"data": map[string]any{
		"title": "bench " + marker, "status": "active", "priority": 3,
		"filler": strings.Repeat("x", 900),
	}}
}

// seedHTTP creates the "bench" collection and n records over HTTP, returning the
// first record id (for Get/History). Runs before the timed loop.
func seedHTTP(b *testing.B, url, key string, n int) string {
	b.Helper()
	resp := doAs(b, "POST", url+"/v1/collections", key, "agent-1", map[string]any{"name": "bench"})
	drain(b, resp, http.StatusCreated)
	first := ""
	for i := 0; i < n; i++ {
		resp := doAs(b, "POST", url+"/v1/collections/bench/records", key, "agent-1", benchPayload(strconv.Itoa(i)))
		if resp.StatusCode != http.StatusCreated {
			b.Fatalf("seed create status = %d", resp.StatusCode)
		}
		if first == "" {
			var created struct {
				ID string `json:"id"`
			}
			_ = json.NewDecoder(resp.Body).Decode(&created)
			first = created.ID
		} else {
			_, _ = io.Copy(io.Discard, resp.Body)
		}
		resp.Body.Close()
	}
	return first
}

func BenchmarkHTTP_CreateRecord(b *testing.B) {
	srv, key, _, _ := newProjServer(b)
	resp := doAs(b, "POST", srv.URL+"/v1/collections", key, "agent-1", map[string]any{"name": "bench"})
	drain(b, resp, http.StatusCreated)
	body := benchPayload("create")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		resp := doAs(b, "POST", srv.URL+"/v1/collections/bench/records", key, "agent-1", body)
		drain(b, resp, http.StatusCreated)
	}
}

func BenchmarkHTTP_GetRecord(b *testing.B) {
	srv, key, _, _ := newProjServer(b)
	id := seedHTTP(b, srv.URL, key, 1)
	url := srv.URL + "/v1/collections/bench/records/" + id
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		resp := doAs(b, "GET", url, key, "agent-1", nil)
		drain(b, resp, http.StatusOK)
	}
}

func benchmarkHTTPList(b *testing.B, n int) {
	b.Helper()
	srv, key, _, _ := newProjServer(b)
	seedHTTP(b, srv.URL, key, n)
	url := srv.URL + "/v1/collections/bench/records?sort=-created_at&limit=50"
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		resp := doAs(b, "GET", url, key, "agent-1", nil)
		drain(b, resp, http.StatusOK)
	}
}

func BenchmarkHTTP_ListRecords_Small(b *testing.B) { benchmarkHTTPList(b, 100) }
func BenchmarkHTTP_ListRecords_Large(b *testing.B) { benchmarkHTTPList(b, 10000) }

func BenchmarkHTTP_History(b *testing.B) {
	srv, key, _, _ := newProjServer(b)
	id := seedHTTP(b, srv.URL, key, 1)
	// Add a few revisions so history has content.
	for r := 0; r < 4; r++ {
		req := mustJSONReq(b, "PATCH", srv.URL+"/v1/collections/bench/records/"+id, benchPayload("u"+strconv.Itoa(r)))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("X-Substrate-Actor", "agent-1")
		req.Header.Set("If-Match", `"`+strconv.Itoa(r+1)+`"`)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			b.Fatalf("seed update: %v", err)
		}
		drain(b, resp, http.StatusOK)
	}
	url := srv.URL + "/v1/collections/bench/records/" + id + "/history"
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		resp := doAs(b, "GET", url, key, "agent-1", nil)
		drain(b, resp, http.StatusOK)
	}
}
```

> `mustJSONReq` already exists in `internal/api/projection_api_test.go` and takes `*testing.T`. The `BenchmarkHTTP_History` call passes `b` (`*testing.B`), so widen `mustJSONReq`'s parameter to `testing.TB` as part of this task: change `func mustJSONReq(t *testing.T, ...` to `func mustJSONReq(tb testing.TB, ...` and replace its `t.Helper()`/`t.Fatalf(...)` with `tb.`.

- [ ] **Step 2: Widen `mustJSONReq`**

In `internal/api/projection_api_test.go`:

```go
func mustJSONReq(tb testing.TB, method, url string, body any) *http.Request {
	tb.Helper()
	b, _ := json.Marshal(body)
	req, err := http.NewRequest(method, url, bytes.NewReader(b))
	if err != nil {
		tb.Fatalf("req: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	return req
}
```

- [ ] **Step 3: Smoke-run (Docker required)**

Run: `go test -tags=integration -run '^$' -bench 'BenchmarkHTTP' -benchmem -benchtime=1x ./internal/api/`
Expected: PASS, lines for `BenchmarkHTTP_CreateRecord`, `BenchmarkHTTP_GetRecord`, `BenchmarkHTTP_ListRecords_Small`, `BenchmarkHTTP_ListRecords_Large`, `BenchmarkHTTP_History`, each with `ns/op`, `B/op`, `allocs/op`.

- [ ] **Step 4: Vet**

Run: `go vet -tags=integration ./internal/api/`
Expected: exit 0.

- [ ] **Step 5: Commit**

```bash
gofmt -w internal/api/api_bench_test.go internal/api/projection_api_test.go
git add internal/api/api_bench_test.go internal/api/projection_api_test.go
git commit -m "bench: add HTTP end-to-end benchmarks"
```

---

## Task 10: Add benchstat tooling and mise tasks

**Files:**
- Modify: `go.mod`, `go.sum` (via `go get -tool`)
- Modify: `mise.toml`
- Modify: `.gitignore`

- [ ] **Step 1: Add benchstat to the tool directive**

Run: `go get -tool golang.org/x/perf/cmd/benchstat`
Expected: `go.mod` gains a `golang.org/x/perf` entry in the `tool` block; `go.sum` updated.

- [ ] **Step 2: Verify benchstat is invokable**

Run: `go tool benchstat -h`
Expected: benchstat usage text prints (exit code may be non-zero for `-h`; the usage output is the signal it is installed).

- [ ] **Step 3: Add bench tasks to `mise.toml`**

Append these tasks to `mise.toml` (after the existing `[tasks.run]` block):

```toml
[tasks."bench:pure"]
description = "Run pure-logic micro-benchmarks (no Docker)"
run = "go test -run '^$' -bench . -benchmem -count=10 ./internal/policy/... ./internal/query/... ./internal/schema/... ./internal/projection/..."

[tasks."bench:db"]
description = "Run DB-backed record benchmarks (requires Docker)"
run = "go test -tags=integration -run '^$' -bench . -benchmem -count=10 ./internal/record/..."

[tasks."bench:http"]
description = "Run HTTP end-to-end benchmarks (requires Docker)"
run = "go test -tags=integration -run '^$' -bench . -benchmem -count=10 ./internal/api/..."

[tasks.bench]
description = "Run all benchmark groups"
depends = ["bench:pure", "bench:db", "bench:http"]

[tasks."bench:baseline"]
description = "Refresh committed baselines (overwrites docs/benchmarks/baseline/*.txt)"
run = [
  "go test -run '^$' -bench . -benchmem -count=10 ./internal/policy/... ./internal/query/... ./internal/schema/... ./internal/projection/... | tee docs/benchmarks/baseline/pure.txt",
  "go test -tags=integration -run '^$' -bench . -benchmem -count=10 ./internal/record/... | tee docs/benchmarks/baseline/db.txt",
  "go test -tags=integration -run '^$' -bench . -benchmem -count=10 ./internal/api/... | tee docs/benchmarks/baseline/http.txt",
]

[tasks."bench:compare"]
description = "Compare baseline vs new with benchstat. Usage: mise run bench:compare -- docs/benchmarks/baseline/pure.txt new.txt"
run = "go tool benchstat"

[tasks."bench:profile"]
description = "Profile a benchmark. Usage: mise run bench:profile -- -tags=integration -bench BenchmarkCreate ./internal/record/"
run = "go test -run '^$' -benchmem -cpuprofile cpu.out -memprofile mem.out"
```

> `mise` appends any args after `--` to the task's command, so `bench:compare` and `bench:profile` receive their file/flag arguments. After `bench:profile`, inspect with `go tool pprof cpu.out` or `go tool pprof mem.out`.

- [ ] **Step 4: Ignore run outputs and profiles**

Add to `.gitignore`:

```
docs/benchmarks/runs/
*.out
```

- [ ] **Step 5: Verify task wiring**

Run: `mise tasks | grep bench`
Expected: lists `bench`, `bench:pure`, `bench:db`, `bench:http`, `bench:baseline`, `bench:compare`, `bench:profile`.

Run: `mise run bench:pure`
Expected: PASS; all pure benchmarks run with `-count=10` and print `ns/op`/`B/op`/`allocs/op`.

- [ ] **Step 6: Commit**

```bash
git add go.mod go.sum mise.toml .gitignore
git commit -m "build: add benchstat tooling and mise bench tasks"
```

---

## Task 11: Capture baselines and write the workflow doc

**Files:**
- Create: `docs/benchmarks/README.md`
- Create: `docs/benchmarks/baseline/pure.txt`, `docs/benchmarks/baseline/db.txt`, `docs/benchmarks/baseline/http.txt`

**Requires Docker** for the db/http baselines.

- [ ] **Step 1: Capture baselines**

Run: `mkdir -p docs/benchmarks/baseline && mise run bench:baseline`
Expected: three baseline files written, each containing benchmark lines with `ns/op`, `B/op`, `allocs/op`. (On a machine without Docker, the db/http files will be empty/failed — capture those on a Docker-capable runner before committing.)

- [ ] **Step 2: Write the workflow doc**

Create `docs/benchmarks/README.md`:

````markdown
# Substrate Benchmarks

A suite for reducing latency, memory, and allocations without breaking functionality.
Three layers: pure-logic micro-benchmarks (no Docker), DB-backed `record.Service`
benchmarks, and HTTP end-to-end benchmarks (both need Docker for testcontainers
Postgres). Every benchmark reports `ns/op`, `B/op`, and `allocs/op` (`-benchmem` is
always on).

## Tasks

| Task | What it does |
| --- | --- |
| `mise run bench:pure` | Pure micro-benchmarks (policy, query, schema, projection). No Docker. |
| `mise run bench:db` | DB-backed record benchmarks. Docker required. |
| `mise run bench:http` | HTTP end-to-end benchmarks. Docker required. |
| `mise run bench` | All three groups. |
| `mise run bench:baseline` | Overwrite the committed baselines under `baseline/`. |
| `mise run bench:compare -- baseline/<g>.txt new.txt` | benchstat diff. |
| `mise run bench:profile -- [flags] <pkg>` | CPU+mem profile a benchmark, then `go tool pprof cpu.out`. |

## The optimization loop

1. **Functionality gate first.** `mise run test && mise run test:integration`.
   Performance work that changes behavior is rejected here — benchmarks alone do not
   prove correctness.
2. **Capture current numbers.** `mise run bench:pure > /tmp/new-pure.txt` (and/or the
   db/http groups).
3. **Make one optimization.**
4. **Re-run and compare.** `mise run bench:compare -- docs/benchmarks/baseline/pure.txt /tmp/new-pure.txt`.
5. **Keep the change only if** functionality still passes *and* benchstat shows a
   significant improvement (or no regression) in `ns/op`, `B/op`, or `allocs/op`. Use
   `mise run bench:profile` to find the hot function/allocation site behind a delta.
6. **Move the floor.** When a genuine improvement lands, `mise run bench:baseline`
   refreshes the committed baselines so future regressions are measured against the new
   floor. Baseline updates are explicit — never automatic.

## Caveats

- **HTTP numbers include loopback overhead.** `httptest` runs a real HTTP round-trip, so
  `BenchmarkHTTP_*` measures full request cost, not pipeline-only. Compare the
  HTTP-vs-service delta to attribute cost rather than chasing loopback noise.
- **DB benchmarks are the noisiest layer.** `-count=10` plus benchstat's p-values absorb
  container jitter; a single run is not trustworthy.
- **Baselines are machine-specific.** Re-capture on the machine/CI you compare against.
- **Honesty guardrails.** Every benchmark checks errors and (for HTTP) asserts status, so
  a change that breaks a path cannot masquerade as faster.
````

- [ ] **Step 3: Commit**

```bash
git add docs/benchmarks/README.md docs/benchmarks/baseline/
git commit -m "docs: add benchmark baselines and optimization-loop workflow"
```

---

## Self-Review

**1. Spec coverage:**
- Pure-logic micro-benchmarks (policy/query/schema/projection) → Tasks 2–5. ✔
- DB-backed service benchmarks (Create/Update/Delete/Get/List + History/GetAsOf, tiered small/large) → Tasks 7–8. ✔
- HTTP e2e benchmarks (Create/Get/List small+large/History) → Task 9. ✔
- testcontainers via `NewTestPool` → Tasks 1, 6, 7, 8, 9. ✔
- Shared `benchfix` seeder (`SeedRecords`, `SeedHistory`, `Payload`) → Task 6. ✔
- Committed baselines + benchstat → Tasks 10, 11. ✔
- pprof mise tasks → Task 10 (`bench:profile`). ✔
- Tiered small/large + ~1 KB payloads → `benchfix.Payload` (900-char filler) + `_Small`/`_Large` in Tasks 7, 9; `_Short`/`_Long`/`_Deep` in Task 8. ✔
- `-run '^$'`, `-benchmem`, explicit `bench:baseline` → Task 10. ✔
- Optimization-loop doc incl. functionality gate + honesty guardrails → Task 11. ✔
- Co-located placement (Approach C) → file structure table. ✔

**2. Placeholder scan:** No `TBD`/`TODO`/"add error handling"/"similar to" — every code step shows complete, compilable code. ✔

**3. Type consistency:**
- `benchfix.Fixture` fields (`Pool`, `Workspace`, `Collection`) and methods (`Setup`, `SeedRecords`, `SeedHistory`, `Payload`) defined in Task 6 are used consistently in Tasks 7–8. `underTest` takes `*benchfix.Fixture` and reads `.Pool`/`.Workspace`/`.Collection`. ✔
- `record.CreateCmd`/`UpdateCmd`/`AsOf` field names match the verbatim signatures. ✔
- `testing.TB` widening (Task 1) precedes all benchmark tasks that call the widened helpers; `mustJSONReq` widening is folded into Task 9 where it is first used by a `*testing.B`. ✔
- External `package record_test` (Tasks 7, 8) vs internal `package api` (Task 9) chosen per the import-cycle rule. ✔

No issues found.

## Sequencing & dependencies

- **Task 1 must come first** (all DB/HTTP benches depend on the `testing.TB` widening).
- **Tasks 2–5 (pure)** are independent of everything else and of each other — parallelizable, no Docker.
- **Task 6 (benchfix)** must precede Tasks 7–8.
- **Tasks 7, 8, 9** depend on Tasks 1 (+6 for 7/8) and need Docker.
- **Task 10** depends on the benchmarks existing (so tasks resolve) but the tooling/tasks edits are otherwise independent.
- **Task 11** is last (captures real numbers; needs Docker for db/http baselines).
