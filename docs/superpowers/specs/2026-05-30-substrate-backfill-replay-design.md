# Substrate Plan 5 — Backfill & Replay — Design Spec

- **Date:** 2026-05-30
- **Status:** Approved (design)
- **Scope:** The `internal/projection` subsystem — event-stream **replay** (projection rebuild / disaster recovery) and lazy **backfill** (advance records toward the active schema version), completing the v0 core
- **Builds on:** [v0 core design](2026-05-30-substrate-v0-core-design.md) + Plan 1 (foundation) + Plan 2 (schema registry) + Plan 3 (query/indexing) + Plan 4 (policy/governance)
- **Language/runtime:** Go 1.26

## 1. Purpose & scope

Plans 1–4 shipped the transactional event-backed record core, the versioned schema registry,
query/indexing, and the policy/governance plane. The v0 core design names one more package —
`internal/projection` — responsible for synchronous current-state maintenance (already done
inline by `record.Service`), **backfill**, and **replay**. Plan 5 builds those two remaining
capabilities.

- **Replay** — rebuild the `records` current-state projection for a record or a whole
  collection from the authoritative `events` stream. This is the disaster-recovery operation:
  if the `records` table is lost or corrupted, it is reconstructable from `events`.
- **Backfill** — when a schema version becomes active, lazily advance records still stamped at
  an older `schema_version` toward the active version: apply the active schema's defaults for
  newly-added fields, re-validate, and re-stamp. Because Substrate only allows non-breaking
  changes without `force`, old data already validates against the new schema, so this is
  re-stamping + default-filling, **not** arbitrary transformation.

### In scope
- A new `internal/projection` package: a pure defaults applier, a `Replayer`, a `Backfiller`,
  and an async `Worker`.
- An additive `events.schema_version` column so events fully describe the state they carry
  (required for faithful replay).
- An `collections.auto_backfill` opt-in flag + a toggle endpoint.
- **Auto-backfill on activation** (opt-in per collection) via a background worker, AND a manual
  trigger endpoint.
- **Admin replay endpoints** (record + collection), admin-token gated.
- `migration` events recording each backfilled record (auditable, time-travel-visible).

### Out of scope (later / documented limitations)
- **Arbitrary/pluggable transform functions.** v0 only applies JSON Schema top-level `default`s
  and re-validates; records that don't validate are skipped and reported, never transformed.
- **Nested-field defaults.** Top-level properties only (consistent with the query subsystem).
- **A persistent backfill-job/cursor table.** Resumability is inherent (idempotent re-run);
  there is no async job status resource.
- **Backfilling soft-deleted records.** Only `status='active'` records are advanced.
- **CLI surface.** Replay/backfill are HTTP-only in v0.

## 2. Key decisions (locked during brainstorming)

| # | Decision | Choice |
|---|----------|--------|
| 1 | Backfill trigger | **Both.** An opt-in (`auto_backfill`) background `Worker` enqueues a collection on schema activation, AND a manual `POST /v1/collections/{c}/backfill` runs it on demand. Both call the same `Backfiller`. |
| 2 | Per-record transform | **Re-stamp + defaults + re-validate.** Apply the active schema's top-level `default`s for missing fields, validate against the active schema, then write the migrated data and bump `schema_version`. Invalid records are skipped + counted, never corrupted. |
| 3 | Replay surface | **Admin HTTP endpoints**, admin-token gated, for a single record and a whole collection. |
| 4 | Resumability | **Inherent idempotent re-run.** No job table. Each bounded batch advances records to the active version; re-running naturally processes only the records still behind. Reports `{scanned, migrated, skipped, remaining}`. |
| 5 | Event self-description | **Add `events.schema_version`** (nullable). Events carry the version their `state_after` conformed to, so replay restores the projection faithfully. Additive (existing callers default NULL, like the Plan 4 `trace` column). |

## 3. Components & file structure

```
internal/projection/defaults.go     # pure: apply top-level JSON Schema defaults to a data map
internal/projection/replay.go       # Replayer: rebuild projection for a record / collection from events
internal/projection/backfill.go     # Backfiller: advance records toward active version (batched, idempotent)
internal/projection/worker.go       # Worker: async queue; satisfies schema.BackfillEnqueuer
internal/migrations/00004_*.sql      # events.schema_version (nullable int)
internal/migrations/00005_*.sql      # collections.auto_backfill (bool, default false)
internal/queries/records.sql         # +ListRecordsBelowVersion, +UpsertRecordProjection, +ListRecordIDsInCollection
internal/queries/events.sql          # AppendEvent gains schema_version; +GetLatestRecordEvent
internal/queries/collections.sql     # +SetAutoBackfill; auto_backfill on create/get
# modified:
internal/record/record.go            # appendEvent carries schema_version (Create/Update/Delete)
internal/record/timetravel.go        # Revert event carries schema_version
internal/schema/registry.go          # BackfillEnqueuer seam; enqueue on activation when auto_backfill
internal/collection/collection.go    # auto_backfill field + Create + SetAutoBackfill
internal/api/projection_handlers.go  # POST /v1/collections/{c}/backfill, POST .../auto-backfill; admin replay
internal/api/router.go               # register routes; Deps gains Backfiller/Replayer/(worker)
cmd/substrate/main.go                # construct Replayer/Backfiller/Worker; start worker; wire schema enqueuer
```

No import cycle: `projection` imports `schema`, `db`, `store`, `apierr`; `schema` defines a
`BackfillEnqueuer` interface that `projection.Worker` satisfies (schema never imports
projection); only `cmd/substrate` wires them together.

## 4. Event self-description: `events.schema_version`

Migration `00004` adds a nullable `schema_version int` to `events`. `AppendEvent` gains the
column (existing struct-literal callers omit it → NULL, build stays green — the Plan 4 `trace`
pattern). Population:

- **record create/update** — the version already computed by the validator (`sv`), threaded
  into the event row (currently it reaches `records` but not `events`).
- **delete** — the record's current `schema_version` (state shape is unchanged by a tombstone).
- **revert** — the version carried by the target event being restored (best-effort; NULL if the
  target predates the column).
- **backfill migration** — the new active version.
- **schema lifecycle events** (`schema_registered`/`activated`/`deprecated`) — NULL (these are
  collection-scoped, not record data).
- **policy_denied** — NULL.

## 5. Replay (projection rebuild)

`Replayer{pool, q}`:

- `RebuildRecord(ctx, ws, col, id uuid.UUID) (bool, error)` — read the **latest** event for the
  record (`GetLatestRecordEvent`, excluding `policy_denied`, highest `seq`); upsert the
  `records` row from it: `data = state_after`, `revision`, `status` (`deleted` when the latest
  event type is `delete`, else `active`), `schema_version` from the event. Returns `false` when
  the record has no events. Idempotent (`INSERT … ON CONFLICT (collection_id, id) DO UPDATE`).
- `RebuildCollection(ctx, ws, col uuid.UUID) (int, error)` — `ListRecordIDsInCollection` (distinct
  `record_id` in the collection's events, excluding the collection-scoped lifecycle pseudo-rows
  where `record_id = collection_id`), rebuild each, return the count rebuilt.

Replay **does not append events** — it only reconstructs the projection from existing events.
It is the inverse of the synchronous projection maintained in the write pipeline.

## 6. Backfill

`Backfiller{pool, q, schema SchemaResolver}` where `SchemaResolver` is the minimal interface
`GetActive(ctx, col) (schema.ActiveSchema, error)` (satisfied by `schema.Service`).

`Run(ctx, ws, col uuid.UUID, batch int) (Report, error)`:

1. `GetActive(col)`. If the collection has no active schema (flexible) → return an empty report
   (nothing to advance).
2. Compile the active schema once (for validation) and extract its top-level `default`s once.
3. Loop in bounded batches of `batch` (clamped, default 200): `ListRecordsBelowVersion(col,
   activeVersion, limit)` selects `status='active'` records where `schema_version IS NULL OR
   schema_version < activeVersion`. When a batch is empty → done.
4. For each record, in its **own transaction**:
   - `SELECT … FOR UPDATE`; re-check it is still active and still below the active version
     (a concurrent normal write may have advanced it) — if not, skip.
   - Apply defaults to a copy of the data (only set missing top-level fields that have a
     `default`).
   - Validate the result against the active schema. **Invalid → skip + count `skipped`** (do not
     write); this protects against records left behind by a forced breaking change.
   - Append a `migration` event: `revision+1`, `state_after` = migrated data,
     `schema_version` = activeVersion, `actor = "system:backfill"`.
   - `UpdateRecordData`: migrated data, `revision+1`, `schema_version = activeVersion`.
   - Count `migrated`.
5. Report `{scanned, migrated, skipped, remaining}` where `remaining` is the count of
   active records still below the active version after this run — `0` when every active
   record validated and advanced, and equal to the number of skipped-as-invalid records
   otherwise (those can never advance via re-stamping and stay reported).

Idempotency/resumability: a migrated record has `schema_version = activeVersion` and is no
longer selected; a crash mid-run leaves a prefix migrated, and any later run (manual or the next
activation) resumes naturally. Each record commits independently, so partial progress is durable.

`migration` events carry `state_after` and a real revision, so they appear in record `History`
and `GET /v1/audit`, and replay reconstructs from them. (They are state events, unlike the
`policy_denied` events excluded from history in Plan 4.)

### Defaults applier (pure)

`applyDefaults(schemaRaw []byte, data map[string]any) (map[string]any, bool)` parses the schema's
top-level `properties`, and for each property carrying a `default`, sets it on a copy of `data`
when that key is absent. Returns the (possibly) updated copy and whether anything changed.
Unit-tested in isolation; no DB, no I/O.

## 7. Auto-backfill worker

- `collections.auto_backfill bool NOT NULL DEFAULT false` (migration `0005`). Settable at
  collection create (`auto_backfill` in the create body, default false) and via
  `POST /v1/collections/{c}/auto-backfill` `{ "enabled": true|false }` (workspace-API-key scoped).
- `schema.Service` gains an optional `BackfillEnqueuer` seam (interface
  `Enqueue(workspace, collection uuid.UUID)`), wired chainably like the Plan 4 evaluator. After a
  successful activation (the same post-commit point as `ensureActiveIndexes`), if the collection's
  `auto_backfill` is true, it calls `enqueuer.Enqueue(ws, col)`. `nil` enqueuer ⇒ no auto-backfill
  (tests unaffected). Enqueue fires on **any** activation; the `Backfiller` self-validates each
  record, so a forced breaking change simply yields skips rather than corruption.
- `projection.Worker{backfiller, ch}` — a buffered channel of `(ws, col)`; `Enqueue` is
  non-blocking (drops a duplicate/﻿full-buffer enqueue silently — the next activation or a manual
  run still converges). `Run(ctx)` drains the channel and calls `Backfiller.Run(ctx, ws, col,
  batch)` to completion per item. `main.go` starts `go worker.Run(ctx)` and wires it as the schema
  service's enqueuer.

## 8. API surface

### Records / collections (workspace-API-key scoped)
- `POST /v1/collections/{collection}/backfill` — run backfill to completion in bounded batches;
  returns `{ "scanned", "migrated", "skipped", "remaining" }`. (Subpath form, not `:backfill`,
  per the Go ServeMux wildcard-suffix limitation already documented in Plan 2.)
- `POST /v1/collections/{collection}/auto-backfill` — body `{ "enabled": bool }`; toggles the
  opt-in flag. `200` with the new value.
- `POST /v1/collections` — create body additionally accepts optional `auto_backfill` (default false).

### Admin (admin-token gated)
- `POST /admin/replay` — body `{ "workspace_id", "collection_id", "record_id"? }`. With
  `record_id` → rebuild that record's projection; without → rebuild the whole collection.
  Returns `{ "rebuilt": N }`.

## 9. Error handling

- Backfill on a flexible collection (no active schema) → `200` with an all-zero report (no-op),
  not an error.
- A record failing re-validation during backfill is **skipped and counted**, not an error; the
  batch continues.
- Replay of a record/collection with no events → `{ "rebuilt": 0 }` (`200`); a missing
  workspace/collection in the admin body → `404`.
- Bad `auto-backfill`/`replay`/`backfill` bodies or ids → `400`; bad admin token → `401`.
- Infra/DB errors → `500`; per-record backfill failures abort that record's transaction only and
  surface as a `500` for the whole call (the already-committed records stay migrated — safe to
  re-run).
- Backfill is a system maintenance operation and is **not** policy-gated; its `migration` events
  use the `system:backfill` actor.

## 10. Performance & operational notes

- Backfill processes in bounded batches (default 200) with one transaction per record, so memory
  stays flat and lock scope is one row at a time; it never blocks reads.
- The worker is single-goroutine and processes collections serially — sufficient for v0; parallel
  workers are a future enhancement.
- The manual endpoint runs to completion synchronously; very large collections are better served
  by enabling `auto_backfill` (async) — documented, not silently truncated.
- Replay reads each record's latest event via the existing `(collection_id, record_id, seq)`
  index; collection replay is O(records) upserts.

## 11. Testing (TDD)

- **Unit (no DB):**
  - `applyDefaults` table tests: missing field gets default; present field untouched; no
    `default` → unchanged; non-object/edge schemas; "changed" flag correctness.
- **Integration (testcontainers, shared container):**
  - Replay: seed events for a record (create→update→delete), drop/garble the `records` row,
    `RebuildRecord` restores data/revision/status/schema_version; `RebuildCollection` rebuilds
    many; replay is idempotent (second run identical); record with no events → not rebuilt;
    lifecycle pseudo-rows (`record_id = collection_id`) are excluded.
  - Backfill: register v1, write records, register+activate a non-breaking v2 that adds an
    optional field with a `default`; `Run` advances all records (data gains the default,
    `schema_version` = 2, revision bumped, a `migration` event per record); a second `Run` is a
    no-op (`migrated=0`); a record made invalid for v2 (forced breaking change) is skipped and
    counted; flexible collection → empty report.
  - `events.schema_version` is populated on create/update and on migration events.
  - Auto-backfill: a collection with `auto_backfill=true` is enqueued on activation and the
    worker advances its records; `auto_backfill=false` is not enqueued.
- **End-to-end HTTP (integration-tagged):** create collection (`auto_backfill` off) → records on
  v1 → activate v2 (adds defaulted field) → `POST …/backfill` returns the migrated count →
  `GET …/records` shows the defaulted field and bumped revisions → `GET /v1/audit?type=migration`
  lists the migration events → `POST /admin/replay` for the collection returns the rebuilt count.
- Reuse the `store.NewTestPool` harness; `go test` (unit) + `go test -tags=integration`.
