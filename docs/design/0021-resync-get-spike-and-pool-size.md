# 0021 — The history GET spike after Redis recovers: coalesce resyncs, size the pool, and know the test machine

**Status**: Accepted (2026-11-24). Closes M10.

## Context

When Redis comes back after an outage, every viewer in every room receives a `resync` frame and fetches the room's
history (`GET /rooms/{id}/messages`: one room lookup plus the newest 100 messages). The client spreads these with
a random 0–2 s delay. Nothing else absorbs them: there is no cache or singleflight in the server, and no CDN or
nginx cache in front, so the `s-maxage=1` header has no effect. [ADR 0020](0020-redis-blackhole-gray-failure.md)
added deferred `resync`, which can send more of these frames. The question for M10d was whether this spike is a
problem, and if so what to change.

The rule set before measuring: **if the baseline is fine, change nothing.**

**Setup**
- The fault environment ([ADR 0017](0017-stuck-clients-fault-env.md)): two servers, one Postgres, one Redis, all
  in a Docker Desktop VM (8 vCPU, 4 GB) on a **MacBook Air M1: 4 performance and 4 efficiency cores, 8 GB RAM, no
  fan**. The load tool runs in the same VM.
- A new load-test scenario, `resync-spike`: N rooms × 10 viewers connected directly to the servers, one message
  per second per room, and every viewer reacting to `resync` like the web client (wait, fetch, ignore further
  resyncs while a fetch is scheduled but not started). `redis.sh stop 10` stops Redis for 10 s, 20 s after
  measurement starts.
- The servers log pool and latency figures every 5 s (`LOADTEST_TIMING_LOG`).
- One run per configuration unless stated.

## Results

### 1. Baseline (pgx pool 8, the default `max(4, NumCPU)` on this VM; client delay 0–2 s)

| Viewers | GETs | Failed | GET p99 | GETs per viewer |
|---|---|---|---|---|
| 1,000 | 1,917 | 0 | 9.3 ms | 1: 230, 2: 623, 3: 147 |
| 3,000 | 6,519 | 0 | 5.0 ms | avg 2.17 |
| 5,000 | 10,061 | 0 | 351 ms | avg about 2 |
| 10,000 | 25,825 | **6,195 timeouts** | 15.5 s | avg about 2.6 |

At 1,000 viewers the spike is harmless (prediction: fine). At 5,000 it slows down but every GET succeeds; at 10,000
GETs time out (prediction: slower, then failures). In the 5 s around the spike at 5,000 viewers, about 73% of pool
acquires had to wait (8,100–8,600 per server), so the wait was for a connection, not for the query (prediction:
pool wait).

### 2. Every viewer got three resyncs

Each viewer received **3** `resync` frames: one from its own server's reconnect, and one deferred resync from
**each** server, published on that server's first successful publish to the room after the outage. A full Redis
outage makes every publish fail, so both servers send one. The server can't tell a full outage, where the reconnect
already reached everyone, from a blackhole like ADR 0020's, where the deferred resync is the only notice. So the
extra frames are accepted false positives, but they doubled the GET spike, because the client's dedupe only covers
a fetch that is scheduled and not yet started.

The spread between a viewer's first and last resync was p95 2.3 s, max 2.5 s (1,000 and 5,000 viewers). That
follows from the traffic: a room posts once a second, alternating between two servers, so each server gets its
first successful publish to a room within about 2 s.

### 3. Fix: wait a fixed 2.5 s before the jittered fetch

The client now waits **2.5 s + random [0, 2 s)** instead of random [0, 2 s). Resyncs that arrive during the fixed
part coalesce into one fetch; the random part still spreads the GETs, because the fixed part delays everyone
equally. Only the history backfill is delayed (2.5–4.5 s after the first resync); live messages are unaffected,
and backfilled messages only go into the list.

| | 5,000 viewers, before → after | 10,000 viewers, before → after |
|---|---|---|
| GETs per viewer | about 2 → **exactly 1** | about 2.6 → **exactly 1** |
| GETs | 10,061 → 5,000 | 25,825 → 10,000 |
| GET failures | 0 → 0 | 6,195 → **0** |
| GET p99 | 351 ms → **21 ms** | 15.5 s → 3.4 s |
| Pool acquires that waited during the spike, per server | about 8,300 → 137–375 | saturated → about 9,000–10,000, none canceled |

Not chosen:
- **1 s fixed wait**: misses most second and third resyncs (p95 spread 2.3 s).
- **A shorter random part**: the fixed part doesn't spread anything, so halving the jitter doubles the peak.
- **Comparing message ids** (a resync carrying the newest unsent id, skipped by a client that already has it):
  correct, but changes server, protocol, client and load tool for a problem the fixed wait already solves.
- Quieter rooms (a resync arriving later than 2.5 s) can still fetch twice. Accepted.

### 4. Pool size: a U shape at 10,000 viewers

The server never set a pool size, so it used pgx's default of 8. `pool_max_conns` in `DATABASE_URL` controls it, but
adding it broke startup: `RunMigration` parsed the URL with `pgx.ParseConfig`, which passes `pool_*` parameters to
Postgres as runtime parameters ("unrecognized configuration parameter"). It now uses `pgxpool.ParseConfig`, the
same parser as the store. The fault compose takes `PG_POOL_MAX_CONNS` and `PG_POOL_MIN_CONNS`.

| Pool | GET p99 (runs) | POSTs dropped by the tool |
|---|---|---|
| 8 | 3.4 s | about 2,000 |
| **16** | **0.53, 0.93, 2.3 s** | 0 – about 950 |
| 32 | 5.8, 7.0, 9.6 s, and 2.5–12.1 s in 12 more runs | about 4,000 – 25,000 |

16 beat 32 in every paired comparison. During the spike, Postgres had 9–12 active queries with pool 16 and **32–41**
with pool 32, while the 8 cores were saturated either way. Inference: once CPU is the limit, more concurrent queries
don't add throughput; 64 Postgres processes competing for 8 cores make every query slower, and the spike lasts
longer (2–8 s at 16, 14–60 s at 32). The wait moves from the Go pool into Postgres and the OS scheduler, and gets
worse. The usual rule of thumb, cores × 2 plus spindles, gives about 16 here.

The predicted cause (pool size) held for 8 → 16. The prediction that a bigger pool keeps helping did not.

### 5. Intermittent overload before the outage

Some pool-32 runs overloaded **before** Redis stopped: `create_message` p99 up to 2–5 s, rate-limiter calls past
their 200 ms budget, even early resyncs. What was ruled out:

| Hypothesis | How it was tested | Result |
|---|---|---|
| GC pauses | `/metrics` every 1 s | Median pause under 1 ms, 0–1 GCs per second, none clustered at the bad seconds |
| Opening many new connections at measurement start | 8 runs alternating `pool_min_conns` 0 / 32, criterion fixed in advance | Cold 1/4 overloaded, warm 0/4; the worst run of the series was warm. Not the trigger |
| Thermal throttling (no fan) | `powermetrics` every 1 s over 4 back-to-back runs | Thermal pressure Nominal for all 35 min; performance cores held about 3.0 GHz at full load |
| Hypervisor steal | `/proc/stat` in the VM | Steal 0 throughout |

What the `powermetrics` data does show: once a run starts, the 4 performance cores are about 95% busy even before
the outage, while the VM reports about 67% idle. Extra work lands on the efficiency cores, which run at about a
third of the speed; a vCPU there is slow but not stopped, so the VM sees no steal. The two worst runs were the two
where macOS background work (an XProtect malware scan, then a burst of system daemons) took the efficiency cores to
97–100%. The best runs had them at 85–88%. **Most likely cause: the test machine has 4 fast cores shared by the
load tool, two servers, Postgres, the hypervisor and macOS.** This is an inference from four runs; the overload
itself stopped reproducing in the last 12 runs.

### 6. The rate limiter fails open when the server is overloaded

Before the outage, under load, limiter calls exceeded their 200 ms budget and failed open (up to about 450 times on one
server before the outage, in a pool-16 run too). [ADR 0020](0020-redis-blackhole-gray-failure.md) chose failing open for a
broken Redis; it also means **rate limiting switches itself off when the server is overloaded**, which is when it
matters most.

## Decision

- Keep the history endpoint as it is: no cache, no singleflight. With coalesced resyncs, 5,000 viewers recover with
  GET p99 21 ms on this machine.
- The client waits 2.5 s + random [0, 2 s) after a `resync`; the load tool mirrors it with
  `-redis-resync-delay-min` (default 2.5 s) and `-redis-resync-delay-max` (2 s).
- Don't hard-code a pool size. Set `pool_max_conns` in `DATABASE_URL` per deployment, starting from cores × 2, and
  measure on the target host. This is on the launch checklist.
- Keep the fault compose's `PG_POOL_MAX_CONNS` / `PG_POOL_MIN_CONNS` for experiments.

## Consequences

- History backfill after a `resync` arrives 2.5–4.5 s later than before.
- Numbers from this machine are for comparison only, and vary a lot run to run (GET p99 up to 5× for the same
  configuration). The relative results held across repeats: coalescing halves the GETs, 16 beats 8 and 32, GC is
  not the bottleneck. "16 is best" and "5,000 viewers is where it slows" do not transfer to production.
- Reproducing the runs of ADRs 0018–0020 now needs `-redis-resync-delay-min 0`, since the load tool's default
  changed with the client.
- Warm connections from `pool_min_conns` are created together and expire together after `MaxConnLifetime` (1 h, no
  jitter), so a pre-warmed pool reconnects all at once every hour.
- Backlog:
  - Rerun `resync-spike` on the production host and choose `pool_max_conns` there.
  - The rate limiter fails open under overload; consider failing closed or a local limiter when Redis calls time
    out for reasons other than Redis being down.
  - A sudden jump in request rate as a trigger (the tool jumps from 0 to the full POST rate) is untested.
- The 10,000-viewer runs also had the POST load (1,000/s) saturating the pool before the outage, so they measure
  POSTs and the spike together.
