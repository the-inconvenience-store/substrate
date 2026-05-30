# Substrate Plan 3 — Query & Indexing — Design Spec

- **Date:** 2026-05-30
- **Status:** Approved (design)
- **Scope:** v0 subsystem 3 (query) — `list_records` with filter/sort/keyset pagination + JSONB expression indexes consuming `indexed_fields`
- **Builds on:** [v0 core design](2026-05-30-substrate-v0-core-design.md) + the merged foundation (Plan 1) + schema registry (Plan 2)
- **Language/runtime:** Go 1.26

## 1. Purpose & scope

Plans 1–2 shipped flexible + typed collections with a transactional event-backed record
core and a versioned schema registry. The list endpoint from the v0 API surface
(`GET /v1/collections/{c}/records`) and the `indexed_fields` metadata (stored on schema rows
but unused) are the remaining pieces of subsystem 3. Plan 3 implements **query**: filtering,
sorting, keyset pagination, and the Postgres expression indexes that make declared fields
fast.

### In scope
- `GET /v1/collections/{c}/records` — list with `filter` / `sort` / `limit` / `cursor`.
- A dedicated `internal/query` package: param parser, SQL builder, opaque keyset cursor
  codec, and an idempotent index manager.
- JSONB expression-index creation wired into schema **activation**, consuming the
  newly-active version's `indexed_fields`.

### Out of scope (later plans / documented limitations)
- Nested field paths (`address.city`) — top-level fields only in v0.
- Full-text search, boolean grouping / `OR`, aggregation/grouping.
- `include_deleted` in list output (list returns active records only).
- Numeric-range **index** optimization — v0 creates text expression indexes only, so numeric
  range filters are *correct* but may fall back to a scan.
- Dropping stale indexes when a field leaves `indexed_fields` — left in place in v0.

## 2. Key decisions (locked during brainstorming)

| # | Decision | Choice |
|---|----------|--------|
| 1 | Pagination | **Keyset cursor**, opaque base64url-wrapped JSON over `(sort_key, id)`. Stable under concurrent inserts, index-friendly, no deep-offset slowdown. |
| 2 | Indexing | On schema activation, **auto-create partial expression indexes** per `indexed_fields` entry, scoped to the collection: `((data->>'f')) WHERE collection_id=… AND status='active'`. Idempotent (`IF NOT EXISTS`), run outside the goose migration set. |
| 3 | Filterable fields | **Any field.** Declared `indexed_fields` hit the index; other fields fall back to a JSONB scan with a documented performance caveat (matches flexible-collection behavior). |
| 4 | Comparison typing | **Infer from the value's JSON type.** `eq`/`neq`/`in` compare as text against `data->>'f'` (index-backed); `exists` uses `data ? 'f'`; range ops (`gt`/`gte`/`lt`/`lte`) cast — number→`::numeric`, bool→`::boolean`, else text. |
| 5 | Index DDL timing | **Post-commit.** Index ensure runs after the activation transaction commits (not inside it), avoiding a write-blocking `CREATE INDEX` lock in the record/activation txn; idempotent so it is safely re-runnable on failure. |

## 3. Components & file structure

```
internal/query/filter.go     # parse filter/sort/limit/cursor params -> ListQuery (pure)
internal/query/builder.go    # ListQuery -> parameterized SQL string + []any args (pure)
internal/query/cursor.go     # opaque keyset cursor encode/decode (base64url JSON, pure)
internal/query/index.go      # EnsureCollectionIndexes(ctx, pool, collectionID, fields) — idempotent DDL
# modified:
internal/record/record.go    # add List(ctx, ws, col, ListQuery) (items, nextCursor, error)
internal/api/handlers.go     # listRecords handler (parse params -> record.List)
internal/api/router.go       # register GET /v1/collections/{collection}/records
internal/schema/registry.go  # optional Indexer dep; ensure indexes post-activation
```

Each unit has one responsibility: `filter.go`/`builder.go`/`cursor.go` are pure and
unit-tested in isolation; `index.go` owns DDL; the record service stays the single records
facade and imports only the pure query helpers (no import cycle).

## 4. Filter / sort grammar

- `filter=field:op:value` — repeatable; predicates are combined with `AND`.
  - Ops: `eq, neq, gt, gte, lt, lte, in, exists`.
  - `in` → comma-separated list (`status:in:open,closed`).
  - `exists` → `true|false` (`note:exists:false`).
- **field**: a top-level JSON field matching `^[a-zA-Z_][a-zA-Z0-9_]*$`, or a system column
  in `{created_at, updated_at, revision, id}`. Anything else → `400 bad_request`.
- **value typing (decision #4):**
  - `eq` → `data->>'f' = $v` (text); `neq` → `data->>'f' <> $v` (or IS DISTINCT FROM for
    null-safety); `in` → `data->>'f' = ANY($v)` (text array); `exists` → `(data ? 'f') = $b`.
  - range ops infer the cast from the value: numeric value → `(data->>'f')::numeric <op> $v`,
    bool → `::boolean`, else text comparison.
  - System columns use their native types directly (e.g. `created_at >= $v::timestamptz`).
- `sort=field` or `sort=-field` (leading `-` = DESC). Default `-created_at`. `id` is always
  appended as the final tiebreaker in the same direction, giving a stable total order.
- `limit`: default 50, clamped to max 200; non-positive or non-numeric → `400`.

## 5. SQL shape

Fetch `limit+1` rows to detect whether a next page exists without a second query:

```sql
SELECT id, collection_id, data, revision, status, actor, created_at
FROM records
WHERE workspace_id = $1 AND collection_id = $2 AND status = 'active'
  [AND <filter exprs…>]
  [AND <keyset predicate>]          -- only when a cursor is supplied
ORDER BY <sort_expr> <dir>, id <dir>
LIMIT <limit + 1>;
```

If `limit+1` rows come back, the extra row is dropped from `items` and `next_cursor` is
derived from the last *returned* row; otherwise `next_cursor` is empty/absent. Response:
`{ "items": [...records...], "next_cursor": "<opaque|absent>" }`.

## 6. Keyset cursor

Opaque token: `base64url( JSON{ "s": "<normalized sort spec>", "v": "<last sort-key value>",
"id": "<uuid>" } )`.

- The keyset predicate is a row-value comparison, with the cursor params cast to the sort
  column's type:
  - DESC: `(<sort_expr>, id) < ($v::<type>, $id::uuid)`
  - ASC:  `(<sort_expr>, id) > ($v::<type>, $id::uuid)`
- For a system-column sort the `<type>` is the column's type (`timestamptz`, `bigint`,
  `uuid`); for a data-field sort `<sort_expr>` is `data->>'f'` and the comparison is text.
- On decode, if the cursor's `s` ≠ the request's normalized sort spec → `400 bad_request`
  ("cursor does not match sort"), preventing inconsistent paging. Malformed/garbled cursor →
  `400`.

**Limitation:** sorting on a *data* field is lexicographic text ordering. `created_at`,
`updated_at`, and `revision` sort with their native types.

## 7. Index management

On a successful activation (the `Register` auto-activate / `activate=true` path, or a
standalone `Activate`), for each entry in the newly-active version's `indexed_fields`:

```sql
CREATE INDEX IF NOT EXISTS idx_rec_<col-hex>_<field>
  ON records ((data->>'<field>'))
  WHERE collection_id = '<col>' AND status = 'active';
```

- Partial, per-collection, **text** expression index — accelerates `eq`/`in`/`exists` and
  text sort/range on declared fields. (Numeric-range index optimization is a future
  enhancement; numeric range filters remain correct via the cast but may scan.)
- Index name is derived deterministically from collection id + field, and truncated with a
  hash suffix to respect Postgres's 63-char identifier limit. Field names are already
  validated identifiers, so the embedded literal is safe; the collection id is a UUID.
- **Idempotent** (`IF NOT EXISTS`) and run **post-commit** (decision #5). On failure the call
  returns `500`, but the schema is already active and the ensure is safely re-runnable on the
  next activation.
- Stale indexes (a field removed in a later version) are left in place in v0.

**Wiring:** `schema.Service` gains an optional `Indexer` dependency (interface, mirroring the
existing `Validator` seam) so the schema package's unit tests stay DB-light:

```go
type Indexer interface {
    EnsureCollectionIndexes(ctx context.Context, collectionID uuid.UUID, fields []string) error
}
```

`nil` indexer → skip (no DDL). The `internal/query` package provides the concrete
implementation backed by the pool.

## 8. Error handling

- Malformed `filter` / `sort` / `cursor` / `limit` → `400 bad_request` (existing code +
  envelope) with a `details` hint identifying the bad parameter.
- Well-formed but unknown field names are **accepted** (any-field policy, decision #3).
- Soft-deleted records (`status='deleted'`) are excluded from list output.
- All existing pipeline guarantees (auth, workspace scoping) are unchanged; list is a read
  and takes no transaction.

## 9. Testing (TDD)

- **Unit (no DB):**
  - Parser table tests — every op, `in`/`exists` value forms, bad syntax, bad identifier,
    limit defaulting/clamping, sort direction + default.
  - Cursor round-trip; tampered/garbled cursor → error; sort-mismatch → error.
  - Builder golden tests — assert the generated SQL string and the `[]any` arg slice for
    representative queries (single filter, multiple filters, `in`, `exists`, range cast,
    sort asc/desc, with/without cursor).
  - Index-name derivation (determinism + length bound).
- **Integration (testcontainers, shared container per run):**
  - Seed records; exercise each op and asc/desc sort; verify result sets.
  - Keyset pagination across multiple pages; stability when a row is inserted mid-paging.
  - Index exists after activation (query `pg_indexes`); flexible vs typed collections.
  - Soft-deleted records excluded from list.
- **End-to-end HTTP (integration-tagged):** create collection → seed records → list with
  combined filters + `limit` → follow `next_cursor` to the next page → assert ordering,
  membership, and exhaustion.
- Reuse the existing `store.NewTestPool` harness; `go test` (unit) + `go test -tags=integration`.
