# 0001 — Per-room Pub/Sub with lazy subscribe and debounce

**Status**: Accepted (2026-10-30). The locking part was later replaced by [0002](0002-reconciliation-loop.md).

## Context

Several Go servers sit behind a load balancer. A viewer's WebSocket can land on any of them, while the
`POST` that creates a message can land on another. Redis Pub/Sub carries each message from the server
that stored it to every server that has viewers in that room.

The first version used **one global channel** and filtered by room on each server. Every server therefore
received the traffic of every room. With 10 servers and 1,000 messages/s site-wide, each server handled all
1,000/s, so adding servers didn't reduce the load per server.

## Decision

- **One channel per room**: `room:{public_id}`. Redis does the filtering, and a server only receives rooms
  it subscribed to.
- **Lazy subscribe, based on this server's own connections only.** A server sends `SUBSCRIBE` when the
  room's local viewer count goes 0→1, and `UNSUBSCRIBE` when it goes 1→0. Whether *other* servers have
  viewers is irrelevant, because each subscription belongs to one server's Redis connection.
- **Debounced unsubscribe (30 s).** Unpopular rooms flap between 0 and 1 viewers. With 20,000 rooms × 10
  servers that is ~400k SUB/UNSUB per second, more than a single Redis can take. The viewer count drops to
  0 immediately, so it stays accurate, but `UNSUBSCRIBE` is delayed. `SUBSCRIBE` is never delayed.
  - A timer that has already fired can't be cancelled with `Stop()`: its callback may already be waiting
    for the lock. Each timer therefore carries a **generation number**, and the callback re-checks it, plus
    the viewer count, after taking the lock. This is the same trick a search box uses to drop out-of-order
    responses.
- **Joining a room happens over the WebSocket itself** (a `join` frame), not over an HTTP API. HTTP and WS
  are separate connections and the load balancer may route them to different servers. To change the state
  of a connection, the information has to arrive on that connection.
- **`join_ok` waits until Redis confirms the subscription.** go-redis `Subscribe()` returns as soon as the
  command is written. Without waiting, this sequence loses message M on both paths:

  ```
  t0 join returns (not yet subscribed) → t1 join_ok → t2 client fetches history (no M yet)
  t3 M saved → t4 PUBLISH M (nobody subscribed) → t5 Redis processes SUBSCRIBE
  ```

  Each room entry has a `ready` channel. It is closed when go-redis delivers the `*redis.Subscription`
  confirmation for that channel (`ChannelWithSubscriptions`). The waiting happens outside the global lock.

## Alternatives considered

| Option | Why not |
|---|---|
| One global channel + filter on each server | Every server receives all traffic, so it doesn't scale horizontally |
| Per-room keyed mutex + refcount | Reading the go-redis source showed that `Subscribe` and `Unsubscribe` already serialize on the PubSub's own mutex, and there's only one PubSub per server, so per-room locks buy no parallelism. Lesson: find the bottleneck before adding concurrency control |
| `?room=` on the WS URL | Two ways to join a room. A frame-based join can later be relaxed to multi-room without changing its semantics |
| PING / `NUMSUB` to detect "subscribed" | go-redis swallows pongs (and sends its own health-check PINGs); `NUMSUB` counts all servers, not this one |

## Consequences

- Redis `PUBSUB NUMSUB room:X` counts **servers**, not viewers. Its upper bound is the number of servers.
  Verified on 2 local servers with `MONITOR`.
- Pub/Sub is at-most-once. Messages published before `join_ok` are recovered by fetching history after
  `join_ok`. Replay across reconnects is planned (ROADMAP M5).
- `TestRoomPubSub` went from 5 failures in 30 runs to 50/50 passing after `join_ok` started waiting for the
  confirmation.
- Verified by pausing Redis (`CLIENT PAUSE`): with a 150 ms pause, `join_ok` arrives only after Redis has
  processed the subscribe. With a 500 ms pause, the client gets `ws_join_unavailable` and retries with
  backoff and jitter.
