# Substrate Plan 2 — Schema Registry & Validation — Design Spec

- **Date:** 2026-05-30
- **Status:** Approved (design)
- **Scope:** v0 subsystem 2 — schema registry, JSON Schema validation, compatibility classifier (typed collections)
- **Builds on:** [v0 core design](2026-05-30-substrate-v0-core-design.md) + the merged foundation (Plan 1)
- **Language/runtime:** Go 1.26

## 1. Purpose & scope

Plan 1 shipped flexible collections with a transactional event-backed record core. Plan 2
adds the **typed** level: a versioned schema registry, JSON Schema validation of record
writes, and a deterministic compatibility classifier governing schema evolution.

### In scope
- `schemas` table + registry service (`internal/schema`): register immutable versions,
  lifecycle (`draft`/`active`/`deprecated`), activation flow.
- JSON Schema validation (`santhosh-tekuri/jsonschema/v6`, draft 2020-12) wired into the
  existing record write pipeline for typed collections.
- Deterministic recursive compatibility classifier (allowed vs breaking changes).
- Flexible→typed promotion via first-schema registration.
- Schema lifecycle changes recorded as events (audit timeline).

### Out of scope (later plans)
- Index DDL / querying on `indexed_fields` — **Plan 3** (metadata is stored now, unused).
- Bulk backfill / migration of grandfathered records — **Plan 5**.
- Schema *synthesiser* (auto-proposing schemas) — promotion engine, later.
- Policy plane, rich query/list, audit endpoint — their own plans.

## 2. Key decisions (locked during brainstorming)

| # | Decision | Choice |
|---|----------|--------|
| 1 | Activation flow | Register defaults to `draft`; explicit `:activate` flips the active pointer. **First schema auto-activates** (and promotes collection to `typed`). Register accepts `activate=true` to register+compatibility-check+activate atomically in one call (agent one-shot path). |
| 2 | Existing records on promotion | **Grandfather**: existing flexible records keep `schema_version = NULL`, remain readable, are NOT retroactively validated. Validated against the active schema only on their next update. Bulk backfill deferred to Plan 5. Promotion is an O(1) metadata op. |
| 3 | Compatibility classifier depth | **Recursive** over `properties`/`required`/`items`/scalar constraints. Ambiguous constructs (`$ref`, `anyOf`/`oneOf`/`allOf`, `patternProperties`) classified **conservatively as breaking**. Not a full satisfiability checker. |
| 4 | `indexed_fields` | **Accept + store as metadata only.** No Postgres index DDL and no query effect in Plan 2 (that is Plan 3). Keeps the schema row shape forward-compatible. |
| 5 | JSON Schema library/dialect | `github.com/santhosh-tekuri/jsonschema/v6`, draft 2020-12. Compiled validators cached per `(collection, version)`. |

## 3. Data model (migration `00002_schemas.sql`)

New goose migration. New table:

| Column | Type | Notes |
|--------|------|-------|
| `id` | uuid PK | |
| `collection_id` | uuid NOT NULL → collections | ON DELETE CASCADE |
| `workspace_id` | uuid NOT NULL → workspaces | tenancy, ON DELETE CASCADE |
| `version` | int NOT NULL | per-collection, monotonic from 1 |
| `json_schema` | jsonb NOT NULL | the JSON Schema document |
| `lifecycle` | text NOT NULL | `draft` \| `active` \| `deprecated` |
| `indexed_fields` | jsonb NOT NULL DEFAULT '[]' | metadata only (Plan 3 consumes) |
| `rationale` | text | recorded when a breaking change is forced |
| `created_by` | text | actor |
| `created_at` | timestamptz NOT NULL DEFAULT now() | |

- `UNIQUE (collection_id, version)`.
- Version numbers are allocated as `MAX(version)+1` per collection inside the registration
  transaction (the collection row is locked `FOR UPDATE` to serialize concurrent registers).
- Existing columns put to use: `collections.active_schema_version` now points at the active
  version (NULL = flexible / no active schema); `collections.level` flips to `typed` when the
  first schema activates; `records.schema_version` is stamped on typed writes (NULL =
  grandfathered).

**Schema lifecycle as events.** Registration/activation/deprecation append events to the
existing `events` table with types `schema_registered`, `schema_activated`,
`schema_deprecated`. These are collection-scoped (no record); the event's `record_id` is set
to the collection id and `state_after` carries `{version, lifecycle}`. This keeps schema
history on the same audit timeline as records. (The `GET /v1/audit` endpoint that surfaces
this is a later plan; the events are written now.)

## 4. Schema registry service & API (`internal/schema`)

New package `internal/schema` with a `Service` wrapping `*pgxpool.Pool` + `*db.Queries`,
following the established service pattern (sqlc-generated queries, `store.WithTx` for
multi-statement mutations).

### Endpoints
- `POST /v1/collections/{c}/schemas` — register an immutable version.
  Body: `{ "json_schema": {...}, "indexed_fields": ["a","b"]?, "activate": bool?, "force": bool?, "rationale": string? }`.
  - Validates `json_schema` compiles (else `422 schema_invalid`).
  - Allocates next version; inserts `draft` (or `active` if `activate`/first-schema).
  - `activate=true` (or first schema): runs the compatibility check vs current active,
    flips `active_schema_version`, deprecates the prior active, promotes `level` to `typed` —
    all in one transaction.
- `POST /v1/collections/{c}/schemas/{v}:activate` — compatibility-check vs current active,
  flip active pointer, deprecate prior. `force=true` allows a breaking change (requires/records
  `rationale`).
- `POST /v1/collections/{c}/schemas/{v}:deprecate` — mark `deprecated` (cannot deprecate the
  active version unless another is activated; deprecating is the delete-by-default).
- `GET /v1/collections/{c}/schemas` — list versions (version, lifecycle, created_at/by).
- `GET /v1/collections/{c}/schemas/{v}` — read one immutable version (full document).

### Behavior
- Versions are immutable once written; only `lifecycle` changes.
- Activating version N deprecates the previously-active version; `active_schema_version` always
  points to exactly one `active` row (or NULL).
- Re-activating an already-`deprecated` version is allowed (re-runs compatibility vs current
  active) — supports rollback to a prior shape.

## 5. Validation in the record write pipeline

The record service (`internal/record`) gains a schema resolver dependency (interface, so it
stays decoupled and unit-testable):

```
type Validator interface {
    // ValidateWrite validates data against a collection's active schema.
    // Returns the active version stamped on the write, or (0, nil) for flexible collections.
    ValidateWrite(ctx, collectionID uuid.UUID, data map[string]any) (version int, err error)
}
```

- Implemented by `internal/schema` (or a thin adapter): looks up the collection's
  `active_schema_version`; if NULL → flexible, returns `(0, nil)` (no validation). If set →
  loads + compiles (cached) the active schema, validates `data`, returns the version.
- On **create/update** of a typed collection: call `ValidateWrite` first; failure →
  `422 validation_failed` with per-field errors in `details`. Success → stamp
  `records.schema_version` = returned version and set it on the event.
- Grandfathered NULL-version records in a now-typed collection: their next update is validated
  against the active schema (same path) and stamped; reads never validate.
- Validator cache keyed by `(collectionID, version)`; invalidated implicitly because versions
  are immutable (a new active version is a new key).

This slots into the existing one-transaction pipeline (Plan 1) between idempotency/replay and
the optimistic-revision check, matching v0 spec §6 step 4.

## 6. Compatibility classifier (`internal/schema`, pure)

A standalone, dependency-free function operating on two parsed JSON Schema documents
(current-active vs candidate):

```
type Change struct { Path string; Kind string; Breaking bool }
func Classify(current, candidate map[string]any) []Change
```

- **Recursive** over `properties` (descend per-key), `required`, `items`, and scalar
  constraint keywords (`type`, `enum`, `minimum`/`maximum`, `minLength`/`maxLength`,
  `pattern`, `format`).
- **Allowed (non-breaking):** add a new optional property; remove a property from `required`
  (relax); widen `type` (e.g. add to a type union, integer→number); add an `enum` value;
  loosen numeric/length bounds; add an index field.
- **Breaking:** remove or rename a `required` property; add a property to `required`; narrow
  `type`; remove an `enum` value; tighten bounds; change `pattern`/`format` incompatibly.
- **Conservative fallback:** any construct the classifier can't confidently diff
  (`$ref`, `anyOf`/`oneOf`/`allOf`/`not`, `patternProperties`, `additionalProperties` schema
  changes) → emit a `Breaking` change. Documented behavior.
- Activation rejects when any change is `Breaking` and `force != true` → `409 schema_incompatible`
  with the breaking changes listed in `details`. With `force=true`, activation proceeds and the
  `rationale` is persisted on the schema row + activation event.

## 7. Error handling

New/used stable codes in `internal/apierr`:
- `schema_invalid` (422) — candidate is not a compilable JSON Schema.
- `validation_failed` (422) — record body fails the active schema (reuses existing `Validation`).
- `schema_incompatible` (409) — breaking change on activate without `force` (new code).

Per-field validation errors and breaking-change lists go in the envelope `details`. All schema
mutations are transactional; partial registers/activations never persist.

## 8. Testing

- **Unit (no DB):** the compatibility classifier — table-driven over add/remove/widen/narrow/
  enum/bounds/nested/ambiguous cases, asserting `Breaking` flags. JSON Schema compile/validate
  helpers.
- **Integration (testcontainers, shared container per run):** register → activate lifecycle;
  first-schema auto-activation + `level` promotion; `activate=true` one-call path;
  register-as-draft then activate; breaking change blocked then forced; grandfathered record
  validated on next update; concurrent register version allocation (FOR UPDATE).
- **End-to-end HTTP (integration-tagged):** create typed collection → register+activate schema
  → valid create `201` → invalid create `422` with field errors → register breaking v2,
  `:activate` → `409` → `:activate?force=true` → `200`.
- Reuse the existing `store.NewTestPool` harness; `go test` (unit) + `go test -tags=integration`.

## 9. File structure (Plan 2)

```
internal/migrations/00002_schemas.sql      # schemas table + indexes
internal/queries/schemas.sql               # sqlc: register/get/list/activate/deprecate/version-alloc
internal/db/schemas.sql.go                 # generated
internal/schema/registry.go                # Service: register/activate/deprecate/get/list
internal/schema/classifier.go              # pure recursive compatibility classifier
internal/schema/validator.go               # active-schema resolver + compiled-validator cache (Validator impl)
internal/api/schema_handlers.go            # HTTP handlers for schema endpoints
# modified:
internal/record/record.go                  # call Validator in create/update; stamp schema_version
internal/api/router.go                     # wire schema routes + Validator into record service
internal/apierr/apierr.go                  # add schema_incompatible code
```

Each unit has one responsibility: `classifier.go` is pure and unit-tested in isolation;
`validator.go` owns schema loading/compilation/caching; `registry.go` owns lifecycle + the DB;
handlers stay transport-only.
