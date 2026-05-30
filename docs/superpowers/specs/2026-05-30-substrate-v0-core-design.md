# Substrate v0 Core — Design Spec

- **Date:** 2026-05-30
- **Status:** Approved (design)
- **Scope:** v0 OSS core runtime — subsystems 1–4
- **Language/runtime:** Go 1.26
- **Source vision:** [Substrate.md](../../Substrate.md)

## 1. Purpose & scope

Substrate is an event-backed state-object store for agent-powered products. This spec
covers the **v0 OSS core runtime** only: a self-hostable Go service that lets agents and
apps create, validate, query, and audit durable state objects with full time-travel.

### In scope (subsystems 1–4)

1. **Storage & event core** — workspaces, collections, records, append-only event log,
   transactional current-state projection, optimistic concurrency, idempotency, soft
   delete, time-travel (history / point-in-time / revert).
2. **Schema registry & validation** — versioned JSON Schemas, lifecycle
   (`draft`/`active`/`deprecated`), validated mutations, deterministic compatibility
   checks, read-time version resolution, optional lazy backfill.
3. **API surface & auth** — HTTP+JSON intentful API that "feels like a normal database,"
   workspace-scoped API keys, explicit actor identity, request tracing.
4. **Policy / governance hooks** — thin declarative allow/deny plane evaluated pre-commit,
   behind a swappable `PolicyEvaluator` interface, with decisions recorded in the audit
   trail.

### Out of scope for v0 (each gets its own later spec)

Promotion engine (flexible→typed→promoted triggers, scoring, schema synthesis,
"dreaming"); client SDKs; hosted control plane (orgs/users/billing/backups/dashboards);
gRPC surface; custom/derived projections beyond the current-state record; full-text and
nested-boolean query; OPA/external policy engines (interface seam only).

## 2. Key decisions (locked during brainstorming)

| # | Decision | Choice |
|---|----------|--------|
| 1 | Storage backend | PostgreSQL (single backing store) via `pgx`; events + JSONB record projections + schemas + policies in one DB. Postgres-*dialect* SQL behind a `Store` interface. |
| 1b | Embeddable target | `embedded-postgres` (managed real Postgres binary) for a low-dependency self-host / dev story. Same Postgres dialect as the external target — no second SQL implementation. |
| 2 | API protocol | HTTP + JSON (`/v1`). Service logic behind interfaces so gRPC can be added later. |
| 3 | Auth & tenancy | Workspace-scoped API keys (hashed at rest) + explicit per-request actor identity threaded into audit/policy. |
| 4 | Schema language | JSON Schema (`santhosh-tekuri/jsonschema` or equivalent) with Substrate metadata (version, lifecycle, indexed fields) wrapped around it. |
| 5 | Policy model | Declarative allow/deny rules evaluated in-process pre-commit, behind a `PolicyEvaluator` interface. |
| 6 | Query | Filter + sort + cursor pagination on declared/indexed fields. Ops: `eq, neq, gt, gte, lt, lte, in, exists`. |
| 7 | Schema evolution | Versioned coexistence + read-time resolution + optional background backfill. Deterministic compatibility classifier; breaking changes gated behind `force`. |
| 8 | Event/record relationship | Events are authoritative append-only history; the `records` row is a projection maintained **in the same transaction** as the event append. |
| 9 | Time-travel | Each event stores full `state_after` snapshot → history walk, point-in-time read, and forward-only revert. |

## 3. Architecture

Single Go 1.26 module, single binary, layered internal packages. Every mutation flows
through one pipeline so reliability guarantees are enforced in exactly one place.

```
HTTP request
  → API layer        (routing, DTOs, error mapping)
  → Auth             (API key → workspace + actor identity)
  → Service layer    (intentful op: create_schema / update_record / list_records …)
  → Policy           (PolicyEvaluator: allow/deny for actor × collection × op)
  → Validation       (JSON Schema for active/typed version)
  → Storage txn      (ONE Postgres transaction:)
        • check optimistic revision + idempotency key
        • append event   (append-only history, state_after snapshot)
        • upsert record  (current-state projection)
        • write audit/trace context
  → Response (record + ETag)
```

**Events-authoritative + transactional projection** (not pure event-sourcing): events give
replay, point-in-time reconstruction, and audit; the `records` row keeps reads a simple
indexed lookup with no async projection lag. A `replay` utility can rebuild any record or
collection from its event stream (used by backfill and disaster recovery).

### Package layout

| Package | Responsibility |
|---------|----------------|
| `cmd/substrate` | bootstrap, config, server wiring |
| `internal/api` | routing, request/response DTOs, error mapping (transport-only) |
| `internal/auth` | API-key verification, actor extraction |
| `internal/workspace` | workspace management |
| `internal/schema` | schema registry, JSON Schema validation, compatibility classifier, lifecycle |
| `internal/record` | record service, intentful CRUD, optimistic concurrency, idempotency |
| `internal/event` | event types, append, stream reads |
| `internal/projection` | synchronous current-state maintenance + backfill worker + replay |
| `internal/policy` | `PolicyEvaluator` interface + declarative rule engine |
| `internal/audit` | audit/event stream queries |
| `internal/query` | filter/sort/paginate → SQL builder |
| `internal/store` | `Store` interface + Postgres impl (`pgx`) + migrations |
| `internal/trace` | trace/request-id context propagation |

Each subsystem sits behind an interface so storage, policy, and (later) the API protocol
can be swapped without touching callers.

## 4. Data model (Postgres / JSONB)

| Table | Purpose | Key columns |
|-------|---------|-------------|
| `workspaces` | tenant boundary | `id`, `name`, `created_at` |
| `api_keys` | auth | `id`, `workspace_id`, `hash`, `label`, `created_at`, `revoked_at` |
| `collections` | a typed/flexible object type | `id`, `workspace_id`, `name`, `level` (`flexible`\|`typed`), `active_schema_version`, timestamps |
| `schemas` | versioned registry (immutable rows) | `id`, `collection_id`, `version`, `json_schema` (JSONB), `lifecycle` (`draft`\|`active`\|`deprecated`), `indexed_fields` (JSONB), `compat` metadata, `created_at` |
| `records` | current-state projection | `id`, `collection_id`, `workspace_id`, `schema_version`, `data` (JSONB), `revision`, `status` (`active`\|`deleted`), `actor`, timestamps |
| `events` | append-only authoritative history | `seq` (bigserial, global order), `id`, `workspace_id`, `collection_id`, `record_id`, `type`, `revision`, `state_after` (JSONB), `actor`, `trace` (JSONB), `idempotency_key`, `created_at` |
| `policies` | declarative rules | `id`, `workspace_id`, `role`/`actor`, `collection` (or `*`), `operation`, `effect` (`allow`\|`deny`), `created_at` |

- **Concurrency:** `records.revision` is the optimistic-lock token, surfaced as `ETag`;
  updates require an expected revision (`If-Match`), else `409`.
- **Idempotency:** unique index on `(workspace_id, idempotency_key)` over events; a
  replayed key returns the original result instead of re-applying.
- **Audit log = the `events` table.** Schema lifecycle changes and policy denials get their
  own event types, so schema and record history share one timeline. No separate audit
  store in v0.
- **Indexes:** typed collections get Postgres expression indexes on declared
  `indexed_fields` (JSONB path extraction); flexible collections fall back to JSONB
  containment with a documented performance caveat.
- **Soft delete:** `status='deleted'` + tombstone event; hard delete is a rare, separate
  admin path.

## 5. API surface (HTTP + JSON, `/v1`)

Workspace-scoped via API key. Actor identity via `X-Substrate-Actor` header (or token
claim), recorded on every event. Standard headers: `Idempotency-Key` on mutations,
`If-Match`/`ETag` for optimistic concurrency.

### Collections & schemas
- `POST /v1/collections` — create (`level: flexible|typed`)
- `GET /v1/collections` · `GET /v1/collections/{c}` — list / inspect
- `POST /v1/collections/{c}/schemas` — register new version (compatibility check; `?force=true` to gate a breaking change)
- `GET /v1/collections/{c}/schemas` · `…/schemas/{version}` — list / read immutable versions
- `POST /v1/collections/{c}/schemas/{version}:activate` — move active pointer
- `POST /v1/collections/{c}/schemas/{version}:deprecate` — deprecate (delete = deprecate-by-default)

### Records
- `POST /v1/collections/{c}/records` — create (validated against active schema; `Idempotency-Key`)
- `GET /v1/collections/{c}/records/{id}` — read current; `?as_of=<ts|revision|event_id>` for point-in-time
- `PATCH /v1/collections/{c}/records/{id}` — update (requires `If-Match`)
- `DELETE /v1/collections/{c}/records/{id}` — soft delete (tombstone event)
- `GET /v1/collections/{c}/records` — list with filter/sort/cursor pagination
- `GET /v1/collections/{c}/records/{id}/history` — ordered event stream for the record
- `POST /v1/collections/{c}/records/{id}/revert` — revert to target revision/event/timestamp (forward event)

**List query params:** `filter=field:op:value` (repeatable; ops `eq,neq,gt,gte,lt,lte,in,exists`),
`sort=field|-field`, `limit`, `cursor`. Response: `{items, next_cursor}`.

### Policy & audit
- `POST /v1/policies` · `GET /v1/policies` · `DELETE /v1/policies/{id}`
- `GET /v1/audit` — workspace-wide event/audit stream (filter by collection, record, actor, type, time range)

### Admin / ops
- `POST /v1/workspaces` · `POST /v1/workspaces/{w}/api-keys` (admin-bootstrap-key gated)
- `GET /healthz` · `GET /readyz`

**Conventions:** JSON bodies; `:verb` suffix for non-CRUD actions; error envelope
`{"error": {"code", "message", "details"}}`.

## 6. Subsystem behaviors

### Schema registry & compatibility
- New version is validated as well-formed JSON Schema, then structurally diffed against the
  current active version.
  - **Allowed (no force):** add optional field, relax constraint, widen type, add enum
    value, add index.
  - **Breaking (require `force=true` + recorded rationale):** remove/rename required field,
    narrow type, tighten constraint, remove enum value.
- The classifier is a **deterministic structural diff** over the two JSON Schemas.
- Versions are immutable; lifecycle transitions are events. Writes validate against the
  collection's `active_schema_version`; reads resolve/validate against each record's stored
  `schema_version`, so old shapes always read cleanly.
- Flexible collections require no schema; writes get system fields only (id, timestamps,
  revision). Promoting flexible→typed = registering a first real schema (the *synthesiser*
  that proposes one is out of scope; the registration mechanism is in).

### Record write pipeline (one transaction)
1. Auth → workspace + actor.
2. Policy evaluates `(actor, collection, op)`; deny ⇒ `403` + denial event.
3. Idempotency key checked; replay ⇒ return prior result.
4. Typed collections: validate body against active schema; fail ⇒ `422`.
5. Optimistic revision check (`If-Match`); mismatch ⇒ `409`.
6. Append event (`state_after`, actor, trace) + upsert `records` + bump revision —
   committed atomically.
7. Return record + new `ETag`.

Any step failing rolls back the whole transaction; no partial events or projections.

### Policy evaluation
- `PolicyEvaluator` interface; v0 impl loads workspace rules and evaluates
  **deny-overrides-allow, most-specific-wins** (`collection`+`operation` beats `*`).
- **Default mode** is per-workspace: default **allow** (usable out of the box); self-hosters
  can flip to default-deny.
- Every decision is attached to the resulting event's trace context.

### Projection & backfill
- The `records` row is maintained synchronously inside the write transaction — no async lag.
- A background **backfill worker** lazily rewrites records from older `schema_version` toward
  the active version when a non-breaking version activates (opt-in per collection). Runs in
  bounded, idempotent batches; writes `migration` events so backfills are auditable and
  resumable. Reads never block on it (read-time resolution handles old shapes).

### Time-travel / reconstruction
- History, point-in-time, and revert read from `events` using the `state_after` snapshot at
  (or just before) the target.
- **Revert writes a new forward event** whose `state_after` equals the chosen prior state and
  bumps revision forward. History is never rewritten; a revert can itself be reverted.
- The `replay` utility rebuilds any record/collection from its event stream (used by backfill
  and as an admin disaster-recovery operation).

## 7. Error handling

- Single envelope: `{"error": {"code", "message", "details"}}` with stable string codes:
  `schema_invalid`, `validation_failed`, `revision_conflict`, `policy_denied`,
  `idempotency_replay`, `not_found`, `schema_incompatible`.
- HTTP mapping: `400` malformed · `401` bad/missing key · `403` policy denied · `404`
  missing · `409` revision conflict / incompatible schema without force · `422` validation
  failure · `429` rate-limit (hook, off by default) · `500` internal.
- Idempotent replays return the **original** status + body.
- Typed Go errors at the service layer, mapped to HTTP only in the API layer (service is
  transport-agnostic).

## 8. Testing (TDD)

- **Unit tests** per package, table-driven: compatibility classifier, policy evaluator,
  query→SQL builder, idempotency, revision checks.
- **Integration tests** against **real Postgres via testcontainers** — a **single shared
  Postgres container per test run** (not one container per test). Per-test isolation via a
  fresh schema/database per test (or truncation), not container spawning. Exercise the full
  transactional pipeline, history/point-in-time/revert, and backfill.
- **API-level tests** through the HTTP handler with golden request/response fixtures.
- **Storage conformance suite** written against the `Store` interface, so the Postgres impl
  (and any future embedded/PGlite target) must pass identical tests.

## 9. Ops & configuration

- Config via flags + env (12-factor): store target (external PG DSN | embedded), bind addr,
  admin bootstrap key, default policy mode, log level.
- `slog` structured logging with a request/trace ID threaded through context and onto events.
- `/healthz`, `/readyz`.
- Substrate's own schema migrations run on startup (embedded migration files).
- Single static binary; embedded mode (`embedded-postgres`) manages a local Postgres
  process for a low-friction self-host / dev path with no separately-provisioned database.

## 10. Open implementation questions (do not block v0 architecture)

- Exact JSON Schema library/version and its draft support level.
- Cursor encoding scheme (opaque keyset vs offset) for list pagination.
