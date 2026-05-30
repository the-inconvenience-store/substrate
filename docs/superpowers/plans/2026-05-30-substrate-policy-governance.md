# Substrate Plan 4 — Policy & Governance Plane Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a declarative allow/deny policy plane evaluated in-process before record reads/writes and schema lifecycle ops, with every decision recorded on the event/audit timeline, plus a `GET /v1/audit` read endpoint.

**Architecture:** A new `internal/policy` package owns a pure precedence engine + a DB-backed `Engine` implementing an `Evaluator` interface. The `Engine` is injected (optionally, builder-style — `nil` ⇒ no enforcement) into `record.Service` and `schema.Service`, mirroring the existing `Validator`/`Indexer` seams. On deny the engine writes a `policy_denied` event and returns `403`; on allow the matched decision is stamped into the event's `trace` JSONB. Rule CRUD, a default-mode flip, and a filtered/paginated audit read round out the API.

**Tech Stack:** Go 1.26, PostgreSQL via pgx/v5 (pgxpool), goose migrations, sqlc for static queries (hand-built parameterized SQL only for the audit filter, mirroring `internal/query`), testcontainers integration tests, `mise` task runner.

---

## Background the implementer needs

- **The write pipeline** lives in [internal/record/record.go](../../../internal/record/record.go): `Create`/`Update`/`Delete` each run `store.WithTx`, calling the local `appendEvent(ctx, qtx, eventRow{...})` helper then an upsert. `Revert`/`History`/`GetAsOf` live in [internal/record/timetravel.go](../../../internal/record/timetravel.go); `Get`/`List` in record.go.
- **`appendEvent`** builds `db.AppendEventParams`. The `events` table has a `trace jsonb` column that is currently never written (defaults NULL). Task 1 adds `trace` to the `AppendEvent` INSERT; existing call sites keep compiling because they omit the new struct field (zero value `nil` → NULL).
- **Schema lifecycle** lives in [internal/schema/registry.go](../../../internal/schema/registry.go): `Register`/`Activate`/`Deprecate`, with the local helpers `appendSchemaEvent(...)` and `activateTx(...)`.
- **Optional-dependency pattern:** `schema.Service` has `New` + `NewWithIndexer`; `record.Service` has `New(pool, validator)`. We add a chainable `WithEvaluator(policy.Evaluator)` method to both so existing constructors and their tests are untouched.
- **Actor** is pinned on the request context by `auth.Middleware` and read via `auth.ActorFrom(ctx)`. Write command structs already carry `Actor`; read methods do not — Task 5 adds an `actor string` parameter to the read methods.
- **sqlc** config is [sqlc.yaml](../../../sqlc.yaml); queries in `internal/queries/*.sql`; generated code in `internal/db/`. Regenerate with `mise run sqlc:generate`. Migrations in `internal/migrations/` (goose), embedded and run at startup.
- **Tests:** unit tests are plain `go test ./...`; integration tests carry `//go:build integration` and use `store.NewTestPool(t)` (a fresh migrated DB in a shared container). Run via `mise run test` and `mise run test:integration`.
- **Operation tokens** (rule `operation` values, plus `*`): `create`, `read`, `update`, `delete`, `register_schema`, `activate_schema`, `deprecate_schema`.

---

## Task 1: DB foundation — policies table, queries, trace column, codegen

**Files:**
- Create: `internal/migrations/00003_policies.sql`
- Create: `internal/queries/policies.sql`
- Modify: `internal/queries/events.sql` (add `trace` to `AppendEvent`)
- Modify: `internal/queries/workspaces.sql` (add policy-mode get/set)
- Modify: `sqlc.yaml` (nullable-uuid override)
- Regenerated: `internal/db/*.go`
- Test: `internal/store/policies_migrate_test.go`

- [ ] **Step 1: Write the migration**

`internal/migrations/00003_policies.sql`:

```sql
-- +goose Up
CREATE TABLE policies (
    id            uuid PRIMARY KEY,
    workspace_id  uuid NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    actor         text NOT NULL DEFAULT '*',
    collection_id uuid REFERENCES collections(id) ON DELETE CASCADE,  -- NULL = any collection
    operation     text NOT NULL DEFAULT '*',
    effect        text NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX policies_ws_idx ON policies(workspace_id);

-- +goose Down
DROP TABLE IF EXISTS policies;
```

- [ ] **Step 2: Add the nullable-uuid override to `sqlc.yaml`**

Under `gen.go.overrides`, after the existing `uuid` override, add a nullable variant so `policies.collection_id` (the only nullable uuid column) maps to `pgtype.UUID`:

```yaml
        overrides:
          - db_type: "uuid"
            go_type: "github.com/google/uuid.UUID"
          - db_type: "uuid"
            nullable: true
            go_type: "github.com/jackc/pgx/v5/pgtype.UUID"
```

- [ ] **Step 3: Write `internal/queries/policies.sql`**

```sql
-- name: InsertPolicy :one
INSERT INTO policies (id, workspace_id, actor, collection_id, operation, effect)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING id, workspace_id, actor, collection_id, operation, effect, created_at;

-- name: ListPolicies :many
SELECT p.id, p.actor, p.collection_id, c.name AS collection_name, p.operation, p.effect, p.created_at
FROM policies p
LEFT JOIN collections c ON c.id = p.collection_id
WHERE p.workspace_id = $1
ORDER BY p.created_at ASC, p.id ASC;

-- name: ListPoliciesForRequest :many
SELECT id, actor, collection_id, operation, effect
FROM policies
WHERE workspace_id = $1
  AND (collection_id = $2 OR collection_id IS NULL);

-- name: DeletePolicy :execrows
DELETE FROM policies WHERE id = $1 AND workspace_id = $2;
```

- [ ] **Step 4: Add policy-mode queries to `internal/queries/workspaces.sql`** (append):

```sql
-- name: GetWorkspacePolicyMode :one
SELECT policy_mode FROM workspaces WHERE id = $1;

-- name: SetWorkspacePolicyMode :exec
UPDATE workspaces SET policy_mode = $2 WHERE id = $1;
```

- [ ] **Step 5: Add `trace` to `AppendEvent` in `internal/queries/events.sql`**

Replace the `AppendEvent` query with:

```sql
-- name: AppendEvent :exec
INSERT INTO events (id, workspace_id, collection_id, record_id, type, revision, state_after, actor, idempotency_key, trace)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10);
```

- [ ] **Step 6: Regenerate sqlc**

Run: `mise run sqlc:generate`
Expected: `internal/db/policies.sql.go` created; `AppendEventParams` gains `Trace []byte`; `ListPoliciesForRequestRow.CollectionID` and `InsertPolicyParams.CollectionID` are `pgtype.UUID`; `ListPoliciesRow.CollectionName` is `pgtype.Text`. Then run `go build ./...` — Expected: PASS (existing `AppendEvent` callers omit `Trace`, so they default to `nil`).

- [ ] **Step 7: Write the migration smoke test**

`internal/store/policies_migrate_test.go`:

```go
//go:build integration

package store_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/substrate/substrate/internal/db"
	"github.com/substrate/substrate/internal/store"
)

func TestPoliciesTableCRUD(t *testing.T) {
	pool := store.NewTestPool(t)
	ctx := context.Background()
	q := db.New(pool)

	ws, err := q.CreateWorkspace(ctx, db.CreateWorkspaceParams{ID: uuid.New(), Name: "w", PolicyMode: "allow"})
	if err != nil {
		t.Fatalf("workspace: %v", err)
	}
	row, err := q.InsertPolicy(ctx, db.InsertPolicyParams{
		ID: uuid.New(), WorkspaceID: ws.ID, Actor: "*",
		CollectionID: pgtype.UUID{}, Operation: "create", Effect: "deny",
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if row.Effect != "deny" {
		t.Fatalf("effect = %q", row.Effect)
	}
	rules, err := q.ListPolicies(ctx, ws.ID)
	if err != nil || len(rules) != 1 {
		t.Fatalf("list: %v len=%d", err, len(rules))
	}
	if rules[0].CollectionName.Valid {
		t.Fatalf("wildcard rule should have NULL collection_name")
	}
	if err := q.SetWorkspacePolicyMode(ctx, db.SetWorkspacePolicyModeParams{ID: ws.ID, PolicyMode: "deny"}); err != nil {
		t.Fatalf("set mode: %v", err)
	}
	mode, err := q.GetWorkspacePolicyMode(ctx, ws.ID)
	if err != nil || mode != "deny" {
		t.Fatalf("mode = %q err=%v", mode, err)
	}
	n, err := q.DeletePolicy(ctx, db.DeletePolicyParams{ID: row.ID, WorkspaceID: ws.ID})
	if err != nil || n != 1 {
		t.Fatalf("delete rows=%d err=%v", n, err)
	}
}
```

> If the generated `InsertPolicyParams.CollectionID` / `ListPoliciesForRequestParams.CollectionID` type is not `pgtype.UUID`, the nullable override (Step 2) did not take — recheck `sqlc.yaml` indentation and rerun Step 6 before adapting test code.

- [ ] **Step 8: Run the test**

Run: `mise run test:integration -- -run TestPoliciesTableCRUD ./internal/store/`
Expected: PASS

- [ ] **Step 9: Commit**

```bash
git add internal/migrations/00003_policies.sql internal/queries/ sqlc.yaml internal/db/ internal/store/policies_migrate_test.go
git commit -m "feat: policies table, policy-mode + audit-trace queries, codegen"
```

---

## Task 2: Pure precedence engine

**Files:**
- Create: `internal/policy/precedence.go`
- Test: `internal/policy/precedence_test.go`

- [ ] **Step 1: Write the failing test**

`internal/policy/precedence_test.go`:

```go
package policy

import (
	"testing"

	"github.com/google/uuid"
)

func ruleID(n byte) uuid.UUID { return uuid.UUID{15: n} }

func TestSelectPrecedence(t *testing.T) {
	colA := uuid.UUID{0: 1}
	colB := uuid.UUID{0: 2}
	req := Request{Actor: "alice", Collection: colA, Operation: OpCreate}

	tests := []struct {
		name      string
		rules     []Rule
		wantOK    bool
		wantEff   string
		wantRule  *uuid.UUID
	}{
		{
			name:   "no rules -> no decision",
			rules:  nil,
			wantOK: false,
		},
		{
			name:   "non-applicable rule ignored",
			rules:  []Rule{{ID: ruleID(1), Actor: "bob", CollectionWildcard: true, Operation: "*", Effect: "deny"}},
			wantOK: false,
		},
		{
			name:    "wildcard allow applies",
			rules:   []Rule{{ID: ruleID(2), Actor: "*", CollectionWildcard: true, Operation: "*", Effect: "allow"}},
			wantOK:  true,
			wantEff: "allow",
		},
		{
			name: "most-specific allow beats broad deny",
			rules: []Rule{
				{ID: ruleID(3), Actor: "*", CollectionWildcard: true, Operation: "*", Effect: "deny"},
				{ID: ruleID(4), Actor: "alice", Collection: colA, Operation: OpCreate, Effect: "allow"},
			},
			wantOK:   true,
			wantEff:  "allow",
			wantRule: ptr(ruleID(4)),
		},
		{
			name: "deny-overrides at equal specificity",
			rules: []Rule{
				{ID: ruleID(5), Actor: "alice", CollectionWildcard: true, Operation: "*", Effect: "allow"},
				{ID: ruleID(6), Actor: "*", Collection: colA, Operation: "*", Effect: "deny"},
			},
			wantOK:  true,
			wantEff: "deny",
		},
		{
			name: "collection mismatch excludes rule",
			rules: []Rule{
				{ID: ruleID(7), Actor: "*", Collection: colB, Operation: "*", Effect: "deny"},
			},
			wantOK: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dec, ok := Select(tc.rules, req)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if dec.Effect != tc.wantEff {
				t.Fatalf("effect = %q, want %q", dec.Effect, tc.wantEff)
			}
			if tc.wantRule != nil && (dec.MatchedRule == nil || *dec.MatchedRule != *tc.wantRule) {
				t.Fatalf("matched rule = %v, want %v", dec.MatchedRule, tc.wantRule)
			}
		})
	}
}

func TestDefaultDecision(t *testing.T) {
	if d := DefaultDecision("deny"); d.Effect != "deny" || d.Reason != "default_deny" {
		t.Fatalf("deny default = %+v", d)
	}
	if d := DefaultDecision("allow"); d.Effect != "allow" || d.Reason != "default_allow" {
		t.Fatalf("allow default = %+v", d)
	}
	if d := DefaultDecision("anything-else"); d.Effect != "allow" {
		t.Fatalf("unknown mode should default allow, got %+v", d)
	}
}

func ptr(id uuid.UUID) *uuid.UUID { return &id }
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/policy/`
Expected: FAIL (undefined `Request`, `Rule`, `Select`, etc.)

- [ ] **Step 3: Write `internal/policy/precedence.go`**

```go
// Package policy implements Substrate's declarative allow/deny governance plane.
package policy

import "github.com/google/uuid"

// Operation tokens a rule may target (besides "*").
const (
	OpCreate          = "create"
	OpRead            = "read"
	OpUpdate          = "update"
	OpDelete          = "delete"
	OpRegisterSchema  = "register_schema"
	OpActivateSchema  = "activate_schema"
	OpDeprecateSchema = "deprecate_schema"
)

// ValidOperations is the set of operation tokens accepted on rule creation.
var ValidOperations = map[string]bool{
	OpCreate: true, OpRead: true, OpUpdate: true, OpDelete: true,
	OpRegisterSchema: true, OpActivateSchema: true, OpDeprecateSchema: true,
}

// Rule is one loaded policy row. CollectionWildcard is true when collection_id is NULL.
type Rule struct {
	ID                 uuid.UUID
	Actor              string // exact actor or "*"
	Collection         uuid.UUID
	CollectionWildcard bool
	Operation          string // operation token or "*"
	Effect             string // "allow" | "deny"
}

// Request is one authorization question.
type Request struct {
	Workspace  uuid.UUID
	Actor      string
	Collection uuid.UUID
	Target     uuid.UUID // record id recorded on the denial event; uuid.Nil when none
	Operation  string
}

// Decision is the evaluation result.
type Decision struct {
	Effect      string     // "allow" | "deny"
	MatchedRule *uuid.UUID // nil when defaulted
	Reason      string     // "rule" | "default_allow" | "default_deny" | "no_evaluator"
}

// Allowed reports whether the decision permits the operation.
func (d Decision) Allowed() bool { return d.Effect == "allow" }

func (r Rule) applies(req Request) bool {
	if r.Actor != "*" && r.Actor != req.Actor {
		return false
	}
	if !r.CollectionWildcard && r.Collection != req.Collection {
		return false
	}
	if r.Operation != "*" && r.Operation != req.Operation {
		return false
	}
	return true
}

// specificity counts concrete (non-wildcard) dimensions, range 0..3.
func (r Rule) specificity() int {
	n := 0
	if r.Actor != "*" {
		n++
	}
	if !r.CollectionWildcard {
		n++
	}
	if r.Operation != "*" {
		n++
	}
	return n
}

// Select applies precedence: highest specificity wins; deny-overrides at equal
// specificity. Returns ok=false when no rule applies (caller uses the default mode).
func Select(rules []Rule, req Request) (Decision, bool) {
	best := -1
	var winner *Rule
	for i := range rules {
		if !rules[i].applies(req) {
			continue
		}
		s := rules[i].specificity()
		switch {
		case s > best:
			best, winner = s, &rules[i]
		case s == best && winner != nil && winner.Effect != "deny" && rules[i].Effect == "deny":
			winner = &rules[i]
		}
	}
	if winner == nil {
		return Decision{}, false
	}
	id := winner.ID
	return Decision{Effect: winner.Effect, MatchedRule: &id, Reason: "rule"}, true
}

// DefaultDecision maps a workspace policy_mode to a defaulted decision.
func DefaultDecision(mode string) Decision {
	if mode == "deny" {
		return Decision{Effect: "deny", Reason: "default_deny"}
	}
	return Decision{Effect: "allow", Reason: "default_allow"}
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/policy/`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/policy/precedence.go internal/policy/precedence_test.go
git commit -m "feat: pure policy precedence engine (specificity + deny-overrides)"
```

---

## Task 3: DB-backed Engine + denial events

**Files:**
- Create: `internal/policy/engine.go`
- Test: `internal/policy/engine_test.go`

- [ ] **Step 1: Write `internal/policy/engine.go`**

```go
package policy

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/substrate/substrate/internal/apierr"
	"github.com/substrate/substrate/internal/db"
)

// Evaluator authorizes an operation. On deny it records a policy_denied event and
// returns an apierr.Forbidden error alongside the (deny) decision.
type Evaluator interface {
	Authorize(ctx context.Context, req Request) (Decision, error)
}

// Engine is the in-process rule evaluator backed by the policies table.
type Engine struct {
	pool *pgxpool.Pool
	q    *db.Queries
}

// NewEngine builds the evaluator over a connection pool.
func NewEngine(pool *pgxpool.Pool) *Engine {
	return &Engine{pool: pool, q: db.New(pool)}
}

// Evaluate computes the decision with no side effects.
func (e *Engine) Evaluate(ctx context.Context, req Request) (Decision, error) {
	rows, err := e.q.ListPoliciesForRequest(ctx, db.ListPoliciesForRequestParams{
		WorkspaceID:  req.Workspace,
		CollectionID: pgtype.UUID{Bytes: req.Collection, Valid: true},
	})
	if err != nil {
		return Decision{}, fmt.Errorf("load policies: %w", err)
	}
	rules := make([]Rule, 0, len(rows))
	for _, r := range rows {
		rule := Rule{ID: r.ID, Actor: r.Actor, Operation: r.Operation, Effect: r.Effect}
		if r.CollectionID.Valid {
			rule.Collection = uuid.UUID(r.CollectionID.Bytes)
		} else {
			rule.CollectionWildcard = true
		}
		rules = append(rules, rule)
	}
	if dec, ok := Select(rules, req); ok {
		return dec, nil
	}
	mode, err := e.q.GetWorkspacePolicyMode(ctx, req.Workspace)
	if err != nil {
		return Decision{}, fmt.Errorf("load policy mode: %w", err)
	}
	return DefaultDecision(mode), nil
}

// Authorize evaluates and, on deny, writes a policy_denied event then returns Forbidden.
func (e *Engine) Authorize(ctx context.Context, req Request) (Decision, error) {
	dec, err := e.Evaluate(ctx, req)
	if err != nil {
		return Decision{}, err
	}
	if dec.Allowed() {
		return dec, nil
	}
	if err := e.recordDenial(ctx, req, dec); err != nil {
		return Decision{}, err
	}
	return dec, apierr.New(apierr.Forbidden, "operation denied by policy").
		WithDetails(denialDetails(dec))
}

func (e *Engine) recordDenial(ctx context.Context, req Request, dec Decision) error {
	err := e.q.AppendEvent(ctx, db.AppendEventParams{
		ID: uuid.New(), WorkspaceID: req.Workspace, CollectionID: req.Collection,
		RecordID: req.Target, Type: "policy_denied", Revision: 0,
		StateAfter:     nil,
		Actor:          pgtype.Text{String: req.Actor, Valid: req.Actor != ""},
		IdempotencyKey: pgtype.Text{},
		Trace:          dec.TraceJSON(req.Operation),
	})
	if err != nil {
		return fmt.Errorf("record denial: %w", err)
	}
	return nil
}

// TraceJSON renders the decision for the events.trace column.
func (d Decision) TraceJSON(op string) []byte {
	m := map[string]any{"effect": d.Effect, "reason": d.Reason, "operation": op}
	if d.MatchedRule != nil {
		m["matched_rule"] = d.MatchedRule.String()
	}
	b, _ := json.Marshal(m)
	return b
}

func denialDetails(d Decision) map[string]any {
	m := map[string]any{"reason": d.Reason}
	if d.MatchedRule != nil {
		m["matched_rule"] = d.MatchedRule.String()
	}
	return m
}
```

- [ ] **Step 2: Write the failing integration test**

`internal/policy/engine_test.go`:

```go
//go:build integration

package policy_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/substrate/substrate/internal/apierr"
	"github.com/substrate/substrate/internal/db"
	"github.com/substrate/substrate/internal/policy"
	"github.com/substrate/substrate/internal/store"
)

func seedWS(t *testing.T, q *db.Queries, mode string) (uuid.UUID, uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	ws, err := q.CreateWorkspace(ctx, db.CreateWorkspaceParams{ID: uuid.New(), Name: "w", PolicyMode: mode})
	if err != nil {
		t.Fatalf("ws: %v", err)
	}
	col, err := q.CreateCollection(ctx, db.CreateCollectionParams{
		ID: uuid.New(), WorkspaceID: ws.ID, Name: "things", Level: "flexible",
	})
	if err != nil {
		t.Fatalf("col: %v", err)
	}
	return ws.ID, col.ID
}

func TestAuthorizeDefaultAllow(t *testing.T) {
	pool := store.NewTestPool(t)
	q := db.New(pool)
	ws, col := seedWS(t, q, "allow")
	eng := policy.NewEngine(pool)

	dec, err := eng.Authorize(context.Background(), policy.Request{
		Workspace: ws, Actor: "alice", Collection: col, Operation: policy.OpCreate,
	})
	if err != nil || !dec.Allowed() || dec.Reason != "default_allow" {
		t.Fatalf("dec=%+v err=%v", dec, err)
	}
}

func TestAuthorizeDenyRuleWritesEvent(t *testing.T) {
	pool := store.NewTestPool(t)
	ctx := context.Background()
	q := db.New(pool)
	ws, col := seedWS(t, q, "allow")
	if _, err := q.InsertPolicy(ctx, db.InsertPolicyParams{
		ID: uuid.New(), WorkspaceID: ws, Actor: "alice",
		CollectionID: pgtype.UUID{Bytes: col, Valid: true}, Operation: policy.OpCreate, Effect: "deny",
	}); err != nil {
		t.Fatalf("rule: %v", err)
	}
	eng := policy.NewEngine(pool)

	dec, err := eng.Authorize(ctx, policy.Request{
		Workspace: ws, Actor: "alice", Collection: col, Target: uuid.Nil, Operation: policy.OpCreate,
	})
	if dec.Allowed() {
		t.Fatalf("expected deny, got %+v", dec)
	}
	var ae *apierr.Error
	if !errors.As(err, &ae) || ae.Code != apierr.Forbidden {
		t.Fatalf("expected Forbidden, got %v", err)
	}
	// A policy_denied event must exist for this workspace.
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM events WHERE workspace_id=$1 AND type='policy_denied'`, ws).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("denial events = %d, want 1", n)
	}
}

func TestAuthorizeDefaultDeny(t *testing.T) {
	pool := store.NewTestPool(t)
	q := db.New(pool)
	ws, col := seedWS(t, q, "deny")
	eng := policy.NewEngine(pool)

	dec, err := eng.Authorize(context.Background(), policy.Request{
		Workspace: ws, Actor: "alice", Collection: col, Operation: policy.OpRead,
	})
	if dec.Allowed() || dec.Reason != "default_deny" {
		t.Fatalf("dec=%+v err=%v", dec, err)
	}
}
```

- [ ] **Step 3: Run to verify it fails, then build**

Run: `go build ./internal/policy/` (Expected: PASS), then
`mise run test:integration -- -run TestAuthorize ./internal/policy/`
Expected: PASS

- [ ] **Step 4: Commit**

```bash
git add internal/policy/engine.go internal/policy/engine_test.go
git commit -m "feat: DB-backed policy engine with denial events"
```

---

## Task 4: Enforce policy on record writes

**Files:**
- Modify: `internal/record/record.go` (add evaluator field, `WithEvaluator`, `authorize` helper, `eventRow.Trace`; gate `Create`/`Update`/`Delete`)
- Modify: `internal/record/timetravel.go` (gate `Revert`)
- Test: `internal/record/policy_write_test.go`

- [ ] **Step 1: Add the evaluator seam + trace plumbing to `record.go`**

Add the import `"github.com/substrate/substrate/internal/policy"`. Add the field and builder:

```go
// Service performs record mutations and reads.
type Service struct {
	pool      *pgxpool.Pool
	q         *db.Queries
	validator Validator
	eval      policy.Evaluator
}
```

```go
// WithEvaluator wires an optional policy evaluator. nil ⇒ no enforcement.
func (s *Service) WithEvaluator(e policy.Evaluator) *Service { s.eval = e; return s }

// authorize runs the policy check for an operation. With no evaluator it allows.
func (s *Service) authorize(ctx context.Context, req policy.Request) (policy.Decision, error) {
	if s.eval == nil {
		return policy.Decision{Effect: "allow", Reason: "no_evaluator"}, nil
	}
	return s.eval.Authorize(ctx, req)
}

// policyTrace returns the trace bytes for an allowed event, or nil when no
// evaluator is wired (preserving the pre-policy NULL-trace behavior).
func (s *Service) policyTrace(dec policy.Decision, op string) []byte {
	if s.eval == nil {
		return nil
	}
	return dec.TraceJSON(op)
}
```

Add `Trace []byte` to the `eventRow` struct and thread it through `appendEvent`:

```go
type eventRow struct {
	Workspace      uuid.UUID
	Collection     uuid.UUID
	RecordID       uuid.UUID
	Type           string
	Revision       int64
	State          map[string]any
	Actor          string
	IdempotencyKey string
	Trace          []byte
}
```

In `appendEvent`, add `Trace: e.Trace,` to the `db.AppendEventParams{...}` literal.

- [ ] **Step 2: Gate `Create`**

At the very top of `Create` (before building `rec`):

```go
	dec, err := s.authorize(ctx, policy.Request{
		Workspace: cmd.Workspace, Actor: cmd.Actor, Collection: cmd.Collection,
		Target: uuid.Nil, Operation: policy.OpCreate,
	})
	if err != nil {
		return Record{}, err
	}
```

(Rename the existing `err` declarations below as needed — the `sv, err :=` becomes `sv, err =` since `err` now exists, or keep `:=` by scoping; simplest: change the later `sv, err := ...` to `sv, err = ...` and ensure `err` is declared once.) In the `appendEvent(ctx, qtx, eventRow{...})` call inside the `store.WithTx` closure, add:

```go
				Trace: s.policyTrace(dec, policy.OpCreate),
```

- [ ] **Step 3: Gate `Update`**

At the top of `Update` (before `var rec Record`):

```go
	dec, aerr := s.authorize(ctx, policy.Request{
		Workspace: cmd.Workspace, Actor: cmd.Actor, Collection: cmd.Collection,
		Target: cmd.ID, Operation: policy.OpUpdate,
	})
	if aerr != nil {
		return Record{}, aerr
	}
```

In the `appendEvent` call inside the closure, add `Trace: s.policyTrace(dec, policy.OpUpdate),`.

- [ ] **Step 4: Gate `Delete`**

At the top of `Delete`, before `return store.WithTx(...)`:

```go
	dec, err := s.authorize(ctx, policy.Request{
		Workspace: ws, Actor: actor, Collection: col, Target: id, Operation: policy.OpDelete,
	})
	if err != nil {
		return err
	}
```

In `Delete`'s `appendEvent` call, add `Trace: s.policyTrace(dec, policy.OpDelete),`.

- [ ] **Step 5: Gate `Revert` in `timetravel.go`**

Add the `policy` import. At the top of `Revert` (before `var rec Record`):

```go
	dec, aerr := s.authorize(ctx, policy.Request{
		Workspace: ws, Actor: actor, Collection: col, Target: id, Operation: policy.OpUpdate,
	})
	if aerr != nil {
		return Record{}, aerr
	}
```

In `Revert`'s `appendEvent` call, add `Trace: s.policyTrace(dec, policy.OpUpdate),`.

- [ ] **Step 6: Write the failing test**

`internal/record/policy_write_test.go`:

```go
//go:build integration

package record_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/substrate/substrate/internal/apierr"
	"github.com/substrate/substrate/internal/db"
	"github.com/substrate/substrate/internal/policy"
	"github.com/substrate/substrate/internal/record"
	"github.com/substrate/substrate/internal/store"
)

func TestCreateDeniedByPolicy(t *testing.T) {
	pool := store.NewTestPool(t)
	ctx := context.Background()
	q := db.New(pool)
	ws, _ := q.CreateWorkspace(ctx, db.CreateWorkspaceParams{ID: uuid.New(), Name: "w", PolicyMode: "allow"})
	col, _ := q.CreateCollection(ctx, db.CreateCollectionParams{ID: uuid.New(), WorkspaceID: ws.ID, Name: "c", Level: "flexible"})
	if _, err := q.InsertPolicy(ctx, db.InsertPolicyParams{
		ID: uuid.New(), WorkspaceID: ws.ID, Actor: "*",
		CollectionID: pgtype.UUID{Bytes: col.ID, Valid: true}, Operation: policy.OpCreate, Effect: "deny",
	}); err != nil {
		t.Fatalf("rule: %v", err)
	}
	svc := record.New(pool, nil).WithEvaluator(policy.NewEngine(pool))

	_, err := svc.Create(ctx, record.CreateCmd{Workspace: ws.ID, Collection: col.ID, Actor: "alice", Data: map[string]any{"x": 1}})
	var ae *apierr.Error
	if !errors.As(err, &ae) || ae.Code != apierr.Forbidden {
		t.Fatalf("want Forbidden, got %v", err)
	}
}

func TestCreateAllowedStampsTrace(t *testing.T) {
	pool := store.NewTestPool(t)
	ctx := context.Background()
	q := db.New(pool)
	ws, _ := q.CreateWorkspace(ctx, db.CreateWorkspaceParams{ID: uuid.New(), Name: "w", PolicyMode: "allow"})
	col, _ := q.CreateCollection(ctx, db.CreateCollectionParams{ID: uuid.New(), WorkspaceID: ws.ID, Name: "c", Level: "flexible"})
	svc := record.New(pool, nil).WithEvaluator(policy.NewEngine(pool))

	rec, err := svc.Create(ctx, record.CreateCmd{Workspace: ws.ID, Collection: col.ID, Actor: "alice", Data: map[string]any{"x": 1}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	var trace []byte
	if err := pool.QueryRow(ctx,
		`SELECT trace FROM events WHERE record_id=$1 AND type='create'`, rec.ID).Scan(&trace); err != nil {
		t.Fatalf("scan trace: %v", err)
	}
	if len(trace) == 0 {
		t.Fatalf("expected non-empty trace on allowed create")
	}
}

func TestCreateNoEvaluatorUnchanged(t *testing.T) {
	pool := store.NewTestPool(t)
	ctx := context.Background()
	q := db.New(pool)
	ws, _ := q.CreateWorkspace(ctx, db.CreateWorkspaceParams{ID: uuid.New(), Name: "w", PolicyMode: "deny"})
	col, _ := q.CreateCollection(ctx, db.CreateCollectionParams{ID: uuid.New(), WorkspaceID: ws.ID, Name: "c", Level: "flexible"})
	svc := record.New(pool, nil) // no evaluator: deny mode must NOT apply

	if _, err := svc.Create(ctx, record.CreateCmd{Workspace: ws.ID, Collection: col.ID, Actor: "alice"}); err != nil {
		t.Fatalf("create without evaluator should succeed, got %v", err)
	}
}
```

- [ ] **Step 7: Build and run**

Run: `go build ./... && mise run test:integration -- -run 'TestCreate(DeniedByPolicy|AllowedStampsTrace|NoEvaluatorUnchanged)' ./internal/record/`
Expected: PASS. Also run the existing record suite to confirm no regression:
`mise run test:integration -- ./internal/record/` — Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/record/record.go internal/record/timetravel.go internal/record/policy_write_test.go
git commit -m "feat: enforce policy on record writes; stamp decision trace"
```

---

## Task 5: Enforce policy on record reads (signature change)

**Files:**
- Modify: `internal/record/record.go` (`Get`, `List`), `internal/record/timetravel.go` (`History`, `GetAsOf`) — add `actor string` param + gate
- Modify: `internal/api/handlers.go` (pass actor into read calls)
- Modify existing call sites/tests: `internal/record/record_test.go`, `internal/record/list_test.go`, `internal/record/timetravel_test.go`, and any api tests that call these methods directly
- Test: `internal/record/policy_read_test.go`

- [ ] **Step 1: Change read method signatures + gate**

`Get` becomes `func (s *Service) Get(ctx context.Context, ws, col, id uuid.UUID, actor string) (Record, error)`. As the first statement:

```go
	if _, err := s.authorize(ctx, policy.Request{
		Workspace: ws, Actor: actor, Collection: col, Target: id, Operation: policy.OpRead,
	}); err != nil {
		return Record{}, err
	}
```

`List` becomes `func (s *Service) List(ctx context.Context, ws, col uuid.UUID, actor string, q query.ListQuery) ([]Record, string, error)`. As the first statement:

```go
	if _, err := s.authorize(ctx, policy.Request{
		Workspace: ws, Actor: actor, Collection: col, Target: uuid.Nil, Operation: policy.OpRead,
	}); err != nil {
		return nil, "", err
	}
```

In `timetravel.go`, `History` becomes `func (s *Service) History(ctx context.Context, ws, col, id uuid.UUID, actor string) ([]HistoryEntry, error)` and `GetAsOf` becomes `func (s *Service) GetAsOf(ctx context.Context, ws, col, id uuid.UUID, at AsOf, actor string) (Record, error)`. Each gets the same `OpRead` authorize guard at the top (`Target: id`), returning the method's zero values + err on denial.

- [ ] **Step 2: Update the handlers in `handlers.go`**

- `getRecord`: `h.records.GetAsOf(r.Context(), c.WorkspaceID, c.ID, id, at, auth.ActorFrom(r.Context()))` and `h.records.Get(r.Context(), c.WorkspaceID, c.ID, id, auth.ActorFrom(r.Context()))`.
- `listRecords`: `h.records.List(r.Context(), c.WorkspaceID, c.ID, auth.ActorFrom(r.Context()), q)`.
- `recordHistory`: `h.records.History(r.Context(), c.WorkspaceID, c.ID, id, auth.ActorFrom(r.Context()))`.

- [ ] **Step 3: Update existing direct call sites in tests**

Run: `grep -rn --include='*.go' '\.Get(\|\.List(\|\.History(\|\.GetAsOf(' internal/record internal/api`
For each call to these record-service methods, add a trailing `actor` argument (use `"tester"` in tests). Do not change unrelated `.Get(`/`.List(` calls on other types (e.g. `db.Queries`, `url.Values`); only the `record.Service` calls. Verify with `go build ./...`.

- [ ] **Step 4: Write the failing test**

`internal/record/policy_read_test.go`:

```go
//go:build integration

package record_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/substrate/substrate/internal/apierr"
	"github.com/substrate/substrate/internal/db"
	"github.com/substrate/substrate/internal/policy"
	"github.com/substrate/substrate/internal/query"
	"github.com/substrate/substrate/internal/record"
	"github.com/substrate/substrate/internal/store"
)

func TestReadDeniedByPolicy(t *testing.T) {
	pool := store.NewTestPool(t)
	ctx := context.Background()
	q := db.New(pool)
	ws, _ := q.CreateWorkspace(ctx, db.CreateWorkspaceParams{ID: uuid.New(), Name: "w", PolicyMode: "allow"})
	col, _ := q.CreateCollection(ctx, db.CreateCollectionParams{ID: uuid.New(), WorkspaceID: ws.ID, Name: "c", Level: "flexible"})
	if _, err := q.InsertPolicy(ctx, db.InsertPolicyParams{
		ID: uuid.New(), WorkspaceID: ws.ID, Actor: "mallory",
		CollectionID: pgtype.UUID{Bytes: col.ID, Valid: true}, Operation: policy.OpRead, Effect: "deny",
	}); err != nil {
		t.Fatalf("rule: %v", err)
	}
	svc := record.New(pool, nil).WithEvaluator(policy.NewEngine(pool))

	// Allowed actor can list; denied actor cannot.
	if _, _, err := svc.List(ctx, ws.ID, col.ID, "alice", mustParse(t)); err != nil {
		t.Fatalf("alice list: %v", err)
	}
	_, _, err := svc.List(ctx, ws.ID, col.ID, "mallory", mustParse(t))
	var ae *apierr.Error
	if !errors.As(err, &ae) || ae.Code != apierr.Forbidden {
		t.Fatalf("mallory list want Forbidden, got %v", err)
	}
}

func mustParse(t *testing.T) query.ListQuery {
	t.Helper()
	q, err := query.Parse(nil, "", "", "")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return q
}
```

- [ ] **Step 5: Build and run**

Run: `go build ./... && mise run test:integration -- ./internal/record/ ./internal/api/`
Expected: PASS (new read-policy test green; existing record + api integration tests still green with the added actor args).

- [ ] **Step 6: Commit**

```bash
git add internal/record/ internal/api/handlers.go
git commit -m "feat: enforce policy on record reads (actor-parameterized read methods)"
```

---

## Task 6: Enforce policy on schema lifecycle

**Files:**
- Modify: `internal/schema/registry.go` (evaluator field + `WithEvaluator`, gate `Register`/`Activate`/`Deprecate`, thread trace through `appendSchemaEvent`/`activateTx`)
- Test: `internal/schema/policy_test.go`

- [ ] **Step 1: Add the evaluator seam**

Add import `"github.com/substrate/substrate/internal/policy"`. Add `eval policy.Evaluator` to `Service`, plus:

```go
// WithEvaluator wires an optional policy evaluator. nil ⇒ no enforcement.
func (s *Service) WithEvaluator(e policy.Evaluator) *Service { s.eval = e; return s }

func (s *Service) authorize(ctx context.Context, req policy.Request) (policy.Decision, error) {
	if s.eval == nil {
		return policy.Decision{Effect: "allow", Reason: "no_evaluator"}, nil
	}
	return s.eval.Authorize(ctx, req)
}

func (s *Service) policyTrace(dec policy.Decision, op string) []byte {
	if s.eval == nil {
		return nil
	}
	return dec.TraceJSON(op)
}
```

- [ ] **Step 2: Thread trace through the event helpers**

Change `appendSchemaEvent` to accept a trace argument:

```go
func appendSchemaEvent(ctx context.Context, q *db.Queries, col, ws uuid.UUID, typ string, version int64, lifecycle, actor string, trace []byte) error {
	stateAfter, _ := json.Marshal(map[string]any{"version": version, "lifecycle": lifecycle})
	return q.AppendEvent(ctx, db.AppendEventParams{
		ID: uuid.New(), WorkspaceID: ws, CollectionID: col, RecordID: col,
		Type: typ, Revision: version, StateAfter: stateAfter,
		Actor: textOrNull(actor), IdempotencyKey: textOrNull(""), Trace: trace,
	})
}
```

Change `activateTx` to accept and forward a `trace []byte` param (its `appendSchemaEvent` call passes `trace`):

```go
func (s *Service) activateTx(ctx context.Context, q *db.Queries, col, ws uuid.UUID, prior pgtype.Int4, version int32, actor string, trace []byte) error {
	// ... unchanged body ...
	return appendSchemaEvent(ctx, q, col, ws, "schema_activated", int64(version), "active", actor, trace)
}
```

- [ ] **Step 3: Gate `Register`**

At the top of `Register` (before `compileSchema`):

```go
	dec, err := s.authorize(ctx, policy.Request{
		Workspace: cmd.Workspace, Actor: cmd.Actor, Collection: cmd.Collection,
		Target: cmd.Collection, Operation: policy.OpRegisterSchema,
	})
	if err != nil {
		return SchemaVersion{}, err
	}
	regTrace := s.policyTrace(dec, policy.OpRegisterSchema)
```

(Adjust the existing `err :=` after this to `err =` where the variable is reused.) Pass `regTrace` to the `appendSchemaEvent(...)` "schema_registered" call (new last arg) and to the `s.activateTx(...)` call inside `Register` (new last arg).

- [ ] **Step 4: Gate `Activate`**

At the top of `Activate` (before `store.WithTx`):

```go
	dec, aerr := s.authorize(ctx, policy.Request{
		Workspace: ws, Actor: actor, Collection: col, Target: col, Operation: policy.OpActivateSchema,
	})
	if aerr != nil {
		return aerr
	}
	actTrace := s.policyTrace(dec, policy.OpActivateSchema)
```

Pass `actTrace` to the `s.activateTx(...)` call inside `Activate`'s closure.

- [ ] **Step 5: Gate `Deprecate`**

`Deprecate`'s signature has no workspace/actor. Change it to
`func (s *Service) Deprecate(ctx context.Context, ws, col uuid.UUID, version int, actor string) error`.
At the top (before `store.WithTx`):

```go
	dec, aerr := s.authorize(ctx, policy.Request{
		Workspace: ws, Actor: actor, Collection: col, Target: col, Operation: policy.OpDeprecateSchema,
	})
	if aerr != nil {
		return aerr
	}
	depTrace := s.policyTrace(dec, policy.OpDeprecateSchema)
```

Pass `depTrace` to the `appendSchemaEvent(...)` "schema_deprecated" call. Update the `deprecate` HTTP handler in `internal/api/schema_handlers.go` to call
`sh.schemas.Deprecate(r.Context(), c.WorkspaceID, c.ID, ver, auth.ActorFrom(r.Context()))`.
Update any other `Deprecate(` call sites (grep `\.Deprecate(` under `internal/`) — pass `c.WorkspaceID`/`ws` and an actor (`"tester"` in tests).

- [ ] **Step 6: Write the failing test**

`internal/schema/policy_test.go`:

```go
//go:build integration

package schema_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/substrate/substrate/internal/apierr"
	"github.com/substrate/substrate/internal/db"
	"github.com/substrate/substrate/internal/policy"
	"github.com/substrate/substrate/internal/schema"
	"github.com/substrate/substrate/internal/store"
)

func TestRegisterDeniedByPolicy(t *testing.T) {
	pool := store.NewTestPool(t)
	ctx := context.Background()
	q := db.New(pool)
	ws, _ := q.CreateWorkspace(ctx, db.CreateWorkspaceParams{ID: uuid.New(), Name: "w", PolicyMode: "allow"})
	col, _ := q.CreateCollection(ctx, db.CreateCollectionParams{ID: uuid.New(), WorkspaceID: ws.ID, Name: "c", Level: "flexible"})
	if _, err := q.InsertPolicy(ctx, db.InsertPolicyParams{
		ID: uuid.New(), WorkspaceID: ws.ID, Actor: "*",
		CollectionID: pgtype.UUID{Bytes: col.ID, Valid: true}, Operation: policy.OpRegisterSchema, Effect: "deny",
	}); err != nil {
		t.Fatalf("rule: %v", err)
	}
	svc := schema.New(pool).WithEvaluator(policy.NewEngine(pool))

	_, err := svc.Register(ctx, schema.RegisterCmd{
		Workspace: ws.ID, Collection: col.ID, Actor: "alice",
		JSONSchema: map[string]any{"type": "object"},
	})
	var ae *apierr.Error
	if !errors.As(err, &ae) || ae.Code != apierr.Forbidden {
		t.Fatalf("want Forbidden, got %v", err)
	}
}
```

- [ ] **Step 7: Build and run**

Run: `go build ./... && mise run test:integration -- ./internal/schema/`
Expected: PASS (new test green; existing schema integration tests still green — they construct `schema.New`/`NewWithIndexer` without an evaluator, so lifecycle ops remain allowed, and `Deprecate` call sites are updated).

- [ ] **Step 8: Commit**

```bash
git add internal/schema/registry.go internal/api/schema_handlers.go internal/schema/policy_test.go
git commit -m "feat: enforce policy on schema lifecycle; stamp decision trace"
```

---

## Task 7: Policy rule CRUD API + default-mode flip

**Files:**
- Create: `internal/policy/service.go` (rule CRUD + mode facade)
- Create: `internal/api/policy_handlers.go`
- Modify: `internal/workspace/workspace.go` (`SetPolicyMode`)
- Modify: `internal/api/router.go` (`Deps.Policies`, routes), `internal/api/handlers.go` (`handlers.policies` field if needed)
- Modify: `internal/api/admin_*` wiring for the admin route
- Test: `internal/api/policy_api_test.go`

- [ ] **Step 1: Add `SetPolicyMode` to the workspace service**

In `internal/workspace/workspace.go`:

```go
// SetPolicyMode flips the workspace's default policy mode ("allow" | "deny").
func (s *Service) SetPolicyMode(ctx context.Context, ws uuid.UUID, mode string) error {
	if mode != "allow" && mode != "deny" {
		return apierr.New(apierr.BadRequest, "mode must be 'allow' or 'deny'")
	}
	if err := s.q.SetWorkspacePolicyMode(ctx, db.SetWorkspacePolicyModeParams{ID: ws, PolicyMode: mode}); err != nil {
		return fmt.Errorf("set policy mode: %w", err)
	}
	return nil
}
```

- [ ] **Step 2: Write `internal/policy/service.go`**

```go
package policy

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/substrate/substrate/internal/apierr"
	"github.com/substrate/substrate/internal/db"
)

// PolicyRule is one rule as returned by the API.
type PolicyRule struct {
	ID         uuid.UUID `json:"id"`
	Actor      string    `json:"actor"`
	Collection string    `json:"collection"` // collection name, or "*"
	Operation  string    `json:"operation"`
	Effect     string    `json:"effect"`
}

// CreateRuleCmd is the input for creating a rule. CollectionID nil ⇒ any collection.
type CreateRuleCmd struct {
	Workspace      uuid.UUID
	Actor          string
	Operation      string
	Effect         string
	CollectionID   *uuid.UUID
	CollectionName string // echoed back; "*" when CollectionID is nil
}

// Service is the rule-management facade (separate from the Engine evaluator).
type Service struct {
	pool *pgxpool.Pool
	q    *db.Queries
}

func NewService(pool *pgxpool.Pool) *Service { return &Service{pool: pool, q: db.New(pool)} }

// CreateRule validates and inserts a rule.
func (s *Service) CreateRule(ctx context.Context, cmd CreateRuleCmd) (PolicyRule, error) {
	actor := cmd.Actor
	if actor == "" {
		actor = "*"
	}
	op := cmd.Operation
	if op == "" {
		op = "*"
	}
	if op != "*" && !ValidOperations[op] {
		return PolicyRule{}, apierr.New(apierr.BadRequest, "unknown operation token")
	}
	if cmd.Effect != "allow" && cmd.Effect != "deny" {
		return PolicyRule{}, apierr.New(apierr.BadRequest, "effect must be 'allow' or 'deny'")
	}
	cid := pgtype.UUID{}
	colName := "*"
	if cmd.CollectionID != nil {
		cid = pgtype.UUID{Bytes: *cmd.CollectionID, Valid: true}
		colName = cmd.CollectionName
	}
	row, err := s.q.InsertPolicy(ctx, db.InsertPolicyParams{
		ID: uuid.New(), WorkspaceID: cmd.Workspace, Actor: actor,
		CollectionID: cid, Operation: op, Effect: cmd.Effect,
	})
	if err != nil {
		return PolicyRule{}, fmt.Errorf("insert policy: %w", err)
	}
	return PolicyRule{ID: row.ID, Actor: row.Actor, Collection: colName, Operation: row.Operation, Effect: row.Effect}, nil
}

// ListRules returns all rules for a workspace.
func (s *Service) ListRules(ctx context.Context, ws uuid.UUID) ([]PolicyRule, error) {
	rows, err := s.q.ListPolicies(ctx, ws)
	if err != nil {
		return nil, fmt.Errorf("list policies: %w", err)
	}
	out := make([]PolicyRule, 0, len(rows))
	for _, r := range rows {
		name := "*"
		if r.CollectionName.Valid {
			name = r.CollectionName.String
		}
		out = append(out, PolicyRule{ID: r.ID, Actor: r.Actor, Collection: name, Operation: r.Operation, Effect: r.Effect})
	}
	return out, nil
}

// DeleteRule removes a rule scoped to the workspace.
func (s *Service) DeleteRule(ctx context.Context, ws, id uuid.UUID) error {
	n, err := s.q.DeletePolicy(ctx, db.DeletePolicyParams{ID: id, WorkspaceID: ws})
	if err != nil {
		return fmt.Errorf("delete policy: %w", err)
	}
	if n == 0 {
		return apierr.New(apierr.NotFound, "policy not found")
	}
	return nil
}

// DefaultMode returns the workspace's current default policy mode.
func (s *Service) DefaultMode(ctx context.Context, ws uuid.UUID) (string, error) {
	mode, err := s.q.GetWorkspacePolicyMode(ctx, ws)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", apierr.New(apierr.NotFound, "workspace not found")
	}
	if err != nil {
		return "", fmt.Errorf("get mode: %w", err)
	}
	return mode, nil
}
```

- [ ] **Step 3: Write `internal/api/policy_handlers.go`**

```go
package api

import (
	"encoding/json"
	"net/http"

	"github.com/google/uuid"

	"github.com/substrate/substrate/internal/apierr"
	"github.com/substrate/substrate/internal/auth"
	"github.com/substrate/substrate/internal/httpx"
	"github.com/substrate/substrate/internal/policy"
)

type policyHandlers struct {
	h        *handlers
	policies *policy.Service
}

func (ph *policyHandlers) create(w http.ResponseWriter, r *http.Request) {
	ws := auth.WorkspaceFrom(r.Context())
	var body struct {
		Actor      string `json:"actor"`
		Collection string `json:"collection"`
		Operation  string `json:"operation"`
		Effect     string `json:"effect"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, apierr.New(apierr.BadRequest, "invalid json"))
		return
	}
	cmd := policy.CreateRuleCmd{Workspace: ws, Actor: body.Actor, Operation: body.Operation, Effect: body.Effect}
	if body.Collection != "" && body.Collection != "*" {
		c, err := ph.h.resolveCollection(r, body.Collection)
		if err != nil {
			writeErr(w, err)
			return
		}
		cmd.CollectionID = &c.ID
		cmd.CollectionName = c.Name
	}
	rule, err := ph.policies.CreateRule(r.Context(), cmd)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, rule)
}

func (ph *policyHandlers) list(w http.ResponseWriter, r *http.Request) {
	ws := auth.WorkspaceFrom(r.Context())
	rules, err := ph.policies.ListRules(r.Context(), ws)
	if err != nil {
		writeErr(w, err)
		return
	}
	mode, err := ph.policies.DefaultMode(r.Context(), ws)
	if err != nil {
		writeErr(w, err)
		return
	}
	if rules == nil {
		rules = []policy.PolicyRule{}
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"default_mode": mode, "rules": rules})
}

func (ph *policyHandlers) delete(w http.ResponseWriter, r *http.Request) {
	ws := auth.WorkspaceFrom(r.Context())
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, apierr.New(apierr.BadRequest, "invalid id"))
		return
	}
	if err := ph.policies.DeleteRule(r.Context(), ws, id); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// setPolicyMode is admin-token gated (wired on the admin handler).
func (a *adminHandlers) setPolicyMode(w http.ResponseWriter, r *http.Request) {
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
		Mode string `json:"mode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, apierr.New(apierr.BadRequest, "invalid json"))
		return
	}
	if err := a.workspaces.SetPolicyMode(r.Context(), wsID, body.Mode); err != nil {
		writeErr(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"workspace_id": wsID, "policy_mode": body.Mode})
}
```

- [ ] **Step 4: Wire routes in `router.go`**

Add `Policies *policy.Service` to `Deps` (import `internal/policy`). Inside `NewRouter`, after the schema routes:

```go
	ph := &policyHandlers{h: h, policies: d.Policies}
	api.HandleFunc("POST /v1/policies", ph.create)
	api.HandleFunc("GET /v1/policies", ph.list)
	api.HandleFunc("DELETE /v1/policies/{id}", ph.delete)
```

And alongside the existing admin routes:

```go
	mux.HandleFunc("PUT /admin/workspaces/{ws}/policy-mode", admin.setPolicyMode)
```

- [ ] **Step 5: Write the failing HTTP test + shared governance harness**

The existing `newTestServer` (in [internal/api/api_test.go](../../../internal/api/api_test.go)) does not wire `Policies`/`Audit`/an evaluator/admin token, and `do` hardcodes the actor. Define a fuller harness here that Tasks 8 and 9 reuse (same `package api`, so define `newGovServer`/`doAs` **once**, in this file only).

`internal/api/policy_api_test.go`:

```go
//go:build integration

package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/substrate/substrate/internal/audit"
	"github.com/substrate/substrate/internal/collection"
	"github.com/substrate/substrate/internal/policy"
	"github.com/substrate/substrate/internal/record"
	"github.com/substrate/substrate/internal/store"
	"github.com/substrate/substrate/internal/workspace"
)

// newGovServer builds a server with the policy engine, rule service, audit reader,
// and an admin token wired in. Returns the server, an API key, the admin token,
// the workspace id, and the pool.
func newGovServer(t *testing.T) (*httptest.Server, string, string, uuid.UUID, *pgxpool.Pool) {
	t.Helper()
	pool := store.NewTestPool(t)
	wsSvc := workspace.New(pool)
	w, err := wsSvc.CreateWorkspace(t.Context(), "acme")
	if err != nil {
		t.Fatalf("workspace: %v", err)
	}
	key, _, err := wsSvc.CreateAPIKey(t.Context(), w.ID, "test")
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	const adminToken = "admin-secret"
	engine := policy.NewEngine(pool)
	srv := httptest.NewServer(NewRouter(Deps{
		Workspaces:  wsSvc,
		Collections: collection.New(pool),
		Records:     record.New(pool, nil).WithEvaluator(engine),
		Policies:    policy.NewService(pool),
		Audit:       audit.New(pool),
		AdminToken:  adminToken,
	}))
	t.Cleanup(srv.Close)
	return srv, key, adminToken, w.ID, pool
}

// doAs issues a request with an explicit X-Substrate-Actor header.
func doAs(t *testing.T, method, url, key, actor string, body any) *http.Response {
	t.Helper()
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
		t.Fatalf("%s %s: %v", method, url, err)
	}
	return resp
}

func TestPolicyRuleCRUDAndEnforcement(t *testing.T) {
	srv, key, _, _, _ := newGovServer(t)

	resp := doAs(t, "POST", srv.URL+"/v1/collections", key, "agent-1", map[string]any{"name": "orders"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("collection = %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = doAs(t, "POST", srv.URL+"/v1/policies", key, "agent-1", map[string]any{
		"collection": "orders", "operation": "create", "effect": "deny",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("policy create = %d", resp.StatusCode)
	}
	var rule struct {
		ID         string `json:"id"`
		Collection string `json:"collection"`
		Effect     string `json:"effect"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&rule)
	resp.Body.Close()
	if rule.Collection != "orders" || rule.Effect != "deny" {
		t.Fatalf("rule = %+v", rule)
	}

	resp = doAs(t, "GET", srv.URL+"/v1/policies", key, "agent-1", nil)
	var listed struct {
		DefaultMode string           `json:"default_mode"`
		Rules       []map[string]any `json:"rules"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&listed)
	resp.Body.Close()
	if listed.DefaultMode != "allow" || len(listed.Rules) != 1 {
		t.Fatalf("listed = %+v", listed)
	}

	resp = doAs(t, "POST", srv.URL+"/v1/collections/orders/records", key, "agent-1", map[string]any{"data": map[string]any{"x": 1}})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("denied create = %d, want 403", resp.StatusCode)
	}
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&env)
	resp.Body.Close()
	if env.Error.Code != "policy_denied" {
		t.Fatalf("code = %q", env.Error.Code)
	}

	resp = doAs(t, "DELETE", srv.URL+"/v1/policies/"+rule.ID, key, "agent-1", nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete = %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = doAs(t, "POST", srv.URL+"/v1/collections/orders/records", key, "agent-1", map[string]any{"data": map[string]any{"x": 1}})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("allowed create = %d, want 201", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestSetPolicyModeAdmin(t *testing.T) {
	srv, key, adminToken, ws, _ := newGovServer(t)

	req, _ := http.NewRequest("PUT", srv.URL+"/admin/workspaces/"+ws.String()+"/policy-mode", bytes.NewBufferString(`{"mode":"deny"}`))
	req.Header.Set("X-Admin-Token", adminToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("mode flip = %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = doAs(t, "POST", srv.URL+"/v1/collections", key, "agent-1", map[string]any{"name": "orders"})
	resp.Body.Close()

	resp = doAs(t, "POST", srv.URL+"/v1/collections/orders/records", key, "agent-1", map[string]any{"data": map[string]any{}})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("default-deny create = %d, want 403", resp.StatusCode)
	}
	resp.Body.Close()

	resp = doAs(t, "GET", srv.URL+"/v1/policies", key, "agent-1", nil)
	var listed struct {
		DefaultMode string `json:"default_mode"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&listed)
	resp.Body.Close()
	if listed.DefaultMode != "deny" {
		t.Fatalf("default_mode = %q", listed.DefaultMode)
	}
}
```

- [ ] **Step 6: Build and run**

Run: `go build ./... && mise run test:integration -- -run 'TestPolicyRuleCRUDAndEnforcement|TestSetPolicyModeAdmin' ./internal/api/`
Expected: PASS

- [ ] **Step 7: Commit**

```bash
git add internal/policy/service.go internal/api/policy_handlers.go internal/api/router.go internal/workspace/workspace.go internal/api/policy_api_test.go
git commit -m "feat: policy rule CRUD API + admin default-mode flip"
```

---

## Task 8: Audit read endpoint

**Files:**
- Create: `internal/audit/audit.go` (filtered, keyset-paginated event reader; hand-built parameterized SQL)
- Create: `internal/api/audit_handlers.go`
- Modify: `internal/api/router.go` (`Deps.Audit`, route)
- Test: `internal/audit/audit_test.go`, `internal/api/audit_api_test.go`

- [ ] **Step 1: Write `internal/audit/audit.go`**

```go
// Package audit reads the workspace event stream with filters and keyset pagination.
package audit

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/substrate/substrate/internal/apierr"
)

const (
	defaultLimit = 50
	maxLimit     = 200
)

// Filter narrows the audit stream. Zero-valued fields are ignored.
type Filter struct {
	Collection *uuid.UUID
	Record     *uuid.UUID
	Actor      string
	Type       string
	Since      *time.Time
	Until      *time.Time
	Limit      int
	Cursor     string // opaque; wraps the last seen seq
}

// Entry is one audit row.
type Entry struct {
	Seq        int64           `json:"seq"`
	ID         uuid.UUID       `json:"id"`
	Type       string          `json:"type"`
	Collection uuid.UUID       `json:"collection_id"`
	Record     uuid.UUID       `json:"record_id"`
	Revision   int64           `json:"revision"`
	Actor      string          `json:"actor"`
	Trace      json.RawMessage `json:"trace,omitempty"`
	State      json.RawMessage `json:"state_after,omitempty"`
	CreatedAt  time.Time       `json:"created_at"`
}

// Service reads audit events.
type Service struct{ pool *pgxpool.Pool }

func New(pool *pgxpool.Pool) *Service { return &Service{pool: pool} }

func encodeCursor(seq int64) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(seq, 10)))
}

func decodeCursor(tok string) (int64, error) {
	raw, err := base64.RawURLEncoding.DecodeString(tok)
	if err != nil {
		return 0, apierr.New(apierr.BadRequest, "invalid cursor")
	}
	seq, err := strconv.ParseInt(string(raw), 10, 64)
	if err != nil {
		return 0, apierr.New(apierr.BadRequest, "invalid cursor")
	}
	return seq, nil
}

// List returns a page of audit entries (newest first) plus a next cursor ("" when last page).
func (s *Service) List(ctx context.Context, ws uuid.UUID, f Filter) ([]Entry, string, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = defaultLimit
	}
	if limit > maxLimit {
		limit = maxLimit
	}

	sql := `SELECT seq, id, type, collection_id, record_id, revision, actor, trace, state_after, created_at
FROM events WHERE workspace_id = $1`
	args := []any{ws}
	add := func(cond string, v any) {
		args = append(args, v)
		sql += fmt.Sprintf(" AND %s $%d", cond, len(args))
	}
	if f.Collection != nil {
		add("collection_id =", *f.Collection)
	}
	if f.Record != nil {
		add("record_id =", *f.Record)
	}
	if f.Actor != "" {
		add("actor =", f.Actor)
	}
	if f.Type != "" {
		add("type =", f.Type)
	}
	if f.Since != nil {
		add("created_at >=", *f.Since)
	}
	if f.Until != nil {
		add("created_at <=", *f.Until)
	}
	if f.Cursor != "" {
		seq, err := decodeCursor(f.Cursor)
		if err != nil {
			return nil, "", err
		}
		add("seq <", seq)
	}
	sql += fmt.Sprintf(" ORDER BY seq DESC LIMIT %d", limit+1)

	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, "", fmt.Errorf("audit query: %w", err)
	}
	defer rows.Close()

	var out []Entry
	for rows.Next() {
		var (
			e     Entry
			actor *string
			trace []byte
			state []byte
		)
		if err := rows.Scan(&e.Seq, &e.ID, &e.Type, &e.Collection, &e.Record, &e.Revision, &actor, &trace, &state, &e.CreatedAt); err != nil {
			return nil, "", fmt.Errorf("scan audit: %w", err)
		}
		if actor != nil {
			e.Actor = *actor
		}
		e.Trace = json.RawMessage(trace)
		e.State = json.RawMessage(state)
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("iterate audit: %w", err)
	}

	next := ""
	if len(out) > limit {
		last := out[limit-1]
		out = out[:limit]
		next = encodeCursor(last.Seq)
	}
	return out, next, nil
}
```

- [ ] **Step 2: Write the failing integration test**

`internal/audit/audit_test.go`:

```go
//go:build integration

package audit_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/substrate/substrate/internal/audit"
	"github.com/substrate/substrate/internal/db"
	"github.com/substrate/substrate/internal/store"
)

func TestAuditListAndPaginate(t *testing.T) {
	pool := store.NewTestPool(t)
	ctx := context.Background()
	q := db.New(pool)
	ws, _ := q.CreateWorkspace(ctx, db.CreateWorkspaceParams{ID: uuid.New(), Name: "w", PolicyMode: "allow"})
	col, _ := q.CreateCollection(ctx, db.CreateCollectionParams{ID: uuid.New(), WorkspaceID: ws.ID, Name: "c", Level: "flexible"})

	rid := uuid.New()
	for i := 0; i < 3; i++ {
		if err := q.AppendEvent(ctx, db.AppendEventParams{
			ID: uuid.New(), WorkspaceID: ws.ID, CollectionID: col.ID, RecordID: rid,
			Type: "create", Revision: int64(i + 1),
		}); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	svc := audit.New(pool)

	page1, next, err := svc.List(ctx, ws.ID, audit.Filter{Limit: 2})
	if err != nil || len(page1) != 2 || next == "" {
		t.Fatalf("page1 len=%d next=%q err=%v", len(page1), next, err)
	}
	page2, next2, err := svc.List(ctx, ws.ID, audit.Filter{Limit: 2, Cursor: next})
	if err != nil || len(page2) != 1 || next2 != "" {
		t.Fatalf("page2 len=%d next=%q err=%v", len(page2), next2, err)
	}
	// Filter by type that doesn't exist -> empty.
	none, _, err := svc.List(ctx, ws.ID, audit.Filter{Type: "nope"})
	if err != nil || len(none) != 0 {
		t.Fatalf("filtered len=%d err=%v", len(none), err)
	}
}
```

- [ ] **Step 3: Write `internal/api/audit_handlers.go`**

```go
package api

import (
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/substrate/substrate/internal/apierr"
	"github.com/substrate/substrate/internal/audit"
	"github.com/substrate/substrate/internal/auth"
	"github.com/substrate/substrate/internal/httpx"
)

type auditHandlers struct {
	h     *handlers
	audit *audit.Service
}

func (ah *auditHandlers) list(w http.ResponseWriter, r *http.Request) {
	ws := auth.WorkspaceFrom(r.Context())
	v := r.URL.Query()
	var f audit.Filter
	if name := v.Get("collection"); name != "" {
		c, err := ah.h.resolveCollection(r, name)
		if err != nil {
			writeErr(w, err)
			return
		}
		f.Collection = &c.ID
	}
	if rec := v.Get("record"); rec != "" {
		id, err := uuid.Parse(rec)
		if err != nil {
			writeErr(w, apierr.New(apierr.BadRequest, "invalid record id"))
			return
		}
		f.Record = &id
	}
	f.Actor = v.Get("actor")
	f.Type = v.Get("type")
	if s := v.Get("since"); s != "" {
		ts, err := time.Parse(time.RFC3339, s)
		if err != nil {
			writeErr(w, apierr.New(apierr.BadRequest, "since must be RFC3339"))
			return
		}
		f.Since = &ts
	}
	if u := v.Get("until"); u != "" {
		ts, err := time.Parse(time.RFC3339, u)
		if err != nil {
			writeErr(w, apierr.New(apierr.BadRequest, "until must be RFC3339"))
			return
		}
		f.Until = &ts
	}
	if l := v.Get("limit"); l != "" {
		n, err := strconv.Atoi(l)
		if err != nil {
			writeErr(w, apierr.New(apierr.BadRequest, "limit must be an integer"))
			return
		}
		f.Limit = n
	}
	f.Cursor = v.Get("cursor")

	items, next, err := ah.audit.List(r.Context(), ws, f)
	if err != nil {
		writeErr(w, err)
		return
	}
	if items == nil {
		items = []audit.Entry{}
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"items": items, "next_cursor": next})
}
```

- [ ] **Step 4: Wire the route in `router.go`**

Add `Audit *audit.Service` to `Deps` (import `internal/audit`). After the policy routes:

```go
	aud := &auditHandlers{h: h, audit: d.Audit}
	api.HandleFunc("GET /v1/audit", aud.list)
```

- [ ] **Step 5: Write the API test**

`internal/api/audit_api_test.go` (reuses `newGovServer`/`doAs` from Task 7's file — do not redefine them):

```go
//go:build integration

package api

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestAuditOverHTTP(t *testing.T) {
	srv, key, _, _, _ := newGovServer(t)

	resp := doAs(t, "POST", srv.URL+"/v1/collections", key, "agent-1", map[string]any{"name": "orders"})
	resp.Body.Close()
	resp = doAs(t, "POST", srv.URL+"/v1/collections/orders/records", key, "agent-1", map[string]any{"data": map[string]any{"x": 1}})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = doAs(t, "GET", srv.URL+"/v1/audit?collection=orders", key, "agent-1", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("audit = %d", resp.StatusCode)
	}
	var page struct {
		Items []struct {
			Type  string          `json:"type"`
			Trace json.RawMessage `json:"trace"`
		} `json:"items"`
		NextCursor string `json:"next_cursor"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&page)
	resp.Body.Close()
	found := false
	for _, it := range page.Items {
		if it.Type == "create" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a create event in audit, got %+v", page.Items)
	}

	resp = doAs(t, "GET", srv.URL+"/v1/audit?type=policy_denied", key, "agent-1", nil)
	var empty struct {
		Items []json.RawMessage `json:"items"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&empty)
	resp.Body.Close()
	if len(empty.Items) != 0 {
		t.Fatalf("expected no denials, got %d", len(empty.Items))
	}
}
```

- [ ] **Step 6: Build and run**

Run: `go build ./... && mise run test:integration -- ./internal/audit/ ./internal/api/`
Expected: PASS

- [ ] **Step 7: Commit**

```bash
git add internal/audit/ internal/api/audit_handlers.go internal/api/router.go internal/api/audit_api_test.go
git commit -m "feat: GET /v1/audit filtered, keyset-paginated event stream"
```

---

## Task 9: Production wiring + end-to-end test

**Files:**
- Modify: `cmd/substrate/main.go`
- Test: `internal/api/governance_e2e_test.go`

- [ ] **Step 1: Wire the engine + services in `main.go`**

Add imports `internal/audit` and `internal/policy`. Replace the wiring block:

```go
	schemaReg := schema.NewWithIndexer(pool, query.NewIndexer(pool))
	engine := policy.NewEngine(pool)
	schemaReg.WithEvaluator(engine)

	router := api.NewRouter(api.Deps{
		Workspaces:  workspace.New(pool),
		Collections: collection.New(pool),
		Records:     record.New(pool, schema.NewValidator(schemaReg)).WithEvaluator(engine),
		Schemas:     schemaReg,
		Policies:    policy.NewService(pool),
		Audit:       audit.New(pool),
		AdminToken:  cfg.AdminToken,
	})
```

- [ ] **Step 2: Write the end-to-end test**

`internal/api/governance_e2e_test.go` (reuses `newGovServer`/`doAs` from Task 7's file):

```go
//go:build integration

package api

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestGovernanceEndToEnd(t *testing.T) {
	srv, key, _, _, _ := newGovServer(t)

	resp := doAs(t, "POST", srv.URL+"/v1/collections", key, "agent-1", map[string]any{"name": "orders"})
	resp.Body.Close()

	// Deny mallory's create on orders.
	resp = doAs(t, "POST", srv.URL+"/v1/policies", key, "agent-1", map[string]any{
		"actor": "mallory", "collection": "orders", "operation": "create", "effect": "deny",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("rule = %d", resp.StatusCode)
	}
	resp.Body.Close()

	// mallory denied.
	resp = doAs(t, "POST", srv.URL+"/v1/collections/orders/records", key, "mallory", map[string]any{"data": map[string]any{}})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("mallory = %d, want 403", resp.StatusCode)
	}
	resp.Body.Close()

	// alice allowed (rule is actor-specific to mallory; alice falls through to default_allow).
	resp = doAs(t, "POST", srv.URL+"/v1/collections/orders/records", key, "alice", map[string]any{"data": map[string]any{}})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("alice = %d, want 201", resp.StatusCode)
	}
	resp.Body.Close()

	// Exactly one policy_denied, for mallory.
	resp = doAs(t, "GET", srv.URL+"/v1/audit?type=policy_denied", key, "agent-1", nil)
	var denied struct {
		Items []struct {
			Actor string `json:"actor"`
		} `json:"items"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&denied)
	resp.Body.Close()
	if len(denied.Items) != 1 || denied.Items[0].Actor != "mallory" {
		t.Fatalf("denials = %+v", denied.Items)
	}

	// alice's create event carries an allow trace.
	resp = doAs(t, "GET", srv.URL+"/v1/audit?actor=alice", key, "agent-1", nil)
	var alicePage struct {
		Items []struct {
			Type  string         `json:"type"`
			Trace map[string]any `json:"trace"`
		} `json:"items"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&alicePage)
	resp.Body.Close()
	var createTrace map[string]any
	for _, it := range alicePage.Items {
		if it.Type == "create" {
			createTrace = it.Trace
		}
	}
	if createTrace == nil || createTrace["effect"] != "allow" {
		t.Fatalf("alice create trace = %+v", createTrace)
	}
}
```

- [ ] **Step 3: Build, vet, run everything**

Run: `go build ./... && go vet ./... && mise run test && mise run test:integration`
Expected: all PASS.

- [ ] **Step 4: Commit**

```bash
git add cmd/substrate/main.go internal/api/governance_e2e_test.go
git commit -m "feat: wire policy engine + audit into the server; governance e2e test"
```

---

## Final review (controller, after all tasks)

- Dispatch a final code-review subagent over the whole branch diff (`git diff main...HEAD`): check the precedence/deny-overrides logic, the nil-evaluator backward-compat path, that every gated operation writes a denial event on deny and a trace on allow, parameterization of the hand-built audit SQL (no interpolated user values), and that existing Plan 1–3 tests still pass.
- Then use **superpowers:finishing-a-development-branch**.

---

## Self-review notes (spec coverage)

- Spec §2 decisions 1–7 → Tasks 1–9 (gated ops: Tasks 4/5/6; actor matching + precedence: Task 2; denial+trace: Tasks 3/4/6; audit: Task 8; evaluator placement/`nil`-skip: Tasks 4/5/6; collection_id ref: Task 1).
- Spec §4 rule model → Task 1 (migration) + Task 7 (validation of effect/operation tokens).
- Spec §5 evaluation → Task 2 (`Select`/`DefaultDecision`) + Task 3 (`Evaluate`/default-mode load).
- Spec §6 enforcement & call-site table → Tasks 4–6 (every listed method gated; reads gain `actor`).
- Spec §7 API → Task 7 (rules CRUD + admin mode) + Task 8 (`GET /v1/audit`).
- Spec §8 errors → Forbidden (Task 3), bad-body 400s (Task 7), audit param 400s (Task 8).
- Spec §10 testing → unit (Task 2), integration per service (Tasks 3–8), e2e HTTP (Task 9).
