# 0023 — A panic is handled per goroutine kind: clean up and log, close one connection, or restart a worker or crash, depending on whether its state survives a restart

**Status**: Accepted (2026-11-25).

## Context

In Go, `recover` only stops a panic on the goroutine where it happens: a panic walks up that goroutine's own stack,
and if nothing there recovers it, the whole process exits. A goroutine started with `go` has a fresh stack, so the
HTTP `RecoverPanic` middleware protected only the handler's own goroutine. Before this change:

- The WebSocket `WriteLoop` / `PingLoop` (one pair per connection) and the server-wide workers (Redis consumer,
  presence, SUB/UNSUB worker, PubSub watchdog, load-test timing) had no recover. One panic in any of them would drop
  every connection on the server.
- A panic while the read loop handled a message *was* caught by `RecoverPanic`, but the cleanup after `ReadLoop`
  (`hub.Unregister`, `leaveRoom`) was plain code after the call, so the panic skipped it. The connection stayed in the
  Hub as a ghost: counted in the room on every server via presence, holding a `maxClients` slot, keeping the room's
  Redis channel subscribed, and logging `ws outbox full` on every broadcast once its outbox filled.
- `RecoverPanic` always wrote a 500. After a WebSocket upgrade (the connection is hijacked) or after the handler
  had already written, that write can't be delivered and net/http logs `WriteHeader on hijacked connection` /
  `superfluous WriteHeader`.
- `/health` checks Postgres and Redis in two goroutines it starts itself, also outside `RecoverPanic`.

Catching every panic is not automatically safer. A server-wide worker that is caught and simply stops leaves a process
that is up, passes `/health` (Postgres and Redis are fine) and keeps receiving connections, but no longer forwards
messages: the same symptom as the half-open PubSub in [ADR 0011](0011-message-delivery-recovery.md), now caused by
code.

## Decision

The policy depends on what the goroutine is responsible for:

| Goroutine | On panic | Why |
|---|---|---|
| HTTP handler, including the WebSocket read loop | Don't recover locally. Cleanup that must always happen is a `defer` registered **before** the code that can panic (`listenConnection`: `defer` Unregister + leaveRoom, then `ReadLoop`). The panic continues to `RecoverPanic`, which logs it with the stacktrace | A local recover would swallow the stacktrace and need the cleanup written twice |
| `RecoverPanic` itself | Always log; write the 500 only if the response isn't committed (`StatusCodeRecorder.Committed()`: header sent, body written, or connection hijacked). It wraps `w` in its own recorder rather than relying on an outer one | The status can't change once sent; the extra write only produces net/http warnings |
| Per-connection loops (`ws.Client.HandleWriteLoop` / `HandlePingLoop`) | Recover, log with stacktrace, `Close()` the connection | Same path as a write failure: `Close` wakes the read loop, which stays the only cleanup owner. Logging without closing would leave the connection "alive" with a frozen screen until the client's keepalive (up to about 45 s) or a full outbox kicked it |
| Server-wide workers that can be restarted (`supervise.Restart`): `StartPresence`, PubSub watchdog, `loadtiming` | Recover, log, count `worker_panics_total{worker}`, restart immediately. The 3rd panic within 1 minute (window starts at the first panic) re-panics so the process exits and the platform restarts it | Restart covers an occasional bug; giving up avoids a half-dead server |
| Server-wide workers that can't (`supervise.Escalate`): `StartBroadcast` (Redis consumer), `rooms.run` (SUB/UNSUB worker) | Recover only to log and count, then re-panic on the first panic: the process exits and the platform restarts it | Part of their state lives in the goroutine; a restarted one can't pick it up (see below) |
| `/health` check goroutines | Convert the panic into an error (`checkSafely`, named return + `defer`); `/health` answers 500 naming the failing side | Short-lived and per request: a failed check is the right answer |
| `time.AfterFunc` callbacks: WebSocket token expiry (`ws_token_expiry`), room SUB/UNSUB debounce / retry / confirm timers (`redis_room_timer`) (added 2026-11-25) | Wrapped in `supervise.Escalate` | One-shot, nothing to restart. A panic on the runtime's timer goroutine already killed the process, but with no log line or metric; now it is recorded first |

Rules that come with this:

- **Only restart workers whose state lives entirely outside the goroutine; otherwise escalate.** Presence re-syncs a
  full snapshot every 10 s (cost: a restarted worker forgets what it wrote, so members who left in between expire
  after about 30 s instead of being removed); the watchdog only needs a ticker (one accepted exception below); `loadtiming` loses at most one
  interval. Two workers looked restartable at first and are not (found in review):
  - `rooms.run` keeps desired vs actual state in `roomSubscriptions`, but takes each batch of dirty rooms into a local
    variable. A panic halfway loses the rest of the batch, and the room it was on is already marked `subSent` with no
    confirmation timer started, so every later Join in that room times out until everyone leaves.
  - The Redis consumer re-reads `this.pubsub` on restart. The watchdog's rebuild swaps in a new `PubSub` before
    closing the old one; if the consumer panics while draining the old channel, the restarted consumer reads the new
    one, the rest of the old channel is dropped and `resetAfterRebuild` never runs. Every room stays marked as
    subscribed while the new `PubSub` only has the heartbeat: the server silently delivers nothing.
  Making them restart-safe was possible (put the batch back, track which `PubSub` is being consumed) but adds
  complexity to the most delicate code for a rare case; a clean process restart already rebuilds all of it, and
  clients reconnect and `resync`.
- **Accepted exception: the watchdog's `old.Close()`.** `rebuildPubSub` swaps in the new `PubSub` and then closes
  the old one. If go-redis's `Close` itself panics, the old `PubSub` stays open and the consumer keeps reading the
  deaf connection; restarting the watchdog can't fix it, because later rebuilds only replace the previous fresh one.
  Making the watchdog `Escalate` would remove this, but then any bug in our own watchdog logic would restart the
  whole server. We keep `Restart`: the window needs a panic inside go-redis, and if it happens
  `worker_panics_total{worker="redis_watchdog"}` and `pubsub_deaf` both keep increasing, so an alert can call for a
  manual restart. (Escalating only around `old.Close()`, by letting a worker mark a panic as not restartable, was
  considered and left for if this ever happens.)
- **The threshold is a default, not a rule.** `Restart` gives up on the 3rd panic within the window;
  `supervise.WithMaxPanics(n)` changes it per worker (Go has no default parameters, so this uses functional options,
  the same shape as `handler.WithMaxClients`). All current workers use the default.
- **Escalate still goes through the supervisor.** Compared with a plain `go`, the panic is logged through slog with
  the stacktrace and counted in `worker_panics_total` before the process exits, so it reaches logs and alerts.
- **Restart without backoff.** Waiting only delays the worker's next round (for presence, renewals arrive late and
  members' entries can expire); an immediate restart has nothing to wait for.
- **Loops are blocking; callers add `go`.** `HandleWriteLoop`, `HandlePingLoop`, `supervise.Restart`,
  `supervise.Escalate` and `checkSafely`
  are synchronous, so every goroutine is visible at the call site. The raw `pingLoop` / `writeLoop` are unexported so
  no caller can start them without the panic guard.

### Not chosen

- **Recover everything and only log.** Server-wide workers would stop silently and leave a half-dead server; the
  read loop would also lose its stacktrace.
- **Recover nothing and let every panic crash.** One bad connection or one bad message would drop every viewer on
  the server.
- **Restart with backoff.** Nothing to wait for; the worker's pending work only piles up.
- **Make `rooms.run` and the Redis consumer restart-safe.** See above: more code in the most delicate part for a rare
  case that a process restart already handles.
- **Give up after N panics in total, regardless of time.** Three panics spread over days would eventually crash a
  healthy server, while counting only within a window lets those through with an alert.
- **A 1-second window.** A low-rate bug (for example one panic every 20 s) would never reach 3 in a second and would
  be caught forever, dropping a message each time. One minute still gives up on it.
- **Start the window when the worker starts.** A worker that ran for hours before its first panic would get an extra
  restart, and two panics 20 s apart could fall into different windows. The window starts at the first panic; a
  zero-value `time.Time` means "no window yet", so the first panic always opens one.
- **`ws.Client.Start()` starting both loops internally.** It guarantees both loops start, but hides two goroutines
  inside a method; rejected in favour of synchronous methods the caller starts with `go`.

## Consequences

- A panic in any goroutine the server starts no longer exits the process by accident. It exits on purpose when a
  restartable worker keeps failing (3 in a minute) or when the Redis consumer or the SUB/UNSUB worker panics at all;
  then every viewer on that server reconnects.
- **A poison message can restart every server at once.** A room's messages fan out to every server subscribed to it.
  If one payload makes the consumer's handler panic, every such server escalates at the same moment: all viewers
  reconnect together, followed by a `resync` and history GET spike ([ADR 0021](0021-resync-get-spike-and-pool-size.md)).
  Pub/Sub doesn't replay, so it happens once. Accepted: a cluster that restarts and recovers is easier to notice than
  one where every server silently stops forwarding.
- New metric `worker_panics_total{worker}` (label values fixed in code). It should alert on any increase (that alert is also what
  covers the watchdog exception above); the alert
  rule waits for the deployment platform decision, like the other alerts.
- `/health` now has tests (there were none). It answers 500, not 503, when a dependency fails; unchanged here.
- Tests, each checked by breaking the code: `TestWebsocketReadLoopPanicStillCleansUp` (defer moved after
  `ReadLoop` → ghost stays), `TestRecoverPanicAfterResponseStartedWritesNothing` (unconditional 500 → error body
  appended), `TestLoopPanicClosesConnection` (no `Close` → not cleaned up; no recover → test binary crashes),
  `supervise_test.go` (window and threshold for `Restart`; `Escalate` gives up on the first panic, checked through
  the public function so a wrong threshold fails), `health_test.go` (no recover → test binary crashes).
- Not verified: behaviour under a real panic in production code paths (all panics in tests are injected), and how
  the platform's restart backoff interacts with repeated crashes.
- Every new long-running goroutine must pick one of these policies. A plain `go f()` for a long-running loop is now a
  review finding.
