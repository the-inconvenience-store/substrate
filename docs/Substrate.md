---
type: project
kind: saas
title: "Substrate"
aliases:
  - "Agent State Layer"
created: 2026-05-30
updated: 2026-05-30
tags:
  - project
  - saas
  - agents
  - infrastructure
  - state
  - backend
status: idea
started: null
url: null
related:
  - "[[index]]"
  - "[[Projects]]"
  - "[[Agent Memory]]"
  - "[[Hermes Agent]]"
  - "[[Runtime Governance]]"
product: "State object store for agent-powered apps"
icp: "Agent products that need durable state objects without building a custom object backend per use case"
pricing: "Usage-based infrastructure pricing, then platform and hosted tiers"
key_metrics:
  - "Active workspaces"
  - "State objects created per workspace"
  - "Retention of active developer teams"
roadmap:
  - "v0: event-backed state object store with schemas, records, and auditability"
  - "v1: SDKs and hosted backend for mobile and web agent products"
  - "v2: typed object packs, workflow triggers, and ecosystem integrations"
open_risks:
  - "Could collapse into a thin abstraction over an existing document database"
  - "Schema flexibility versus strong developer ergonomics is a hard trade-off"
  - "Too horizontal without a clear wedge could recreate the platform-trap problem"
sources:
  - "[[Source - NEES Core Engine Governance Runtime]]"
---

# Substrate

> [!note] Working framing
> Shared substrate behind agent apps. Not a custom backend for each use case, but one generic system for durable state objects.

**One-liner**: Substrate is an event-backed state object store for agent-powered products that need durable, queryable objects without building a bespoke backend for every workflow.

## Problem

Agent products keep running into the same wall. Chat is easy to start with, but weak as the long-term source of truth.

- Conversation history is useful as context, but bad as the canonical store for live app state.
- Building a custom backend for every agent use case does not scale. Trips, budgets, plans, projects, checklists, journals, research packs, and shopping lists should not each require a separate product stack.

The missing layer is a general state substrate for agent-created objects.

## Product

### Core primitives

The core primitive should be the **state object store**.

- **State objects**: typed or semi-typed objects such as `trip`, `budget`, `plan`, `shopping_list`, `project`, or `journal_entry`.
- **Schemas**: versioned definitions that describe what a valid object looks like.
- **Records**: current materialised object state.
- **Events**: append-only mutations written underneath the record layer.
- **Projections and snapshots**: read-optimised views that make the system feel like a normal database.

### Three levels of structure

The three levels of structure should work as an **upgrade path for objects**, not as three separate products.

1. **Flexible**: loose JSON-like objects with minimal schema and light metadata.
2. **Typed**: versioned object types with validation, indexes, and stronger query support.
3. **Promoted**: high-value object types that get stronger tooling, projections, operators, and integrations once demand is proven.

This lets the system stay general at the storage layer while still supporting richer object models for high-frequency patterns later.

#### What each level means

**Flexible**

- Good for early exploration, one-off workflows, and unknown object shapes.
- Agents can create records without waiting for a fully defined schema.
- The system still provides IDs, timestamps, permissions, audit events, and basic filtering.

**Typed**

- Used once an object shape starts repeating enough to deserve a real schema.
- Fields, validation rules, required attributes, and indexes become explicit.
- Records are still document-shaped underneath, but the system now understands what fields matter operationally.

**Promoted**

- Used for strategically important object types that deserve first-class treatment.
- The system can provide richer projections, better operators, workflow hooks, and stronger ergonomics.
- These objects stop feeling like generic documents and start feeling like native primitives.

#### How it works in practice

The normal path should be:

1. An agent starts by creating loose objects in the flexible layer.
2. Once a shape repeats, a developer or system promotes it into a typed schema.
3. If that type becomes especially important, it gets promoted again into a first-class object type with stronger system support.

That gives Substrate a natural progression: **explore, stabilise, specialise**.

#### Example progression

For a trip-planning product:

1. **Flexible**: the agent creates a loose object like `{ destination: "Tokyo", dates: "October", notes: [...] }`.
2. **Typed**: that object becomes a `trip` schema with fields like travellers, dates, status, bookings, and budget.
3. **Promoted**: `trip` gains better projections, timeline queries, booking-related hooks, and stronger operators because it is now a high-value recurring type.

The key idea is to avoid forcing too much structure too early, while still letting successful shapes harden over time.

#### How promotion happens with low developer effort

The best model is probably **trigger-driven promotion with optional background synthesis**, rather than a vague dreaming phase as the main mechanism.

The system should work like this:

1. deterministic triggers identify candidate promotions,
2. a background schema synthesiser proposes the next schema or object upgrade,
3. safe rollout rules apply the change gradually,
4. the system keeps learning from real usage after promotion.

##### Flexible to Typed triggers

Substrate should watch for signals like:

- the same shape appearing repeatedly,
- the same fields showing up across many records,
- the same fields being queried often,
- repeated correction of missing or malformed fields,
- stable field meanings over time,
- repeated user or agent intent like "make this a trip", "make this a project", or "treat these like tasks".

Once those signals are strong enough, the system can cluster similar flexible records, draft a candidate schema, test it against existing data, and either auto-apply it or ask for very lightweight approval.

##### Typed to Promoted triggers

Promotion into a first-class object type should happen when a typed schema starts behaving like strategic infrastructure, not just a repeated shape.

Useful signals include:

- high record volume,
- repeated need for special operators,
- repeated creation of the same projections or derived views,
- recurring workflow hooks,
- repeated integrations attached to the same type,
- clear product importance beyond raw frequency.

At that point, Substrate can propose stronger capabilities around that type, such as better projections, lifecycle states, custom operators, or specialised workflow hooks.

##### Promotion scoring

This is easier to operate if promotion is based on **scores**, not vague intuition.

Useful scoring inputs:

- frequency,
- shape stability,
- query concentration,
- correction rate,
- workflow reuse,
- integration reuse,
- business importance.

Then the policy becomes simple:

- low score: stay flexible,
- medium score: propose typed schema,
- high score: propose first-class promotion.

##### Where dreaming fits

A dreaming phase can still be useful, but mainly as a **background optimiser**:

- nightly clustering of flexible records,
- schema refinement suggestions,
- index or projection suggestions,
- candidate merges between near-duplicate object types.

It should not be the main mechanism that arbitrarily redesigns important schemas. The core path should stay deterministic enough that the system feels reliable rather than magical.

### Product form

The right shape is **server-first, library-enabled**.

- **Server**: the main product. It owns the API, auth, sync, policy enforcement, traces, storage orchestration, and the durable source of truth.
- **Library**: SDKs plus a self-host package for developers who want to run the same runtime on their own infrastructure.
- **Embedded mode**: possible later for offline or single-user cases, but not the main identity.

This gives the project a clean split between an OSS self-hosted runtime and a hosted SaaS control plane.

### OSS and SaaS split

The clean model is open core runtime, hosted control plane.

| Layer            | OSS                                   | SaaS                                                      |
| ---------------- | ------------------------------------- | --------------------------------------------------------- |
| Runtime          | Self-hosted server plus SDKs          | Managed hosted API                                        |
| Structured state | Adapter over a default database stack | Managed default database stack                            |
| Governance       | Policy hooks and runtime controls     | Hosted policy, audit, and observability layer             |
| Operations       | Developer runs it                     | Multi-tenant management, billing, backups, and dashboards |

The OSS version should be useful on its own. The SaaS version should remove operational burden rather than replace the core model.

### Default technical stance

This should present as a regular database at the API surface, while hiding the harder machinery underneath.

- **Developer surface**: collections, schemas, records, CRUD-style operations, and current-state queries.
- **Underlying model**: append-only events, projections, snapshots, replay, and audit history.
- **Storage shape**: document-oriented state model first.
- **Extra backends**: adapters later for other storage systems if demand is real.

The initial opinionated path is likely an **event-backed document database for agents**. The system should feel like a normal database to developers, not like an event-sourcing product they have to learn directly.

### Auditability and event sourcing

For auditability, event sourcing is likely the right underlying pattern. Every mutation becomes an append-only event, which gives the system a native audit trail, replay capability, and point-in-time reconstruction.

The important product choice is to **abstract event sourcing away**:

- agents and developers work with schemas, records, and queries over current state,
- the system writes events underneath,
- projections and snapshots keep reads fast,
- audit and trace views come from the event log when needed.

This makes the product **event-backed, state-oriented at the surface**.

### Reliable schema and record operations

The agent should not mutate raw database state directly. The reliable model is **schema registry plus validated mutation API plus transactional storage plus event log**.

#### Schema lifecycle

1. The agent proposes schema changes through a schema API, not direct SQL.
2. Every schema has a version and a lifecycle such as `draft`, `active`, or `deprecated`.
3. Because the underlying state is document-oriented, the main challenge is usually soft schema evolution rather than rigid relational migration.
4. Changes should still be explicit, versioned, and compatibility-aware, even if old and new record shapes can coexist.
5. Older schema versions should remain readable during transition periods, with lazy upgrade or background backfill where needed.

#### Record lifecycle

1. The agent reads and writes through a typed record API bound to a schema version.
2. Every write is validated against the active schema before commit.
3. Updates should use optimistic concurrency such as revision numbers or ETags.
4. Writes should run inside transactions.
5. Every mutation should carry an idempotency key so retries do not duplicate work.

| Operation     | Reliability mechanism                                       |
| ------------- | ----------------------------------------------------------- |
| Create schema | Versioned registry, validation, compatibility rules         |
| Update schema | Soft schema evolution, compatibility checks, approval gates |
| Read schema   | Immutable versions, active-version pointer                  |
| Delete schema | Deprecate by default, hard delete rarely                    |
| Create record | Schema validation, transaction, idempotency key             |
| Update record | Revision check, transaction, append-only audit event        |
| Read record   | Typed API, version-aware resolver                           |
| Delete record | Soft delete or tombstone by default                         |

The key product decision is that agents operate on **intentful state operations**, not storage internals. The interface should look more like `create_schema`, `update_record`, or `list_records`, and less like raw SQL, raw Mongo updates, or arbitrary JSON mutation.

Document storage reduces some migration pain, but it does not remove evolution work entirely. The system still has to manage:

- validation changes,
- renamed or split fields,
- projection rebuilds,
- index changes,
- policy changes,
- API compatibility while old and new record shapes coexist.

#### Extra reliability layers

- **Audit log**: every schema and record change is recorded.
- **Policy layer**: defines which agent, user, or workflow can mutate which objects.
- **Reconciliation jobs**: detect broken references, failed migrations, partial writes, and drift.

This pushes the system toward a safe state machine rather than a thin database wrapper, which is likely the right abstraction if agents are going to create, update, read, and delete state reliably across many product surfaces.

## Why it matters

If agent apps are going to feel real, they need a source of truth beyond the transcript. The state layer is what turns "cool chat" into durable application state:

- chat can create or update state,
- app actions can mutate the same state,
- multiple clients can read the same current object,
- audits and replays can explain how that object changed over time.

## Possible wedge

The cleanest starting point is not "backend for everything". It is "state object store for products that need agent-created records to persist, evolve, and stay auditable".

Good first customers could be:

1. Mobile agent apps.
2. AI workspaces with editable structured records.
3. Vertical agent products that keep reinventing the same object model.

## Why now

The ecosystem is getting good at model calls, tools, and chat shells, but weak at persistent app state for agents. The current market is full of agent demos that can talk and maybe act, but cannot maintain a durable product-grade object model across sessions and interfaces. That gap becomes more obvious as products move from chat novelty to repeat use.

## New design pressure from governance runtimes

[[Source - NEES Core Engine Governance Runtime]] is useful because it sharpens a missing part of this project. A state layer probably cannot just be object storage alone. It may also need a thin runtime-governance plane that controls:

- which state mutations are allowed,
- what trace metadata and audit context are attached to each action.

This does not mean the whole project should become a safety product. It means governed execution may be one of the primitives that makes a general state layer production-grade rather than demo-grade.

## Maybe later

These are adjacent primitives, but they should not be the centre of this project note:

- thread storage,
- artefact storage,
- UI schema and rendering layers,
- memory systems and recall layers.

All of them matter to full agent products, but they already have stronger existing solution categories. The main focus here should stay on the **state object store**: schemas, records, mutations, auditability, projections, and safe evolution over time.

## Open questions

- How much of the governance layer belongs in the core runtime versus an optional hosted control plane?
- Is the right abstraction closer to a document database, an event-sourced workflow engine, or a graph of agent-managed objects?
- How much schema freedom is healthy before the developer experience gets too loose?
- How much of the event model should be visible to developers versus kept fully behind the database-like API?
