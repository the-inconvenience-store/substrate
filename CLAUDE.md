# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

Substrate is an event-backed state-object store: a single Go binary serving an HTTP+JSON `/v1` API over PostgreSQL. See `README.md` for running the server, flags, admin bootstrap, and an end-to-end API walkthrough.

## Commands

The toolchain and task runner is **mise** (`mise.toml`) — not Make/Taskfile. Run `mise tasks` to list.

```sh
mise run build              # go build ./...
mise run test               # unit tests (no external deps)
mise run test:integration   # integration tests — requires Docker (testcontainers)
mise run vet                # go vet ./...
mise run sqlc:generate      # regenerate internal/db from SQL (go tool sqlc generate)
mise run run                # run the server (embedded Postgres)
```

Run a single test (mise passes flags after `--`):
```sh
go test -run TestName ./internal/pkg/                      # one unit test
go test -tags=integration -run TestName ./internal/pkg/    # one integration test
```
Note: `mise run test:integration` ignores path/`-run` args and runs the whole tagged suite; use `go test -tags=integration -run ... ./internal/pkg/` directly to scope it.

Format with `gofmt -w` before committing; `gofmt -l internal/ cmd/` must be empty.

## Data layer workflow (sqlc + goose)

Three directories form a generation pipeline — **never edit `internal/db/` by hand**:
- `internal/migrations/*.sql` — goose migrations (5-digit zero-padded, e.g. `00006_*.sql`). Embedded via `//go:embed *.sql` and applied automatically at server startup, so a new file needs no registration. The schema here is also what sqlc type-checks queries against.
- `internal/queries/*.sql` — sqlc query sources (annotated `-- name: X :one|:many|:exec|:execrows`).
- `internal/db/` — generated output (`sqlc.yaml` config).

After changing either `internal/migrations` or `internal/queries`, run `mise run sqlc:generate`, then `go build ./...`.

sqlc gotchas this codebase relies on (`sqlc.yaml` overrides):
- `uuid` → `github.com/google/uuid.UUID`; **nullable** `uuid` → `pgtype.UUID`.
- sqlc infers a query **parameter's** nullability from the column it's compared against. A param compared to a nullable column (e.g. `schema_version < $2`) generates as `pgtype.Int4`/`pgtype.UUID`, not a bare `int32`/`uuid.UUID`. Check the generated param type, not just the column.
- Adding a column to an `INSERT`/`AppendEvent` is backward-compatible: existing struct-literal callers omit the new field (→ zero value → SQL NULL). The same applies to extra `SELECT`/`RETURNING` columns (extra struct fields, unused by old callers).

## Architecture

Single module (`github.com/substrate/substrate`), single binary, layered `internal/` packages. Wiring lives in `cmd/substrate/main.go`; transport in `internal/api`; everything else is service logic behind interfaces.

**Events are authoritative; the `records` row is a projection maintained in the SAME transaction as the event append** (not async/event-sourced). Every write appends an immutable `events` row (full `state_after` snapshot per revision) and upserts the current-state `records` row atomically via `store.WithTx`. This gives time-travel/audit/replay from `events` while keeping reads a simple indexed lookup.

**The one write pipeline** (`internal/record`, `record.Service`): `policy authorize → idempotency check → schema validate → optimistic revision (If-Match) → append event + upsert record` — all in one transaction. Reliability guarantees (idempotency, optimistic concurrency, soft delete) live here, in one place. Reads (`Get`/`List`/`History`/`GetAsOf`) take an `actor` param so they can be policy-gated too.

**Optional-dependency seam pattern (the key pattern to understand).** Services declare optional collaborators as interfaces and accept them via chainable `WithX(...)` methods that return the service; `nil` means the feature is off (a no-op), which keeps unit tests DB-light and prevents import cycles. Only `main.go` injects the concrete implementations. Examples:
- `record.Service` — `Validator` (schema) + `WithEvaluator(policy.Evaluator)`.
- `schema.Service` — `New`/`NewWithIndexer` + `WithEvaluator(policy.Evaluator)` + `WithBackfillEnqueuer(...)`.
- The interface is defined in the **consuming** package (or in `policy` for the shared evaluator); the implementing package satisfies it structurally. When adding a cross-cutting concern, follow this pattern rather than importing concrete types downward.

**Package map** (each behind an interface so storage/policy/protocol can be swapped):
- `api` — routing, DTOs, HTTP error mapping (transport only). Net/http `ServeMux` **cannot** match `{wildcard}:literal`; non-CRUD actions use subpaths (e.g. `…/schemas/{version}/activate`, `…/collections/{c}/backfill`).
- `auth` — API-key middleware; pins workspace id + actor (`X-Substrate-Actor`) onto the request context (`auth.WorkspaceFrom`/`auth.ActorFrom`).
- `record` — the write pipeline + reads + time-travel (`timetravel.go`).
- `schema` — versioned JSON Schema registry, lifecycle (draft/active/deprecated), validation, and a deterministic compatibility classifier (breaking changes gated behind `force`). Reads resolve against each record's stored `schema_version`; writes validate against the collection's active version.
- `policy` — declarative allow/deny governance: a pure precedence engine (`Select`: most-specific-wins, deny-overrides ties, default mode fallback) + a DB-backed `Engine` that writes `policy_denied` events on deny.
- `query` — list endpoint param parser + **hand-built parameterized** SQL builder + keyset cursor. This and the `audit` list query are the only hand-built SQL (sqlc covers everything else); always bind values as parameters.
- `projection` — `Replayer` (rebuild the `records` projection from `events` for disaster recovery) and `Backfiller` (advance records below the active schema version: apply defaults → re-validate → `migration` event → re-stamp) + an async `Worker`.
- `audit` — `GET /v1/audit` filtered event stream (hand-built keyset SQL).
- `store` — pool open, goose migration runner, `WithTx`, and `NewTestPool` (integration harness).
- `apierr` — typed service errors (`apierr.Error{Code, Message, Details}`) with stable string codes, mapped to HTTP status **only** in the api layer (`writeErr`). Service code returns these and stays transport-agnostic; do not write HTTP statuses outside `api`.

**Decisions on the audit trail.** Allowed mutations stamp the policy decision into the (otherwise unused) `events.trace` column; denials become `policy_denied` events. `policy_denied` events are deliberately **excluded** from the record history / time-travel queries (`internal/queries/events.sql`) so they don't corrupt state reconstruction, but they remain visible via `GET /v1/audit`.

## Testing

- Integration tests carry `//go:build integration` and use `store.NewTestPool(t)` — a fresh migrated database per test inside a single shared Postgres container per run. They need Docker.
- API integration tests are white-box (`package api`, not `api_test`) and reuse server/request helpers across files: `newTestServer`/`newGovServer`/`newProjServer` build a wired `Deps`, and `do(t, …)`/`doAs(t, …, actor, …)` issue requests. Define a new harness only when existing ones lack a needed dependency; don't redefine the shared `doAs` helper.
- Follow TDD: write the failing test first. Most logic is unit-testable without a DB (e.g. `policy` precedence, `projection` defaults applier, `query`/`schema` classifier are pure).

## Design docs

`docs/superpowers/specs/` (approved design specs) and `docs/superpowers/plans/` (implementation plans) document each subsystem and the locked decisions behind it. Start with `2026-05-30-substrate-v0-core-design.md` for the whole-system rationale; consult the per-subsystem spec before changing schema-registry, query, policy, or projection behavior.
