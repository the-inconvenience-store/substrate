# Substrate Benchmarks

A suite for reducing latency, memory, and allocations without breaking functionality.
Three layers: pure-logic micro-benchmarks (no Docker), DB-backed `record.Service`
benchmarks, and HTTP end-to-end benchmarks (both need Docker for testcontainers
Postgres). Every benchmark reports `ns/op`, `B/op`, and `allocs/op` (`-benchmem` is
always on).

## Tasks

| Task | What it does |
| --- | --- |
| `mise run bench:pure` | Pure micro-benchmarks (policy, query, schema, projection). No Docker. |
| `mise run bench:db` | DB-backed record benchmarks. Docker required. |
| `mise run bench:http` | HTTP end-to-end benchmarks. Docker required. |
| `mise run bench` | All three groups. |
| `mise run bench:baseline` | Overwrite the committed baselines under `baseline/`. |
| `mise run bench:compare -- baseline/<g>.txt new.txt` | benchstat diff. |
| `mise run bench:profile -- [flags] <pkg>` | CPU+mem profile a benchmark, then `go tool pprof cpu.out`. |

## The optimization loop

1. **Functionality gate first.** `mise run test && mise run test:integration`.
   Performance work that changes behavior is rejected here — benchmarks alone do not
   prove correctness.
2. **Capture current numbers.** `mise run bench:pure > /tmp/new-pure.txt` (and/or the
   db/http groups).
3. **Make one optimization.**
4. **Re-run and compare.** `mise run bench:compare -- docs/benchmarks/baseline/pure.txt /tmp/new-pure.txt`.
5. **Keep the change only if** functionality still passes *and* benchstat shows a
   significant improvement (or no regression) in `ns/op`, `B/op`, or `allocs/op`. Use
   `mise run bench:profile` to find the hot function/allocation site behind a delta.
6. **Move the floor.** When a genuine improvement lands, `mise run bench:baseline`
   refreshes the committed baselines so future regressions are measured against the new
   floor. Baseline updates are explicit — never automatic.

## Caveats

- **HTTP numbers include loopback overhead.** `httptest` runs a real HTTP round-trip, so
  `BenchmarkHTTP_*` measures full request cost, not pipeline-only. Compare the
  HTTP-vs-service delta to attribute cost rather than chasing loopback noise.
- **DB benchmarks are the noisiest layer.** `-count=10` plus benchstat's p-values absorb
  container jitter; a single run is not trustworthy.
- **Baselines are machine-specific.** Re-capture on the machine/CI you compare against.
- **Honesty guardrails.** Every benchmark checks errors and (for HTTP) asserts status, so
  a change that breaks a path cannot masquerade as faster.
