# 0011 — Message delivery recovery: catch up on reconnect, detect a deaf Pub/Sub connection

**Status**: Accepted (2026-11-05)

## Context

Every message is committed to Postgres before it is broadcast, so a lost broadcast never loses data. The
problem is that a viewer can miss it without anyone knowing:

| # | What happens | Before this change |
|---|---|---|
| 1 | The client's WebSocket drops for a few seconds | Reconnect and fetch the latest 50, about 1 s of a busy room at 50 msg/s |
| 2 | A slow client's outbox fills and it gets disconnected | Same as #1 |
| 3 | The server's Pub/Sub connection reconnects while WebSockets stay up | Messages during the gap are gone; the client never fetches |
| 4 | The Pub/Sub connection goes half-open: the peer vanishes without a RST | **Every room on that server silently stops receiving** until TCP gives up |
| 5 | `PUBLISH` fails, or go-redis drops a message because its buffer is full | Stored and answered with 201, but nobody is told |

Case 4 is the worst. HTTP and WebSocket keep working, including pongs, and nothing is logged. go-redis does
send a PING every 3 s, but it reconnects only when the write fails, and it discards the PONG
(`initAllChan`, v9.23), so a connection that accepts writes and delivers nothing looks healthy.

This is a danmaku product: comments are read as they fly past. An old message nobody saw is not worth much,
so the goal here is not "every message is delivered". It is **a short interruption repairs itself, and a
server that has gone deaf recovers without a person noticing first**.

## Decision

**1. Catch up by fetching the latest 100 messages, with no `after` cursor.** It reuses the existing endpoint
and existing dedupe by id. At 50 msg/s, 100 messages cover about 2 s, enough for a brief interruption. Longer
gaps are skipped without telling the user. 200 was measured and rejected: see Verification.

**2. Mark the response cacheable for shared caches only: `Cache-Control: public, s-maxage=1`.** The latest
100 messages are identical for everyone in a room, so a cache is a better shield for the database than a
second copy of the messages. `s-maxage` leaves browsers out, so a client asking again after `resync` won't
reuse its own old copy, and errors are never cached. Production has no CDN yet, so today this line does
nothing. Adding one later needs no application change. The TTL is short because a snapshot older than the gap
cannot contain the gap.

**3. Back off until the room is really joined.** The server upgrades before it checks `maxClients`, so a
rejected connection still fires `onopen`. Resetting the backoff there meant it never grew: 900 rejected
clients retried about 1,200 times a second, each with a GET. The attempt count now resets on `join_ok`.

**4. A watchdog proves the receive path by using it.** Every 15 s the server publishes to its own
`heartbeat:<random>` channel over the normal connection and expects to receive it on the Pub/Sub connection.
The check happens on the next tick:

```
received since the last tick                → misses = 0
not received, and the last publish succeeded → misses++
last publish failed, or nothing published yet → unchanged
misses reach 2                               → rebuild the Pub/Sub connection, misses = 0
always, last                                 → publish the next heartbeat
```

A failed publish does not count, because that means Redis itself is unreachable, not that this connection is
deaf. Rebuilding wouldn't help, and go-redis already handles that case. Detection takes 30–45 s.

**5. Rebuilding reuses the reconciliation worker** ([ADR 0002](0002-reconciliation-loop.md)). go-redis has no
public way to force a reconnect, so the watchdog swaps in a new `PubSub`, then closes the old one. Subscribed
rooms live in `roomSubscriptions`, so the reset only changes the recorded state and the existing worker does
the resubscribing. Two details matter:

- **The reset runs only after the old channel is drained**, triggered by the consumer loop. A subscribe
  confirmation still buffered from the old connection must not be taken for one from the new connection,
  or a viewer would get `join_ok` before the room is subscribed.
- **Confirmed rooms get a new `ready` channel.** The old one is already closed, so without this a new viewer
  would pass straight through while nothing is subscribed.

**6. Clients learn about the gap from a `resync` frame.** A deaf period leaves the WebSocket up, so nothing on
the client would trigger a fetch. Once a room is confirmed on the new connection, the server sends `resync`
to it. Sending it earlier would open a new gap between the fetch and the subscription. It goes only to rooms
that had viewers when the rebuild happened, at most once per rebuild.

**7. Metrics measure the indirect signals.** A client can't report a message it never knew about, so "messages
missed" isn't directly measurable. These are unlabelled, per [ADR 0007](0007-metrics-cardinality.md):
`redis_publish_failed_total`, `redis_pubsub_deaf_total`, `redis_pubsub_resubscribe_duration_seconds`,
`websocket_connections_total`, `websocket_resync_sent_total`. First connections and reconnections can't be
told apart, and once a CDN is in place, cached GETs won't appear either.

## Alternatives considered

- **`GET ...?after=<id>`.** Nothing needs it once gaps may be skipped, and it has two problems. `ORDER BY id
  ASC LIMIT n` returns the *oldest* part of a large gap, the opposite of what's wanted. And ids are assigned
  before commit: if 101 commits before 100, a client that saw 101 asks for `after=101` and never gets 100.
- **Keep recent messages in a Redis List.** Every message costs an extra write, each GET still queries the
  room, and an empty list can't distinguish "no messages" from "Redis restarted". When `PUBLISH` fails because
  Redis is down, the list is missing that message too, while Postgres has it. Postgres handled 4,000 GET/s in
  the local test, so the second copy solves a problem not yet measured.
- **A client timer that refetches after silence.** It only fires when a room is completely quiet, so it misses
  partial loss in a busy room, and no single interval suits both a busy room and a quiet one. A deaf server
  would also stay deaf. Dropped once `resync` covered the case that mattered.
- **PING and wait for the PONG.** It would prove the same path, but go-redis hides PONGs from
  `ChannelWithSubscriptions`. Seeing them means rewriting the receive loop.
- **Kick every connection on detection.** No new frame, but everyone reconnects at once, viewer counts jump
  (see [ADR 0010](0010-room-presence.md)), and the other server's `maxClients` may reject them.
- **`TCP_USER_TIMEOUT` on the dialer.** Linux-only, and it can't detect a proxy that keeps acknowledging while
  delivering nothing, so it wouldn't test the path that matters.
- **TTL of 30–60 s.** Far fewer origin requests, but the snapshot would almost always predate the gap: with a
  5 s gap the chance of recovering nothing is about 83% at 30 s, by calculation.

## Verification

- 8 watchdog tests with fake publish, rebuild and ticker. `run` was written by hand against them.
- A real-Redis integration test, `TestRebuildPubSubWiring`, covers the wiring end to end. An earlier review
  found that deleting the reset still left every test green, so the wiring mutations were re-run: all are now
  caught.
- Half-open, on the final code, with a temporary TCP proxy that keeps the connection open and stops forwarding.
  One run per scenario, 18 checks passed, with a headless browser in the room for the first scenario:

| Scenario | Result |
|---|---|
| Only the Pub/Sub connection goes deaf | `resync` after 35.0 s, sent once; the browser showed the missed message without a reload |
| All existing connections go deaf | Recovered after 44.1 s; the missed message was retrievable |
| New Pub/Sub connections are deaf too | Rebuilt at intervals of 29.9 s and 29.8 s; a new `join` got `ws_join_unavailable`, not a false `join_ok`; no `resync` |
| Network restored | Recovered by itself after 7.4 s |

- Reconnect backoff, with a simulated client following the same logic, 1,000 clients against one server: about
  1,200 retries/s before, about 36/s once settled.
- Cache, through a temporary nginx, with 1,000 concurrent GETs per round: at most 1 reached the server with
  `proxy_cache_use_stale updating`. With `proxy_cache_lock` alone, **all 1,000 did** once the entry had expired,
  because the lock only covers entries that don't exist yet. Run with 200 messages per response; the cache
  behaviour doesn't depend on the count. 404s were not cached.
- GET latency against one local server, 5,000 messages in the room, all on one Mac, so comparisons only:

| Rate | 50 messages, p99 | 100 messages, p99 | 200 messages, p99 |
|---|---|---|---|
| 1,000/s | 5.2 ms | 4.2 ms | 17.6 ms |
| 2,000/s | 5.4 ms | 74.4 ms | 227.0 ms |
| 4,000/s | 23.1 ms | 792.0 ms | 2,463.4 ms |

No errors at any rate. In the 200-message 4,000/s run the load generator itself lagged 847 ms, so part of that
number may be the generator.

## Consequences

- **A deaf period loses up to 45 s of live delivery.** `resync` restores only the latest 100, so a busy room has
  a hole. Accepted: the alternative is a server that stays deaf.
- **Case 3 is not addressed.** When go-redis reconnects by itself, 1–2 messages can be missed, and in a busy
  room later messages keep arriving so nothing looks wrong. Accepted. The same `resync` frame could cover it
  cheaply if that changes.
- **Case 5 waits for the next fetch.** In a quiet room that can mean until the page is reloaded.
- **A CDN needs its own stampede setting.** The header alone won't prevent the thundering herd at expiry, as the
  nginx result shows, and each catch-up may then miss the last second or so.
- **Each reconnect transfers about 24 KB**: two GETs at about 12 KB, assuming short messages and no compression.
- **`publishHeartbeat`'s 500 ms context doesn't bound socket I/O** because `ContextTimeoutEnabled` isn't set: it
  took about 5 s against an unresponsive server. The watchdog tolerates this. The same behaviour affects message
  `PUBLISH`, where `POST` can take about 5 s; that was already there and is tracked in the ROADMAP.
- A false positive, such as the consumer falling more than about 30 s behind or go-redis dropping heartbeats,
  costs one extra rebuild and one round of fetches. `redis_pubsub_deaf_total` makes this visible.
- If the deafness persists, the watchdog keeps rebuilding about every 30 s for as long as it lasts. There is one
  watchdog per server, so the rate doesn't grow with viewers. A climbing counter means a person needs to look.
- Rare leftover: a room can stay subscribed with no viewers if it expires in the milliseconds between the swap
  and the reset. It only wastes a little traffic, and it clears the next time someone enters that room. Inferred
  from the code, not observed.
