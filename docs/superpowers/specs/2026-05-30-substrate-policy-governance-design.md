# Substrate Plan 4 — Policy & Governance Plane — Design Spec

- **Date:** 2026-05-30
- **Status:** Approved (design)
- **Scope:** v0 subsystem 4 (policy/governance) — declarative allow/deny plane evaluated in-process, decisions recorded on the audit trail, plus a `GET /v1/audit` read endpoint
- **Builds on:** [v0 core design](2026-05-30-substrate-v0-core-design.md) + Plan 1 (foundation) + Plan 2 (schema registry) + Plan 3 (query & indexing)
- **Language/runtime:** Go 1.26

## 1. Purpose & scope

Plans 1–3 shipped the transactional event-backed record core, the versioned schema
registry, and list/query with indexing. The remaining v0 subsystem is the **policy /
governance plane**: a thin declarative allow/deny layer evaluated in-process before each
operation, behind a swappable `PolicyEvaluator` interface, with every decision recorded on
the existing event/audit timeline. This plan also adds the `GET /v1/audit` read endpoint so
those decisions (and the whole event stream) are observable.

### In scope
- A new `internal/policy` package: `Evaluator` interface + concrete rule `Engine`
  (rule loading, precedence evaluation, default-mode fallback, denial-event writing).
- A `policies` table + rule CRUD API (`POST`/`GET`/`DELETE /v1/policies`).
- Enforcement wired into **record reads + writes** (`get`/`list`/`history` and
  `create`/`update`/`delete`) and **schema lifecycle** (`register`/`activate`/`deprecate`),
  injected like the existing optional `Validator` seam.
- Decisions on the audit trail: `policy_denied` events on deny; the matched decision stamped
  into the (currently unused) `events.trace` column on allowed writes and schema events.
- `GET /v1/audit` — workspace event stream with filters + keyset pagination.
- `PUT /admin/workspaces/{ws}/policy-mode` — flip the per-workspace default mode
  (`allow`↔`deny`) so default-deny is usable.

### Out of scope (later plans / documented limitations)
- **Roles / role assignment.** Rules match on the actor string only (with `*`); no
  actor→role mapping exists yet. A `role` dimension is a later addition.
- **External policy engines** (OPA, etc.) — the `Evaluator` interface is the seam; only the
  in-process engine is implemented.
- **Rule caching.** Each gated operation runs one `policies` SELECT (see §9 perf caveat);
  caching/invalidation is a future enhancement.
- **Gating the audit endpoint by collection policy.** `GET /v1/audit` spans collections, so
  it is workspace-API-key-scoped but not collection-policy-gated in v0.
- Time-windowed / scheduled rules, quota/rate policies, field-level policies.

## 2. Key decisions (locked during brainstorming)

| # | Decision | Choice |
|---|----------|--------|
| 1 | Gated operations | **Reads, writes, and schema lifecycle.** `create/read/update/delete` on records and `register_schema/activate_schema/deprecate_schema`. Broadest governance coverage. |
| 2 | Principal matching | **Actor string + `*` wildcard.** No role system exists yet; rules match the `X-Substrate-Actor` value exactly or `*`. |
| 3 | Decision recording | **Denial events + trace on allowed writes.** Deny → a `policy_denied` event (403). Allow → the matched rule/effect stamped into the resulting event's `trace` JSONB. |
| 4 | Audit read | **Include `GET /v1/audit`** — workspace event stream so denials/decisions are observable. |
| 5 | Evaluator placement | **Optional dependency injected into the services** (mirrors the `Validator` seam). `nil` ⇒ no enforcement (backward-compatible). The `policy.Engine` owns denial-event writing so the logic is not duplicated across the six gated operations. |
| 6 | Rule collection ref | **Store `collection_id` (NULL = any).** The API resolves a collection name → id at rule-create time; services already hold ids, so no name threading is needed. `ON DELETE CASCADE` removes a collection's rules with it. |
| 7 | Precedence | **Specificity = count of concrete (non-`*`) dimensions** over `(actor, collection, operation)`; highest wins; **deny-overrides-allow** on ties. No match ⇒ workspace default mode. Fully deterministic. |

## 3. Components & file structure

```
internal/policy/engine.go        # Evaluator interface, Request/Decision types, Engine (load+evaluate+default)
internal/policy/precedence.go    # pure rule-selection: filter applicable, rank specificity, deny-overrides
internal/policy/denial.go        # append a policy_denied event (Engine method, own append)
internal/migrations/00003_policies.sql   # policies table
internal/queries/policies.sql    # sqlc: insert/list/delete rules, set workspace policy_mode
internal/queries/events.sql      # +ListAuditEvents (filtered, keyset on seq)
# modified:
internal/record/record.go        # optional policy.Evaluator; Authorize before each op; stamp trace on writes
internal/schema/registry.go      # optional policy.Evaluator; Authorize before lifecycle ops; stamp trace
internal/policy/policy_service.go# PolicyService: rule CRUD + default-mode (thin facade over db.Queries)
internal/audit/audit.go          # AuditService: ListAuditEvents -> typed results + cursor
internal/api/policy_handlers.go  # POST/GET/DELETE /v1/policies; PUT default-mode (admin)
internal/api/audit_handlers.go   # GET /v1/audit
internal/api/router.go           # register new routes
internal/api/handlers.go         # pass actor+target through read handlers if needed
cmd/substrate/main.go            # construct policy.Engine, wire into record + schema services
```

Each unit keeps one responsibility: `precedence.go` is pure and table-tested; `engine.go`
loads rules and produces a `Decision`; `denial.go` writes the audit event; the record/schema
services call one `Authorize` method and stamp the returned decision. No import cycle —
`policy` imports only `db`/`apierr`/`store`; `record` and `schema` import `policy`.

## 4. Rule model

`policies` table (migration `00003_policies.sql`):

```sql
CREATE TABLE policies (
    id            uuid PRIMARY KEY,
    workspace_id  uuid NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    actor         text NOT NULL DEFAULT '*',
    collection_id uuid REFERENCES collections(id) ON DELETE CASCADE,  -- NULL = any collection
    operation     text NOT NULL DEFAULT '*',
    effect        text NOT NULL,                                       -- 'allow' | 'deny'
    created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX policies_ws_idx ON policies(workspace_id);
```

- **Operation tokens** (validated on rule create; `*` = any):
  `create`, `read`, `update`, `delete`,
  `register_schema`, `activate_schema`, `deprecate_schema`.
- **effect** ∈ `{allow, deny}` (validated; anything else → `400`).
- **actor**: exact string or `*`.
- **collection**: the create API accepts a collection *name* (resolved to `collection_id`)
  or `*`/omitted (stored as NULL = any collection). Listing a rule echoes the collection
  name (resolved from id) for readability; NULL renders as `*`.

The per-workspace default lives in the existing `workspaces.policy_mode` column
(`allow` default, set at workspace creation; flippable — see §7).

## 5. Evaluation semantics

`Request{Workspace, Actor string, Collection uuid.UUID, Target uuid.UUID, Operation string}`
→ `Decision{Effect string, MatchedRule *uuid.UUID, Reason string}`.

1. **Load** all rules for `Request.Workspace` where
   `collection_id = Request.Collection OR collection_id IS NULL`.
2. **Filter to applicable rules:** `actor = Request.Actor OR actor = '*'`, and
   `operation = Request.Operation OR operation = '*'`.
3. **Rank by specificity** = number of concrete (non-`*`/non-NULL) dimensions among
   `(actor, collection, operation)`, range 0–3. Keep the highest-specificity tier.
4. **Within the top tier, deny-overrides-allow**: if any rule in the tier is `deny`, the
   decision is `deny` (Reason `rule`, MatchedRule = that rule's id); else `allow`.
5. **No applicable rule** → fall back to `workspaces.policy_mode`: `allow` ⇒
   `Decision{allow, Reason:"default_allow"}`; `deny` ⇒ `Decision{deny, Reason:"default_deny"}`
   (MatchedRule nil).

This honours both locked principles: a more-specific `allow` overrides a broad `deny`
(most-specific-wins), and equally-specific conflicts resolve to `deny` (deny-overrides).

`nil` evaluator (not wired) ⇒ services skip policy entirely: allow, no events. Existing
tests that construct services without an evaluator are unaffected.

## 6. Enforcement & the audit trail

`Engine.Authorize(ctx, Request) (Decision, error)`:
- Computes the `Decision` (§5).
- **On deny:** appends a `policy_denied` event (own append, outside any record txn) and
  returns `(Decision, *apierr.Error{Forbidden})`. The caller propagates the error → `403`.
- **On allow:** returns `(Decision, nil)`. The caller proceeds and, for operations that write
  an event, stamps the decision into that event's `trace`.

**`policy_denied` event shape:** `type='policy_denied'`, `workspace_id`,
`collection_id = Request.Collection`, `record_id = Request.Target` (or `uuid.Nil`),
`revision = 0`, `state_after = NULL`, `actor = Request.Actor`,
`trace = {"effect":"deny","reason":<reason>,"operation":<op>,"matched_rule":<id|null>}`.

**Trace on allow.** The events table's `trace` column (added in Plan 1, unused until now)
receives `{"effect":"allow","reason":<reason>,"operation":<op>,"matched_rule":<id|null>}`:
- Record writes — `appendEvent` gains a `Trace []byte` field threaded from the `Decision`.
- Schema lifecycle — `appendSchemaEvent` gains a `trace` parameter.
- Reads (`get`/`list`/`history`) write **no** event on allow (they are read-only); only a
  denial produces an event.

**Call sites** (each calls `Authorize` before doing its work):

| Service method | Operation | Target |
|----------------|-----------|--------|
| `record.Create` | `create` | `uuid.Nil` (no id yet) |
| `record.Get` / `GetAsOf` | `read` | record id |
| `record.List` | `read` | `uuid.Nil` |
| `record.History` | `read` | record id |
| `record.Update` | `update` | record id |
| `record.Delete` | `delete` | record id |
| `record.Revert` | `update` | record id |
| `schema.Register` | `register_schema` | collection id |
| `schema.Activate` | `activate_schema` | collection id |
| `schema.Deprecate` | `deprecate_schema` | collection id |

Read methods (`Get`, `List`, `History`, `GetAsOf`) gain an `actor string` parameter so the
handler can pass `auth.ActorFrom(ctx)`; writes already carry the actor on their command
structs.

## 7. API surface

All `/v1/*` routes are workspace-API-key scoped (existing `auth.Middleware`).

### Policy rules
- `POST /v1/policies` — body `{actor?, collection?, operation?, effect}`. `actor`/`operation`
  default to `*`; `collection` omitted/`"*"` ⇒ any-collection (NULL). Validates effect and
  operation token; resolves a named collection to id (404 if unknown). Returns the rule
  (collection echoed by name). `201`.
- `GET /v1/policies` — `{ "default_mode": "allow"|"deny", "rules": [ … ] }` for the workspace.
- `DELETE /v1/policies/{id}` — remove a rule (404 if not in this workspace). `204`.

### Default mode (admin)
- `PUT /admin/workspaces/{ws}/policy-mode` — admin-token gated (same guard as workspace/key
  creation). Body `{ "mode": "allow"|"deny" }`. Updates `workspaces.policy_mode`. `200`.

### Audit
- `GET /v1/audit` — workspace event stream, newest first. Query params (all optional):
  `collection` (name → id), `record` (uuid), `actor`, `type`, `since`/`until` (RFC3339),
  `limit` (default 50, max 200), `cursor` (opaque `before_seq`). Response
  `{ "items": [ {seq, id, type, collection_id, record_id, revision, actor, trace, state_after, created_at} ], "next_cursor": "<opaque|absent>" }`.
  Keyset paginated on `seq` descending (fetch `limit+1` to detect the next page); the cursor
  wraps the last returned `seq`.

## 8. Error handling

- Policy deny → `403` with code `policy_denied` (existing `apierr.Forbidden`), plus the
  `policy_denied` audit event. Details may name the matched rule id.
- Malformed rule body (bad effect, unknown operation token, bad collection) → `400`.
- Unknown collection name on rule create / audit filter → `404`.
- Bad admin token on default-mode flip → `401` (existing admin guard).
- Malformed `audit` params (bad uuid, bad timestamp, bad limit/cursor) → `400`.
- Infra failure loading rules or writing a denial event → `500`; the gated operation does
  not proceed.
- Evaluation order in the write pipeline is unchanged from the v0 spec: **policy first**,
  then idempotency/validation/revision/txn. A denied write produces no record/event beyond
  the denial event.

## 9. Performance & operational notes

- **One `policies` SELECT per gated operation**, including hot read paths (`get`/`list`).
  Acceptable for v0; the `Evaluator` interface is the seam for a future cached engine.
  Documented caveat, not a silent cost.
- Denial events are appended outside the (denied) operation's transaction — a denied write
  opens no record transaction.
- `policies_ws_idx` keeps rule loads to a small per-workspace scan.

## 10. Testing (TDD)

- **Unit (no DB):**
  - `precedence` table tests: specificity ranking across all tiers; deny-overrides on ties;
    most-specific allow beats broad deny; actor/operation/collection wildcard matching;
    empty rule set → default mode (both allow and deny).
  - Decision shape (effect/reason/matched_rule) for representative rule sets.
- **Integration (testcontainers, shared container):**
  - Rule CRUD round-trip; collection name↔id resolution; `*`/NULL collection.
  - Default-allow workspace: ungoverned op succeeds; add a deny rule → `403` + `policy_denied`
    event present in the timeline.
  - Default-deny workspace (flipped via admin endpoint): op denied until a matching `allow`
    rule is added.
  - Each gated operation (`create/read/update/delete/register/activate/deprecate`) denied and
    allowed; verify `403`+event on deny and `trace` stamped on allowed writes/schema events.
  - `nil` evaluator path: services behave exactly as today (no events, all allowed).
  - `GET /v1/audit`: seed mixed events (writes, schema lifecycle, denials); filter by
    collection/record/actor/type/time; paginate with `cursor` to exhaustion.
- **End-to-end HTTP (integration-tagged):** create collection → add a deny rule for an actor →
  that actor's write returns `403` and appears in `GET /v1/audit` as `policy_denied`; a
  different actor's write succeeds and its audit entry carries an `allow` trace.
- Reuse the existing `store.NewTestPool` harness; `go test` (unit) + `go test -tags=integration`.
