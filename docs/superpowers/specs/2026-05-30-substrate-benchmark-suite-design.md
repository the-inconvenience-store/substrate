# Substrate Benchmark Suite — Design

- **Date:** 2026-05-30
- **Status:** Approved (design)
- **Author:** brainstormed with Claude Code

## Goal

Build a benchmark suite that lets an agent (or human) **iteratively reduce resource
consumption and latency in Substrate while preserving functionality**. The suite must
expose, for the hot paths, the four metrics that matter for optimization work:

- **Latency** — `ns/op`
- **Throughput** — derived from `ns/op` (and `-benchtime` if needed)
- **Memory** — `B/op`
- **Allocations** — `allocs/op`

CPU/heap drill-down is available on demand via pprof. Functionality is guarded by the
existing test suite, which the optimization loop runs as a hard gate before trusting any
benchmark delta.

There are currently **no benchmarks** in the repo — this is a clean slate.

## Scope

Three measurement layers (server startup/migration is explicitly out of scope):

1. **Pure-logic micro-benchmarks** — no DB, no Docker, deterministic. The tight inner
   loop for allocation reduction.
2. **DB-backed service benchmarks** — `record.Service` against real Postgres
   (testcontainers), measuring the true write/read pipeline.
3. **HTTP end-to-end benchmarks** — full stack through the `api` router via
   `httptest.Server`, adding auth, routing, and JSON DTO encode/decode cost.

## Locked decisions

| Decision | Choice |
| --- | --- |
| Layers | Pure-logic + DB-backed + HTTP e2e (no startup/migration) |
| Database | Reuse testcontainers via `store.NewTestPool` (one shared container/run) |
| Reporting | Committed baselines + `benchstat` for statistically-significant deltas |
| Profiling | First-class `mise` tasks for pprof (`-cpuprofile`/`-memprofile`) |
| Workload | Tiered small + large per read path; ~1 KB / ~6-field payloads |
| Code placement | Co-located `*_bench_test.go` + one shared `benchfix` seeder (Approach C) |

## Architecture & file layout

Benchmarks live next to the code they measure, split by whether they need a DB. Pure
benches are untagged (fast, Docker-free); DB/HTTP benches carry `//go:build integration`
and reuse the existing white-box helpers (`newTestServer`/`newGovServer`/`do`/`doAs`,
`store.NewTestPool`). This is the only approach that avoids re-implementing — and
drifting from — `cmd/substrate/main.go` wiring.

```
internal/
  policy/      policy_bench_test.go        # pure, untagged
  query/       builder_bench_test.go       # pure, untagged
  schema/      validator_bench_test.go     # pure, untagged
  projection/  defaults_bench_test.go      # pure, untagged (defaults applier)
  record/      record_bench_test.go        # DB,  //go:build integration
               timetravel_bench_test.go    # DB,  //go:build integration
  api/         api_bench_test.go           # HTTP e2e, //go:build integration
  store/
    benchfix/  seed.go                     # shared fixture seeding, integration-tagged
docs/benchmarks/
  README.md                                # the agent optimization-loop workflow
  baseline/                                # committed benchstat baselines
    pure.txt  db.txt  http.txt
  runs/                                    # timestamped run outputs (gitignored)
mise.toml                                  # bench / bench:* tasks
```

**Two run classes:**

- **Pure** (`mise run bench:pure`) — no build tag, no Docker, sub-second, deterministic
  allocs.
- **Integration** (`mise run bench:db`, `mise run bench:http`) — `//go:build
  integration`, testcontainers Postgres, white-box helper reuse.

`benchfix` is integration-tagged (it touches a pool), keeping pure benches free of any DB
import.

## Layer 1 — Pure-logic micro-benchmarks

Deterministic `ns/op` and `allocs/op`, sub-second. Each targets a hot, allocation-prone
pure function. Setup (compile schema, build ruleset) happens **before** `b.ResetTimer()`;
each uses `b.ReportAllocs()`; each consumes its result to defeat dead-code elimination.

- **policy** (`internal/policy/policy_bench_test.go`) — `Select`/precedence resolution
  against a realistic ruleset (most-specific-wins + deny-override ties + default fallback).
  Tiered: small ruleset vs large (~50 rules). Runs on every write.
- **query** (`internal/query/builder_bench_test.go`) — `Parse` (filter/sort/limit/cursor
  parsing) and `Build` (`ListQuery` → parameterized SQL + args), varying filter count
  (1 vs ~8 filters + sort + cursor); plus `NextCursor` encode / cursor decode round-trip.
- **schema** (`internal/schema/validator_bench_test.go`) — JSON-Schema `Validate` of a
  payload against a pre-compiled schema (santhosh-tekuri/v6). Tiered: small (~6 fields) vs
  large/nested. Plus the compatibility classifier (it is pure). Runs on every write.
- **projection** (`internal/projection/defaults_bench_test.go`) — the defaults applier
  (apply schema defaults to a record map) and any pure re-stamp logic.

## Layer 2 — DB-backed service benchmarks

`//go:build integration`, one shared container/run via `store.NewTestPool`. Each
benchmark wires a real `record.Service` with schema validator + policy evaluator
(mirroring `main.go`) so the full pipeline is measured: `authorize → idempotency →
validate → revision check → append event + upsert record` in one transaction.

**Write paths:**

- `BenchmarkCreate` — fresh record, ~1 KB / ~6-field payload; the full write tx.
- `BenchmarkUpdate` — optimistic `If-Match` update on an existing record.
- `BenchmarkDelete` — soft delete.

**Read paths (tiered small ~100 / large ~10k):**

- `BenchmarkGet` — single keyed projection lookup.
- `BenchmarkList_Small` / `BenchmarkList_Large` — list with filter + sort + keyset limit.
  Large reveals scaling of the hand-built query builder.
- `BenchmarkHistory_Short` (~5 revisions) / `BenchmarkHistory_Long` (~500 revisions) —
  event-stream read.
- `BenchmarkGetAsOf_Deep` — point-in-time reconstruction against deep history.

**Measurement discipline.** Writes mutate state, so the timed loop cannot naively reuse
one row:

- Seed fixtures **before** `b.ResetTimer()`.
- For writes, **pre-generate `b.N` distinct payloads/ids up front** (preferred over
  `b.StopTimer()`/`StartTimer()` per iteration, which introduces timer-toggle skew).
- Each iteration checks `err`.
- A single functional assertion runs **once before** the loop so a broken change fails
  loudly instead of benchmarking a no-op.

This is the noisiest layer; `-count=10` + benchstat p-values absorb container jitter.

## Layer 3 — HTTP end-to-end benchmarks

`//go:build integration`, white-box `package api`, reusing
`newTestServer`/`newGovServer` + `do`/`doAs` against an `httptest.Server`. Adds what
Layer 2 omits: auth middleware, routing, and JSON DTO encode/decode (a real allocation
source).

- `BenchmarkHTTP_CreateRecord` — `POST …/records` (decode → pipeline → encode).
- `BenchmarkHTTP_GetRecord` — `GET …/records/{id}`.
- `BenchmarkHTTP_ListRecords_Small` / `_Large` — `GET …/records?filter…&sort…` both scales.
- `BenchmarkHTTP_History` — `GET …/records/{id}/history`.

Each builds one server + key **before** `ResetTimer`, **drains and closes `resp.Body`
every iteration** (else FD/alloc leak skews numbers), asserts status once before the loop,
and reports allocs.

**Caveat (documented in README):** `httptest` includes loopback HTTP overhead, so these
are "full request cost," not pipeline-only. The agent should compare the HTTP-vs-service
delta to attribute cost, not chase loopback noise.

## Shared fixture seeder — `internal/store/benchfix`

One integration-tagged helper package so seeding is not duplicated across the record and
api benchmarks. Seeding uses **direct pool inserts** (not the HTTP path) so fixture build
time stays out of the measured loop and large tiers seed quickly.

- `SeedRecords(tb, pool, ws, col, n)` — bulk-insert `n` records (with their events),
  returning ids for keyed lookups. Backs List/Get tiers.
- `SeedHistory(tb, pool, …, revisions)` — one record advanced through N revisions for
  History/GetAsOf depth.
- `Payload(sizeHint)` — deterministic ~1 KB / ~6-field record map (+ a large variant)
  built from a fixed seed so runs are reproducible (no time/random — also unavailable in
  the harness).
- Helpers to register + activate a schema and install a baseline policy set, mirroring
  `main.go` wiring.

## Reporting, profiling & the agent loop

### `mise` tasks (added to `mise.toml`)

- `bench:pure` — `go test -run '^$' -bench . -benchmem -count=10 ./internal/policy/...
  ./internal/query/... ./internal/schema/... ./internal/projection/...`
- `bench:db` — `go test -tags=integration -run '^$' -bench . -benchmem -count=10
  ./internal/record/...`
- `bench:http` — `go test -tags=integration -run '^$' -bench . -benchmem -count=10
  ./internal/api/...`
- `bench` — runs all three; writes timestamped output under `docs/benchmarks/runs/`.
- `bench:compare` — `benchstat docs/benchmarks/baseline/<group>.txt <new>`.
- `bench:baseline` — refresh and overwrite committed baselines (explicit, never automatic).
- `bench:profile` — run a named benchmark with `-cpuprofile`/`-memprofile`, then
  `go tool pprof`.

`-run '^$'` prevents normal tests from running during benchmarking. `-benchmem` is always
on, so every result carries `B/op` + `allocs/op`. `benchstat`
(`golang.org/x/perf/cmd/benchstat`) is added to the `go.mod` tool directive.

### Optimization loop (documented in `docs/benchmarks/README.md`)

1. `mise run test && mise run test:integration` — **functionality gate**. Perf work that
   breaks behavior is rejected here; benchmarks alone do not prove correctness.
2. `mise run bench` — capture current numbers.
3. Make one optimization.
4. Re-run; `mise run bench:compare` against the committed baseline.
5. Keep the change only if functionality still passes **and** benchstat shows a
   significant improvement (or no regression) in `ns/op`, `B/op`, or `allocs/op`. Use
   `bench:profile` to locate the hot spot behind a delta.
6. When a genuine improvement lands, `mise run bench:baseline` updates the committed
   baseline so future regressions are measured against the new floor.

### Honesty guardrails

- Every benchmark checks errors and asserts a functional result **once before** its timed
  loop, so a change that secretly breaks a path cannot masquerade as "faster."
- `-count=10` + benchstat p-values gate on statistical significance so container/loopback
  jitter is not mistaken for a win.
- Baseline updates are explicit (`bench:baseline`) — never silent — so the regression
  floor only moves on intentional, verified improvements.

## Out of scope

- Server cold-start / migration-time benchmarking.
- External-DSN benchmark mode (env-var Postgres) — testcontainers only for now.
- A custom JSON-baseline regression-gate test — benchstat + committed baselines cover the
  agent loop; revisit only if CI needs a hard pass/fail gate.
- Concurrency/load benchmarks (`b.RunParallel`, sustained-throughput soak) — the suite
  measures single-op cost; parallel contention is a possible follow-up.
