# 0020 — One server loses Redis silently: a gray failure that health checks can't see

**Status**: Accepted (2026-11-18). Records the behaviour; the fixes are in the backlog.

## Context

M10b's last fault. [ADR 0018](0018-redis-restart-best-effort-resync.md) took Redis away for every server at once,
and Redis refused connections or answered nothing. Here Redis stays healthy and only the path between **one**
server and Redis is cut. Packets are dropped, so neither side gets an RST. This is the usual shape of a partial
network failure.

**Setup**, one run
- The fault environment ([ADR 0017](0017-stuck-clients-fault-env.md)) with two servers and one Redis.
- The tool's `redis` scenario: 10 viewers in one room, 5 connected directly to each server, and one poster
  sending 5 numbered messages per second for 120 s, alternating between the two servers.
- `deploy/fault/blackhole.sh 60`: 30 s in, a throwaway `nicolaka/netshoot:v0.13` container joins `server-1`'s
  network namespace and adds two iptables `DROP` rules, for packets to and from Redis' IP. After 60 s it removes
  them. A trap removes them on every exit path, and the rules are checked with `iptables -C` before the clock
  starts.
- New in the tool: per-server presence (the viewer count each server's viewers saw, in 5 s buckets) and
  per-server POST outcomes and latency.

Before the run, three paths were known to touch Redis on a `POST`: the rate limiter twice (burst and sustained),
then `PUBLISH` after the database write. The limiter fails open, but only when Redis returns an error.

## Results

Timeline, t = 0 when posting started:

| t | Event |
|---|---|
| ≈ 28 s | Blackhole on |
| ≈ 84 s | `server-1`'s POSTs suddenly answer 201 in about 300 ms; `PUBLISH` fails with `dial tcp …: i/o timeout` |
| ≈ 89 s | Blackhole off |
| 121.7 s | `server-1` judges its PubSub connection deaf, rebuilds it, and sends `resync` |

**POSTs to `server-1`** (300; `server-2` answered all of its 300 with 201 in at most 21 ms):

| Outcome | Count | Evidence |
|---|---|---|
| Client gave up at 10 s | 128 | 128 × `create message failed` with `context canceled`, logged as 500 |
| 201, not broadcast | 13 | 13 × `publish to redis failed` |
| 201 after 6.1–9.9 s | 11 | requests stuck when the blackhole ended, finished right after |
| 201, normal | 148 | before and after the blackhole |

The 128 failures were not stored. Each `context canceled` came from the database write after the client had
left, and no viewer received a message whose POST didn't get 201 (`received_not_201 = 0`). The database couldn't
be checked directly: the tool deletes its seeded data after the run.

**Delivery**

| Viewers on | Missed | Recovered | Lost for good |
|---|---|---|---|
| `server-1` | 1640 | 500 (the latest 100 each, after `resync`) | **1140** |
| `server-2` | 65 | 0 | **65** (= 13 unpublished × 5 viewers) |

`server-1`'s viewers received nothing for about 93 s, **including everything posted to the healthy `server-2`**.
Their server's subscription to Redis was cut too, so the fan-out from Redis never reached them.
`server-2`'s viewers lost exactly the 13 messages `server-1` stored but couldn't publish. Nothing told them, so
nothing made them fetch.

**Viewer count**
- `server-2`'s viewers saw 10 drop to 5 in the 45–55 s bucket. `server-1` stopped renewing its viewers' presence,
  and they expired after the 30 s TTL. The count was back to 10 at about 90 s.
- `server-1`'s viewers got no presence frames at all from about 30 s to 85 s, so their count stayed at 10.
  `server-1` logged no `presence count failed` and no `presence sync failed` in that time. Why it logged nothing
  is **not investigated**.

## Why it behaved this way

**The server looked healthy.** Its HTTP answered, so nothing in front of it would have taken it out. The fault
was only visible as slow POSTs and as messages that never arrived. Nothing reported it.

**POSTs hung for 55 s, then turned fast.** Inferred from the logs and go-redis defaults, not verified step by step:
- At first the limiter's commands went out on connections already in go-redis' pool. Each attempt waits for the
  3 s read timeout, and go-redis retries, so one call outlasts the client's 10 s. The request context isn't
  applied to socket I/O (`ContextTimeoutEnabled` is off, as already noted in `redisstore`). 256 of the 284
  `rate limiter failed` lines are `context canceled`: the client had already gone.
- Once those connections had failed, go-redis had to dial new ones, and dials kept failing. After enough failed
  dials the pool returns the last dial error immediately instead of dialing again. From then on the limiter
  failed fast and opened, the database write succeeded, and `PUBLISH` failed fast too. 28 limiter lines are
  `dial tcp …: i/o timeout`.

**`resync` came 32 s after the blackhole ended.** The watchdog ([ADR 0011](0011-message-delivery-recovery.md))
counts a missed heartbeat only when publishing that heartbeat succeeded. During the blackhole every publish failed
(4 × `publish heartbeat failed`), so no miss was counted, which is deliberate: rebuilding while Redis can't be
reached does nothing. After the blackhole ended the old subscription connection was still dead but had never
been reset, so two more 15 s rounds had to pass before the watchdog rebuilt it.

## Comparison with predictions

| Prediction | Result |
|---|---|
| A POST to `server-1` gets 201, fast, because only Redis is broken | Wrong. The rate limiter sits in front of the write and hung until the client gave up |
| It fails with 499 and is not stored | Not stored: right. 499 is nginx's code and no nginx was in the path; the server logged 500 for a client already gone |
| — | Not predicted: 201 but never broadcast (13) |
| Both servers' viewers miss `server-1`'s posts | Right, and worse: `server-1`'s viewers missed **everything** |
| The two servers show different viewer counts | Right |
| Only `server-1`'s viewers get `resync` | Right, 32 s after the blackhole ended |
| `server-2`'s viewers lose the unpublished messages for good | Right, 65 |

## Decision

Record the behaviour now and fix it later; each fix is a separate change with its own run. Backlog:

1. **Give the limiter's Redis calls a short budget.** A blackholed Redis should make the limiter fail open within
   a fraction of a second, not hang the request. That means the context deadline has to reach the socket
   (`ContextTimeoutEnabled`, or short read and write timeouts on this client) and retries have to stop counting
   against the user. This removes the 128 failed POSTs.
2. **Deferred `resync`**, already in the backlog from ADR 0018: a server whose `PUBLISH` failed sends a `resync`
   through Redis once publishing works again. This reaches `server-2`'s viewers, who today have no way to learn
   they missed anything.
3. **Find out why presence went silent** without logging an error.

Not chosen: making the watchdog count misses while publishing fails. During the blackhole a rebuild can't succeed
either, and after it a faster rebuild would save at most 15 s. Fix 2 matters more.

## Consequences

- **A server that can't reach Redis keeps taking traffic.** Its viewers get nothing from other servers, and its
  messages reach nobody, until the path heals and the watchdog rebuilds, at least 30 s later.
- **What a viewer gets back is the latest 100.** At 5 msg/s that is 20 s; this blackhole lasted 60 s.
- **The two servers disagree and neither is right.** `server-1`'s viewers see a frozen count; `server-2`'s see the
  other half of the room disappear.
- One run. The 55 s before POSTs turned fast depends on go-redis' pool size, timeouts and retries, and wasn't
  measured in isolation.
