# 0014 — Load baseline: keep synchronous writes, the cost is fan-out

**Status**: Accepted (2026-11-16)

## Context

`POST /rooms/{public_id}/messages` inserts the message into Postgres and only then publishes it to Redis.
The obvious next step for a chat-like system is to make the write asynchronous: publish first, batch the
inserts through a queue (ROADMAP M7). That adds real complexity: a message can be broadcast and then lost,
the queue needs its own failure handling, and the shadow-ban check in `CreateMessage` would need a cache
because `POST` would no longer reach the database.

Before building it, M6 asked one question: **are synchronous writes the first bottleneck?** The rule was
fixed before measuring:

- pure writes at 100 msg/s with `POST` p99 ≤ 200 ms → don't build M7;
- otherwise, look at the time breakdown of the last valid step of the mixed scenario, and build M7 only if
  writes are what gives out first.

## How it was measured

`server/cmd/loadtest` (written for M6) against two local servers, Postgres 17 and Redis 7.4 in Docker
Desktop, all on one M1 MacBook Air (8 cores, 8 GB, macOS 15.6), on commit `e3a7b53`.

- Messages are sent on a fixed schedule and latency is timed from the **scheduled** send, so a slow server
  cannot hide its slowness by delaying the next request (coordinated omission). Timeouts count in the p99.
- A step is marked invalid when the tool falls behind its schedule, uses too much CPU, sees any 429, or errors
  at or above 1 %, or when other programs use more than one core.
- The servers run with `LOADTEST_TIMING_LOG=true`, which logs every 5 s how long `CreateMessage`, waiting
  for a pool connection, the rate limiter and `PUBLISH` took.

Numbers from one machine are only good for before/after comparisons. They are not a capacity figure.

## Results

**Pure writes, 100 msg/s, three runs, all valid, no message missing:**

| Run | `POST` p50 / p95 / p99 / max (ms) |
|---|---|
| 1 | 3.5 / 4.2 / 5.7 / 19.8 |
| 2 | 3.5 / 4.3 / 6.6 / 23.0 |
| 3 | 3.5 / 4.2 / 6.4 / 28.7 |

The p99 is about 30 times below the 200 ms threshold.

**Mixed scenario, one room, viewers and message rate both growing:**

| Viewers × msg/s | Valid | `POST` p99 | Frames sent / s | `CreateMessage` mean | Whole machine |
|---|---|---|---|---|---|
| 100 × 10 | yes | 14.3 ms | 1 k | 2.8 ms | 0.1 cores |
| 500 × 20 | yes | 15.8 ms | 10 k | 2.3 ms | 0.3 cores |
| 1,000 × 50 | yes | 10.1 ms | 50 k | 0.9 ms | 1.3 cores |
| 2,500 × 100 | yes | 27.4 ms | 250 k | 0.8 ms | 5.2 of 8 cores |
| 5,000 × 100 | no, collapsed | — | needed 500 k | — | — |

`CreateMessage` is the mean of the two servers. At 2,500 × 100 none of the 1,500 connection acquires per
server found the pool empty (the 5-second snapshots never showed more than 1 of 8 connections in use), the
insert took 0.8 ms, each of the two rate-limiter calls about 0.5 ms and `PUBLISH` 0.4 ms. The cores went to
sending each message to every viewer, which grows with viewers × messages. Writes stayed between 0.8 and
3 ms and did not get slower with load, while the machine went from 0.1 to 5.2 cores. The lighter steps
being slower (2.8 ms against 0.8 ms) was not investigated; a mostly idle CPU running at lower clock speed
is a guess. It does not affect the decision.

**5,000 viewers at 10 msg/s** (50 k frames/s) was valid: `POST` p99 27.6 ms, no message missing, 1.5 cores.
5,000 connections are not the problem by themselves.

## Why 5,000 × 100 collapses: the test machine runs out first, and a server dies with it

The step was run twice and collapsed both times:

| Run | `POST`s answered 201 | Connections closed | CPU during the measured 30 s |
|---|---|---|---|
| First (mixed step 5) | 59 of 3,000, most timed out at 10 s | 1,150 | servers 0.17 cores each, tool 0.16 |
| Second (diagnostic re-run) | 1,003 of 3,000 | 2,691 | one server crashed (below) |

The low CPU in the first run is misleading. The collapse started during the 5-second warm-up, before the
measured window, so the window only saw what was left afterwards. The second run sampled the machine once a
second (the tool had warned that the machine was not idle when it started, which is acceptable for a
diagnostic run):

```
11:46:12  "requests for memory denied" = 5454   PING Redis from the Mac: 5.7 ms
11:46:13                                         126.7 ms
11:46:14                                         923 ms
11:46:15                                         timeout (both pings time out, 3 s + 4 s, so the next sample is 7 s later)
11:46:22  "requests for memory denied" = 7617
11:46:34                                         2.2 ms again
```

The server logged its first `publish to redis failed` at 11:46:20, seven seconds after the Mac's own
connection to Redis had started to slow down. So the machine's networking degraded before the server
reported anything.

**One server crashed.** Its log ends with `runtime/cgo: pthread_create failed: Resource temporarily
unavailable` and `SIGABRT`. The goroutine dump shows about 2,000 goroutines in `[syscall]`, almost all in
`syscall.Write` under gorilla's `WriteMessage`: WebSocket writes that the kernel did not return from. Go
gives every goroutine blocked in a syscall its own OS thread and starts new threads so other goroutines can
run, so the thread count grew until macOS refused to create more (the highest thread number in the dump is
2048). The other server and the earlier runs did not crash.

The explanation that fits all of this: the tool reads 500 k frames/s more slowly than the servers write
them, so unread bytes pile up in the kernel's socket buffers for 5,000 connections. Those buffers come out
of the kernel's network memory (mbufs), a pool shared by every socket on the machine. When it runs out,
socket writes everywhere stall: the servers' writes to viewers, their connections to Redis and Postgres,
the Docker port forwarding. Everything waits on I/O, so the CPU looks idle, and because the pool is shared
the failure is a cliff rather than a slowdown. The server's slow-client protection (kick a connection whose
outbox is full) never fired: an outbox only fills while new messages keep arriving, and Redis Pub/Sub was
stalled too, so few did. Even when it fires, closing the connection does not release a thread stuck inside
`write()`.

How strong the evidence is:

- The writes stuck inside the kernel, in the crash dump, are the most direct evidence.
- The kernel's "requests for memory denied" counter (`netstat -m`) went from 5454 to 7617 between the
  samples at 11:46:15 and 11:46:22, and did not move in the 5,000 × 10 run or at any other time it was
  sampled. That is consistent with the explanation, but the ping had already slowed at 11:46:13, so it
  does not show that memory ran out *before* things stalled. The starting value of 5454 was already there
  when sampling began; it probably comes from the first collapse, which was not sampled.
- It could not be shown directly that Redis itself stayed healthy. `docker exec redis-cli ping`, meant as
  the control, also failed, because `docker exec` talks to Docker Desktop through a socket on the same Mac.

In production half of that buffering, the receive side, moves to the viewers' devices, so this exact
trigger is mostly a test-setup artifact. The send side stays in the server's kernel, and whether a Linux
server stalls the same way was not tested. What it revealed about the server is real, though: when the kernel blocks socket writes, the server
does not slow down, it uses one thread per stuck connection until the process dies.

## Decision

**Keep synchronous writes and don't build M7.** Writes are not the first bottleneck by a wide margin. If
anything needs optimizing first it is fan-out: the per-viewer work of encoding and writing every message.

The baseline above, mixed steps 1–4 and the 5,000 × 10 run, is what M10 compares against after its fault
scenarios.

## Alternatives considered

- **Build M7 anyway, "because chat systems do".** Pays the costs listed in Context for a problem the
  numbers don't show.
- **Skip M6 and decide by intuition.** The intuition before running was "waiting for a database
  connection will be the most expensive part". It measured 0.00 ms.
- **Move the tool to a second machine to find the server's real ceiling.** That is the right way to get a
  capacity figure. It is not needed to answer whether writes come first, and only one machine is
  available.

## Consequences

- `POST` still reaches the database, so the shadow-ban check stays in the same SQL statement as the insert
  ([ADR 0012](0012-room-moderation.md)) with no cache.
- On this Mac the local load test handled 250 k frames/s and collapsed at 500 k; the ceiling is somewhere
  in between. Larger steps need fewer frames: more rooms with fewer viewers each, or a lower rate.
- Known gaps in the tool, moved to M10: other programs' CPU is judged against a 1.0-core limit that was never
  calibrated and probably counts the Docker Desktop VM, so `fanout` stopped at its third step and `rooms`
  was marked invalid. Cleanup can run while timed-out `POST`s are still being inserted (one run left rows
  behind). The tool reports a negative CPU figure when a server process dies mid-step.
- Known issue in the server, not fixed: a write blocked in the kernel holds an OS thread, and the only cap on
  how many connections can be in that state is `WS_MAX_CLIENTS` (6,000 in this run, above macOS's 2,048
  threads per process), so the process can crash instead of degrading. The write
  deadline (`writeWait`) does not help when the `write()` call itself does not return. Only seen on the
  test Mac so far.
- Possible future work, only if real traffic shows it: cheaper fan-out. The JSON is already encoded once
  per message and shared by every connection; what remains per viewer is one WebSocket write each. Options
  are batching several messages into one frame, or spreading viewers over more servers.
