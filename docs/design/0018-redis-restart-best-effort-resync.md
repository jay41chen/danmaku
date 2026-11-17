# 0018 — Redis restarts: best-effort delivery, and a resync when go-redis reconnects by itself

**Status**: Accepted (2026-11-17)

## Context

[ADR 0011](0011-message-delivery-recovery.md) left two cases unhandled:

- **Case 3**: go-redis reconnects by itself while WebSockets stay up. Estimated at "1–2 messages".
- **Case 5**: `PUBLISH` fails. The message is stored and answered with 201, but nobody is told.

M10b measured both by taking the fault environment's Redis away mid-run ([ADR 0017](0017-stuck-clients-fault-env.md)
describes the environment).

**Setup**
- 10 viewers in one room, five on each of two servers.
- One poster sending 5 numbered messages per second for 120 s, alternating servers.
- The load tool compares the messages that got 201 with what each viewer received over WebSocket and through
  history fetches after a `resync`.

**Two ways to take Redis away**
- `stop`: the process is gone. Connections fail.
- `pause`: the process is frozen. Connections stay open and get no answer.

Before any change (single runs):

| Run | POSTs | Missing per viewer | Recovered | Notes |
|---|---|---|---|---|
| stop 10 s | 600 × 201 | 52 | 0 | `publish to redis failed`: 25 + 26 lines |
| stop 60 s | 600 × 201 | 303 | 0 | 151 + 152 lines, matching the even/odd split of the missing seqs |
| pause 10 s | 598 × 201, 2 failed | 0 | — | Failures at the tool's 10 s timeout |
| pause 60 s | 347 × 201, 253 failed | 0 | — | A rerun with more counters: 346 × 201, 254 failed (below) |

No run sent a `resync` or rebuilt a Pub/Sub connection, and no viewer disconnected.

**`stop` loses messages silently, and the loss grows with the outage.**
- Every POST during the outage commits to Postgres, gets 201, and fails to publish. The 60 s run matches
  exactly (303 missing, 151 + 152 failed publishes). The 10 s run has one more missing than failed (52 against
  25 + 26). Most likely its last message was published just after Redis returned and before go-redis had
  re-subscribed, so it reached no subscriber. That is inferred: the server doesn't log successful publishes.
- When Redis returns, go-redis reconnects and re-subscribes on its own. Redis sends a fresh subscribe
  confirmation for every room. The server receives those (`ChannelWithSubscriptions`) but ignored a confirmation
  for a room that was already confirmed.
- So no `resync` went out. The poster saw success, and viewers saw no error and no gap marker.
- Case 3's "1–2 messages" holds only for a blip. When Redis is down for the whole gap, every message in it is
  also case 5.
- The watchdog never fires here: go-redis keeps delivering heartbeats once it reconnects.

**`pause` loses nothing that was acknowledged.**
- The rate limiter's two Redis calls hang until go-redis times out. Fail-open only helps once the error comes
  back, so POSTs stall.
- The poster gives up at 10 s, and `net/http` cancels the request context. The rerun counted 127 + 127
  `create message failed` lines, one per failed POST. Their error was `context canceled` when checked in the
  server logs at the time; the logs were not saved.
- The tool saw no message over WebSocket that hadn't got a 201 (`received_not_201` = 0), and no publish
  failed. So a POST the client gave up on was not stored. That is inferred from these two counts; the database
  was not queried.
- The watchdog doesn't fire here either: it skips a round when its own heartbeat publish fails, and every one
  failed.

## Decision

**1. Delivery across a Redis outage is best-effort.**
- The product is a danmaku stream: reactions to what is happening now. A ten-second-old "lol" has little value.
  A chat log, where a gap breaks the conversation, would need a different answer.
- So the goal is "recent context comes back", not "every message arrives".
- Loss stays visible through metrics, not guarantees.

**2. A confirmation for an already-confirmed room is treated as a reconnect** (`confirmSubscribed`, `internal/redisstore`).
- The room's state doesn't change.
- If the room has viewers on this server, the server sends the existing `resync` frame. The client already
  re-fetches the latest 100 messages on `resync`.
- New counter `redis_pubsub_reconfirm_total`: +1 per such confirmation, per room.
- Duplicate confirmations have other sources, and they also trigger a `resync`. We accept these false
  positives: each costs every viewer in the room one extra fetch.
  - The confirm timer resends a `SUBSCRIBE`.
  - A `SUBSCRIBE` write fails while Redis is down. go-redis still records the channel, and the later resend can
    re-subscribe all channels and then this one again.

**3. The client waits a random 0–2 s after `resync` before fetching.**
- If a delayed fetch is already pending, a new `resync` is ignored. If a fetch is already in flight, a new
  `resync` schedules another one.
- Two reasons:
  - Every viewer of a room gets the `resync` at the same moment, and the delay spreads their fetches.
  - Publishing can recover later than subscribing (below), and a later fetch covers more of that.
- The load tool mirrors this (`-redis-resync-delay-max`, default 2 s).

**4. The load tool and the fault scripts.**
- `loadtest -scenario redis` reports missing, recovered and final missing per viewer, and `received_not_201`.
- For each history fetch it records the seq range it returned, and it counts final-missing messages older than
  the latest fetch, which the 100-message window could not have covered.
- `deploy/fault/redis.sh <stop|pause> <seconds>` takes Redis away mid-run, always restores it, and counts the
  relevant server log lines.

## Alternatives considered

- **Per-room sequence numbers and gap detection in the client.**
  - The most complete: the receiving end checks continuity, so a loss anywhere in the path is caught (the
    end-to-end argument).
  - Postgres sequences can't be used: a rolled-back transaction leaves a gap that looks like a loss. A per-room
    counter has to be incremented in the same transaction as the insert, which serializes writes to a room.
  - A quiet room still waits for the next message to notice.
  - Rejected for now: it needs a schema change, and best-effort doesn't need it.
- **Catch up from a cursor (`after=<last id>`) instead of the latest 100.** This removes the 100-message limit.
  - The global `id` is assigned at insert, not at commit. A lower id can commit after a higher one the client
    already saw, and if its publish also fails, a cursor skips it forever. Overlapping the cursor would cover
    that.
  - A long outage then means many pages, mostly of messages too old to matter.
  - Rejected with the product decision above.
- **The server that failed to publish sends a `resync` later.**
  - It knows the publish failed. But the only way to reach viewers on other servers is the Redis that just
    failed.
  - So it would remember "room X has unsent messages since T" and publish a `resync` to the room after its next
    successful publish there.
  - This covers what the client delay only makes less likely (below), using one timestamp per room.
  - **Deferred**, not rejected: it is the next step if the gap matters.
- **Retry the failed message itself.** Needs a bounded queue and a fallback when it fills, and is lost if the
  server dies. A transactional outbox would survive that, at a much higher cost. Not needed for best-effort.
- **Absorb the fetch spike on the server**: singleflight, a short in-process cache, or nginx in front with
  `proxy_cache_lock` and `proxy_cache_use_stale updating`.
  - The spike was not measured.
  - A cache can also serve a snapshot from before the last messages, working against `resync`.
  - nginx is planned for the rest of M10, so decide there with numbers.

## Verification

All runs were single runs in the fault environment: Docker Desktop on an arm64 Mac, 10 viewers, 5 messages/s.

| Run | Missing per viewer | Recovered | Final missing | Notes |
|---|---|---|---|---|
| stop 10 s, reconnect fix, no delay | 55 | 53 | 2 (seqs 201, 203) | `resync` at 06:52:32.196, as Redis returned (`T_up` 06:52:32, second precision) |
| stop 60 s, reconnect fix, no delay | 302 | 99 | 203 | All 203 older than the fetched window (349–448) |
| stop 10 s, with delay, run 1 | 56 | 54.8 on average | 0–3 per viewer, 12 in total | 6 of 10 viewers fetched before publishing recovered |
| stop 10 s, with delay, run 2 | 54 | 54 | 0 | |

- **stop 60 behaves as designed.** The fetch returns the newest 100. One of those (448) had arrived over
  WebSocket, so 99 were recovered, and the 203 older ones are beyond the window.
- **stop 10 showed that publishing recovers after subscribing.**
  - server-2 kept failing to publish for 0.58 s after the `resync`, with
    `dial tcp: lookup redis on 127.0.0.11:53: no such host`.
  - Pub/Sub had reconnected, but new connections from the publish pool still failed: the errors are lookup
    failures from Docker's embedded DNS. Why the pool recovered later than Pub/Sub was not investigated.
  - Seqs 201 and 203 were committed after the immediate fetch and never published.
- **How long publishing lags varies.** The last failed publish came after the `resync` by, per server:

  | Run | server-1 | server-2 |
  |---|---|---|
  | reconnect fix, no delay | 0.06 s | 0.58 s |
  | with delay, run 1 | 0.50 s | 0.63 s |
  | with delay, run 2 | 0.05 s | 0.18 s |

  Three runs, all in this environment. The ordering can happen anywhere; these durations may not.
- **The client delay reduces this but doesn't remove it.**
  - Viewers whose random delay ended before publishing recovered missed the last 1–3 messages. Those whose
    delay ended later recovered everything.
  - In run 1 (lag about 0.5–0.6 s), 6 of 10 viewers fetched too early. With a uniform 0–2 s delay, about 3 in
    10 would be expected for a 0.6 s lag; that is an estimate, and 10 viewers is a small sample.
  - In run 2 the lag was shorter (0.05–0.18 s). Three viewers fetched within 0.6 s and still recovered
    everything, so run 2's result comes mostly from the shorter lag, not only from the delay.
- **Each change was reviewed by a separate agent.**
  - The reconnect fix passed the second round, after fixing a metric's help text that still said `resync` only
    follows a rebuild.
  - The reviewer checked the go-redis source: it re-subscribes every known channel on reconnect and delivers the
    confirmations to `ChannelWithSubscriptions`.

## Consequences

- **A Redis restart now loses only what doesn't fit in the latest 100 messages**, plus a short tail if
  publishing recovers later than subscribing. At 5 messages/s, 100 messages is 20 s. In a room at 50/s it is
  2 s, so a busy room still has a hole after almost any outage.
- **Messages whose publish fails while Redis is otherwise healthy are still silent** (case 5 without a
  reconnect). `redis_publish_failed_total` counts them. The deferred server-side `resync` would cover them.
- **The metrics are counted, not watched.** There are no alert rules, and production is down. A rising
  `redis_publish_failed_total` or `redis_pubsub_reconfirm_total` is visible only to someone who looks.
- **`redis_pubsub_reconfirm_total` overcounts reconnects.** It counts rooms, not connections, and includes
  duplicate confirmations from resends.
- **The fetch spike is unmeasured.** Every viewer of every room on a server fetches within 2 s after Redis
  returns. There is no server-side cache or request coalescing yet, and no CDN or nginx in front.
- **A paused Redis stalls POSTs for up to the client's timeout.** The rate limiter's calls aren't bounded by
  their context (`ContextTimeoutEnabled` is not set; known since ADR 0011). Nothing acknowledged was lost.
- **Not covered**: Redis down for longer than 60 s, more than one room, more than 10 viewers, a Redis failover,
  and either server dying.
