# Profiling & throughput investigation runbook

Two sections: **How to enable** (the pprof tooling), and **Throughput
investigation runbook** (the decision tree the next throughput question
should follow before anyone touches pipeline code).

The runbook exists because the first full init narration showed a flat
~28 records/sec across resources with a ~40x complexity spread between
them. That shape is diagnostic — see the **Hypothesis** subsection below.

---

## How to enable

### Config

`default.config.yaml`:

```yaml
profiling:
  enabled: false  # off in checked-in default; never enable in production
  port: 6060
```

Override locally in `config.yaml` or via env (`MLS_SYNC_PROFILING_ENABLED=true`).
When enabled, the server binds to `127.0.0.1:<port>` only — pprof endpoints
expose heap, goroutine, and CPU/block profiles that must never face the
public network.

On startup you'll see:

```
INF pprof: listening on http://127.0.0.1:6060/debug/pprof/
```

### `go tool pprof` invocations

CPU profile (30-second capture mid-pass):

```sh
go tool pprof http://localhost:6060/debug/pprof/profile?seconds=30
```

Block profile (where the process is waiting — DB round-trips, mutexes,
channels):

```sh
go tool pprof http://localhost:6060/debug/pprof/block
```

Heap (allocations live at the moment of capture):

```sh
go tool pprof http://localhost:6060/debug/pprof/heap
```

Goroutine dump (counts and stacks — useful for stuck-pass investigations):

```sh
curl -s http://localhost:6060/debug/pprof/goroutine?debug=2
```

Inside the `pprof` REPL: `top`, `list <Func>`, `web` (renders an SVG if
Graphviz is installed), `quit`.

The block profile is sampled at rate 1 (every blocking event reported)
while `profiling.enabled = true`. That's appropriate for an
investigation; if profiling is ever left on in steady state, dial the
sampling rate down — see `runtime.SetBlockProfileRate` in
`cmd/root.go:startProfilingServer`.

---

## Throughput investigation runbook

### Hypothesis to test first

When throughput is flat across resources of very different per-record
complexity (Lookup at 4 fields and no version table runs the same speed
as Property at ~150 fields with version machinery), the per-record cost
is dominated by **fixed transaction overhead** — sequential DB
round-trips plus the COMMIT fsync — not by computation.

The arithmetic: 28/s ≈ 36 ms/record. Each record performs ~5-7
sequential round-trips inside one transaction (`processor.go:307-325`):
entity lookup → version lookup → close-version (if updating) →
version insert → entity write → cursor UPDATE → COMMIT. At ~5 ms per
round-trip on a containerized dev volume, 36 ms is fully accounted for
with nothing left for CPU.

A CPU profile against that pipeline shows mostly waiting — informative
only in confirming the negative. **Run the latency-first investigation
below before reaching for `go tool pprof profile`.**

### Step 1 — Confirm the hypothesis without a profiler (~30 min)

**1a. Round-trip census.** Open `sync/processor/processor.go:280-325`
plus the heaviest processor (`property.go`) and the lightest
(`lookup.go`). Write down the exact count of sequential SQL operations
per record per outcome path (INSERT / UPDATE / DELETE / skip).
**Include reads, BEGIN, COMMIT, and the cursor UPDATE.** The
expectation: 5-7 round-trips per record.

**1b. Postgres latency floor.** On the dev DB:

```sql
SHOW synchronous_commit;  -- expect 'on'
SHOW fsync;               -- expect 'on'
```

Measure a single-row INSERT+COMMIT wall time from the same network
position the app sits in (or `\timing` in `psql`). Multiply by step-1a's
count. Does the product ≈ 36 ms/record? If yes, the round-trip + commit
budget alone explains the measured rate.

**1c. `pg_stat_statements`.** Enable if not on, then run one reprocess
pass and inspect:

```sql
CREATE EXTENSION IF NOT EXISTS pg_stat_statements;
SELECT pg_stat_statements_reset();
-- in another shell:
--   go run . reprocess Member --all
SELECT query, calls, mean_exec_time
FROM pg_stat_statements
ORDER BY calls DESC
LIMIT 15;
```

Expected shape **if** the hypothesis holds: the top rows are the
per-record SELECT/INSERT/UPDATE family with very high `calls` and
sub-millisecond `mean_exec_time`. Cheap queries × many calls = the
round-trip / commit-fsync signature. The opposite (one query with large
`mean_exec_time`) **falsifies** the hypothesis and names a slow query
directly — investigate that query instead.

**1d. The decisive A/B (the cheap experiment).** On the dev DB:

```sql
ALTER SYSTEM SET synchronous_commit = off;
SELECT pg_reload_conf();
```

Re-run `go run . reprocess Member --all`. Read the rate from:

```
INF processor[member]: pass complete — 2800 records in 1m38s (28.6/s): ...
```

This line comes from `sync/processor/processor.go:251-252`.

> **Important — why this measurement uses `ALTER SYSTEM` and not a
> session-level `SET`.** The processor's per-record transactions draw
> fresh connections from the pool (`processor.go:307`), so a session-level
> `SET synchronous_commit = off` would silently no-op against the txns.
> The advisory-lock conn at `sync/processor/lock.go:23-47` is pinned, but
> that pinning doesn't extend to the work connections. `ALTER SYSTEM` +
> reload is dev-wide, applies to every new connection, and is the
> cheapest correct way to get the A/B number.

**Decision table:**

| A/B result | Diagnosis | Highest-leverage remedy |
|---|---|---|
| Rate jumps dramatically (28 → 100+/s) | Commit fsync dominates | Relax durability on the processor connection (architecturally safe — see "Why this is safe" below) |
| Rate barely moves | Round-trips themselves dominate | Batch commits (N records/tx) and/or collapse the entity+version lookups |
| Rate moves moderately | Both terms matter | Size each remedy against operational need |

**Revert:**

```sql
ALTER SYSTEM RESET synchronous_commit;
SELECT pg_reload_conf();
```

#### Why relaxed durability is architecturally safe (for the processor only)

`raw_output` is the source of truth and the processor cursor advances in
the **same transaction** as the entity/version writes. A crash that
loses the last few async commits loses the cursor advance *together
with* the data, so the re-run replays exactly the lost records. The
pipeline's replayability makes relaxed durability free **on the processor
layer only.**

Do **not** extend the same relaxation to:

- `sync_event` lifecycle writes — they record what actually happened on
  the API side; losing those breaks the replay-enabling layer.
- `raw_output` landing — these are the source of truth themselves; if
  they're lost, replay has nothing to replay from.

### Step 2 — Block profile (only if step 1 leaves ambiguity)

With `profiling.enabled = true`, during a `reprocess Member --all` pass:

```sh
go tool pprof http://localhost:6060/debug/pprof/block
```

Expected: `database/sql` wait frames dominate the top. This distinguishes
"waiting on Postgres" (the hypothesis) from in-process lock contention
(advisory-lock conn, pool starvation, mutex on the ent client) — things
the arithmetic in step 1 can't see.

### Step 3 — CPU profile (confirmation only)

```sh
go tool pprof http://localhost:6060/debug/pprof/profile?seconds=30
```

…30 seconds mid-pass. **Expected:** parse + diff + ent-builder is a
small fraction of wall time. If parse and diff dominate, that's a
genuine surprise worth its own look — Property's ~150-field diff is the
only plausible candidate. The flat-rate evidence says it won't, so this
step's job is to confirm the negative.

---

## Caveats

- **Dev numbers are not production numbers.** Today's measurements live
  on a containerized dev volume with default Postgres tuning. The real
  production DB will have different disk, durability, and pool-size
  posture. Re-measure on the deployment target before extending any
  conclusion past dev. Phase 5 deployment is the right place to repeat
  this runbook end-to-end against a production-shaped DB.
- **The investigation is measurement-only.** Don't change pipeline code
  while running steps 1-3 — the A/B is only meaningful between identical
  binaries. Remedies live in separate plans, sized against what the
  measurements show.

## Where the rate metric comes from

`sync/processor/processor.go:251-252` — the `pass complete` log line.
It's the single number every step of this runbook compares against, so
any change that affects its denominator (e.g. retrying records,
double-counting) would invalidate the A/B. If you change that math,
update this runbook too.

---

## Case study: `version.Info()` forking git per record (2026-06)

The first time this runbook was used, the runbook's own hypothesis was
wrong. Worth recording for the next investigator, because the
arithmetic-first ordering still holds — it just doesn't take you all the
way when the cost lives outside the round-trip budget.

**The signature.** Flat ~28 records/sec across all 4 resources (Lookup,
Office, Member, Property) despite a ~40x per-record complexity spread.
That looked like fixed-cost-per-record — round-trips + commit fsync. The
arithmetic fit (5-7 round-trips × ~5ms ≈ 36ms/record).

**The block profile (step 2) misled.** Top frames were
`Tx.awaitDone` with 212s cumulative wait time, which read as "waiting on
Postgres" and made the hypothesis look confirmed. It wasn't —
`Tx.awaitDone` was waiting because the *worker* goroutine inside the
transaction was parked in a syscall, and the block profile reports the
visible await, not the in-syscall reason for it.

**The goroutine dump (not the profiles) named the culprit.** A `curl
http://localhost:6060/debug/pprof/goroutine?debug=2` mid-pass showed:

```
goroutine 47 [syscall]:
syscall.Syscall6(...)
os.(*Process).blockUntilWaitable(...)
os.(*Process).Wait(...)
git.(*repo).run(...)
version.Info()
processor.newPropertyVersionCreate(...)   <-- inside per-record tx
```

…plus two live `io.Copy` goroutines fed by an executing `git`
subprocess's stdout/stderr pipes. The flat rate, the missing CPU time,
and the block-profile blind spot all reconcile against one fact: every
record forked `git rev-parse --short HEAD` and `git status --porcelain`
while the per-record transaction was open. Two fork+exec+wait cycles
×~15ms each ≈ 30ms/record of pure transaction hold time spent waiting on
git.

**The fix.** `sync.Once`-memoize `version.Info()` (`version/version.go`).
Nothing it measures (HEAD sha, dirty flag, build id) can change within a
process lifetime; per-call recomputation was never load-bearing. Add
`debug.ReadBuildInfo` VCS metadata as the preferred source (zero
subprocess cost on `go build`-stamped binaries) with the git-exec path
as fallback for `go run`. Contract test
(`version/version_test.go:TestInfo_ComputesOnce`) calls `Info()` 100
times and asserts the underlying git function ran at most once.

**Lesson encoded in the contract.** The `ResourceProcessor.Process`
interface comment at `sync/processor/processor.go:111-123` now states
that nothing slow — no subprocess, no network, no unbounded computation
— belongs inside the per-record transaction. The lesson lives where the
next contributor reads it.

**Lessons for the runbook itself.**

1. **An on-CPU 25% function whose work is mostly `syscall.Wait` is
   invisible on every standard profile.** CPU profile undercounts it
   (the process is parked, not on-CPU). Block profile attributes the
   wait to whatever was waiting visibly (here, `Tx.awaitDone`).
   Goroutine dumps name it directly. The arithmetic alone won't see it
   either — fork+exec+wait fits the same ~5ms/round-trip budget that
   round-trips would, so the units cancel.
2. **Add a goroutine-dump step before deciding step 1 is conclusive.**
   The runbook's "step 1 → step 2 block → step 3 CPU" ordering is still
   correct for the typical case, but a single `curl
   /debug/pprof/goroutine?debug=2` is 10 seconds of work and could have
   pointed at `git.(*repo).run` immediately.
3. **The `synchronous_commit` A/B was about to fire and would have
   shown "barely moved" — falsely implicating round-trips and pushing
   the team toward R2 (batch commits) when the actual fix was a single
   `sync.Once`.** Order matters: re-baseline after any non-trivial code
   fix before running the A/B.
