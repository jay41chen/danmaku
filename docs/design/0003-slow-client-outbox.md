# 0003 — Slow clients must not stall broadcast: per-connection outbox

**Status**: Accepted (2026-11-03)

## Context

All Redis messages and subscription confirmations are read by **one** consumer goroutine per server. It
broadcast each message synchronously, writing to every client while holding the global lock, with a write
timeout of 10 s.

A single slow client (a phone in a tunnel, a full TCP buffer) could block that goroutine for 10 s. During
that time:

- broadcasts to every other room queued up behind it;
- subscription confirmations couldn't be read, so the first viewer of any room hit the 200 ms join timeout;
- the global lock was held, so every `join_ok` and `auth_ok` reply stalled too;
- go-redis's 100-message channel buffer filled up and started **dropping** messages.

The first idea, "use a lock per room", was wrong: the blocked thing was the single consumer goroutine
itself, not the lock.

## Decision

Isolate slow consumers, following the `writePump` from the gorilla/websocket chat example:

- Each connection has an **outbox** (`chan []byte`, capacity 256) and a **writeLoop**, the only goroutine
  allowed to write to that connection.
- Broadcasts and replies only **enqueue** messages, non-blocking (`select` with `default`). No socket I/O
  ever happens under the global lock.
- **Outbox full → close the connection.** The client reconnects and fetches the history.
  - Why not drop the oldest messages: the client stays connected but never learns it missed something, and
    the server has to track what it dropped. Closing is one rule, and the client always knows to catch up.
  - Why 256: a busy room at about 50 msg/s gets roughly 5 s of slack. A larger buffer only discovers a dead
    client later.
- Messages are encoded once, before enqueueing, and all outboxes share the same `[]byte`. That is safe
  because nobody writes to it.
- **The read loop is the only owner of cleanup.** writeLoop, pingLoop and enqueue only call `Close()`,
  which wakes the read loop out of `ReadMessage`.
- The outbox is never `close`d, because it has several senders; closing it would make a later send panic.
  writeLoop exits on a shared `done` channel instead.

## Consequences

- Accepted: a burst of more than 256 messages, faster than the writeLoop can drain them, also disconnects
  healthy clients. Reconnecting and catching up handles it.
- Tests: a client that never reads, combined with 64 KB messages, fills the TCP buffer. Each broadcast to
  the healthy client must still be delivered in under 500 ms (the old design stalled for 10 s). Six
  mutations (blocking send, no close when full, synchronous broadcast, a nil channel, …) all fail the tests.
- This led to splitting the WebSocket code into a transport-only `internal/ws` (Hub, Client) and the
  business logic (`handler/ws_session.go`).
