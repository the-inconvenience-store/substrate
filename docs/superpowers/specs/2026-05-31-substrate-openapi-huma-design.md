# Substrate OpenAPI via Huma rewrite — design

**Date:** 2026-05-31
**Status:** Approved (design)
**Author:** brainstormed with Claude

## Goal

Produce a correct, always-in-sync **OpenAPI 3.1** description of the entire
Substrate HTTP API so that **TypeScript and Python clients can be generated**
from it in separate repositories.

The chosen strategy is **code-first with [Huma v2](https://huma.rocks)**: rewrite
the `internal/api` transport layer so that Go operation + struct definitions
*are* the single source of truth, and Huma emits the OpenAPI document
automatically. This guarantees the spec cannot drift from the implementation.

## Decisions (locked)

| Decision | Choice | Rationale |
|---|---|---|
| Sync strategy | Full rewrite to Huma (code-first) | Spec is generated from the handlers, so it can never silently drift. |
| Error model | RFC 7807 `application/problem+json` | Idiomatic for generated clients and OpenAPI tooling. |
| Error code preservation | `apierr.Code` → problem `title` | Keeps a machine-discernible code without a custom error envelope. |
| Surface in spec | **Everything**: `/healthz`, all `/v1/*`, all `/admin/*` | One spec, one typed client surface (incl. bootstrap + health). |
| Router | Single `huma.API` over stdlib `http.ServeMux` via the `humago` adapter | Minimal infra change; keeps Go 1.26 `ServeMux` routing. |
| Clients | `@hey-api/openapi-ts` (TS) + `openapi-python-client` (Python) | Modern, JVM-free, idiomatic typed output. |
| Client location | **Separate repositories** | This repo only publishes `api/openapi.yaml`; generation lives downstream. |
| Docs UI | Keep Huma's `/docs` (Stoplight Elements) | Free, useful, no cost to leave on. |

## Architecture

### Router & adapter

`NewRouter(Deps) http.Handler` keeps its signature and still returns an
`http.Handler`. Internally it now:

1. creates a base `http.ServeMux`,
2. builds a `huma.API` via the `humago` adapter on that mux,
3. registers **all** endpoints as Huma operations,
4. returns the mux.

`cmd/substrate/main.go` wiring is unchanged.

The existing subpath form for non-CRUD actions
(`…/schemas/{version}/activate`, `…/collections/{c}/backfill`) is retained —
that is wildcard-then-literal-*segment*, which `ServeMux` (and therefore the
`humago` adapter) allows. Only the single-segment `{x}:literal` form is
forbidden, and the codebase already avoids it.

### Operations & typed I/O

Each endpoint becomes:

```go
huma.Register(api, huma.Operation{
    OperationID: "createRecord",
    Method:      http.MethodPost,
    Path:        "/v1/collections/{collection}/records",
    Tags:        []string{"records"},
    Security:    workspaceSecurity,
}, h.createRecord)
```

with a handler of signature `func(ctx, *Input) (*Output, error)`.

- **Input structs** use Huma tags: `path:"collection"`, `query:"filter"`,
  `header:"If-Match"`, and a `Body` field for the JSON request body.
- **Output structs** carry the response body plus typed response headers
  (e.g. `ETag string \`header:"ETag"\``).
- **Response body types** are the existing service-layer DTOs used directly —
  they already have clean `json:` tags:
  - `record.Record`, `collection.Collection`, `schema.SchemaVersion`,
    `schema.Change`, `policy.PolicyRule`, `audit.Entry`, `workspace.Workspace`.
  - These gain optional `doc:` / `example:` annotations only.

This is the principal mechanical change: **handlers return `(*Output, error)`
instead of writing to an `http.ResponseWriter`.** The service-call bodies inside
each handler are otherwise nearly identical to today's.

List responses keep their `{items, next_cursor}` envelope as a typed struct.

### Auth as a security-aware middleware

Three distinct auth regimes exist and must remain distinct:

| Scheme | Operations | Credential |
|---|---|---|
| *(none)* | `healthz` | — |
| `workspaceKey` | all `/v1/*` | `Authorization: Bearer <key>` or `X-Api-Key: <key>` |
| `adminToken` | all `/admin/*` | `X-Admin-Token: <token>` |

All three security schemes are declared in the OpenAPI document (so generated
clients know exactly which credential each call needs). Because everything lives
on one `huma.API`, we can no longer wrap only the `/v1` subtree. Instead, a
single `huma.Middleware` branches on the operation's declared security:

- **no security** → pass through (e.g. `healthz`);
- **`workspaceKey`** → port the current `auth.Middleware` key-validation logic;
  on success set workspace id + actor into ctx **using the existing context
  keys**, so `auth.WorkspaceFrom(ctx)` / `auth.ActorFrom(ctx)` keep working
  unchanged inside handlers;
- **`adminToken`** → port the current inline `adminHandlers.authed` check.

The `X-Substrate-Actor` header is additionally modeled as an optional header
param on `/v1/*` operations so it surfaces in the generated clients.

Consequences:
- `adminHandlers` lose their per-method inline token checks (moved to the
  middleware).
- `healthz` moves off the outer mux into a tagged `health`/`meta` operation.
- The standalone `auth.Middleware` HTTP wrapper is replaced by the Huma
  middleware; the `auth` package's context helpers and key-validation core are
  reused.

### Error model — RFC 7807

`writeErr` and the `httpx.Envelope` path are retired for the Huma surface.
A translation layer maps `apierr.Error` into Huma's error pipeline:

- `apierr.Code` → HTTP status (the existing `HTTPStatus()` mapping) and the
  problem **`title`** field (preserves the stable string code);
- `apierr.Message` → problem **`detail`**;
- `apierr.Details` → an extension member on the problem object.

Implemented via a `huma.NewError` override (or an equivalent small
`apierr.Error → huma.StatusError` adapter invoked where handlers return errors).
The api layer remains the only place that knows HTTP status — consistent with
the existing `apierr` rule.

### Spec artifact & freshness guarantee

Huma auto-serves `/openapi.yaml`, `/openapi.json`, and interactive docs at
`/docs`. On top of that:

- A committed **`api/openapi.yaml`** is the single artifact the downstream
  TS/Python client repos consume.
- A `mise run openapi:dump` task builds the API in-process and writes that file.
- A unit test **`TestOpenAPISpecUpToDate`** regenerates the spec and diffs it
  against the committed file, **failing on drift**. This is the enforced sync
  guarantee: the committed spec cannot go stale without a red test.

### Headers & query params (modeled explicitly)

- `If-Match` — required input header on update (optimistic concurrency).
- `ETag` — output header on create / get / update / revert.
- `Idempotency-Key` — input header on create / update.
- `filter` (repeatable), `sort`, `limit`, `cursor` — list query params.
- `as_of` — get query param (revision | event id | RFC3339 timestamp).

## Endpoint inventory

All become Huma operations (tag in parentheses):

**health** — `GET /healthz`

**collections** — `POST /v1/collections`

**records** —
`POST /v1/collections/{collection}/records`,
`GET …/records`,
`GET …/records/{id}`,
`PATCH …/records/{id}`,
`DELETE …/records/{id}`,
`GET …/records/{id}/history`,
`POST …/records/{id}/revert`

**schemas** —
`POST /v1/collections/{collection}/schemas`,
`GET …/schemas`,
`GET …/schemas/{version}`,
`POST …/schemas/{version}/activate`,
`POST …/schemas/{version}/deprecate`

**policies** —
`POST /v1/policies`,
`GET /v1/policies`,
`DELETE /v1/policies/{id}`

**audit** — `GET /v1/audit`

**projection** —
`POST /v1/collections/{collection}/backfill`,
`POST /v1/collections/{collection}/auto-backfill`

**admin** —
`POST /admin/workspaces`,
`POST /admin/workspaces/{ws}/api-keys`,
`PUT /admin/workspaces/{ws}/policy-mode`,
`POST /admin/replay`

## Migration & testing strategy (TDD)

Migrate one endpoint group at a time (records → schemas → policies → audit →
projection → admin → health), keeping `mise run test` green at each step.

- The existing white-box api tests (`do`/`doAs`, `newTestServer`,
  `newGovServer`, `newProjServer`) keep exercising the **same routes, JSON
  request/response bodies, and status codes**.
- The **only intentional behavior change is the error body shape** (now RFC
  7807). Those specific assertions are updated; the `code → title` mapping keeps
  code-based assertions cheap.
- New: `TestOpenAPISpecUpToDate` (spec-drift guard) and the `openapi:dump` task.

## Out of scope

- Actual client generation (`@hey-api/openapi-ts`, `openapi-python-client`) —
  lives in separate repositories. We only document the recommended commands they
  run against `api/openapi.yaml`.
- No changes to the service layer, data layer (sqlc/goose), or business logic.
- No new endpoints; this is a transport-layer rewrite that preserves behavior
  (except the error body shape).

## Risks & mitigations

- **Behavioral regressions during rewrite** → group-by-group migration with the
  existing integration tests as the safety net; only error-body assertions
  change intentionally.
- **Auth regime divergence** → security-aware middleware reuses the existing
  `auth` validation core and context keys; covered by existing auth tests.
- **Spec drift** → `TestOpenAPISpecUpToDate` makes drift a build failure.
- **Huma path/`ServeMux` mismatch** → retained subpath form is already
  `ServeMux`-compatible; verified against the Go 1.22+ wildcard rules.
