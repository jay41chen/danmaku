# 0002 — Reconciliation loop: a stuck Redis must not freeze the server

**Status**: Accepted (2026-11-02). Replaces the locking scheme in [0001](0001-per-room-pubsub.md).

## Context

Each server tracks its room subscriptions in memory (`roomSubscriptions`). The first version sent
`SUBSCRIBE` / `UNSUBSCRIBE` **while holding the global lock**, so that changing the viewer count and sending
the command stayed atomic. Fault-injection experiments found three failures:

- **B1** — When Redis black-holes traffic, go-redis holds its own lock while reconnecting, for seconds. The
  goroutine holding our lock waits with it, so every Join and Leave on the server stalls for 5–15 s, even
  the purely in-memory ones. `sync.Mutex` doesn't take a context, so the 200 ms join timeout can't cover
  the time spent waiting for the lock.
- **B2** — A goroutine that queued for the lock already has an expired context when it gets the lock. Its
  write times out, go-redis decides the connection is broken, and it tears down the **whole** PubSub
  connection. Every room then loses messages, so one slow room becomes a server-wide incident.
- **B4** — If a subscription confirmation is lost, the room waits forever. Anyone joining it within the
  next 30 s gets `ws_join_unavailable`.

## Decision

Use a **reconciliation loop**, the same pattern as a Kubernetes controller.

- **Event side** (Join / Leave / timer expiry / confirmation; many goroutines): under the lock it only
  updates memory and marks the room **dirty**. It never does I/O.
- **Worker** (one goroutine per server): takes the dirty set, compares *desired* with *actual*, and sends
  commands **outside the lock**.
  - *Desired* is computed, not stored: `viewers > 0 || debounceTimer != nil`.
  - *Actual* is a small state machine:

| state | meaning |
|---|---|
| `subIdle` | not subscribed, or UNSUBSCRIBE already decided |
| `subSent` | SUBSCRIBE written, waiting for confirmation |
| `subConfirmed` | confirmed; waiters released |
| `subRetry` | SUBSCRIBE sent but not confirmed (write failed / confirmation timed out): **maybe** subscribed |

- The worker always acts on the **current** desired state instead of replaying events. A
  SUB → UNSUB → SUB burst therefore collapses into nothing, and commands can't be reordered because only
  one goroutine sends them.
- Each reconcile has three steps, and each step takes and releases the lock on its own:
  1. **decide**: update state *before* sending, because a confirmation can arrive before `Subscribe()`
     returns.
  2. **send**: uses only local variables.
  3. **record**: re-reads the entry and always marks it dirty again, because desired may have changed
     during the I/O (TOCTOU). This doesn't loop forever: once desired equals actual, the next decision is
     "nothing to do".
- Commands get their context **when they're sent** (500 ms), not when the event happened, so time spent
  waiting doesn't count against the timeout (fixes B2). An unconfirmed SUBSCRIBE moves to `subRetry` after
  500 ms and is resent; SUBSCRIBE is idempotent (fixes B4). Write failures back off from 100 ms, doubling,
  capped at 5 s.

## Why `subRetry` exists

Tests exposed a design hole. A failed SUBSCRIBE originally went back to `subIdle`, so when everyone left
the entry was deleted without sending UNSUBSCRIBE. But go-redis records the channel **even when the write
fails**, and resubscribes to it on reconnect. The server would then stay subscribed forever. The fix is to
record *actual* as the worst case: an uncertain state gets its own state and is never folded into
"definitely not subscribed".

## Alternatives considered

| Option | Why not |
|---|---|
| Work queue (buffered channel) of events | When the queue is full, the writer, who holds the lock, blocks, which is B1 again. The dirty **set** (`map[string]struct{}`) is bounded by the number of rooms and never blocks |
| Per-room locks | We need isolation, not parallelism (one go-redis PubSub serializes commands anyway), and two lock levels add a TOCTOU window between them |
| Multiple workers | No parallelism to gain for the same reason |

## Consequences

- Known limitation: **head-of-line blocking**. One stuck command, for at most 500 ms, delays other *new*
  rooms. Rooms that already have viewers are unaffected.
- `TestRoomSubscriptionsStuckSubscribeDoesNotBlockJoinLeave` locks in the B1 fix.
