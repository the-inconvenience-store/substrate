# Substrate OpenAPI via Huma — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Rewrite `internal/api` onto Huma v2 so the Go operation + struct definitions are the single source of truth for an auto-generated OpenAPI 3.1 document, published as `api/openapi.yaml` for downstream TypeScript/Python client generation.

**Architecture:** Strangler-fig migration. A single `huma.API` is mounted on the existing `http.ServeMux` via the `humago` adapter. Endpoints move from the legacy `net/http` handlers to typed Huma operations **one group at a time**; at every step the build and tests stay green because migrated routes are served by Huma (more-specific `ServeMux` patterns win) while not-yet-migrated routes fall through to the legacy `/v1/` mount. Auth becomes a security-aware Huma middleware; errors become RFC 7807 problem+json.

**Tech Stack:** Go 1.26, `github.com/danielgtaylor/huma/v2` (v2.38.0) + `adapters/humago`, existing `apierr`/`auth`/service packages, mise task runner.

**Design spec:** [docs/superpowers/specs/2026-05-31-substrate-openapi-huma-design.md](../specs/2026-05-31-substrate-openapi-huma-design.md)

---

## File structure

| File | Responsibility |
|---|---|
| `internal/api/openapi.go` (new) | `buildAPI(mux, Deps) huma.API`: config, security schemes, error mapping, auth middleware, group registration. `SpecYAML()` for dumping. |
| `internal/api/errors.go` (new) | `problem` RFC 7807 error type + `toHuma(error) error` adapter. |
| `internal/api/io.go` (new) | Shared I/O helpers: `actorHeader` embed, typed response-body structs, `resolveCollectionCtx`, `etag`. |
| `internal/api/op_records.go` (new) | Records + collections Huma operations (input/output structs + handlers). |
| `internal/api/op_schemas.go` (new) | Schema-registry Huma operations. |
| `internal/api/op_policies.go` (new) | Policy Huma operations. |
| `internal/api/op_audit.go` (new) | Audit Huma operation. |
| `internal/api/op_projection.go` (new) | Backfill/auto-backfill Huma operations. |
| `internal/api/op_admin.go` (new) | Admin Huma operations. |
| `internal/api/router.go` (modify) | `NewRouter`: build huma API + shrinking legacy mount during migration; legacy removed at the end. |
| `internal/api/handlers.go` / `*_handlers.go` (delete incrementally) | Legacy `ResponseWriter` handlers, removed per group as migrated. |
| `internal/auth/auth.go` (modify) | Export context keys so the Huma middleware can set workspace id. |
| `cmd/openapi-dump/main.go` (new) | Writes `api/openapi.yaml` from `api.SpecYAML()`. |
| `internal/api/spec_test.go` (new) | `TestOpenAPISpecUpToDate` drift guard (unit, no DB). |
| `api/openapi.yaml` (new, generated) | Committed spec artifact consumed by client repos. |
| `mise.toml` (modify) | `openapi:dump` task. |
| `README.md` (modify) | Client-generation instructions. |

---

## Task 1: Add Huma dependency and export auth context keys

**Files:**
- Modify: `go.mod`, `go.sum`
- Modify: `internal/auth/auth.go`

- [ ] **Step 1: Add the Huma dependency**

Run (network required; if the sandbox blocks it, re-run with sandbox disabled):

```bash
go get github.com/danielgtaylor/huma/v2@v2.38.0
go mod tidy
```

Expected: `github.com/danielgtaylor/huma/v2 v2.38.0` appears in `go.mod` as a direct dependency.

- [ ] **Step 2: Export the auth context keys**

The Huma middleware (Task 2) must store the workspace id under the *same* context key that `auth.WorkspaceFrom` reads. Export the key type and constants. In `internal/auth/auth.go` replace:

```go
type ctxKey int

const (
	wsKey ctxKey = iota
	actorKey
)
```

with:

```go
// CtxKey identifies values this package stores on a request context. Exported so
// transport adapters (e.g. the Huma middleware) can set them via huma.WithValue.
type CtxKey int

const (
	WorkspaceKey CtxKey = iota
	ActorKey
)
```

Then update the three internal references in this file: in `Middleware`, change `context.WithValue(r.Context(), wsKey, ws)` → `context.WithValue(r.Context(), WorkspaceKey, ws)` and `context.WithValue(ctx, actorKey, actor)` → `context.WithValue(ctx, ActorKey, actor)`; in `WorkspaceFrom`, change `ctx.Value(wsKey)` → `ctx.Value(WorkspaceKey)`; in `ActorFrom`, change `ctx.Value(actorKey)` → `ctx.Value(ActorKey)`.

- [ ] **Step 3: Build**

Run: `mise run build`
Expected: success.

- [ ] **Step 4: Vet + format**

Run: `mise run vet && gofmt -l internal/ cmd/`
Expected: vet passes, `gofmt -l` prints nothing.

- [ ] **Step 5: Commit**

```bash
git add go.mod go.sum internal/auth/auth.go
git commit -m "build: add huma v2 dep; export auth context keys"
```

---

## Task 2: Core scaffolding — config, errors, auth middleware, healthz

This task introduces the Huma API alongside the legacy router and migrates only `/healthz`. All `/v1/*` and `/admin/*` routes remain on the legacy mount and keep passing.

**Files:**
- Create: `internal/api/errors.go`
- Create: `internal/api/openapi.go`
- Modify: `internal/api/router.go`
- Test: `internal/api/router_test.go` (existing healthz test still passes)

- [ ] **Step 1: Write the RFC 7807 error type and adapter**

Create `internal/api/errors.go`:

```go
package api

import (
	"github.com/danielgtaylor/huma/v2"

	"github.com/substrate/substrate/internal/apierr"
)

// problem is an RFC 7807 application/problem+json error body. The stable
// machine-readable apierr.Code is carried in Title; Details (if any) are merged
// in as an extension member.
type problem struct {
	status  int
	Type    string         `json:"type,omitempty"`
	Title   string         `json:"title"`
	Status  int            `json:"status"`
	Detail  string         `json:"detail,omitempty"`
	Details map[string]any `json:"details,omitempty"`
}

func (p *problem) Error() string                 { return p.Title }
func (p *problem) GetStatus() int                { return p.status }
func (p *problem) ContentType(string) string     { return "application/problem+json" }

// toHuma converts a service error into an RFC 7807 problem. apierr.Error maps its
// code to the HTTP status and Title; anything else becomes a 500.
func toHuma(err error) error {
	if err == nil {
		return nil
	}
	if e, ok := apierr.As(err); ok {
		st := e.HTTPStatus()
		return &problem{status: st, Status: st, Title: string(e.Code), Detail: e.Message, Details: e.Details}
	}
	return &problem{status: 500, Status: 500, Title: string(apierr.Internal), Detail: "internal error"}
}

var _ huma.StatusError = (*problem)(nil)
```

- [ ] **Step 2: Build to verify it fails (huma not yet imported elsewhere is fine)**

Run: `mise run build`
Expected: success (this file compiles on its own).

- [ ] **Step 3: Write the API builder, security schemes, and auth middleware**

Create `internal/api/openapi.go`:

```go
package api

import (
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"

	"github.com/substrate/substrate/internal/auth"
)

const (
	schemeWorkspace = "workspaceKey"
	schemeAdmin     = "adminToken"
)

// buildAPI constructs the Huma API on mux and registers every migrated group.
// Registration never calls the services, so SpecYAML can pass zero-value Deps.
func buildAPI(mux *http.ServeMux, d Deps) huma.API {
	cfg := huma.DefaultConfig("Substrate API", "0.1.0")
	if cfg.OpenAPI.Components == nil {
		cfg.OpenAPI.Components = &huma.Components{}
	}
	cfg.OpenAPI.Components.SecuritySchemes = map[string]*huma.SecurityScheme{
		schemeWorkspace: {Type: "http", Scheme: "bearer", Description: "Workspace API key (also accepted as X-Api-Key)."},
		schemeAdmin:     {Type: "apiKey", In: "header", Name: "X-Admin-Token", Description: "Admin bootstrap token."},
	}

	api := humago.New(mux, cfg)
	api.UseMiddleware(authMiddleware(api, d.Workspaces, d.AdminToken))

	registerHealth(api)
	if d.Records == nil {
		return api // health-only mode (scaffold test)
	}
	// Group registrations are added here as they migrate (Tasks 4-9).
	return api
}

// SpecYAML renders the full OpenAPI document. Used by the dump command and the
// drift test. Uses zero-value service pointers because registration is inert.
func SpecYAML() ([]byte, error) {
	api := buildAPI(http.NewServeMux(), specDeps())
	return api.OpenAPI().YAML()
}

func registerHealth(api huma.API) {
	type out struct {
		Body struct {
			Status string `json:"status" example:"ok"`
		}
	}
	huma.Register(api, huma.Operation{
		OperationID: "health-check",
		Method:      http.MethodGet,
		Path:        "/healthz",
		Tags:        []string{"meta"},
		Summary:     "Liveness probe",
	}, func(ctx context.Context, _ *struct{}) (*out, error) {
		o := &out{}
		o.Body.Status = "ok"
		return o, nil
	})
}

// authMiddleware enforces the security scheme each operation declares:
//   - no security        -> pass through
//   - schemeWorkspace     -> verify API key, pin workspace id on the context
//   - schemeAdmin         -> require matching X-Admin-Token
func authMiddleware(api huma.API, verify auth.Verifier, adminToken string) func(huma.Context, func(huma.Context)) {
	return func(ctx huma.Context, next func(huma.Context)) {
		switch primaryScheme(ctx.Operation()) {
		case schemeWorkspace:
			key := bearerFromCtx(ctx)
			if key == "" {
				_ = huma.WriteErr(api, ctx, http.StatusUnauthorized, "missing api key")
				return
			}
			ws, err := verify.VerifyKey(ctx.Context(), key)
			if err != nil {
				_ = huma.WriteErr(api, ctx, http.StatusUnauthorized, "invalid api key")
				return
			}
			next(huma.WithValue(ctx, auth.WorkspaceKey, ws))
		case schemeAdmin:
			if adminToken == "" || ctx.Header("X-Admin-Token") != adminToken {
				_ = huma.WriteErr(api, ctx, http.StatusUnauthorized, "invalid admin token")
				return
			}
			next(ctx)
		default:
			next(ctx)
		}
	}
}

func primaryScheme(op *huma.Operation) string {
	for _, req := range op.Security {
		for name := range req {
			return name
		}
	}
	return ""
}

func bearerFromCtx(ctx huma.Context) string {
	if h := ctx.Header("Authorization"); len(h) > 7 && h[:7] == "Bearer " {
		return h[7:]
	}
	return ctx.Header("X-Api-Key")
}
```

Add the missing `context` import to the file's import block (used by `registerHealth`).

- [ ] **Step 4: Add `specDeps` helper**

Append to `internal/api/openapi.go`:

```go
// specDeps returns Deps with inert (zero-value) service pointers for spec
// generation; handlers are registered but never executed.
func specDeps() Deps {
	return Deps{
		Workspaces:  &workspace.Service{},
		Collections: &collection.Service{},
		Records:     &record.Service{},
		Schemas:     &schema.Service{},
		Policies:    &policy.Service{},
		Audit:       &audit.Service{},
		Backfiller:  &projection.Backfiller{},
		Replayer:    &projection.Replayer{},
	}
}
```

Add imports `workspace`, `collection`, `record`, `schema`, `policy`, `audit`, `projection` to the file. (If any of these types cannot be instantiated as a bare `&T{}` zero value, instead capture them at the first real `NewRouter` call; but all are plain structs here.)

- [ ] **Step 5: Wire `buildAPI` into `NewRouter`, migrating healthz off the legacy mux**

In `internal/api/router.go`, remove the existing `mux.HandleFunc("GET /healthz", ...)` block and the `httpx` import if now unused. Restructure `NewRouter` so the huma API is built first, then the legacy mounts are added for the still-unmigrated groups. Replace the body with:

```go
func NewRouter(d Deps) http.Handler {
	mux := http.NewServeMux()
	_ = buildAPI(mux, d) // registers /healthz (+ migrated groups)

	if d.Records == nil {
		return mux // health-only mode
	}

	h := &handlers{collections: d.Collections, records: d.Records}

	// --- legacy mount: groups not yet migrated to Huma ---
	api := http.NewServeMux()
	api.HandleFunc("POST /v1/collections", h.createCollection)
	api.HandleFunc("POST /v1/collections/{collection}/records", h.createRecord)
	api.HandleFunc("GET /v1/collections/{collection}/records", h.listRecords)
	api.HandleFunc("GET /v1/collections/{collection}/records/{id}", h.getRecord)
	api.HandleFunc("PATCH /v1/collections/{collection}/records/{id}", h.updateRecord)
	api.HandleFunc("DELETE /v1/collections/{collection}/records/{id}", h.deleteRecord)
	api.HandleFunc("GET /v1/collections/{collection}/records/{id}/history", h.recordHistory)
	api.HandleFunc("POST /v1/collections/{collection}/records/{id}/revert", h.revertRecord)

	sh := &schemaHandlers{h: h, schemas: d.Schemas}
	api.HandleFunc("POST /v1/collections/{collection}/schemas", sh.register)
	api.HandleFunc("GET /v1/collections/{collection}/schemas", sh.list)
	api.HandleFunc("GET /v1/collections/{collection}/schemas/{version}", sh.get)
	api.HandleFunc("POST /v1/collections/{collection}/schemas/{version}/activate", sh.activate)
	api.HandleFunc("POST /v1/collections/{collection}/schemas/{version}/deprecate", sh.deprecate)

	ph := &policyHandlers{h: h, policies: d.Policies}
	api.HandleFunc("POST /v1/policies", ph.create)
	api.HandleFunc("GET /v1/policies", ph.list)
	api.HandleFunc("DELETE /v1/policies/{id}", ph.delete)

	aud := &auditHandlers{h: h, audit: d.Audit}
	api.HandleFunc("GET /v1/audit", aud.list)

	pjh := &projectionHandlers{h: h, backfiller: d.Backfiller, eval: d.Evaluator}
	api.HandleFunc("POST /v1/collections/{collection}/backfill", pjh.backfill)
	api.HandleFunc("POST /v1/collections/{collection}/auto-backfill", pjh.setAutoBackfill)

	mux.Handle("/v1/", auth.Middleware(d.Workspaces)(api))

	admin := &adminHandlers{workspaces: d.Workspaces, token: d.AdminToken, replayer: d.Replayer}
	mux.HandleFunc("POST /admin/workspaces", admin.createWorkspace)
	mux.HandleFunc("POST /admin/workspaces/{ws}/api-keys", admin.createKey)
	mux.HandleFunc("PUT /admin/workspaces/{ws}/policy-mode", admin.setPolicyMode)
	mux.HandleFunc("POST /admin/replay", admin.replay)

	return mux
}
```

The only behavioral change in this task: `/healthz` is now served by Huma. The legacy `/v1/` mount and the more-specific Huma `/healthz` pattern coexist on the same `ServeMux` without conflict.

- [ ] **Step 6: Build, vet, format**

Run: `mise run build && mise run vet && gofmt -l internal/ cmd/`
Expected: build + vet pass; `gofmt -l` empty.

- [ ] **Step 7: Verify healthz via the existing router test**

Run: `go test -run TestHealth ./internal/api/ 2>&1 | tail -5` (use the actual healthz test name from `router_test.go`; if it is integration-tagged, run `go test -tags=integration -run TestHealth ./internal/api/`).
Expected: PASS — `GET /healthz` returns 200 with `{"status":"ok"}`.

- [ ] **Step 8: Verify the full legacy suite still passes (Docker required)**

Run: `go test -tags=integration ./internal/api/ 2>&1 | tail -15`
Expected: PASS — all existing endpoints still work through the legacy mount.

- [ ] **Step 9: Commit**

```bash
git add internal/api/errors.go internal/api/openapi.go internal/api/router.go
git commit -m "feat(api): mount huma API alongside legacy router; migrate healthz"
```

---

## Task 3: Shared I/O helpers

**Files:**
- Create: `internal/api/io.go`

- [ ] **Step 1: Write the shared helpers**

Create `internal/api/io.go`:

```go
package api

import (
	"context"
	"strconv"

	"github.com/substrate/substrate/internal/auth"
	"github.com/substrate/substrate/internal/collection"
)

// actorHeader is embedded into every workspace-scoped input so the optional
// X-Substrate-Actor header is documented in the generated clients.
type actorHeader struct {
	Actor string `header:"X-Substrate-Actor" doc:"Logical actor performing the request." default:"anonymous"`
}

func (a actorHeader) actor() string {
	if a.Actor == "" {
		return "anonymous"
	}
	return a.Actor
}

// resolveCollectionCtx looks up a collection by name within the request's
// workspace (pinned on the context by the auth middleware).
func (h *handlers) resolveCollectionCtx(ctx context.Context, name string) (collection.Collection, error) {
	return h.collections.GetByName(ctx, auth.WorkspaceFrom(ctx), name)
}

// etag formats a revision as a strong ETag value, e.g. "1".
func etag(rev int64) string {
	return strconv.Quote(strconv.FormatInt(rev, 10))
}
```

- [ ] **Step 2: Build**

Run: `mise run build`
Expected: success (helpers are unused for now; Go allows unused package-level funcs/methods).

- [ ] **Step 3: Commit**

```bash
git add internal/api/io.go
git commit -m "feat(api): shared huma I/O helpers (actor header, collection resolve, etag)"
```

---

## Task 4: Migrate collections + records

**Files:**
- Create: `internal/api/op_records.go`
- Modify: `internal/api/router.go` (move group from legacy to huma)
- Modify: `internal/api/handlers.go` (delete migrated legacy handlers)

- [ ] **Step 1: Write the records/collections operations**

Create `internal/api/op_records.go`:

```go
package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"

	"github.com/substrate/substrate/internal/apierr"
	"github.com/substrate/substrate/internal/auth"
	"github.com/substrate/substrate/internal/query"
	"github.com/substrate/substrate/internal/record"
)

var workspaceSec = []map[string][]string{{schemeWorkspace: {}}}

// recordListBody is the typed list envelope (replaces the ad-hoc map).
type recordListBody struct {
	Items      []record.Record `json:"items"`
	NextCursor string          `json:"next_cursor"`
}

func registerRecords(api huma.API, h *handlers) {
	// --- create collection ---
	type createCollectionIn struct {
		actorHeader
		Body struct {
			Name         string `json:"name"`
			Level        string `json:"level"`
			AutoBackfill bool   `json:"auto_backfill"`
		}
	}
	type collectionOut struct{ Body collection.Collection }
	huma.Register(api, huma.Operation{
		OperationID: "create-collection", Method: http.MethodPost, Path: "/v1/collections",
		Tags: []string{"collections"}, Security: workspaceSec, DefaultStatus: http.StatusCreated,
	}, func(ctx context.Context, in *createCollectionIn) (*collectionOut, error) {
		ws := auth.WorkspaceFrom(ctx)
		c, err := h.collections.Create(ctx, ws, in.Body.Name)
		if err != nil {
			return nil, toHuma(err)
		}
		if in.Body.AutoBackfill {
			if err := h.collections.SetAutoBackfill(ctx, c.WorkspaceID, c.ID, true); err != nil {
				return nil, toHuma(err)
			}
			c.AutoBackfill = true
		}
		return &collectionOut{Body: c}, nil
	})

	// --- create record ---
	type createRecordIn struct {
		actorHeader
		Collection     string `path:"collection"`
		IdempotencyKey string `header:"Idempotency-Key"`
		Body           struct {
			Data map[string]any `json:"data"`
		}
	}
	type recordOut struct {
		ETag string `header:"ETag"`
		Body record.Record
	}
	huma.Register(api, huma.Operation{
		OperationID: "create-record", Method: http.MethodPost,
		Path: "/v1/collections/{collection}/records",
		Tags: []string{"records"}, Security: workspaceSec, DefaultStatus: http.StatusCreated,
	}, func(ctx context.Context, in *createRecordIn) (*recordOut, error) {
		c, err := h.resolveCollectionCtx(ctx, in.Collection)
		if err != nil {
			return nil, toHuma(err)
		}
		rec, err := h.records.Create(ctx, record.CreateCmd{
			Workspace: c.WorkspaceID, Collection: c.ID, Actor: in.actor(),
			Data: in.Body.Data, IdempotencyKey: in.IdempotencyKey,
		})
		if err != nil {
			return nil, toHuma(err)
		}
		return &recordOut{ETag: etag(rec.Revision), Body: rec}, nil
	})

	// --- list records ---
	type listRecordsIn struct {
		actorHeader
		Collection string   `path:"collection"`
		Filter     []string `query:"filter"`
		Sort       string   `query:"sort"`
		Limit      string   `query:"limit"`
		Cursor     string   `query:"cursor"`
	}
	type listOut struct{ Body recordListBody }
	huma.Register(api, huma.Operation{
		OperationID: "list-records", Method: http.MethodGet,
		Path: "/v1/collections/{collection}/records",
		Tags: []string{"records"}, Security: workspaceSec,
	}, func(ctx context.Context, in *listRecordsIn) (*listOut, error) {
		c, err := h.resolveCollectionCtx(ctx, in.Collection)
		if err != nil {
			return nil, toHuma(err)
		}
		q, err := query.Parse(in.Filter, in.Sort, in.Limit, in.Cursor)
		if err != nil {
			return nil, toHuma(err)
		}
		items, next, err := h.records.List(ctx, c.WorkspaceID, c.ID, in.actor(), q)
		if err != nil {
			return nil, toHuma(err)
		}
		if items == nil {
			items = []record.Record{}
		}
		return &listOut{Body: recordListBody{Items: items, NextCursor: next}}, nil
	})

	// --- get record (optionally as_of) ---
	type getRecordIn struct {
		actorHeader
		Collection string `path:"collection"`
		ID         string `path:"id"`
		AsOf       string `query:"as_of"`
	}
	huma.Register(api, huma.Operation{
		OperationID: "get-record", Method: http.MethodGet,
		Path: "/v1/collections/{collection}/records/{id}",
		Tags: []string{"records"}, Security: workspaceSec,
	}, func(ctx context.Context, in *getRecordIn) (*recordOut, error) {
		c, err := h.resolveCollectionCtx(ctx, in.Collection)
		if err != nil {
			return nil, toHuma(err)
		}
		id, err := uuid.Parse(in.ID)
		if err != nil {
			return nil, toHuma(apierr.New(apierr.BadRequest, "invalid id"))
		}
		if in.AsOf != "" {
			at, perr := parseAsOf(in.AsOf)
			if perr != nil {
				return nil, toHuma(perr)
			}
			rec, err := h.records.GetAsOf(ctx, c.WorkspaceID, c.ID, id, at, in.actor())
			if err != nil {
				return nil, toHuma(err)
			}
			return &recordOut{Body: rec}, nil
		}
		rec, err := h.records.Get(ctx, c.WorkspaceID, c.ID, id, in.actor())
		if err != nil {
			return nil, toHuma(err)
		}
		return &recordOut{ETag: etag(rec.Revision), Body: rec}, nil
	})

	// --- update record ---
	type updateRecordIn struct {
		actorHeader
		Collection     string `path:"collection"`
		ID             string `path:"id"`
		IfMatch        string `header:"If-Match"`
		IdempotencyKey string `header:"Idempotency-Key"`
		Body           struct {
			Data map[string]any `json:"data"`
		}
	}
	huma.Register(api, huma.Operation{
		OperationID: "update-record", Method: http.MethodPatch,
		Path: "/v1/collections/{collection}/records/{id}",
		Tags: []string{"records"}, Security: workspaceSec,
	}, func(ctx context.Context, in *updateRecordIn) (*recordOut, error) {
		c, err := h.resolveCollectionCtx(ctx, in.Collection)
		if err != nil {
			return nil, toHuma(err)
		}
		id, err := uuid.Parse(in.ID)
		if err != nil {
			return nil, toHuma(apierr.New(apierr.BadRequest, "invalid id"))
		}
		rev, err := parseIfMatch(in.IfMatch)
		if err != nil {
			return nil, toHuma(err)
		}
		rec, err := h.records.Update(ctx, record.UpdateCmd{
			Workspace: c.WorkspaceID, Collection: c.ID, ID: id, ExpectedRevision: rev,
			Actor: in.actor(), Data: in.Body.Data, IdempotencyKey: in.IdempotencyKey,
		})
		if err != nil {
			return nil, toHuma(err)
		}
		return &recordOut{ETag: etag(rec.Revision), Body: rec}, nil
	})

	// --- delete record ---
	type deleteRecordIn struct {
		actorHeader
		Collection string `path:"collection"`
		ID         string `path:"id"`
	}
	huma.Register(api, huma.Operation{
		OperationID: "delete-record", Method: http.MethodDelete,
		Path: "/v1/collections/{collection}/records/{id}",
		Tags: []string{"records"}, Security: workspaceSec, DefaultStatus: http.StatusNoContent,
	}, func(ctx context.Context, in *deleteRecordIn) (*struct{}, error) {
		c, err := h.resolveCollectionCtx(ctx, in.Collection)
		if err != nil {
			return nil, toHuma(err)
		}
		id, err := uuid.Parse(in.ID)
		if err != nil {
			return nil, toHuma(apierr.New(apierr.BadRequest, "invalid id"))
		}
		if err := h.records.Delete(ctx, c.WorkspaceID, c.ID, id, in.actor()); err != nil {
			return nil, toHuma(err)
		}
		return nil, nil
	})

	// --- record history ---
	type historyIn struct {
		actorHeader
		Collection string `path:"collection"`
		ID         string `path:"id"`
	}
	type historyOut struct{ Body []record.HistoryEntry }
	huma.Register(api, huma.Operation{
		OperationID: "record-history", Method: http.MethodGet,
		Path: "/v1/collections/{collection}/records/{id}/history",
		Tags: []string{"records"}, Security: workspaceSec,
	}, func(ctx context.Context, in *historyIn) (*historyOut, error) {
		c, err := h.resolveCollectionCtx(ctx, in.Collection)
		if err != nil {
			return nil, toHuma(err)
		}
		id, err := uuid.Parse(in.ID)
		if err != nil {
			return nil, toHuma(apierr.New(apierr.BadRequest, "invalid id"))
		}
		hist, err := h.records.History(ctx, c.WorkspaceID, c.ID, id, in.actor())
		if err != nil {
			return nil, toHuma(err)
		}
		return &historyOut{Body: hist}, nil
	})

	// --- revert record ---
	type revertIn struct {
		actorHeader
		Collection string `path:"collection"`
		ID         string `path:"id"`
		Body       struct {
			To string `json:"to"`
		}
	}
	huma.Register(api, huma.Operation{
		OperationID: "revert-record", Method: http.MethodPost,
		Path: "/v1/collections/{collection}/records/{id}/revert",
		Tags: []string{"records"}, Security: workspaceSec,
	}, func(ctx context.Context, in *revertIn) (*recordOut, error) {
		c, err := h.resolveCollectionCtx(ctx, in.Collection)
		if err != nil {
			return nil, toHuma(err)
		}
		id, err := uuid.Parse(in.ID)
		if err != nil {
			return nil, toHuma(apierr.New(apierr.BadRequest, "invalid id"))
		}
		at, perr := parseAsOf(in.Body.To)
		if perr != nil {
			return nil, toHuma(perr)
		}
		rec, err := h.records.Revert(ctx, c.WorkspaceID, c.ID, id, at, in.actor())
		if err != nil {
			return nil, toHuma(err)
		}
		return &recordOut{ETag: etag(rec.Revision), Body: rec}, nil
	})
}
```

Note: `If-Match` is modeled as an optional header (not `required:"true"`) so that a missing header continues to return a 400 from `parseIfMatch` rather than Huma's 422 — preserving existing behavior.

- [ ] **Step 2: Register the group in `buildAPI`**

In `internal/api/openapi.go`, inside `buildAPI`, replace the `// Group registrations ...` comment with:

```go
	h := &handlers{collections: d.Collections, records: d.Records}
	registerRecords(api, h)
```

- [ ] **Step 3: Remove the records/collections routes from the legacy mount**

In `internal/api/router.go`, delete these lines from the legacy `api` mux block:

```go
	api.HandleFunc("POST /v1/collections", h.createCollection)
	api.HandleFunc("POST /v1/collections/{collection}/records", h.createRecord)
	api.HandleFunc("GET /v1/collections/{collection}/records", h.listRecords)
	api.HandleFunc("GET /v1/collections/{collection}/records/{id}", h.getRecord)
	api.HandleFunc("PATCH /v1/collections/{collection}/records/{id}", h.updateRecord)
	api.HandleFunc("DELETE /v1/collections/{collection}/records/{id}", h.deleteRecord)
	api.HandleFunc("GET /v1/collections/{collection}/records/{id}/history", h.recordHistory)
	api.HandleFunc("POST /v1/collections/{collection}/records/{id}/revert", h.revertRecord)
```

The `h := &handlers{...}` line in `router.go` stays (still used by other legacy groups).

- [ ] **Step 4: Delete the migrated legacy handlers**

In `internal/api/handlers.go`, delete the now-unused methods: `createCollection`, `createRecord`, `listRecords`, `getRecord`, `updateRecord`, `deleteRecord`, `recordHistory`, `revertRecord`. **Keep** the request-based `resolveCollection` method — it is still called by the legacy schema/policy/audit/projection handlers until their tasks (it is removed in Task 10). Also keep `writeErr`, `setETag`, `parseIfMatch`, `parseAsOf`, `parseTime`, the `handlers` struct, and the admin handlers. Remove imports that become unused (e.g. `query`, `record` if no longer referenced in this file — let the compiler guide you).

- [ ] **Step 5: Build, vet, format**

Run: `mise run build && mise run vet && gofmt -l internal/ cmd/`
Expected: build + vet pass; `gofmt -l` empty. (If `setETag` is now unused, leave it — it is still referenced by other legacy handlers until Task 9; the compiler will confirm.)

- [ ] **Step 6: Run the records integration tests**

Run: `go test -tags=integration -run 'TestRecord|TestList' ./internal/api/ 2>&1 | tail -20`
Expected: PASS. These assert status codes, JSON bodies (`id`, `revision`), and the `ETag: "1"` header — all preserved by the Huma operations.

- [ ] **Step 7: Commit**

```bash
git add internal/api/op_records.go internal/api/openapi.go internal/api/router.go internal/api/handlers.go
git commit -m "feat(api): migrate collections+records to huma operations"
```

---

## Task 5: Migrate schemas

**Files:**
- Create: `internal/api/op_schemas.go`
- Modify: `internal/api/openapi.go`, `internal/api/router.go`, delete `internal/api/schema_handlers.go`

- [ ] **Step 1: Write the schema operations**

Create `internal/api/op_schemas.go`:

```go
package api

import (
	"context"
	"net/http"
	"strconv"

	"github.com/danielgtaylor/huma/v2"

	"github.com/substrate/substrate/internal/apierr"
	"github.com/substrate/substrate/internal/auth"
	"github.com/substrate/substrate/internal/schema"
)

func registerSchemas(api huma.API, h *handlers, schemas *schema.Service) {
	type schemaOut struct{ Body schema.SchemaVersion }

	// --- register schema ---
	type registerIn struct {
		actorHeader
		Collection string `path:"collection"`
		Body       struct {
			JSONSchema    map[string]any `json:"json_schema"`
			IndexedFields []string       `json:"indexed_fields"`
			Activate      bool           `json:"activate"`
			Force         bool           `json:"force"`
			Rationale     string         `json:"rationale"`
		}
	}
	huma.Register(api, huma.Operation{
		OperationID: "register-schema", Method: http.MethodPost,
		Path: "/v1/collections/{collection}/schemas",
		Tags: []string{"schemas"}, Security: workspaceSec, DefaultStatus: http.StatusCreated,
	}, func(ctx context.Context, in *registerIn) (*schemaOut, error) {
		c, err := h.resolveCollectionCtx(ctx, in.Collection)
		if err != nil {
			return nil, toHuma(err)
		}
		out, err := schemas.Register(ctx, schema.RegisterCmd{
			Workspace: c.WorkspaceID, Collection: c.ID, Actor: in.actor(),
			JSONSchema: in.Body.JSONSchema, IndexedFields: in.Body.IndexedFields,
			Activate: in.Body.Activate, Force: in.Body.Force, Rationale: in.Body.Rationale,
		})
		if err != nil {
			return nil, toHuma(err)
		}
		return &schemaOut{Body: out}, nil
	})

	// --- list schemas ---
	type listIn struct {
		actorHeader
		Collection string `path:"collection"`
	}
	type listOut struct{ Body []schema.SchemaVersion }
	huma.Register(api, huma.Operation{
		OperationID: "list-schemas", Method: http.MethodGet,
		Path: "/v1/collections/{collection}/schemas",
		Tags: []string{"schemas"}, Security: workspaceSec,
	}, func(ctx context.Context, in *listIn) (*listOut, error) {
		c, err := h.resolveCollectionCtx(ctx, in.Collection)
		if err != nil {
			return nil, toHuma(err)
		}
		out, err := schemas.List(ctx, c.ID)
		if err != nil {
			return nil, toHuma(err)
		}
		if out == nil {
			out = []schema.SchemaVersion{}
		}
		return &listOut{Body: out}, nil
	})

	// --- get schema version ---
	type getIn struct {
		actorHeader
		Collection string `path:"collection"`
		Version    string `path:"version"`
	}
	huma.Register(api, huma.Operation{
		OperationID: "get-schema", Method: http.MethodGet,
		Path: "/v1/collections/{collection}/schemas/{version}",
		Tags: []string{"schemas"}, Security: workspaceSec,
	}, func(ctx context.Context, in *getIn) (*schemaOut, error) {
		c, err := h.resolveCollectionCtx(ctx, in.Collection)
		if err != nil {
			return nil, toHuma(err)
		}
		ver, err := strconv.Atoi(in.Version)
		if err != nil {
			return nil, toHuma(apierr.New(apierr.BadRequest, "invalid version"))
		}
		out, err := schemas.Get(ctx, c.ID, ver)
		if err != nil {
			return nil, toHuma(err)
		}
		return &schemaOut{Body: out}, nil
	})

	// --- activate schema version ---
	type activateIn struct {
		actorHeader
		Collection string `path:"collection"`
		Version    string `path:"version"`
		Body       struct {
			Force     bool   `json:"force"`
			Rationale string `json:"rationale"`
		}
	}
	huma.Register(api, huma.Operation{
		OperationID: "activate-schema", Method: http.MethodPost,
		Path: "/v1/collections/{collection}/schemas/{version}/activate",
		Tags: []string{"schemas"}, Security: workspaceSec, DefaultStatus: http.StatusNoContent,
	}, func(ctx context.Context, in *activateIn) (*struct{}, error) {
		c, err := h.resolveCollectionCtx(ctx, in.Collection)
		if err != nil {
			return nil, toHuma(err)
		}
		ver, err := strconv.Atoi(in.Version)
		if err != nil {
			return nil, toHuma(apierr.New(apierr.BadRequest, "invalid version"))
		}
		if err := schemas.Activate(ctx, c.WorkspaceID, c.ID, ver, in.actor(), in.Body.Force, in.Body.Rationale); err != nil {
			return nil, toHuma(err)
		}
		return nil, nil
	})

	// --- deprecate schema version ---
	type deprecateIn struct {
		actorHeader
		Collection string `path:"collection"`
		Version    string `path:"version"`
	}
	huma.Register(api, huma.Operation{
		OperationID: "deprecate-schema", Method: http.MethodPost,
		Path: "/v1/collections/{collection}/schemas/{version}/deprecate",
		Tags: []string{"schemas"}, Security: workspaceSec, DefaultStatus: http.StatusNoContent,
	}, func(ctx context.Context, in *deprecateIn) (*struct{}, error) {
		c, err := h.resolveCollectionCtx(ctx, in.Collection)
		if err != nil {
			return nil, toHuma(err)
		}
		ver, err := strconv.Atoi(in.Version)
		if err != nil {
			return nil, toHuma(apierr.New(apierr.BadRequest, "invalid version"))
		}
		if err := schemas.Deprecate(ctx, c.WorkspaceID, c.ID, ver, in.actor()); err != nil {
			return nil, toHuma(err)
		}
		return nil, nil
	})
	_ = auth.WorkspaceKey // (auth imported for context-key parity; remove if unused)
}
```

Remove the trailing `_ = auth.WorkspaceKey` line and the `auth` import if the compiler reports them unused (they are present only to mirror the records file; schemas reads workspace via `resolveCollectionCtx`).

- [ ] **Step 2: Register in `buildAPI`**

In `internal/api/openapi.go`, after `registerRecords(api, h)` add:

```go
	registerSchemas(api, h, d.Schemas)
```

- [ ] **Step 3: Remove schema routes from the legacy mount and delete legacy file**

In `internal/api/router.go`, delete the `sh := &schemaHandlers{...}` line and its five `api.HandleFunc(... schemas ...)` lines. Then delete the file `internal/api/schema_handlers.go`.

- [ ] **Step 4: Build, vet, format**

Run: `mise run build && mise run vet && gofmt -l internal/ cmd/`
Expected: pass; empty.

- [ ] **Step 5: Run schema integration tests**

Run: `go test -tags=integration -run TestSchema ./internal/api/ 2>&1 | tail -20`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/api/op_schemas.go internal/api/openapi.go internal/api/router.go
git rm internal/api/schema_handlers.go
git commit -m "feat(api): migrate schema registry to huma operations"
```

---

## Task 6: Migrate policies

**Files:**
- Create: `internal/api/op_policies.go`
- Modify: `internal/api/openapi.go`, `internal/api/router.go`, `internal/api/policy_handlers.go` (keep admin `setPolicyMode`, delete the three policy methods), `internal/api/policy_api_test.go` (update error-shape assertion)

- [ ] **Step 1: Write the policy operations**

Create `internal/api/op_policies.go`:

```go
package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"

	"github.com/substrate/substrate/internal/apierr"
	"github.com/substrate/substrate/internal/auth"
	"github.com/substrate/substrate/internal/policy"
)

type policyListBody struct {
	DefaultMode string              `json:"default_mode"`
	Rules       []policy.PolicyRule `json:"rules"`
}

func registerPolicies(api huma.API, h *handlers, policies *policy.Service) {
	// --- create policy rule ---
	type createIn struct {
		actorHeader
		Body struct {
			Actor      string `json:"actor"`
			Collection string `json:"collection"`
			Operation  string `json:"operation"`
			Effect     string `json:"effect"`
		}
	}
	type ruleOut struct{ Body policy.PolicyRule }
	huma.Register(api, huma.Operation{
		OperationID: "create-policy", Method: http.MethodPost, Path: "/v1/policies",
		Tags: []string{"policies"}, Security: workspaceSec, DefaultStatus: http.StatusCreated,
	}, func(ctx context.Context, in *createIn) (*ruleOut, error) {
		ws := auth.WorkspaceFrom(ctx)
		cmd := policy.CreateRuleCmd{Workspace: ws, Actor: in.Body.Actor, Operation: in.Body.Operation, Effect: in.Body.Effect}
		if in.Body.Collection != "" && in.Body.Collection != "*" {
			c, err := h.resolveCollectionCtx(ctx, in.Body.Collection)
			if err != nil {
				return nil, toHuma(err)
			}
			cmd.CollectionID = &c.ID
			cmd.CollectionName = c.Name
		}
		rule, err := policies.CreateRule(ctx, cmd)
		if err != nil {
			return nil, toHuma(err)
		}
		return &ruleOut{Body: rule}, nil
	})

	// --- list policy rules ---
	type listIn struct{ actorHeader }
	type listOut struct{ Body policyListBody }
	huma.Register(api, huma.Operation{
		OperationID: "list-policies", Method: http.MethodGet, Path: "/v1/policies",
		Tags: []string{"policies"}, Security: workspaceSec,
	}, func(ctx context.Context, _ *listIn) (*listOut, error) {
		ws := auth.WorkspaceFrom(ctx)
		rules, err := policies.ListRules(ctx, ws)
		if err != nil {
			return nil, toHuma(err)
		}
		mode, err := policies.DefaultMode(ctx, ws)
		if err != nil {
			return nil, toHuma(err)
		}
		if rules == nil {
			rules = []policy.PolicyRule{}
		}
		return &listOut{Body: policyListBody{DefaultMode: mode, Rules: rules}}, nil
	})

	// --- delete policy rule ---
	type deleteIn struct {
		actorHeader
		ID string `path:"id"`
	}
	huma.Register(api, huma.Operation{
		OperationID: "delete-policy", Method: http.MethodDelete, Path: "/v1/policies/{id}",
		Tags: []string{"policies"}, Security: workspaceSec, DefaultStatus: http.StatusNoContent,
	}, func(ctx context.Context, in *deleteIn) (*struct{}, error) {
		ws := auth.WorkspaceFrom(ctx)
		id, err := uuid.Parse(in.ID)
		if err != nil {
			return nil, toHuma(apierr.New(apierr.BadRequest, "invalid id"))
		}
		if err := policies.DeleteRule(ctx, ws, id); err != nil {
			return nil, toHuma(err)
		}
		return nil, nil
	})
}
```

- [ ] **Step 2: Register in `buildAPI`**

In `internal/api/openapi.go`, after `registerSchemas(...)` add:

```go
	registerPolicies(api, h, d.Policies)
```

- [ ] **Step 3: Remove policy routes from legacy mount, delete legacy methods**

In `internal/api/router.go` delete the `ph := &policyHandlers{...}` line and its three `api.HandleFunc(... policies ...)` lines.

In `internal/api/policy_handlers.go`, delete the `policyHandlers` struct and its `create`, `list`, `delete` methods. **Keep** the `setPolicyMode` method (it is on `adminHandlers`, migrated in Task 9). Remove imports that become unused.

- [ ] **Step 4: Update the policy error-shape assertion to RFC 7807**

The policy "denied" path now returns problem+json. In `internal/api/policy_api_test.go`, find the block decoding the error envelope (around lines 108-116):

```go
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	// ...
	if env.Error.Code != "policy_denied" {
		t.Fatalf("code = %q", env.Error.Code)
	}
```

Replace it with the RFC 7807 shape (code is carried in `title`):

```go
	var prob struct {
		Title string `json:"title"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&prob)
	if prob.Title != "policy_denied" {
		t.Fatalf("title = %q, want policy_denied", prob.Title)
	}
```

(Keep the surrounding decode/`resp.Body.Close()` calls; only the struct and comparison change. Ensure `encoding/json` is imported.)

- [ ] **Step 5: Build, vet, format**

Run: `mise run build && mise run vet && gofmt -l internal/ cmd/`
Expected: pass; empty.

- [ ] **Step 6: Run policy integration tests**

Run: `go test -tags=integration -run TestPolic ./internal/api/ 2>&1 | tail -20`
Expected: PASS, including the updated `title == "policy_denied"` assertion.

- [ ] **Step 7: Commit**

```bash
git add internal/api/op_policies.go internal/api/openapi.go internal/api/router.go internal/api/policy_handlers.go internal/api/policy_api_test.go
git commit -m "feat(api): migrate policies to huma operations; RFC7807 error assertions"
```

---

## Task 7: Migrate audit

**Files:**
- Create: `internal/api/op_audit.go`
- Modify: `internal/api/openapi.go`, `internal/api/router.go`, delete `internal/api/audit_handlers.go`

- [ ] **Step 1: Write the audit operation**

Create `internal/api/op_audit.go`:

```go
package api

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"

	"github.com/substrate/substrate/internal/apierr"
	"github.com/substrate/substrate/internal/audit"
	"github.com/substrate/substrate/internal/auth"
)

type auditListBody struct {
	Items      []audit.Entry `json:"items"`
	NextCursor string        `json:"next_cursor"`
}

func registerAudit(api huma.API, h *handlers, svc *audit.Service) {
	type listIn struct {
		actorHeader
		Collection string `query:"collection"`
		Record     string `query:"record"`
		Actor_     string `query:"actor"`
		Type       string `query:"type"`
		Since      string `query:"since"`
		Until      string `query:"until"`
		Limit      string `query:"limit"`
		Cursor     string `query:"cursor"`
	}
	type listOut struct{ Body auditListBody }
	huma.Register(api, huma.Operation{
		OperationID: "list-audit", Method: http.MethodGet, Path: "/v1/audit",
		Tags: []string{"audit"}, Security: workspaceSec,
	}, func(ctx context.Context, in *listIn) (*listOut, error) {
		ws := auth.WorkspaceFrom(ctx)
		var f audit.Filter
		if in.Collection != "" {
			c, err := h.resolveCollectionCtx(ctx, in.Collection)
			if err != nil {
				return nil, toHuma(err)
			}
			f.Collection = &c.ID
		}
		if in.Record != "" {
			id, err := uuid.Parse(in.Record)
			if err != nil {
				return nil, toHuma(apierr.New(apierr.BadRequest, "invalid record id"))
			}
			f.Record = &id
		}
		f.Actor = in.Actor_
		f.Type = in.Type
		if in.Since != "" {
			ts, err := time.Parse(time.RFC3339, in.Since)
			if err != nil {
				return nil, toHuma(apierr.New(apierr.BadRequest, "since must be RFC3339"))
			}
			f.Since = &ts
		}
		if in.Until != "" {
			ts, err := time.Parse(time.RFC3339, in.Until)
			if err != nil {
				return nil, toHuma(apierr.New(apierr.BadRequest, "until must be RFC3339"))
			}
			f.Until = &ts
		}
		if in.Limit != "" {
			n, err := strconv.Atoi(in.Limit)
			if err != nil {
				return nil, toHuma(apierr.New(apierr.BadRequest, "limit must be an integer"))
			}
			f.Limit = n
		}
		f.Cursor = in.Cursor

		items, next, err := svc.List(ctx, ws, f)
		if err != nil {
			return nil, toHuma(err)
		}
		if items == nil {
			items = []audit.Entry{}
		}
		return &listOut{Body: auditListBody{Items: items, NextCursor: next}}, nil
	})
}
```

Note: the `actor` query param field is named `Actor_` to avoid colliding with the embedded `actorHeader.Actor`; the `query:"actor"` tag preserves the wire name. (`actorHeader.Actor` remains the `X-Substrate-Actor` header.)

- [ ] **Step 2: Register in `buildAPI`**

After `registerPolicies(...)` add:

```go
	registerAudit(api, h, d.Audit)
```

- [ ] **Step 3: Remove from legacy mount, delete legacy file**

In `internal/api/router.go` delete the `aud := &auditHandlers{...}` line and the `api.HandleFunc("GET /v1/audit", aud.list)` line. Delete `internal/api/audit_handlers.go`.

- [ ] **Step 4: Build, vet, format**

Run: `mise run build && mise run vet && gofmt -l internal/ cmd/`
Expected: pass; empty.

- [ ] **Step 5: Run audit integration tests**

Run: `go test -tags=integration -run TestAudit ./internal/api/ 2>&1 | tail -20`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/api/op_audit.go internal/api/openapi.go internal/api/router.go
git rm internal/api/audit_handlers.go
git commit -m "feat(api): migrate audit stream to huma operation"
```

---

## Task 8: Migrate projection (backfill / auto-backfill)

**Files:**
- Create: `internal/api/op_projection.go`
- Modify: `internal/api/openapi.go`, `internal/api/router.go`, `internal/api/projection_handlers.go` (keep admin `replay`, delete projection methods), `internal/api/projection_api_test.go` (update error-shape assertion)

- [ ] **Step 1: Write the projection operations**

Create `internal/api/op_projection.go`:

```go
package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/substrate/substrate/internal/auth"
	"github.com/substrate/substrate/internal/collection"
	"github.com/substrate/substrate/internal/policy"
	"github.com/substrate/substrate/internal/projection"
)

type autoBackfillBody struct {
	Collection   string `json:"collection"`
	AutoBackfill bool   `json:"auto_backfill"`
}

func registerProjection(api huma.API, h *handlers, backfiller *projection.Backfiller, eval policy.Evaluator) {
	authorize := func(ctx context.Context, in actorHeader, c collection.Collection, op string) error {
		if eval == nil {
			return nil
		}
		_, err := eval.Authorize(ctx, policy.Request{
			Workspace: c.WorkspaceID, Actor: in.actor(),
			Collection: c.ID, Target: c.ID, Operation: op,
		})
		return err
	}

	// --- run backfill ---
	type backfillIn struct {
		actorHeader
		Collection string `path:"collection"`
	}
	type reportOut struct{ Body projection.Report }
	huma.Register(api, huma.Operation{
		OperationID: "run-backfill", Method: http.MethodPost,
		Path: "/v1/collections/{collection}/backfill",
		Tags: []string{"projection"}, Security: workspaceSec,
	}, func(ctx context.Context, in *backfillIn) (*reportOut, error) {
		c, err := h.resolveCollectionCtx(ctx, in.Collection)
		if err != nil {
			return nil, toHuma(err)
		}
		if err := authorize(ctx, in.actorHeader, c, policy.OpBackfill); err != nil {
			return nil, toHuma(err)
		}
		rep, err := backfiller.Run(ctx, c.WorkspaceID, c.ID, 0)
		if err != nil {
			return nil, toHuma(err)
		}
		return &reportOut{Body: rep}, nil
	})

	// --- set auto-backfill ---
	type autoIn struct {
		actorHeader
		Collection string `path:"collection"`
		Body       struct {
			Enabled bool `json:"enabled"`
		}
	}
	type autoOut struct{ Body autoBackfillBody }
	huma.Register(api, huma.Operation{
		OperationID: "set-auto-backfill", Method: http.MethodPost,
		Path: "/v1/collections/{collection}/auto-backfill",
		Tags: []string{"projection"}, Security: workspaceSec,
	}, func(ctx context.Context, in *autoIn) (*autoOut, error) {
		c, err := h.resolveCollectionCtx(ctx, in.Collection)
		if err != nil {
			return nil, toHuma(err)
		}
		if err := authorize(ctx, in.actorHeader, c, policy.OpBackfill); err != nil {
			return nil, toHuma(err)
		}
		if err := h.collections.SetAutoBackfill(ctx, c.WorkspaceID, c.ID, in.Body.Enabled); err != nil {
			return nil, toHuma(err)
		}
		return &autoOut{Body: autoBackfillBody{Collection: c.Name, AutoBackfill: in.Body.Enabled}}, nil
	})
	_ = auth.WorkspaceKey // remove if unused
}
```

Remove the trailing `_ = auth.WorkspaceKey` and the `auth` import if the compiler flags them unused.

- [ ] **Step 2: Register in `buildAPI`**

After `registerAudit(...)` add:

```go
	registerProjection(api, h, d.Backfiller, d.Evaluator)
```

- [ ] **Step 3: Remove from legacy mount, delete projection methods**

In `internal/api/router.go` delete the `pjh := &projectionHandlers{...}` line and its two `api.HandleFunc(... backfill ...)` lines.

In `internal/api/projection_handlers.go`, delete the `projectionHandlers` struct and its `authorize`, `backfill`, `setAutoBackfill` methods. **Keep** the `replay` method (on `adminHandlers`, migrated in Task 9). Remove now-unused imports.

- [ ] **Step 4: Update the projection error-shape assertion to RFC 7807**

In `internal/api/projection_api_test.go` (around lines 167-175), replace the error-envelope decode/assert:

```go
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	// ...
	if env.Error.Code != "policy_denied" {
		t.Fatalf("code = %q, want policy_denied", env.Error.Code)
	}
```

with:

```go
	var prob struct {
		Title string `json:"title"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&prob)
	if prob.Title != "policy_denied" {
		t.Fatalf("title = %q, want policy_denied", prob.Title)
	}
```

(Ensure `encoding/json` is imported.)

- [ ] **Step 5: Build, vet, format**

Run: `mise run build && mise run vet && gofmt -l internal/ cmd/`
Expected: pass; empty.

- [ ] **Step 6: Run projection integration tests**

Run: `go test -tags=integration -run TestProj ./internal/api/ 2>&1 | tail -20`
Expected: PASS, including the RFC 7807 assertion.

- [ ] **Step 7: Commit**

```bash
git add internal/api/op_projection.go internal/api/openapi.go internal/api/router.go internal/api/projection_handlers.go internal/api/projection_api_test.go
git commit -m "feat(api): migrate backfill endpoints to huma operations"
```

---

## Task 9: Migrate admin endpoints

**Files:**
- Create: `internal/api/op_admin.go`
- Modify: `internal/api/openapi.go`, `internal/api/router.go`, delete remaining legacy admin handlers in `internal/api/handlers.go`, `internal/api/policy_handlers.go`, `internal/api/projection_handlers.go`

- [ ] **Step 1: Write the admin operations**

Create `internal/api/op_admin.go`:

```go
package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"

	"github.com/substrate/substrate/internal/apierr"
	"github.com/substrate/substrate/internal/projection"
	"github.com/substrate/substrate/internal/workspace"
)

var adminSec = []map[string][]string{{schemeAdmin: {}}}

type createKeyBody struct {
	ID  uuid.UUID `json:"id"`
	Key string    `json:"key"`
}

type setPolicyModeBody struct {
	WorkspaceID uuid.UUID `json:"workspace_id"`
	PolicyMode  string    `json:"policy_mode"`
}

type replayBody struct {
	Rebuilt int `json:"rebuilt"`
}

func registerAdmin(api huma.API, workspaces *workspace.Service, replayer *projection.Replayer) {
	// --- create workspace ---
	type createWsIn struct {
		Body struct {
			Name string `json:"name"`
		}
	}
	type wsOut struct{ Body workspace.Workspace }
	huma.Register(api, huma.Operation{
		OperationID: "create-workspace", Method: http.MethodPost, Path: "/admin/workspaces",
		Tags: []string{"admin"}, Security: adminSec, DefaultStatus: http.StatusCreated,
	}, func(ctx context.Context, in *createWsIn) (*wsOut, error) {
		ws, err := workspaces.CreateWorkspace(ctx, in.Body.Name)
		if err != nil {
			return nil, toHuma(err)
		}
		return &wsOut{Body: ws}, nil
	})

	// --- create API key ---
	type createKeyIn struct {
		WS   string `path:"ws"`
		Body struct {
			Label string `json:"label"`
		}
	}
	type keyOut struct{ Body createKeyBody }
	huma.Register(api, huma.Operation{
		OperationID: "create-api-key", Method: http.MethodPost, Path: "/admin/workspaces/{ws}/api-keys",
		Tags: []string{"admin"}, Security: adminSec, DefaultStatus: http.StatusCreated,
	}, func(ctx context.Context, in *createKeyIn) (*keyOut, error) {
		wsID, err := uuid.Parse(in.WS)
		if err != nil {
			return nil, toHuma(apierr.New(apierr.BadRequest, "invalid workspace id"))
		}
		plaintext, id, err := workspaces.CreateAPIKey(ctx, wsID, in.Body.Label)
		if err != nil {
			return nil, toHuma(err)
		}
		return &keyOut{Body: createKeyBody{ID: id, Key: plaintext}}, nil
	})

	// --- set policy mode ---
	type setModeIn struct {
		WS   string `path:"ws"`
		Body struct {
			Mode string `json:"mode"`
		}
	}
	type modeOut struct{ Body setPolicyModeBody }
	huma.Register(api, huma.Operation{
		OperationID: "set-policy-mode", Method: http.MethodPut, Path: "/admin/workspaces/{ws}/policy-mode",
		Tags: []string{"admin"}, Security: adminSec,
	}, func(ctx context.Context, in *setModeIn) (*modeOut, error) {
		wsID, err := uuid.Parse(in.WS)
		if err != nil {
			return nil, toHuma(apierr.New(apierr.BadRequest, "invalid workspace id"))
		}
		if err := workspaces.SetPolicyMode(ctx, wsID, in.Body.Mode); err != nil {
			return nil, toHuma(err)
		}
		return &modeOut{Body: setPolicyModeBody{WorkspaceID: wsID, PolicyMode: in.Body.Mode}}, nil
	})

	// --- replay (rebuild projection) ---
	type replayIn struct {
		Body struct {
			WorkspaceID  string `json:"workspace_id"`
			CollectionID string `json:"collection_id"`
			RecordID     string `json:"record_id"`
		}
	}
	type replayOut struct{ Body replayBody }
	huma.Register(api, huma.Operation{
		OperationID: "replay", Method: http.MethodPost, Path: "/admin/replay",
		Tags: []string{"admin"}, Security: adminSec,
	}, func(ctx context.Context, in *replayIn) (*replayOut, error) {
		ws, err := uuid.Parse(in.Body.WorkspaceID)
		if err != nil {
			return nil, toHuma(apierr.New(apierr.BadRequest, "invalid workspace_id"))
		}
		col, err := uuid.Parse(in.Body.CollectionID)
		if err != nil {
			return nil, toHuma(apierr.New(apierr.BadRequest, "invalid collection_id"))
		}
		if in.Body.RecordID != "" {
			id, err := uuid.Parse(in.Body.RecordID)
			if err != nil {
				return nil, toHuma(apierr.New(apierr.BadRequest, "invalid record_id"))
			}
			ok, err := replayer.RebuildRecord(ctx, ws, col, id)
			if err != nil {
				return nil, toHuma(err)
			}
			n := 0
			if ok {
				n = 1
			}
			return &replayOut{Body: replayBody{Rebuilt: n}}, nil
		}
		n, err := replayer.RebuildCollection(ctx, ws, col)
		if err != nil {
			return nil, toHuma(err)
		}
		return &replayOut{Body: replayBody{Rebuilt: n}}, nil
	})
}
```

- [ ] **Step 2: Register in `buildAPI`**

After `registerProjection(...)` add:

```go
	registerAdmin(api, d.Workspaces, d.Replayer)
```

- [ ] **Step 3: Remove legacy admin routes and handlers**

In `internal/api/router.go` delete the `admin := &adminHandlers{...}` line and the four `mux.HandleFunc("...admin...")` lines. The legacy `/v1/` mount block should now be empty of `api.HandleFunc` calls — proceed to Task 10 to remove it entirely. (If it is fully empty already, you may remove it in this task instead; otherwise leave it for Task 10.)

In `internal/api/handlers.go` delete the `adminHandlers` struct, `authed`, `createWorkspace`, `createKey` methods. In `internal/api/policy_handlers.go` delete the `setPolicyMode` method. In `internal/api/projection_handlers.go` delete the `replay` method. Remove now-unused imports across these files.

- [ ] **Step 4: Build, vet, format**

Run: `mise run build && mise run vet && gofmt -l internal/ cmd/`
Expected: pass; empty.

- [ ] **Step 5: Run admin integration tests**

Run: `go test -tags=integration -run TestAdmin ./internal/api/ 2>&1 | tail -20`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/api/op_admin.go internal/api/openapi.go internal/api/router.go internal/api/handlers.go internal/api/policy_handlers.go internal/api/projection_handlers.go
git commit -m "feat(api): migrate admin endpoints to huma operations"
```

---

## Task 10: Remove the legacy router scaffolding

**Files:**
- Modify: `internal/api/router.go`, `internal/api/handlers.go`

- [ ] **Step 1: Delete the legacy mount from `NewRouter`**

In `internal/api/router.go`, `NewRouter` should reduce to:

```go
func NewRouter(d Deps) http.Handler {
	mux := http.NewServeMux()
	_ = buildAPI(mux, d)
	return mux
}
```

Remove the now-unused `h := &handlers{...}` line, the empty legacy `api` mux, the `mux.Handle("/v1/", auth.Middleware(...))` line, and the `auth`/other imports that are no longer used in this file.

- [ ] **Step 2: Remove dead legacy helpers**

In `internal/api/handlers.go`, the only remaining symbols should be helpers still used by the Huma handlers: `parseIfMatch`, `parseAsOf`, `parseTime`. Delete `writeErr`, `setETag` (replaced by `toHuma` and `etag`), and the now-dead request-based `resolveCollection` (the Huma path uses `resolveCollectionCtx`). Confirm nothing references them:

Run: `grep -rn "writeErr\|setETag\|httpx.Envelope\|auth.Middleware" internal/api/ | grep -v _test`
Expected: no matches (all replaced).

If `internal/httpx` is now unused project-wide except by `auth`, leave it — `auth.Middleware` may still exist as exported API even if unused by the router. (Do not delete the `auth.Middleware` function; it is a small, harmless export.)

- [ ] **Step 3: Build, vet, format**

Run: `mise run build && mise run vet && gofmt -l internal/ cmd/`
Expected: pass; empty.

- [ ] **Step 4: Run the entire integration suite**

Run: `go test -tags=integration ./... 2>&1 | tail -30`
Expected: PASS across the whole project.

- [ ] **Step 5: Run unit tests**

Run: `mise run test 2>&1 | tail -20`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/api/router.go internal/api/handlers.go
git commit -m "refactor(api): remove legacy net/http router; huma is the only transport"
```

---

## Task 11: Spec artifact, dump command, and drift guard

**Files:**
- Create: `cmd/openapi-dump/main.go`
- Create: `api/openapi.yaml` (generated)
- Create: `internal/api/spec_test.go`
- Modify: `mise.toml`

- [ ] **Step 1: Write the dump command**

Create `cmd/openapi-dump/main.go`:

```go
// Command openapi-dump writes the generated OpenAPI document to api/openapi.yaml.
package main

import (
	"log"
	"os"

	"github.com/substrate/substrate/internal/api"
)

func main() {
	out := "api/openapi.yaml"
	if len(os.Args) > 1 {
		out = os.Args[1]
	}
	spec, err := api.SpecYAML()
	if err != nil {
		log.Fatalf("generate spec: %v", err)
	}
	if err := os.WriteFile(out, spec, 0o644); err != nil {
		log.Fatalf("write %s: %v", out, err)
	}
	log.Printf("wrote %s (%d bytes)", out, len(spec))
}
```

- [ ] **Step 2: Add the mise task**

In `mise.toml`, add a task (match the existing task syntax in the file):

```toml
[tasks."openapi:dump"]
description = "Regenerate api/openapi.yaml from the Huma operations"
run = "go run ./cmd/openapi-dump api/openapi.yaml"
```

- [ ] **Step 3: Generate the spec**

Run: `mkdir -p api && mise run openapi:dump`
Expected: `wrote api/openapi.yaml (...)`. Inspect it: it must contain `openapi: 3.1` and paths for `/healthz`, `/v1/collections`, `/v1/audit`, `/admin/workspaces`, etc., plus `securitySchemes` `workspaceKey` and `adminToken`.

- [ ] **Step 4: Write the drift guard test**

Create `internal/api/spec_test.go` (no build tag — runs under `mise run test`, no DB):

```go
package api

import (
	"os"
	"testing"
)

// TestOpenAPISpecUpToDate fails if the committed api/openapi.yaml differs from the
// spec generated from the current Huma operations. Run `mise run openapi:dump`.
func TestOpenAPISpecUpToDate(t *testing.T) {
	got, err := SpecYAML()
	if err != nil {
		t.Fatalf("generate spec: %v", err)
	}
	want, err := os.ReadFile("../../api/openapi.yaml")
	if err != nil {
		t.Fatalf("read committed spec: %v (run `mise run openapi:dump`)", err)
	}
	if string(got) != string(want) {
		t.Fatalf("api/openapi.yaml is stale; run `mise run openapi:dump` and commit the result")
	}
}
```

- [ ] **Step 5: Run the drift test (must pass right after dumping)**

Run: `go test -run TestOpenAPISpecUpToDate ./internal/api/ -v 2>&1 | tail -10`
Expected: PASS.

- [ ] **Step 6: Sanity-check the test catches drift**

Temporarily change a summary in `internal/api/openapi.go` (e.g. `Summary: "Liveness probe"` → `"Liveness probe X"`), then run the test:

Run: `go test -run TestOpenAPISpecUpToDate ./internal/api/ 2>&1 | tail -5`
Expected: FAIL with the "stale" message. Revert the change; re-run; expected PASS.

- [ ] **Step 7: Commit**

```bash
git add cmd/openapi-dump/main.go api/openapi.yaml internal/api/spec_test.go mise.toml
git commit -m "feat(api): commit generated OpenAPI spec + dump task + drift guard"
```

---

## Task 12: Document client generation

**Files:**
- Modify: `README.md`

- [ ] **Step 1: Add a client-generation section to the README**

Add a section to `README.md` (place it near the API walkthrough):

```markdown
## OpenAPI & generated clients

The HTTP API is described by an OpenAPI 3.1 document generated from the Huma
operations. The committed copy lives at [`api/openapi.yaml`](api/openapi.yaml);
regenerate it with `mise run openapi:dump` (a unit test fails if it drifts). The
running server also serves it at `GET /openapi.yaml` (and interactive docs at
`/docs`).

Native clients are generated in separate repositories from that spec:

- **TypeScript** ([@hey-api/openapi-ts](https://heyapi.dev)):
  `npx @hey-api/openapi-ts -i path/to/openapi.yaml -o src/client`
- **Python** ([openapi-python-client](https://github.com/openapi-generators/openapi-python-client)):
  `openapi-python-client generate --path path/to/openapi.yaml`

Authentication: send the workspace API key as `Authorization: Bearer <key>`
(or `X-Api-Key`); admin endpoints require `X-Admin-Token`.
```

- [ ] **Step 2: Final full verification**

Run: `mise run build && mise run vet && gofmt -l internal/ cmd/ && mise run test && go test -tags=integration ./... 2>&1 | tail -20`
Expected: build + vet pass, `gofmt -l` empty, unit + integration suites PASS.

- [ ] **Step 3: Commit**

```bash
git add README.md
git commit -m "docs: OpenAPI spec location and client generation commands"
```

---

## Self-review notes

- **Spec coverage:** Adapter/routing (Task 2), typed I/O for every endpoint group (Tasks 4-9), RFC 7807 errors (Task 2 + assertion updates in Tasks 6/8), security-aware auth middleware incl. all three regimes (Task 2), spec artifact + freshness guard (Task 11), headers/query params (Tasks 4/7), client-gen docs (Task 12). Every design section maps to a task.
- **Endpoint inventory:** all 24 endpoints from the spec are registered (health 1, collections 1, records 7, schemas 5, policies 3, audit 1, projection 2, admin 4).
- **Type consistency:** `toHuma`, `etag`, `actorHeader.actor()`, `resolveCollectionCtx`, `workspaceSec`/`adminSec`, `schemeWorkspace`/`schemeAdmin`, and `auth.WorkspaceKey` are defined once and used with identical signatures throughout.
- **Known version-sensitivity (call out during execution):** exact Huma field paths (`cfg.OpenAPI.Components`, `huma.WriteErr`, `huma.WithValue`, `huma.SecurityScheme` field names, `api.OpenAPI().YAML()`) target v2.38.0; if a symbol differs, consult `go doc github.com/danielgtaylor/huma/v2` and adjust — the structure of the plan is unaffected.
```

