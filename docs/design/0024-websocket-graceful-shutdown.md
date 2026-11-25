# 0024 — On shutdown, send every WebSocket client 1001 Going Away and wait for the close handshake, bounded by the shutdown deadline

**Status**: Accepted (2026-11-25).

## Context

`http.Server.Shutdown` closes the listeners and waits for in-flight requests, but it stops tracking a connection as
soon as it is hijacked, and a WebSocket upgrade is a hijack. So on every deploy the process exited with all
WebSocket connections still open, and clients saw an abnormal close (`1006`) instead of being told the server was
going away.

A WebSocket close is a two-way handshake: one side sends a close frame, the other replies with one, and only then is
TCP closed. Browsers report `wasClean = true` only if that exchange finished before TCP went away. gorilla's
`conn.Close()` closes TCP without sending a close frame at all.

A first version (commit `1d617a2`) sent `1001` and closed the connection right after. Clients got the `1001` code but
`wasClean = false`, because the server closed TCP before the client's reply arrived.

Constraints:

- The whole shutdown has a 10 s budget (`shutdownCtx` in `main.go`), shared by `server.Shutdown` and the WebSocket
  close. The platform kills the process after its own grace period.
- A client with a dead network never replies. Waiting for it without a limit would run into that kill.
- Per-connection cleanup does not end at `Hub.Unregister`: `leaveRoom` (Redis presence, UNSUB) runs after it. Exiting
  before `leaveRoom` leaves a stale presence entry until its 30 s TTL expires.

## Decision

`main.go` calls `server.Shutdown(shutdownCtx)` first. This closes the listener so a client reacting to `1001` cannot
reconnect to this server. It then calls `Hub.CloseAll(shutdownCtx)`.

| Step | Behavior |
|---|---|
| Track each connection | `Hub.TrackConn()` does `wg.Add(1)` and returns `wg.Done`. `HandleWebsocket` calls it **before** `Upgrade` and defers the returned function **first**, so it runs last: the count drops only after `Unregister`, `leaveRoom` and `client.Close` have run. |
| Notify | `CloseAll` sends `1001 Going Away` to every client, each in its own goroutine, and does **not** close the connection. The client's reply ends the read loop, and the normal cleanup path runs. |
| Wait | Wait until the WaitGroup reaches zero or `ctx` expires, whichever comes first. `WaitGroup.Wait` takes no context, so a goroutine runs `Wait()` and closes a channel, and `CloseAll` selects on that channel and `ctx.Done()`. |
| Force close | If `ctx` expires first, take a fresh snapshot of the clients still registered, `Close()` each one, and log one warning with the count. |
| Write deadline | The close frame's write deadline is `min(now + writeWait, ctx deadline)`, so one stuck write cannot outlive the shutdown budget. Failures after `ctx` has expired are not logged per connection; the force-close warning already carries the count. |

Why `TrackConn` runs before `Upgrade`: until the hijack, the request is still in-flight, so `server.Shutdown` waits
for it. That puts the `Add(1)` before `Shutdown` returns, and therefore before `CloseAll` starts waiting. Placed after
`Upgrade`, a connection upgraded while `Shutdown` is running could be counted only after `CloseAll` had taken its
snapshot. It would miss the `1001`, and its `Add` could race with `Wait` at a zero count. If `Upgrade` fails, the
deferred release still runs.

`CloseAll`'s `ctx` must have a deadline or be cancelable. Otherwise one unresponsive client blocks it forever.

## Alternatives considered

- **Close TCP only** (the behavior before this work): clients see `1006` and cannot tell a deploy from a crash.
- **Send `1001`, then close immediately** (commit `1d617a2`): the code arrives, but the handshake never completes
  (`wasClean = false`).
- **Wait without a limit**: one client with a dead network holds shutdown until the platform kills the process, and
  the force-close step never runs.
- **Send `1001` to clients one by one**: a single slow client at the front of the list can use up the whole budget,
  so the others get no close frame and no time to reply.
- **Count in `Register` / `Unregister`**: `leaveRoom` runs after `Unregister`, so the wait could finish and the
  process exit before presence was cleaned up.
- **`Add` in `NewClient`, `Done` in `Client.Close`**: `Close` is called from many places (kicks, write failures, the
  force-close itself) and also runs before `leaveRoom`.
- **A separate `UntrackConn()` method**: any caller could decrement without having incremented. Returning the
  decrement from `TrackConn` keeps the two paired, like `context.WithCancel`'s `cancel`.
- **`RunTracked(fn func())`** wrapping the handler body: same pairing guarantee, but the whole handler moves into a
  closure.

## Consequences

- Verified with Node 22's built-in WebSocket, which follows the same spec as browsers: two clients both closed with
  `code=1001 wasClean=true`. The server took the normal path, with no force-close warning, and finished about 2 ms
  after the signal.
- Tests: `TestCloseAllWaitsForCloseHandshake` checks that `CloseAll` returns before the deadline and that cleanup has
  already finished. `TestCloseAllForceClosesUnresponsiveClient` checks that a client that never replies is closed
  when `ctx` expires and still cleaned up. Mutation testing confirmed both: skipping the wait fails both tests, and
  skipping the force close fails the second.
- No test covers sending `1001` in parallel; making one connection's write block reliably is complex.
- Shutdown can now take up to the remaining `shutdownCtx` budget when clients don't reply. Before, it was almost
  instant.
- Known limitation: on the force-close path `CloseAll` returns right away and the process exits. Force-closed
  connections may not reach `leaveRoom`, so their presence entries linger until the 30 s TTL expires
  ([0010](0010-room-presence.md)).
- If `server.Shutdown` itself fails, `main` exits without calling `CloseAll`. This is existing behavior.
- The client decides whether to reconnect from whether it closed the socket itself, not from `code` or `wasClean`,
  so no client change was needed.
