# Substrate Foundation & Event-Backed Record Core — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Stand up a running Substrate HTTP server backed by Postgres where you can create a workspace + API key, create a flexible collection, perform record CRUD with optimistic concurrency and idempotency, and walk/replay each record's full history (history, point-in-time read, revert).

**Architecture:** Single Go 1.26 module, single binary, layered `internal/` packages. Every mutation runs through one pipeline that, inside a single Postgres transaction, appends an authoritative append-only **event** (carrying a full `state_after` snapshot) and upserts the current-state **record** projection. Time-travel reads come straight from the event log. Reads of current state are a simple indexed table lookup. Auth is workspace-scoped API keys; an explicit actor identity is threaded onto every event.

**Tech Stack:** Go 1.26 · PostgreSQL via `github.com/jackc/pgx/v5` (`pgxpool`) · `net/http` with the Go 1.22+ pattern-matching `ServeMux` (no external router) · `github.com/google/uuid` · `log/slog` · embedded SQL migrations via `embed.FS` · integration tests with `github.com/testcontainers/testcontainers-go` (+ its `modules/postgres`) using **one shared container per test run** · runtime embedded-DB mode via `github.com/fergusstrange/embedded-postgres`.

**Spec:** [docs/superpowers/specs/2026-05-30-substrate-v0-core-design.md](../specs/2026-05-30-substrate-v0-core-design.md)

---

## Conventions used throughout this plan

- **Module path:** `github.com/substrate/substrate`. If the engineer uses a different remote, replace this path consistently in every `import`.
- **IDs:** UUID v4 (`github.com/google/uuid`), stored as Postgres `uuid`.
- **Timestamps:** `timestamptz`, generated with `now()` in SQL except where a deterministic value is needed.
- **Revisions:** `bigint`, starts at `1` on create, `+1` per mutation. Surfaced to clients as the `ETag` header (the raw integer as a quoted string, e.g. `ETag: "3"`).
- **API keys:** plaintext form `sk_<32 url-safe base64 random bytes>`, shown to the caller exactly once; only a SHA-256 hash is stored.
- **Actor:** free-form string from the `X-Substrate-Actor` request header (defaults to `"anonymous"` when absent), recorded on every event.
- **Error envelope:** every non-2xx response body is `{"error":{"code":"...","message":"...","details":{...}}}`.

## File structure (created across this plan)

```
go.mod
go.sum
Taskfile.yml
cmd/substrate/main.go              # bootstrap: config, DB open, migrate, HTTP server
internal/config/config.go          # flags + env config
internal/store/store.go            # pgxpool open + WithTx transaction helper
internal/store/migrate.go          # embedded-migration runner
internal/store/migrations/0001_init.sql
internal/store/testdb.go           # test-only: shared container + per-test database
internal/apierr/apierr.go          # typed service errors + HTTP mapping + envelope
internal/httpx/respond.go          # JSON write helpers
internal/workspace/workspace.go    # workspace + API-key repository/service
internal/auth/auth.go              # API-key auth middleware + context helpers
internal/collection/collection.go  # flexible collection repository/service
internal/record/record.go          # record service: CRUD + idempotency + revision
internal/record/timetravel.go      # history / as-of / revert
internal/api/router.go             # ServeMux wiring
internal/api/handlers.go           # HTTP handlers (DTOs, decode, call service, encode)
```

Tests live next to the code as `*_test.go`.

---

## Task 0: Project scaffold and a passing health check

**Files:**
- Create: `go.mod`, `Taskfile.yml`, `cmd/substrate/main.go`, `internal/api/router.go`, `internal/httpx/respond.go`
- Test: `internal/api/router_test.go`

- [ ] **Step 1: Initialize the module and tidy tooling**

Run:
```bash
go mod init github.com/substrate/substrate
go get github.com/jackc/pgx/v5@latest
go get github.com/google/uuid@latest
```
Then edit `go.mod` so the Go directive reads exactly:
```
go 1.26
```

- [ ] **Step 2: Write the JSON response helpers (needed by every handler)**

Create `internal/httpx/respond.go`:
```go
package httpx

import (
	"encoding/json"
	"net/http"
)

// JSON writes v as a JSON body with the given status code.
func JSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}
```

- [ ] **Step 3: Write the failing router test**

Create `internal/api/router_test.go`:
```go
package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHealthz(t *testing.T) {
	srv := httptest.NewServer(NewRouter(Deps{}))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}
```

- [ ] **Step 4: Run it to confirm it fails to compile**

Run: `go test ./internal/api/...`
Expected: FAIL — `undefined: NewRouter` / `undefined: Deps`.

- [ ] **Step 5: Implement the minimal router**

Create `internal/api/router.go`:
```go
package api

import (
	"net/http"

	"github.com/substrate/substrate/internal/httpx"
)

// Deps holds the collaborators handlers need. Fields are added by later tasks.
type Deps struct{}

// NewRouter builds the HTTP handler for the whole API.
func NewRouter(d Deps) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		httpx.JSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	return mux
}
```

- [ ] **Step 6: Run the test to confirm it passes**

Run: `go test ./internal/api/...`
Expected: PASS.

- [ ] **Step 7: Add a minimal main and Taskfile**

Create `cmd/substrate/main.go`:
```go
package main

import (
	"log/slog"
	"net/http"
	"os"

	"github.com/substrate/substrate/internal/api"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	addr := ":8080"
	logger.Info("substrate starting", "addr", addr)
	if err := http.ListenAndServe(addr, api.NewRouter(api.Deps{})); err != nil {
		logger.Error("server exited", "err", err)
		os.Exit(1)
	}
}
```

Create `Taskfile.yml` (this project uses [Task](https://taskfile.dev), not Make):
```yaml
version: '3'

tasks:
  build:
    desc: Build all packages
    cmds:
      - go build ./...
  test:
    desc: Run unit tests
    cmds:
      - go test ./...
  test:integration:
    desc: Run integration tests (requires Docker)
    cmds:
      - go test -tags=integration ./...
  test:all:
    desc: Run unit and integration tests
    cmds:
      - task: test
      - task: test:integration
  vet:
    desc: Run go vet
    cmds:
      - go vet ./...
  run:
    desc: Run the server
    cmds:
      - go run ./cmd/substrate
```

- [ ] **Step 8: Verify build and commit**

Run: `task build && go test ./...`
Expected: builds; the health test passes.
```bash
git add go.mod go.sum Taskfile.yml cmd internal
git commit -m "feat: scaffold module, router, and health check"
```

---

## Task 1: Integration test harness — one Postgres container per run

**Files:**
- Create: `internal/store/testdb.go`
- Test: `internal/store/testdb_test.go`

This task builds the shared-container helper the rest of the plan's integration tests depend on. The container starts once per package test binary; each test gets its own freshly-created database for isolation (no per-test containers).

- [ ] **Step 1: Add test dependencies**

Run:
```bash
go get github.com/testcontainers/testcontainers-go@latest
go get github.com/testcontainers/testcontainers-go/modules/postgres@latest
```

- [ ] **Step 2: Write the harness**

Create `internal/store/testdb.go` (build-tagged so it never ships in the binary):
```go
//go:build integration

package store

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpg "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

var (
	sharedOnce sync.Once
	sharedDSN  string // admin DSN to the shared container's "postgres" database
	sharedErr  error
	dbCounter  atomicCounter
)

type atomicCounter struct {
	mu sync.Mutex
	n  int
}

func (c *atomicCounter) next() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n++
	return c.n
}

// startShared boots a single Postgres container for the whole test binary.
func startShared(ctx context.Context) (string, error) {
	sharedOnce.Do(func() {
		container, err := tcpg.Run(ctx, "postgres:16-alpine",
			tcpg.WithDatabase("postgres"),
			tcpg.WithUsername("postgres"),
			tcpg.WithPassword("postgres"),
			testcontainers.WithWaitStrategy(
				wait.ForLog("database system is ready to accept connections").
					WithOccurrence(2).WithStartupTimeout(60*time.Second)),
		)
		if err != nil {
			sharedErr = err
			return
		}
		sharedDSN, sharedErr = container.ConnectionString(ctx, "sslmode=disable")
	})
	return sharedDSN, sharedErr
}

// NewTestPool creates a brand-new database in the shared container, applies all
// migrations, and returns a pool connected to it. Use for per-test isolation.
func NewTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()

	adminDSN, err := startShared(ctx)
	if err != nil {
		t.Fatalf("start shared postgres: %v", err)
	}

	admin, err := pgxpool.New(ctx, adminDSN)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	defer admin.Close()

	dbName := fmt.Sprintf("test_%d_%d", time.Now().UnixNano(), dbCounter.next())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		t.Fatalf("create db %s: %v", dbName, err)
	}

	dsn := replaceDBName(adminDSN, dbName)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect %s: %v", dbName, err)
	}
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { pool.Close() })
	return pool
}

// replaceDBName swaps the database path segment of a postgres URL.
func replaceDBName(dsn, db string) string {
	// DSNs from testcontainers look like postgres://user:pass@host:port/postgres?sslmode=disable
	slash := -1
	q := len(dsn)
	for i := len(dsn) - 1; i >= 0; i-- {
		if dsn[i] == '?' {
			q = i
		}
		if dsn[i] == '/' {
			slash = i
			break
		}
	}
	return dsn[:slash+1] + db + dsn[q:]
}
```

> Note: `Migrate` is implemented in Task 2. This file references it; both land before any integration test runs.

- [ ] **Step 3: Write a smoke test for the harness**

Create `internal/store/testdb_test.go`:
```go
//go:build integration

package store

import (
	"context"
	"testing"
)

func TestNewTestPool_Connects(t *testing.T) {
	pool := NewTestPool(t)
	var one int
	if err := pool.QueryRow(context.Background(), "SELECT 1").Scan(&one); err != nil {
		t.Fatalf("query: %v", err)
	}
	if one != 1 {
		t.Fatalf("got %d, want 1", one)
	}
}
```

- [ ] **Step 4: Defer running until Task 2 exists**

The harness calls `Migrate`, written in Task 2. Do not run integration tests yet. Commit the harness:
```bash
git add internal/store/testdb.go internal/store/testdb_test.go go.mod go.sum
git commit -m "test: add shared-container postgres harness"
```

---

## Task 2: Migrations — schema and a runner

**Files:**
- Create: `internal/store/store.go`, `internal/store/migrate.go`, `internal/store/migrations/0001_init.sql`
- Test: `internal/store/migrate_test.go`

- [ ] **Step 1: Write the connection + transaction helper**

Create `internal/store/store.go`:
```go
package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Open creates a connection pool for the given DSN.
func Open(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("open pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return pool, nil
}

// WithTx runs fn inside a transaction, committing on success and rolling back on error.
func WithTx(ctx context.Context, pool *pgxpool.Pool, fn func(pgx.Tx) error) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback(ctx)
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}
```

- [ ] **Step 2: Write the initial schema migration**

Create `internal/store/migrations/0001_init.sql`:
```sql
CREATE TABLE workspaces (
    id          uuid PRIMARY KEY,
    name        text NOT NULL,
    policy_mode text NOT NULL DEFAULT 'allow',
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE api_keys (
    id           uuid PRIMARY KEY,
    workspace_id uuid NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    hash         bytea NOT NULL,
    label        text,
    created_at   timestamptz NOT NULL DEFAULT now(),
    revoked_at   timestamptz
);
CREATE UNIQUE INDEX api_keys_hash_idx ON api_keys(hash);

CREATE TABLE collections (
    id                    uuid PRIMARY KEY,
    workspace_id          uuid NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    name                  text NOT NULL,
    level                 text NOT NULL DEFAULT 'flexible',
    active_schema_version int,
    created_at            timestamptz NOT NULL DEFAULT now(),
    updated_at            timestamptz NOT NULL DEFAULT now(),
    UNIQUE (workspace_id, name)
);

CREATE TABLE records (
    id             uuid NOT NULL,
    collection_id  uuid NOT NULL REFERENCES collections(id) ON DELETE CASCADE,
    workspace_id   uuid NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    schema_version int,
    data           jsonb NOT NULL,
    revision       bigint NOT NULL,
    status         text NOT NULL DEFAULT 'active',
    actor          text,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (collection_id, id)
);

CREATE TABLE events (
    seq             bigserial PRIMARY KEY,
    id              uuid NOT NULL,
    workspace_id    uuid NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    collection_id   uuid NOT NULL REFERENCES collections(id) ON DELETE CASCADE,
    record_id       uuid NOT NULL,
    type            text NOT NULL,
    revision        bigint NOT NULL,
    state_after     jsonb,
    actor           text,
    trace           jsonb,
    idempotency_key text,
    created_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX events_record_idx ON events(collection_id, record_id, seq);
CREATE UNIQUE INDEX events_idempotency_idx
    ON events(workspace_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL;
```

- [ ] **Step 3: Write the failing migrate test**

Create `internal/store/migrate_test.go`:
```go
//go:build integration

package store

import (
	"context"
	"testing"
)

func TestMigrate_CreatesTables(t *testing.T) {
	pool := NewTestPool(t) // NewTestPool already runs Migrate
	ctx := context.Background()

	var n int
	err := pool.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.tables
		 WHERE table_schema='public'
		   AND table_name IN ('workspaces','api_keys','collections','records','events')`,
	).Scan(&n)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if n != 5 {
		t.Fatalf("found %d of 5 expected tables", n)
	}
}
```

- [ ] **Step 4: Run it to confirm it fails**

Run: `go test -tags=integration ./internal/store/...`
Expected: FAIL — `undefined: Migrate`.

- [ ] **Step 5: Implement the migration runner**

Create `internal/store/migrate.go`:
```go
package store

import (
	"context"
	"embed"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// Migrate applies every embedded migration that has not yet been recorded.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	if _, err := pool.Exec(ctx,
		`CREATE TABLE IF NOT EXISTS schema_migrations (
			name text PRIMARY KEY,
			applied_at timestamptz NOT NULL DEFAULT now()
		)`); err != nil {
		return fmt.Errorf("ensure schema_migrations: %w", err)
	}

	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		return fmt.Errorf("read migrations: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	for _, name := range names {
		var exists bool
		if err := pool.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE name=$1)`, name,
		).Scan(&exists); err != nil {
			return fmt.Errorf("check %s: %w", name, err)
		}
		if exists {
			continue
		}
		sqlBytes, err := migrationFS.ReadFile("migrations/" + name)
		if err != nil {
			return fmt.Errorf("read %s: %w", name, err)
		}
		if _, err := pool.Exec(ctx, string(sqlBytes)); err != nil {
			return fmt.Errorf("apply %s: %w", name, err)
		}
		if _, err := pool.Exec(ctx,
			`INSERT INTO schema_migrations(name) VALUES($1)`, name); err != nil {
			return fmt.Errorf("record %s: %w", name, err)
		}
	}
	return nil
}
```

- [ ] **Step 6: Run the test to confirm it passes**

Run: `go test -tags=integration ./internal/store/...`
Expected: PASS (both `TestNewTestPool_Connects` and `TestMigrate_CreatesTables`).

- [ ] **Step 7: Commit**

```bash
git add internal/store
git commit -m "feat: postgres connection, tx helper, and migrations"
```

---

## Task 3: Typed service errors and HTTP mapping

**Files:**
- Create: `internal/apierr/apierr.go`
- Test: `internal/apierr/apierr_test.go`

This is the single place service-layer failures become HTTP responses, so handlers stay transport-only.

- [ ] **Step 1: Write the failing test**

Create `internal/apierr/apierr_test.go`:
```go
package apierr

import "testing"

func TestStatusMapping(t *testing.T) {
	cases := map[*Error]int{
		New(NotFound, "x"):         404,
		New(Conflict, "x"):         409,
		New(Validation, "x"):       422,
		New(Unauthorized, "x"):     401,
		New(Forbidden, "x"):        403,
		New(BadRequest, "x"):       400,
		New(Internal, "x"):         500,
	}
	for e, want := range cases {
		if got := e.HTTPStatus(); got != want {
			t.Errorf("code %s: status %d, want %d", e.Code, got, want)
		}
	}
}

func TestAsError(t *testing.T) {
	var generic error = New(NotFound, "missing")
	e, ok := As(generic)
	if !ok || e.Code != NotFound {
		t.Fatalf("As failed: %+v ok=%v", e, ok)
	}
	if _, ok := As(errString("plain")); ok {
		t.Fatal("plain error should not map")
	}
}

type errString string

func (e errString) Error() string { return string(e) }
```

- [ ] **Step 2: Run it to confirm it fails**

Run: `go test ./internal/apierr/...`
Expected: FAIL — `undefined: New` etc.

- [ ] **Step 3: Implement**

Create `internal/apierr/apierr.go`:
```go
package apierr

import (
	"errors"
	"net/http"
)

// Code is a stable, machine-readable error code returned in the API envelope.
type Code string

const (
	NotFound     Code = "not_found"
	Conflict     Code = "revision_conflict"
	Validation   Code = "validation_failed"
	Unauthorized Code = "unauthorized"
	Forbidden    Code = "policy_denied"
	BadRequest   Code = "bad_request"
	Internal     Code = "internal"
)

// Error is a typed service error carrying an API code and optional details.
type Error struct {
	Code    Code
	Message string
	Details map[string]any
}

func New(code Code, msg string) *Error { return &Error{Code: code, Message: msg} }

func (e *Error) WithDetails(d map[string]any) *Error { e.Details = d; return e }

func (e *Error) Error() string { return string(e.Code) + ": " + e.Message }

// HTTPStatus maps the code to an HTTP status.
func (e *Error) HTTPStatus() int {
	switch e.Code {
	case NotFound:
		return http.StatusNotFound
	case Conflict:
		return http.StatusConflict
	case Validation:
		return http.StatusUnprocessableEntity
	case Unauthorized:
		return http.StatusUnauthorized
	case Forbidden:
		return http.StatusForbidden
	case BadRequest:
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
}

// As extracts an *Error from any error, if present.
func As(err error) (*Error, bool) {
	var e *Error
	if errors.As(err, &e) {
		return e, true
	}
	return nil, false
}
```

- [ ] **Step 4: Run the test to confirm it passes**

Run: `go test ./internal/apierr/...`
Expected: PASS.

- [ ] **Step 5: Add the envelope writer to httpx**

Append to `internal/httpx/respond.go`:
```go
// Envelope is the standard error body shape.
type Envelope struct {
	Error EnvelopeError `json:"error"`
}

// EnvelopeError is the inner error object.
type EnvelopeError struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}
```

- [ ] **Step 6: Commit**

```bash
git add internal/apierr internal/httpx
git commit -m "feat: typed service errors with HTTP mapping and error envelope"
```

---

## Task 4: Workspaces and API keys

**Files:**
- Create: `internal/workspace/workspace.go`
- Test: `internal/workspace/workspace_test.go`

- [ ] **Step 1: Write the failing integration test**

Create `internal/workspace/workspace_test.go`:
```go
//go:build integration

package workspace

import (
	"context"
	"testing"

	"github.com/substrate/substrate/internal/store"
)

func TestCreateAndVerifyKey(t *testing.T) {
	pool := store.NewTestPool(t)
	ctx := context.Background()
	svc := New(pool)

	ws, err := svc.CreateWorkspace(ctx, "acme")
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}

	plaintext, _, err := svc.CreateAPIKey(ctx, ws.ID, "default")
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	if len(plaintext) < 10 {
		t.Fatalf("key too short: %q", plaintext)
	}

	gotWS, err := svc.VerifyKey(ctx, plaintext)
	if err != nil {
		t.Fatalf("verify key: %v", err)
	}
	if gotWS != ws.ID {
		t.Fatalf("verify returned %s, want %s", gotWS, ws.ID)
	}

	if _, err := svc.VerifyKey(ctx, "sk_wrong"); err == nil {
		t.Fatal("expected error for bad key")
	}
}
```

- [ ] **Step 2: Run it to confirm it fails**

Run: `go test -tags=integration ./internal/workspace/...`
Expected: FAIL — `undefined: New`.

- [ ] **Step 3: Implement workspaces + keys**

Create `internal/workspace/workspace.go`:
```go
package workspace

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/substrate/substrate/internal/apierr"
)

// Workspace is a tenant boundary.
type Workspace struct {
	ID         uuid.UUID `json:"id"`
	Name       string    `json:"name"`
	PolicyMode string    `json:"policy_mode"`
}

// Service manages workspaces and their API keys.
type Service struct{ pool *pgxpool.Pool }

func New(pool *pgxpool.Pool) *Service { return &Service{pool: pool} }

func (s *Service) CreateWorkspace(ctx context.Context, name string) (Workspace, error) {
	ws := Workspace{ID: uuid.New(), Name: name, PolicyMode: "allow"}
	_, err := s.pool.Exec(ctx,
		`INSERT INTO workspaces(id, name, policy_mode) VALUES($1,$2,$3)`,
		ws.ID, ws.Name, ws.PolicyMode)
	if err != nil {
		return Workspace{}, fmt.Errorf("insert workspace: %w", err)
	}
	return ws, nil
}

// CreateAPIKey returns the plaintext key (shown once) and the stored key id.
func (s *Service) CreateAPIKey(ctx context.Context, ws uuid.UUID, label string) (string, uuid.UUID, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", uuid.Nil, fmt.Errorf("rand: %w", err)
	}
	plaintext := "sk_" + base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(plaintext))
	id := uuid.New()
	_, err := s.pool.Exec(ctx,
		`INSERT INTO api_keys(id, workspace_id, hash, label) VALUES($1,$2,$3,$4)`,
		id, ws, sum[:], label)
	if err != nil {
		return "", uuid.Nil, fmt.Errorf("insert key: %w", err)
	}
	return plaintext, id, nil
}

// VerifyKey resolves a plaintext key to its workspace, or returns an Unauthorized error.
func (s *Service) VerifyKey(ctx context.Context, plaintext string) (uuid.UUID, error) {
	sum := sha256.Sum256([]byte(plaintext))
	var ws uuid.UUID
	err := s.pool.QueryRow(ctx,
		`SELECT workspace_id FROM api_keys WHERE hash=$1 AND revoked_at IS NULL`,
		sum[:]).Scan(&ws)
	if err == pgx.ErrNoRows {
		return uuid.Nil, apierr.New(apierr.Unauthorized, "invalid api key")
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("verify: %w", err)
	}
	return ws, nil
}
```

- [ ] **Step 4: Run the test to confirm it passes**

Run: `go test -tags=integration ./internal/workspace/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/workspace
git commit -m "feat: workspace and api-key service"
```

---

## Task 5: Auth middleware and request context

**Files:**
- Create: `internal/auth/auth.go`
- Test: `internal/auth/auth_test.go`

- [ ] **Step 1: Write the failing test (unit, with a fake verifier)**

Create `internal/auth/auth_test.go`:
```go
package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
)

type fakeVerifier struct {
	ws  uuid.UUID
	err error
}

func (f fakeVerifier) VerifyKey(_ context.Context, _ string) (uuid.UUID, error) {
	return f.ws, f.err
}

func TestMiddleware_ValidKey(t *testing.T) {
	ws := uuid.New()
	mw := Middleware(fakeVerifier{ws: ws})
	var gotWS uuid.UUID
	var gotActor string
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		gotWS = WorkspaceFrom(r.Context())
		gotActor = ActorFrom(r.Context())
	})
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer sk_abc")
	req.Header.Set("X-Substrate-Actor", "agent-7")
	rec := httptest.NewRecorder()

	mw(next).ServeHTTP(rec, req)

	if gotWS != ws {
		t.Errorf("workspace = %s, want %s", gotWS, ws)
	}
	if gotActor != "agent-7" {
		t.Errorf("actor = %q, want agent-7", gotActor)
	}
}

func TestMiddleware_MissingKey(t *testing.T) {
	mw := Middleware(fakeVerifier{err: errors.New("nope")})
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("next should not run")
		_ = w
	})
	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	mw(next).ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}
```

- [ ] **Step 2: Run it to confirm it fails**

Run: `go test ./internal/auth/...`
Expected: FAIL — undefined symbols.

- [ ] **Step 3: Implement the middleware**

Create `internal/auth/auth.go`:
```go
package auth

import (
	"context"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/substrate/substrate/internal/apierr"
	"github.com/substrate/substrate/internal/httpx"
)

type ctxKey int

const (
	wsKey ctxKey = iota
	actorKey
)

// Verifier resolves a plaintext API key to a workspace id.
type Verifier interface {
	VerifyKey(ctx context.Context, plaintext string) (uuid.UUID, error)
}

// Middleware authenticates requests via API key and pins workspace + actor on the context.
func Middleware(v Verifier) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := bearer(r)
			if key == "" {
				writeUnauthorized(w, "missing api key")
				return
			}
			ws, err := v.VerifyKey(r.Context(), key)
			if err != nil {
				writeUnauthorized(w, "invalid api key")
				return
			}
			actor := r.Header.Get("X-Substrate-Actor")
			if actor == "" {
				actor = "anonymous"
			}
			ctx := context.WithValue(r.Context(), wsKey, ws)
			ctx = context.WithValue(ctx, actorKey, actor)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func bearer(r *http.Request) string {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer ")
	}
	return r.Header.Get("X-Api-Key")
}

func writeUnauthorized(w http.ResponseWriter, msg string) {
	e := apierr.New(apierr.Unauthorized, msg)
	httpx.JSON(w, e.HTTPStatus(), httpx.Envelope{
		Error: httpx.EnvelopeError{Code: string(e.Code), Message: e.Message},
	})
}

// WorkspaceFrom returns the workspace id pinned by Middleware (uuid.Nil if absent).
func WorkspaceFrom(ctx context.Context) uuid.UUID {
	ws, _ := ctx.Value(wsKey).(uuid.UUID)
	return ws
}

// ActorFrom returns the actor pinned by Middleware ("" if absent).
func ActorFrom(ctx context.Context) string {
	a, _ := ctx.Value(actorKey).(string)
	return a
}
```

- [ ] **Step 4: Run the test to confirm it passes**

Run: `go test ./internal/auth/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/auth
git commit -m "feat: api-key auth middleware with workspace and actor context"
```

---

## Task 6: Flexible collections

**Files:**
- Create: `internal/collection/collection.go`
- Test: `internal/collection/collection_test.go`

- [ ] **Step 1: Write the failing integration test**

Create `internal/collection/collection_test.go`:
```go
//go:build integration

package collection

import (
	"context"
	"testing"

	"github.com/substrate/substrate/internal/store"
	"github.com/substrate/substrate/internal/workspace"
)

func TestCreateAndGetCollection(t *testing.T) {
	pool := store.NewTestPool(t)
	ctx := context.Background()
	ws, err := workspace.New(pool).CreateWorkspace(ctx, "acme")
	if err != nil {
		t.Fatalf("workspace: %v", err)
	}

	svc := New(pool)
	c, err := svc.Create(ctx, ws.ID, "trips")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if c.Level != "flexible" {
		t.Fatalf("level = %q, want flexible", c.Level)
	}

	got, err := svc.GetByName(ctx, ws.ID, "trips")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ID != c.ID {
		t.Fatalf("get returned %s, want %s", got.ID, c.ID)
	}

	if _, err := svc.Create(ctx, ws.ID, "trips"); err == nil {
		t.Fatal("expected duplicate-name error")
	}
}
```

- [ ] **Step 2: Run it to confirm it fails**

Run: `go test -tags=integration ./internal/collection/...`
Expected: FAIL — `undefined: New`.

- [ ] **Step 3: Implement collections**

Create `internal/collection/collection.go`:
```go
package collection

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/substrate/substrate/internal/apierr"
)

// Collection is a flexible (v0) object type within a workspace.
type Collection struct {
	ID          uuid.UUID `json:"id"`
	WorkspaceID uuid.UUID `json:"workspace_id"`
	Name        string    `json:"name"`
	Level       string    `json:"level"`
}

// Service manages collections.
type Service struct{ pool *pgxpool.Pool }

func New(pool *pgxpool.Pool) *Service { return &Service{pool: pool} }

func (s *Service) Create(ctx context.Context, ws uuid.UUID, name string) (Collection, error) {
	if name == "" {
		return Collection{}, apierr.New(apierr.BadRequest, "collection name required")
	}
	c := Collection{ID: uuid.New(), WorkspaceID: ws, Name: name, Level: "flexible"}
	_, err := s.pool.Exec(ctx,
		`INSERT INTO collections(id, workspace_id, name, level) VALUES($1,$2,$3,$4)`,
		c.ID, c.WorkspaceID, c.Name, c.Level)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return Collection{}, apierr.New(apierr.Conflict, "collection name already exists")
		}
		return Collection{}, fmt.Errorf("insert collection: %w", err)
	}
	return c, nil
}

func (s *Service) GetByName(ctx context.Context, ws uuid.UUID, name string) (Collection, error) {
	var c Collection
	err := s.pool.QueryRow(ctx,
		`SELECT id, workspace_id, name, level FROM collections WHERE workspace_id=$1 AND name=$2`,
		ws, name).Scan(&c.ID, &c.WorkspaceID, &c.Name, &c.Level)
	if err == pgx.ErrNoRows {
		return Collection{}, apierr.New(apierr.NotFound, "collection not found")
	}
	if err != nil {
		return Collection{}, fmt.Errorf("get collection: %w", err)
	}
	return c, nil
}
```

- [ ] **Step 4: Run the test to confirm it passes**

Run: `go test -tags=integration ./internal/collection/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/collection
git commit -m "feat: flexible collection service"
```

---

## Task 7: Record core — create, read, update, delete (with events, revision, idempotency)

**Files:**
- Create: `internal/record/record.go`
- Test: `internal/record/record_test.go`

Every mutation appends an event and upserts the projection inside one transaction. This task is the heart of the system.

- [ ] **Step 1: Write the failing integration test for create + read**

Create `internal/record/record_test.go`:
```go
//go:build integration

package record

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/substrate/substrate/internal/apierr"
	"github.com/substrate/substrate/internal/collection"
	"github.com/substrate/substrate/internal/store"
	"github.com/substrate/substrate/internal/workspace"
)

// setup creates a workspace + flexible collection and returns ids and a record service.
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

func TestCreateAndGet(t *testing.T) {
	svc, ws, col := setup(t)
	ctx := context.Background()

	rec, err := svc.Create(ctx, CreateCmd{
		Workspace: ws, Collection: col, Actor: "agent-1",
		Data: map[string]any{"destination": "Tokyo"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if rec.Revision != 1 {
		t.Fatalf("revision = %d, want 1", rec.Revision)
	}

	got, err := svc.Get(ctx, ws, col, rec.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Data["destination"] != "Tokyo" {
		t.Fatalf("data = %v", got.Data)
	}
}

func TestUpdateRevisionConflict(t *testing.T) {
	svc, ws, col := setup(t)
	ctx := context.Background()
	rec, _ := svc.Create(ctx, CreateCmd{Workspace: ws, Collection: col, Data: map[string]any{"n": 1}})

	// Correct revision succeeds and bumps to 2.
	updated, err := svc.Update(ctx, UpdateCmd{
		Workspace: ws, Collection: col, ID: rec.ID,
		ExpectedRevision: 1, Data: map[string]any{"n": 2},
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.Revision != 2 {
		t.Fatalf("revision = %d, want 2", updated.Revision)
	}

	// Stale revision fails with Conflict.
	_, err = svc.Update(ctx, UpdateCmd{
		Workspace: ws, Collection: col, ID: rec.ID,
		ExpectedRevision: 1, Data: map[string]any{"n": 3},
	})
	e, ok := apierr.As(err)
	if !ok || e.Code != apierr.Conflict {
		t.Fatalf("err = %v, want revision_conflict", err)
	}
}

func TestIdempotentCreate(t *testing.T) {
	svc, ws, col := setup(t)
	ctx := context.Background()
	cmd := CreateCmd{
		Workspace: ws, Collection: col, IdempotencyKey: "k1",
		Data: map[string]any{"n": 1},
	}
	first, err := svc.Create(ctx, cmd)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, err := svc.Create(ctx, cmd)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if first.ID != second.ID || second.Revision != 1 {
		t.Fatalf("replay produced %+v, want same id and revision 1", second)
	}
}

func TestSoftDelete(t *testing.T) {
	svc, ws, col := setup(t)
	ctx := context.Background()
	rec, _ := svc.Create(ctx, CreateCmd{Workspace: ws, Collection: col, Data: map[string]any{"n": 1}})
	if err := svc.Delete(ctx, ws, col, rec.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	_, err := svc.Get(ctx, ws, col, rec.ID)
	e, ok := apierr.As(err)
	if !ok || e.Code != apierr.NotFound {
		t.Fatalf("get after delete: %v, want not_found", err)
	}
}
```

- [ ] **Step 2: Run it to confirm it fails**

Run: `go test -tags=integration ./internal/record/...`
Expected: FAIL — `undefined: Service`, `CreateCmd`, etc.

- [ ] **Step 3: Implement the record service**

Create `internal/record/record.go`:
```go
package record

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/substrate/substrate/internal/apierr"
	"github.com/substrate/substrate/internal/store"
)

// Record is the current materialized state of an object.
type Record struct {
	ID         uuid.UUID      `json:"id"`
	Collection uuid.UUID      `json:"collection_id"`
	Data       map[string]any `json:"data"`
	Revision   int64          `json:"revision"`
	Status     string         `json:"status"`
	Actor      string         `json:"actor"`
}

// CreateCmd is the input for creating a record.
type CreateCmd struct {
	Workspace      uuid.UUID
	Collection     uuid.UUID
	Actor          string
	Data           map[string]any
	IdempotencyKey string
}

// UpdateCmd is the input for replacing a record's data.
type UpdateCmd struct {
	Workspace        uuid.UUID
	Collection       uuid.UUID
	ID               uuid.UUID
	ExpectedRevision int64
	Actor            string
	Data             map[string]any
	IdempotencyKey   string
}

// Service performs record mutations and reads.
type Service struct{ pool *pgxpool.Pool }

func New(pool *pgxpool.Pool) *Service { return &Service{pool: pool} }

func (s *Service) Create(ctx context.Context, cmd CreateCmd) (Record, error) {
	rec := Record{
		ID: uuid.New(), Collection: cmd.Collection, Data: cmd.Data,
		Revision: 1, Status: "active", Actor: cmd.Actor,
	}
	if rec.Data == nil {
		rec.Data = map[string]any{}
	}
	err := store.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		if cmd.IdempotencyKey != "" {
			if replayed, ok, err := lookupReplay(ctx, tx, cmd.Workspace, cmd.IdempotencyKey); err != nil {
				return err
			} else if ok {
				rec = replayed
				return nil
			}
		}
		if err := appendEvent(ctx, tx, eventRow{
			Workspace: cmd.Workspace, Collection: cmd.Collection, RecordID: rec.ID,
			Type: "create", Revision: 1, State: rec.Data, Actor: cmd.Actor,
			IdempotencyKey: cmd.IdempotencyKey,
		}); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO records(id, collection_id, workspace_id, data, revision, status, actor)
			 VALUES($1,$2,$3,$4,$5,'active',$6)`,
			rec.ID, cmd.Collection, cmd.Workspace, mustJSON(rec.Data), 1, cmd.Actor)
		return err
	})
	if err != nil {
		return Record{}, err
	}
	return rec, nil
}

func (s *Service) Get(ctx context.Context, ws, col, id uuid.UUID) (Record, error) {
	var rec Record
	var raw []byte
	err := s.pool.QueryRow(ctx,
		`SELECT id, collection_id, data, revision, status, COALESCE(actor,'')
		 FROM records WHERE workspace_id=$1 AND collection_id=$2 AND id=$3 AND status='active'`,
		ws, col, id).Scan(&rec.ID, &rec.Collection, &raw, &rec.Revision, &rec.Status, &rec.Actor)
	if err == pgx.ErrNoRows {
		return Record{}, apierr.New(apierr.NotFound, "record not found")
	}
	if err != nil {
		return Record{}, fmt.Errorf("get record: %w", err)
	}
	if err := json.Unmarshal(raw, &rec.Data); err != nil {
		return Record{}, fmt.Errorf("decode data: %w", err)
	}
	return rec, nil
}

func (s *Service) Update(ctx context.Context, cmd UpdateCmd) (Record, error) {
	var rec Record
	err := store.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		if cmd.IdempotencyKey != "" {
			if replayed, ok, err := lookupReplay(ctx, tx, cmd.Workspace, cmd.IdempotencyKey); err != nil {
				return err
			} else if ok {
				rec = replayed
				return nil
			}
		}
		var current int64
		err := tx.QueryRow(ctx,
			`SELECT revision FROM records
			 WHERE workspace_id=$1 AND collection_id=$2 AND id=$3 AND status='active' FOR UPDATE`,
			cmd.Workspace, cmd.Collection, cmd.ID).Scan(&current)
		if err == pgx.ErrNoRows {
			return apierr.New(apierr.NotFound, "record not found")
		}
		if err != nil {
			return err
		}
		if current != cmd.ExpectedRevision {
			return apierr.New(apierr.Conflict, "revision mismatch").
				WithDetails(map[string]any{"expected": cmd.ExpectedRevision, "current": current})
		}
		next := current + 1
		if cmd.Data == nil {
			cmd.Data = map[string]any{}
		}
		if err := appendEvent(ctx, tx, eventRow{
			Workspace: cmd.Workspace, Collection: cmd.Collection, RecordID: cmd.ID,
			Type: "update", Revision: next, State: cmd.Data, Actor: cmd.Actor,
			IdempotencyKey: cmd.IdempotencyKey,
		}); err != nil {
			return err
		}
		_, err = tx.Exec(ctx,
			`UPDATE records SET data=$1, revision=$2, actor=$3, updated_at=now()
			 WHERE workspace_id=$4 AND collection_id=$5 AND id=$6`,
			mustJSON(cmd.Data), next, cmd.Actor, cmd.Workspace, cmd.Collection, cmd.ID)
		if err != nil {
			return err
		}
		rec = Record{ID: cmd.ID, Collection: cmd.Collection, Data: cmd.Data,
			Revision: next, Status: "active", Actor: cmd.Actor}
		return nil
	})
	if err != nil {
		return Record{}, err
	}
	return rec, nil
}

func (s *Service) Delete(ctx context.Context, ws, col, id uuid.UUID) error {
	return store.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		var current int64
		var raw []byte
		err := tx.QueryRow(ctx,
			`SELECT revision, data FROM records
			 WHERE workspace_id=$1 AND collection_id=$2 AND id=$3 AND status='active' FOR UPDATE`,
			ws, col, id).Scan(&current, &raw)
		if err == pgx.ErrNoRows {
			return apierr.New(apierr.NotFound, "record not found")
		}
		if err != nil {
			return err
		}
		next := current + 1
		var data map[string]any
		_ = json.Unmarshal(raw, &data)
		if err := appendEvent(ctx, tx, eventRow{
			Workspace: ws, Collection: col, RecordID: id,
			Type: "delete", Revision: next, State: data,
		}); err != nil {
			return err
		}
		_, err = tx.Exec(ctx,
			`UPDATE records SET status='deleted', revision=$1, updated_at=now()
			 WHERE workspace_id=$2 AND collection_id=$3 AND id=$4`,
			next, ws, col, id)
		return err
	})
}

// --- internal helpers ---

type eventRow struct {
	Workspace      uuid.UUID
	Collection     uuid.UUID
	RecordID       uuid.UUID
	Type           string
	Revision       int64
	State          map[string]any
	Actor          string
	IdempotencyKey string
}

func appendEvent(ctx context.Context, tx pgx.Tx, e eventRow) error {
	var key any
	if e.IdempotencyKey != "" {
		key = e.IdempotencyKey
	}
	_, err := tx.Exec(ctx,
		`INSERT INTO events(id, workspace_id, collection_id, record_id, type, revision, state_after, actor, idempotency_key)
		 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		uuid.New(), e.Workspace, e.Collection, e.RecordID, e.Type, e.Revision,
		mustJSON(e.State), nullStr(e.Actor), key)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			// idempotency collision is handled by the caller's replay lookup; treat as conflict otherwise
			return apierr.New(apierr.Conflict, "duplicate idempotency key")
		}
		return fmt.Errorf("append event: %w", err)
	}
	return nil
}

// lookupReplay returns the materialized record for a prior idempotency key, if any.
func lookupReplay(ctx context.Context, tx pgx.Tx, ws uuid.UUID, key string) (Record, bool, error) {
	var (
		recordID uuid.UUID
		col      uuid.UUID
		rev      int64
		raw      []byte
		actor    string
		typ      string
	)
	err := tx.QueryRow(ctx,
		`SELECT record_id, collection_id, revision, state_after, COALESCE(actor,''), type
		 FROM events WHERE workspace_id=$1 AND idempotency_key=$2 ORDER BY seq DESC LIMIT 1`,
		ws, key).Scan(&recordID, &col, &rev, &raw, &actor, &typ)
	if err == pgx.ErrNoRows {
		return Record{}, false, nil
	}
	if err != nil {
		return Record{}, false, fmt.Errorf("replay lookup: %w", err)
	}
	rec := Record{ID: recordID, Collection: col, Revision: rev, Status: "active", Actor: actor}
	if typ == "delete" {
		rec.Status = "deleted"
	}
	if err := json.Unmarshal(raw, &rec.Data); err != nil {
		return Record{}, false, fmt.Errorf("decode replay: %w", err)
	}
	return rec, true, nil
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("marshal: %v", err))
	}
	return b
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}
```

- [ ] **Step 4: Run the test to confirm it passes**

Run: `go test -tags=integration ./internal/record/...`
Expected: PASS (all four tests).

- [ ] **Step 5: Commit**

```bash
git add internal/record
git commit -m "feat: record core with events, optimistic concurrency, idempotency, soft delete"
```

---

## Task 8: Time-travel — history, point-in-time read, revert

**Files:**
- Create: `internal/record/timetravel.go`
- Test: `internal/record/timetravel_test.go`

- [ ] **Step 1: Write the failing integration test**

Create `internal/record/timetravel_test.go`:
```go
//go:build integration

package record

import (
	"context"
	"strconv"
	"testing"
)

func TestHistoryAndAsOfAndRevert(t *testing.T) {
	svc, ws, col := setup(t)
	ctx := context.Background()

	rec, _ := svc.Create(ctx, CreateCmd{Workspace: ws, Collection: col, Data: map[string]any{"n": float64(1)}})
	_, _ = svc.Update(ctx, UpdateCmd{Workspace: ws, Collection: col, ID: rec.ID, ExpectedRevision: 1, Data: map[string]any{"n": float64(2)}})
	_, _ = svc.Update(ctx, UpdateCmd{Workspace: ws, Collection: col, ID: rec.ID, ExpectedRevision: 2, Data: map[string]any{"n": float64(3)}})

	// History has 3 entries in order.
	hist, err := svc.History(ctx, ws, col, rec.ID)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(hist) != 3 {
		t.Fatalf("history len = %d, want 3", len(hist))
	}
	if hist[0].Revision != 1 || hist[2].Revision != 3 {
		t.Fatalf("history order wrong: %+v", hist)
	}

	// Point-in-time read at revision 1.
	old, err := svc.GetAsOf(ctx, ws, col, rec.ID, AsOf{Revision: 1})
	if err != nil {
		t.Fatalf("as-of: %v", err)
	}
	if old.Data["n"] != float64(1) {
		t.Fatalf("as-of n = %v, want 1", old.Data["n"])
	}

	// Revert back to revision 1 -> new revision 4 with old data.
	reverted, err := svc.Revert(ctx, ws, col, rec.ID, AsOf{Revision: 1})
	if err != nil {
		t.Fatalf("revert: %v", err)
	}
	if reverted.Revision != 4 || reverted.Data["n"] != float64(1) {
		t.Fatalf("reverted = %+v, want revision 4 n=1", reverted)
	}

	// Current read now reflects the revert.
	cur, _ := svc.Get(ctx, ws, col, rec.ID)
	if cur.Data["n"] != float64(1) {
		t.Fatalf("current n = %v, want 1", cur.Data["n"])
	}

	_ = strconv.Itoa // keep import if unused after edits
}
```

- [ ] **Step 2: Run it to confirm it fails**

Run: `go test -tags=integration ./internal/record/...`
Expected: FAIL — `undefined: History`, `GetAsOf`, `Revert`, `AsOf`.

- [ ] **Step 3: Implement time-travel**

Create `internal/record/timetravel.go`:
```go
package record

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/substrate/substrate/internal/apierr"
	"github.com/substrate/substrate/internal/store"
)

// HistoryEntry is one event in a record's timeline.
type HistoryEntry struct {
	Revision  int64          `json:"revision"`
	Type      string         `json:"type"`
	Actor     string         `json:"actor"`
	State     map[string]any `json:"state"`
	CreatedAt time.Time      `json:"created_at"`
}

// AsOf selects a point in a record's history. Exactly one field should be set;
// precedence is Revision, then EventID, then Timestamp.
type AsOf struct {
	Revision  int64
	EventID   uuid.UUID
	Timestamp time.Time
}

// History returns the ordered event stream for a record.
func (s *Service) History(ctx context.Context, ws, col, id uuid.UUID) ([]HistoryEntry, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT revision, type, COALESCE(actor,''), state_after, created_at
		 FROM events WHERE workspace_id=$1 AND collection_id=$2 AND record_id=$3
		 ORDER BY seq ASC`, ws, col, id)
	if err != nil {
		return nil, fmt.Errorf("history query: %w", err)
	}
	defer rows.Close()

	var out []HistoryEntry
	for rows.Next() {
		var h HistoryEntry
		var raw []byte
		if err := rows.Scan(&h.Revision, &h.Type, &h.Actor, &raw, &h.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan history: %w", err)
		}
		if len(raw) > 0 {
			_ = json.Unmarshal(raw, &h.State)
		}
		out = append(out, h)
	}
	if len(out) == 0 {
		return nil, apierr.New(apierr.NotFound, "record not found")
	}
	return out, rows.Err()
}

// GetAsOf resolves the record's state at the requested point in time.
func (s *Service) GetAsOf(ctx context.Context, ws, col, id uuid.UUID, at AsOf) (Record, error) {
	state, rev, status, err := s.resolveAsOf(ctx, s.pool, ws, col, id, at)
	if err != nil {
		return Record{}, err
	}
	return Record{ID: id, Collection: col, Data: state, Revision: rev, Status: status}, nil
}

// Revert appends a new event restoring the record to a prior point.
func (s *Service) Revert(ctx context.Context, ws, col, id uuid.UUID, to AsOf) (Record, error) {
	var rec Record
	err := store.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		state, _, _, err := s.resolveAsOfTx(ctx, tx, ws, col, id, to)
		if err != nil {
			return err
		}
		var current int64
		err = tx.QueryRow(ctx,
			`SELECT revision FROM records
			 WHERE workspace_id=$1 AND collection_id=$2 AND id=$3 FOR UPDATE`,
			ws, col, id).Scan(&current)
		if err == pgx.ErrNoRows {
			return apierr.New(apierr.NotFound, "record not found")
		}
		if err != nil {
			return err
		}
		next := current + 1
		if err := appendEvent(ctx, tx, eventRow{
			Workspace: ws, Collection: col, RecordID: id,
			Type: "revert", Revision: next, State: state,
		}); err != nil {
			return err
		}
		_, err = tx.Exec(ctx,
			`UPDATE records SET data=$1, revision=$2, status='active', updated_at=now()
			 WHERE workspace_id=$3 AND collection_id=$4 AND id=$5`,
			mustJSON(state), next, ws, col, id)
		if err != nil {
			return err
		}
		rec = Record{ID: id, Collection: col, Data: state, Revision: next, Status: "active"}
		return nil
	})
	if err != nil {
		return Record{}, err
	}
	return rec, nil
}

// queryer is satisfied by both *pgxpool.Pool and pgx.Tx.
type queryer interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func (s *Service) resolveAsOf(ctx context.Context, q queryer, ws, col, id uuid.UUID, at AsOf) (map[string]any, int64, string, error) {
	var raw []byte
	var rev int64
	var typ string
	var err error
	switch {
	case at.Revision > 0:
		err = q.QueryRow(ctx,
			`SELECT state_after, revision, type FROM events
			 WHERE workspace_id=$1 AND collection_id=$2 AND record_id=$3 AND revision<=$4
			 ORDER BY seq DESC LIMIT 1`, ws, col, id, at.Revision).Scan(&raw, &rev, &typ)
	case at.EventID != uuid.Nil:
		err = q.QueryRow(ctx,
			`SELECT state_after, revision, type FROM events
			 WHERE workspace_id=$1 AND collection_id=$2 AND record_id=$3
			   AND seq <= (SELECT seq FROM events WHERE id=$4)
			 ORDER BY seq DESC LIMIT 1`, ws, col, id, at.EventID).Scan(&raw, &rev, &typ)
	default:
		err = q.QueryRow(ctx,
			`SELECT state_after, revision, type FROM events
			 WHERE workspace_id=$1 AND collection_id=$2 AND record_id=$3 AND created_at<=$4
			 ORDER BY seq DESC LIMIT 1`, ws, col, id, at.Timestamp).Scan(&raw, &rev, &typ)
	}
	if err == pgx.ErrNoRows {
		return nil, 0, "", apierr.New(apierr.NotFound, "no state at requested point")
	}
	if err != nil {
		return nil, 0, "", fmt.Errorf("resolve as-of: %w", err)
	}
	state := map[string]any{}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &state)
	}
	status := "active"
	if typ == "delete" {
		status = "deleted"
	}
	return state, rev, status, nil
}

func (s *Service) resolveAsOfTx(ctx context.Context, tx pgx.Tx, ws, col, id uuid.UUID, at AsOf) (map[string]any, int64, string, error) {
	return s.resolveAsOf(ctx, tx, ws, col, id, at)
}
```

> Remove the unused `strconv` import in the test if `go vet` flags it; the line `_ = strconv.Itoa` keeps it referenced so the test compiles as written.

- [ ] **Step 4: Run the test to confirm it passes**

Run: `go test -tags=integration ./internal/record/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/record
git commit -m "feat: time-travel history, point-in-time read, and revert"
```

---

## Task 9: HTTP handlers and router wiring

**Files:**
- Create: `internal/api/handlers.go`
- Modify: `internal/api/router.go`
- Test: `internal/api/api_test.go`

This wires the services behind the HTTP API with auth, idempotency, and ETag handling, and is verified end-to-end against a real database.

- [ ] **Step 1: Write the failing end-to-end test**

Create `internal/api/api_test.go`:
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
	"github.com/substrate/substrate/internal/store"
	"github.com/substrate/substrate/internal/workspace"
)

func newTestServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	pool := store.NewTestPool(t)
	ws := workspace.New(pool)
	w, err := ws.CreateWorkspace(t.Context(), "acme")
	if err != nil {
		t.Fatalf("workspace: %v", err)
	}
	key, _, err := ws.CreateAPIKey(t.Context(), w.ID, "test")
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	srv := httptest.NewServer(NewRouter(Deps{
		Workspaces:  ws,
		Collections: collection.New(pool),
		Records:     record.New(pool),
	}))
	t.Cleanup(srv.Close)
	return srv, key
}

func do(t *testing.T, method, url, key string, body any) *http.Response {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req, _ := http.NewRequest(method, url, &buf)
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("X-Substrate-Actor", "agent-1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	return resp
}

func TestRecordLifecycleOverHTTP(t *testing.T) {
	srv, key := newTestServer(t)

	// Create collection.
	resp := do(t, "POST", srv.URL+"/v1/collections", key, map[string]any{"name": "trips", "level": "flexible"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create collection status = %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Create record.
	resp = do(t, "POST", srv.URL+"/v1/collections/trips/records", key, map[string]any{"data": map[string]any{"dest": "Tokyo"}})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create record status = %d", resp.StatusCode)
	}
	var created struct {
		ID       string `json:"id"`
		Revision int64  `json:"revision"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	if created.Revision != 1 {
		t.Fatalf("revision = %d, want 1", created.Revision)
	}

	// Read it back.
	resp = do(t, "GET", srv.URL+"/v1/collections/trips/records/"+created.ID, key, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get status = %d", resp.StatusCode)
	}
	if etag := resp.Header.Get("ETag"); etag != `"1"` {
		t.Fatalf("etag = %q, want \"1\"", etag)
	}
	resp.Body.Close()

	// Update with If-Match.
	req, _ := http.NewRequest("PATCH", srv.URL+"/v1/collections/trips/records/"+created.ID, bytes.NewBufferString(`{"data":{"dest":"Kyoto"}}`))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("If-Match", `"1"`)
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("update status = %d", resp.StatusCode)
	}
	resp.Body.Close()

	// History has 2 entries.
	resp = do(t, "GET", srv.URL+"/v1/collections/trips/records/"+created.ID+"/history", key, nil)
	var hist []map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&hist)
	resp.Body.Close()
	if len(hist) != 2 {
		t.Fatalf("history len = %d, want 2", len(hist))
	}

	// Unauthorized without key.
	bad, _ := http.NewRequest("GET", srv.URL+"/v1/collections/trips/records/"+created.ID, nil)
	resp, _ = http.DefaultClient.Do(bad)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no-key status = %d, want 401", resp.StatusCode)
	}
	resp.Body.Close()
}
```

> `t.Context()` is available in Go 1.24+; on 1.26 it is the idiomatic per-test context.

- [ ] **Step 2: Run it to confirm it fails**

Run: `go test -tags=integration ./internal/api/...`
Expected: FAIL — `Deps` has no such fields; handlers undefined.

- [ ] **Step 3: Implement the handlers**

Create `internal/api/handlers.go`:
```go
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/google/uuid"

	"github.com/substrate/substrate/internal/apierr"
	"github.com/substrate/substrate/internal/auth"
	"github.com/substrate/substrate/internal/collection"
	"github.com/substrate/substrate/internal/httpx"
	"github.com/substrate/substrate/internal/record"
)

// CollectionService is the subset of collection behavior handlers need.
type CollectionService interface {
	Create(ctx context.Context, ws uuid.UUID, name string) (collection.Collection, error)
	GetByName(ctx context.Context, ws, _ uuid.UUID, name string) (collection.Collection, error)
}

func writeErr(w http.ResponseWriter, err error) {
	e, ok := apierr.As(err)
	if !ok {
		e = apierr.New(apierr.Internal, "internal error")
	}
	httpx.JSON(w, e.HTTPStatus(), httpx.Envelope{
		Error: httpx.EnvelopeError{Code: string(e.Code), Message: e.Message, Details: e.Details},
	})
}

// resolveCollection looks up a collection by name within the request's workspace.
func (h *handlers) resolveCollection(r *http.Request, name string) (collection.Collection, error) {
	ws := auth.WorkspaceFrom(r.Context())
	return h.collections.GetByName(r.Context(), ws, uuid.Nil, name)
}

type handlers struct {
	collections *collection.Service
	records     *record.Service
}

// --- collections ---

func (h *handlers) createCollection(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name  string `json:"name"`
		Level string `json:"level"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, apierr.New(apierr.BadRequest, "invalid json"))
		return
	}
	c, err := h.collections.Create(r.Context(), auth.WorkspaceFrom(r.Context()), body.Name)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, c)
}

// --- records ---

func (h *handlers) createRecord(w http.ResponseWriter, r *http.Request) {
	c, err := h.resolveCollection(r, r.PathValue("collection"))
	if err != nil {
		writeErr(w, err)
		return
	}
	var body struct {
		Data map[string]any `json:"data"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, apierr.New(apierr.BadRequest, "invalid json"))
		return
	}
	rec, err := h.records.Create(r.Context(), record.CreateCmd{
		Workspace: c.WorkspaceID, Collection: c.ID, Actor: auth.ActorFrom(r.Context()),
		Data: body.Data, IdempotencyKey: r.Header.Get("Idempotency-Key"),
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	setETag(w, rec.Revision)
	httpx.JSON(w, http.StatusCreated, rec)
}

func (h *handlers) getRecord(w http.ResponseWriter, r *http.Request) {
	c, err := h.resolveCollection(r, r.PathValue("collection"))
	if err != nil {
		writeErr(w, err)
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, apierr.New(apierr.BadRequest, "invalid id"))
		return
	}
	if asOf := r.URL.Query().Get("as_of"); asOf != "" {
		at, perr := parseAsOf(asOf)
		if perr != nil {
			writeErr(w, perr)
			return
		}
		rec, err := h.records.GetAsOf(r.Context(), c.WorkspaceID, c.ID, id, at)
		if err != nil {
			writeErr(w, err)
			return
		}
		httpx.JSON(w, http.StatusOK, rec)
		return
	}
	rec, err := h.records.Get(r.Context(), c.WorkspaceID, c.ID, id)
	if err != nil {
		writeErr(w, err)
		return
	}
	setETag(w, rec.Revision)
	httpx.JSON(w, http.StatusOK, rec)
}

func (h *handlers) updateRecord(w http.ResponseWriter, r *http.Request) {
	c, err := h.resolveCollection(r, r.PathValue("collection"))
	if err != nil {
		writeErr(w, err)
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, apierr.New(apierr.BadRequest, "invalid id"))
		return
	}
	rev, err := parseIfMatch(r.Header.Get("If-Match"))
	if err != nil {
		writeErr(w, err)
		return
	}
	var body struct {
		Data map[string]any `json:"data"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, apierr.New(apierr.BadRequest, "invalid json"))
		return
	}
	rec, err := h.records.Update(r.Context(), record.UpdateCmd{
		Workspace: c.WorkspaceID, Collection: c.ID, ID: id, ExpectedRevision: rev,
		Actor: auth.ActorFrom(r.Context()), Data: body.Data,
		IdempotencyKey: r.Header.Get("Idempotency-Key"),
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	setETag(w, rec.Revision)
	httpx.JSON(w, http.StatusOK, rec)
}

func (h *handlers) deleteRecord(w http.ResponseWriter, r *http.Request) {
	c, err := h.resolveCollection(r, r.PathValue("collection"))
	if err != nil {
		writeErr(w, err)
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, apierr.New(apierr.BadRequest, "invalid id"))
		return
	}
	if err := h.records.Delete(r.Context(), c.WorkspaceID, c.ID, id); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handlers) recordHistory(w http.ResponseWriter, r *http.Request) {
	c, err := h.resolveCollection(r, r.PathValue("collection"))
	if err != nil {
		writeErr(w, err)
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, apierr.New(apierr.BadRequest, "invalid id"))
		return
	}
	hist, err := h.records.History(r.Context(), c.WorkspaceID, c.ID, id)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, hist)
}

func (h *handlers) revertRecord(w http.ResponseWriter, r *http.Request) {
	c, err := h.resolveCollection(r, r.PathValue("collection"))
	if err != nil {
		writeErr(w, err)
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, apierr.New(apierr.BadRequest, "invalid id"))
		return
	}
	var body struct {
		To string `json:"to"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	at, perr := parseAsOf(body.To)
	if perr != nil {
		writeErr(w, perr)
		return
	}
	rec, err := h.records.Revert(r.Context(), c.WorkspaceID, c.ID, id, at)
	if err != nil {
		writeErr(w, err)
		return
	}
	setETag(w, rec.Revision)
	httpx.JSON(w, http.StatusOK, rec)
}

// --- helpers ---

func setETag(w http.ResponseWriter, rev int64) {
	w.Header().Set("ETag", strconv.Quote(strconv.FormatInt(rev, 10)))
}

func parseIfMatch(h string) (int64, error) {
	if h == "" {
		return 0, apierr.New(apierr.BadRequest, "If-Match header required for update")
	}
	unq, err := strconv.Unquote(h)
	if err != nil {
		unq = h
	}
	rev, err := strconv.ParseInt(unq, 10, 64)
	if err != nil {
		return 0, apierr.New(apierr.BadRequest, "invalid If-Match revision")
	}
	return rev, nil
}

func parseAsOf(s string) (record.AsOf, error) {
	if rev, err := strconv.ParseInt(s, 10, 64); err == nil {
		return record.AsOf{Revision: rev}, nil
	}
	if id, err := uuid.Parse(s); err == nil {
		return record.AsOf{EventID: id}, nil
	}
	if ts, err := parseTime(s); err == nil {
		return record.AsOf{Timestamp: ts}, nil
	}
	return record.AsOf{}, apierr.New(apierr.BadRequest, "as_of must be a revision, event id, or RFC3339 timestamp")
}
```

- [ ] **Step 4: Add the time parser**

Append to `internal/api/handlers.go`:
```go
func parseTime(s string) (time.Time, error) {
	return time.Parse(time.RFC3339, s)
}
```
And add `"time"` to the import block.

- [ ] **Step 5: Wire the routes**

Replace `internal/api/router.go` with:
```go
package api

import (
	"net/http"

	"github.com/substrate/substrate/internal/auth"
	"github.com/substrate/substrate/internal/collection"
	"github.com/substrate/substrate/internal/httpx"
	"github.com/substrate/substrate/internal/record"
	"github.com/substrate/substrate/internal/workspace"
)

// Deps holds the collaborators the API needs.
type Deps struct {
	Workspaces  *workspace.Service
	Collections *collection.Service
	Records     *record.Service
}

// NewRouter builds the HTTP handler for the whole API.
func NewRouter(d Deps) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		httpx.JSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	if d.Records == nil {
		return mux // health-only mode (used by the scaffold test)
	}

	h := &handlers{collections: d.Collections, records: d.Records}

	api := http.NewServeMux()
	api.HandleFunc("POST /v1/collections", h.createCollection)
	api.HandleFunc("POST /v1/collections/{collection}/records", h.createRecord)
	api.HandleFunc("GET /v1/collections/{collection}/records/{id}", h.getRecord)
	api.HandleFunc("PATCH /v1/collections/{collection}/records/{id}", h.updateRecord)
	api.HandleFunc("DELETE /v1/collections/{collection}/records/{id}", h.deleteRecord)
	api.HandleFunc("GET /v1/collections/{collection}/records/{id}/history", h.recordHistory)
	api.HandleFunc("POST /v1/collections/{collection}/records/{id}/revert", h.revertRecord)

	protected := auth.Middleware(d.Workspaces)(api)
	mux.Handle("/v1/", protected)
	return mux
}
```

> Note: the `CollectionService` interface declared in handlers.go is not used by the concrete wiring (handlers hold `*collection.Service` directly). Delete that interface from handlers.go to avoid an unused-type lint; it was scaffolding. The concrete `*collection.Service` already provides `Create` and `GetByName`.

- [ ] **Step 6: Fix the GetByName signature mismatch**

The handler calls `h.collections.GetByName(ctx, ws, uuid.Nil, name)` but Task 6 defined `GetByName(ctx, ws, name)`. Align them: edit `internal/api/handlers.go` `resolveCollection` to:
```go
func (h *handlers) resolveCollection(r *http.Request, name string) (collection.Collection, error) {
	ws := auth.WorkspaceFrom(r.Context())
	return h.collections.GetByName(r.Context(), ws, name)
}
```
and remove the now-unused `CollectionService` interface block.

- [ ] **Step 7: Run the test to confirm it passes**

Run: `go test -tags=integration ./internal/api/...`
Expected: PASS.

- [ ] **Step 8: Run the full suite**

Run: `go test ./... && go test -tags=integration ./...`
Expected: all pass.

- [ ] **Step 9: Commit**

```bash
git add internal/api
git commit -m "feat: HTTP API for collections and records with auth, ETag, time-travel"
```

---

## Task 10: Config and runnable binary (external + embedded Postgres)

**Files:**
- Create: `internal/config/config.go`
- Modify: `cmd/substrate/main.go`
- Test: `internal/config/config_test.go`

- [ ] **Step 1: Write the failing config test**

Create `internal/config/config_test.go`:
```go
package config

import "testing"

func TestLoadDefaults(t *testing.T) {
	c := Load([]string{}, func(string) string { return "" })
	if c.Addr != ":8080" {
		t.Errorf("addr = %q, want :8080", c.Addr)
	}
	if c.Embedded {
		t.Errorf("embedded should default false")
	}
}

func TestEnvOverridesFlagFallback(t *testing.T) {
	env := map[string]string{"SUBSTRATE_DATABASE_URL": "postgres://x"}
	c := Load([]string{"-addr", ":9000"}, func(k string) string { return env[k] })
	if c.Addr != ":9000" {
		t.Errorf("addr = %q, want :9000", c.Addr)
	}
	if c.DatabaseURL != "postgres://x" {
		t.Errorf("db url = %q", c.DatabaseURL)
	}
}
```

- [ ] **Step 2: Run it to confirm it fails**

Run: `go test ./internal/config/...`
Expected: FAIL — `undefined: Load`.

- [ ] **Step 3: Implement config**

Create `internal/config/config.go`:
```go
package config

import "flag"

// Config holds runtime configuration.
type Config struct {
	Addr        string
	DatabaseURL string
	Embedded    bool
	LogLevel    string
}

// Load parses flags (args, without program name) with environment fallback.
// getenv lets tests inject the environment.
func Load(args []string, getenv func(string) string) Config {
	fs := flag.NewFlagSet("substrate", flag.ContinueOnError)
	addr := fs.String("addr", ":8080", "listen address")
	dbURL := fs.String("database-url", "", "postgres DSN (external mode)")
	embedded := fs.Bool("embedded", false, "run an embedded postgres")
	logLevel := fs.String("log-level", "info", "log level")
	_ = fs.Parse(args)

	c := Config{
		Addr:        *addr,
		DatabaseURL: *dbURL,
		Embedded:    *embedded,
		LogLevel:    *logLevel,
	}
	if v := getenv("SUBSTRATE_DATABASE_URL"); v != "" && c.DatabaseURL == "" {
		c.DatabaseURL = v
	}
	if getenv("SUBSTRATE_EMBEDDED") == "true" {
		c.Embedded = true
	}
	return c
}
```

- [ ] **Step 4: Run the test to confirm it passes**

Run: `go test ./internal/config/...`
Expected: PASS.

- [ ] **Step 5: Add the embedded-postgres dependency**

Run:
```bash
go get github.com/fergusstrange/embedded-postgres@latest
```

- [ ] **Step 6: Wire main to config, migrations, embedded mode, and the full router**

Replace `cmd/substrate/main.go` with:
```go
package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"

	"github.com/substrate/substrate/internal/api"
	"github.com/substrate/substrate/internal/collection"
	"github.com/substrate/substrate/internal/config"
	"github.com/substrate/substrate/internal/record"
	"github.com/substrate/substrate/internal/store"
	"github.com/substrate/substrate/internal/workspace"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	cfg := config.Load(os.Args[1:], os.Getenv)
	ctx := context.Background()

	dsn := cfg.DatabaseURL
	if cfg.Embedded {
		ep := embeddedpostgres.NewDatabase(
			embeddedpostgres.DefaultConfig().Database("substrate").Port(5433))
		if err := ep.Start(); err != nil {
			logger.Error("start embedded postgres", "err", err)
			os.Exit(1)
		}
		defer func() { _ = ep.Stop() }()
		dsn = "postgres://postgres:postgres@localhost:5433/substrate?sslmode=disable"
	}
	if dsn == "" {
		logger.Error("no database configured: set --database-url or --embedded")
		os.Exit(1)
	}

	pool, err := store.Open(ctx, dsn)
	if err != nil {
		logger.Error("open db", "err", err)
		os.Exit(1)
	}
	defer pool.Close()

	if err := store.Migrate(ctx, pool); err != nil {
		logger.Error("migrate", "err", err)
		os.Exit(1)
	}

	router := api.NewRouter(api.Deps{
		Workspaces:  workspace.New(pool),
		Collections: collection.New(pool),
		Records:     record.New(pool),
	})

	logger.Info("substrate starting", "addr", cfg.Addr, "embedded", cfg.Embedded)
	if err := http.ListenAndServe(cfg.Addr, router); err != nil {
		logger.Error("server exited", "err", err)
		os.Exit(1)
	}
}
```

- [ ] **Step 7: Verify build and full test suite**

Run: `go build ./... && go test ./... && go test -tags=integration ./...`
Expected: builds; all tests pass.

- [ ] **Step 8: Commit**

```bash
git add internal/config cmd go.mod go.sum
git commit -m "feat: config, embedded-postgres mode, and full server wiring"
```

---

## Task 11: Bootstrap admin endpoint for workspaces and keys

**Files:**
- Modify: `internal/api/router.go`, `internal/api/handlers.go`, `cmd/substrate/main.go`, `internal/config/config.go`
- Test: `internal/api/admin_test.go`

Without this, there is no way to create the first workspace/key over HTTP. Gate it behind an admin bootstrap token from config.

- [ ] **Step 1: Add an admin token to config**

In `internal/config/config.go`, add field `AdminToken string` to `Config`, a flag `admin-token` (default ""), and env fallback `SUBSTRATE_ADMIN_TOKEN`. Add to the struct literal in `Load`:
```go
adminToken := fs.String("admin-token", "", "bootstrap admin token")
// ... after parse:
c.AdminToken = *adminToken
if v := getenv("SUBSTRATE_ADMIN_TOKEN"); v != "" && c.AdminToken == "" {
	c.AdminToken = v
}
```

- [ ] **Step 2: Write the failing admin test**

Create `internal/api/admin_test.go`:
```go
//go:build integration

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/substrate/substrate/internal/collection"
	"github.com/substrate/substrate/internal/record"
	"github.com/substrate/substrate/internal/store"
	"github.com/substrate/substrate/internal/workspace"
)

func TestAdminBootstrap(t *testing.T) {
	pool := store.NewTestPool(t)
	srv := httptest.NewServer(NewRouter(Deps{
		Workspaces:  workspace.New(pool),
		Collections: collection.New(pool),
		Records:     record.New(pool),
		AdminToken:  "secret",
	}))
	defer srv.Close()

	// Wrong token rejected.
	req, _ := http.NewRequest("POST", srv.URL+"/admin/workspaces", nil)
	req.Header.Set("X-Admin-Token", "nope")
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad token status = %d, want 401", resp.StatusCode)
	}
	resp.Body.Close()

	// Create workspace.
	req, _ = http.NewRequest("POST", srv.URL+"/admin/workspaces", jsonBody(`{"name":"acme"}`))
	req.Header.Set("X-Admin-Token", "secret")
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create ws status = %d", resp.StatusCode)
	}
	var ws struct {
		ID string `json:"id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&ws)
	resp.Body.Close()

	// Create key.
	req, _ = http.NewRequest("POST", srv.URL+"/admin/workspaces/"+ws.ID+"/api-keys", jsonBody(`{"label":"default"}`))
	req.Header.Set("X-Admin-Token", "secret")
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create key status = %d", resp.StatusCode)
	}
	var key struct {
		Key string `json:"key"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&key)
	resp.Body.Close()
	if key.Key == "" {
		t.Fatal("expected plaintext key in response")
	}
}
```

Add to `internal/api/api_test.go` a small helper (or put it in admin_test.go):
```go
func jsonBody(s string) *strings.Reader { return strings.NewReader(s) }
```
with `"strings"` imported in that file.

- [ ] **Step 3: Run it to confirm it fails**

Run: `go test -tags=integration ./internal/api/...`
Expected: FAIL — `Deps` has no `AdminToken`; admin routes missing.

- [ ] **Step 4: Implement admin handlers**

Append to `internal/api/handlers.go`:
```go
type adminHandlers struct {
	workspaces *workspace.Service
	token      string
}

func (a *adminHandlers) authed(r *http.Request) bool {
	return a.token != "" && r.Header.Get("X-Admin-Token") == a.token
}

func (a *adminHandlers) createWorkspace(w http.ResponseWriter, r *http.Request) {
	if !a.authed(r) {
		writeErr(w, apierr.New(apierr.Unauthorized, "invalid admin token"))
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, apierr.New(apierr.BadRequest, "invalid json"))
		return
	}
	ws, err := a.workspaces.CreateWorkspace(r.Context(), body.Name)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, ws)
}

func (a *adminHandlers) createKey(w http.ResponseWriter, r *http.Request) {
	if !a.authed(r) {
		writeErr(w, apierr.New(apierr.Unauthorized, "invalid admin token"))
		return
	}
	wsID, err := uuid.Parse(r.PathValue("ws"))
	if err != nil {
		writeErr(w, apierr.New(apierr.BadRequest, "invalid workspace id"))
		return
	}
	var body struct {
		Label string `json:"label"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	plaintext, id, err := a.workspaces.CreateAPIKey(r.Context(), wsID, body.Label)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, map[string]any{"id": id, "key": plaintext})
}
```
Add `"github.com/substrate/substrate/internal/workspace"` to the import block.

- [ ] **Step 5: Wire admin routes and Deps field**

In `internal/api/router.go`, add `AdminToken string` to `Deps`, and before `return mux` (in the non-health-only branch) add:
```go
	admin := &adminHandlers{workspaces: d.Workspaces, token: d.AdminToken}
	mux.HandleFunc("POST /admin/workspaces", admin.createWorkspace)
	mux.HandleFunc("POST /admin/workspaces/{ws}/api-keys", admin.createKey)
```

- [ ] **Step 6: Pass the admin token from main**

In `cmd/substrate/main.go`, add `AdminToken: cfg.AdminToken,` to the `api.Deps{...}` literal.

- [ ] **Step 7: Run the test to confirm it passes**

Run: `go test -tags=integration ./internal/api/...`
Expected: PASS.

- [ ] **Step 8: Full suite + manual smoke (optional)**

Run: `go build ./... && go test ./... && go test -tags=integration ./...`
Expected: all pass.

Optional manual smoke with embedded mode:
```bash
SUBSTRATE_ADMIN_TOKEN=secret go run ./cmd/substrate --embedded &
curl -s -XPOST localhost:8080/admin/workspaces -H 'X-Admin-Token: secret' -d '{"name":"acme"}'
```

- [ ] **Step 9: Commit**

```bash
git add internal/api internal/config cmd
git commit -m "feat: admin bootstrap endpoints for workspaces and api keys"
```

---

## Task 12: README quickstart

**Files:**
- Create: `README.md`

- [ ] **Step 1: Write the quickstart**

Create `README.md` documenting: prerequisites (Go 1.26, [Task](https://taskfile.dev), Docker for integration tests), `task build`, running with `--embedded` vs `--database-url`, the admin bootstrap flow, and a `curl` walkthrough of create-collection → create-record → update (If-Match) → history → revert. Include the test commands (and note the `task test` / `task test:integration` equivalents):
```bash
go test ./...                      # unit tests
go test -tags=integration ./...    # integration tests (needs Docker)
```

- [ ] **Step 2: Commit**

```bash
git add README.md
git commit -m "docs: add quickstart README"
```

---

## Self-review notes (for the implementer)

- **Spec coverage:** This plan delivers spec subsystems 1 (storage/event core, time-travel) and 3-auth + the flexible-level slice of the API. Schema validation (spec §6), declarative policy (spec §6), rich query/list + indexes (spec §5 query params, §6 backfill), and the audit endpoint are **explicitly deferred** to Plans 2–5 per the roadmap at the top. `list_records` and `GET /v1/audit` are NOT implemented here.
- **Idempotency replay nuance:** replay returns the prior event's `state_after`. The unique-index collision path in `appendEvent` is a backstop; the primary path is the `lookupReplay` check inside the same transaction.
- **Concurrency:** `Update`/`Delete`/`Revert` take `FOR UPDATE` row locks, so concurrent writers serialize and the revision check is race-free.
- **Type consistency check:** `record.AsOf`, `record.CreateCmd`, `record.UpdateCmd`, `collection.Collection.WorkspaceID`, and `workspace.Service.VerifyKey` signatures are used identically across record, api, and auth packages. `GetByName(ctx, ws, name)` is reconciled in Task 9 Step 6.
- **Run all tests** at the end: `go test ./... && go test -tags=integration ./...`.
