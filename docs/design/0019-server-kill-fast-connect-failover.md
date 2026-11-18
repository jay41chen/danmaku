# 0019 — Killing a server: nginx has to give up on connects quickly, and clients retry history fetches

**Status**: Accepted (2026-11-18)

## Context

M10b's third fault: one of the two servers dies with `SIGKILL` while viewers and a poster go through nginx. The
fault environment ([ADR 0017](0017-stuck-clients-fault-env.md)) gained an nginx service using the same
`config/nginx.conf` as the dev compose: `least_conn` over `server-1` and `server-2`, nothing else tuned.

**Setup**, one run each
- 10 viewers in one room, connected through nginx, half logged in and half anonymous. Each viewer reconnects
  like the browser client does: equal-jitter backoff from 1 s, capped at 32 s, and a history fetch of the
  latest 100 after every `join_ok`.
- One poster sending 5 numbered messages per second for 120 s, through nginx.
- `deploy/fault/kill.sh` kills `server-1` 30 s in and starts it again only after the run.
- The tool compares the messages that got 201 with what each viewer received, live or by fetch, and samples
  the room's viewer count and each server's `websocket_viewers{auth}`.

**Before the change:**

| | Result |
|---|---|
| Viewers disconnected | 5, all anonymous |
| Disconnect to `join_ok` | 0.85, 0.90, 12.4, 12.6 and 25.7 s |
| Missing | 206; 101 recovered by fetches, **105 lost for good** |
| POST | 565 × 201, **35 failed** (nginx logged 499: the tool gave up at 10 s), no 502 |
| Viewer count | Up to 5 too high, correct again 26 s after the first disconnect |

**Why it took so long.** A killed container doesn't answer, so a connect to its IP mostly just hangs.
nginx's `proxy_connect_timeout` defaults to 60 s, longer than the client's 10 s timeout. The client gave up
first, nginx logged 499 and never counted a failure, so `least_conn` kept sending requests to the dead server.
The first `connect() failed (113: Host is unreachable)` appeared about 50 s after the kill, presumably when
ARP for the old IP expired.

**Why 105 were lost.** One viewer reconnected to `server-2`, but its history fetch went through nginx to the
dead `server-1`, timed out, and was never retried. Everything it missed was gone.

The viewer count also couldn't show whether logged-in viewers are deduplicated
([ADR 0010](0010-room-presence.md)): with strict alternation and `least_conn`, all the anonymous viewers
landed on `server-1`, so no logged-in viewer was ever disconnected.

## Decision

**1. `proxy_connect_timeout 2s` in `config/nginx.conf`.** It applies to the dev compose as well. Once a connect
fails, nginx's defaults take over:
- `proxy_next_upstream error timeout` sends the same request to the other server. A failed connect means
  nothing was sent, so this is safe even for `POST`.
- `max_fails=1 fail_timeout=10s` stops sending new requests to the failed server for 10 s.

2 s is generous: connects inside one Docker network take well under 1 ms. It is still far below the client's
10 s.

**2. The client retries a failed history fetch.** At most 3 attempts in total, using the same equal-jitter backoff
as reconnects. It retries network errors and 5xx only. A 4xx, or a body that won't parse, won't get better.
A newer fetch replaces a pending retry, and `stop()` cancels it. The fetch is a read, so a retry has no side
effects.

These two cover different failures. nginx fixes routing to a dead server. The retry covers what nginx can't
see: nginx itself restarting, or a brief network drop between browser and nginx.

**3. The load tool mirrors both.** Its viewers retry history fetches with the same policy and count attempts,
failures and give-ups. The login order is now two logged-in, two anonymous, so both kinds end up on both
servers. Per-server numbers the tool can't measure through nginx are printed as `n/a` instead of 0.

**4. `kill.sh` also restarts nginx when it restores `server-1`**, because nginx resolves upstream names once at
startup and could keep the old IP. Inferred from how nginx resolves names; not observed in these runs.

## Alternatives considered

- **A watchdog that probes the servers and takes a dead one out of nginx.** Open-source nginx can't remove an
  upstream at runtime, so it means rewriting the config and reloading. Probing every 5 s with two misses still
  takes about 10 s, and the watchdog can fail too. In production this is the platform's job, for example
  Kubernetes removing an exited container from the endpoints. The compose environment has no such layer.
- **1 s instead of 2 s.** A slow network would be marked down by mistake more often, for less than a second
  gained.
- **Retrying `POST` in the client.** Without an idempotency key a retry can store the message twice. That stays
  a known gap. nginx's own retry is safe only because it happens before anything is sent.

## Verification

- Go unit tests for the tool's retry policy (5xx then success, 3 × 5xx, 404 not retried, network errors, a
  cancelled run not counted as giving up, a superseded retry chain), the login order, and the `n/a` output.
  `go test -race` passes.
- **The client's retry has no unit tests**: the client has no test runner yet. Only `tsc` and `eslint` were
  run.
- `nginx -T` inside the container showed the new directive before the run.

After the change, one run:

| | Before | After |
|---|---|---|
| Viewers disconnected | 5 (all anonymous) | 5 (3 logged in, 2 anonymous) |
| Disconnect to `join_ok` | 0.85–25.7 s, 9 attempts | **0.6–0.9 s**, 5 attempts, all first-try |
| Missing | 206, 105 lost | **18, all recovered** |
| POST | 565 × 201, 35 failed | **600 × 201**, no 499 or 502 |
| History fetches | one timed out, never retried | 15 attempts, 0 failed |
| Viewer count | up to +5, correct after 26 s | up to +2, correct after 28 s |

The nginx error log explains it:
- At the moment of the kill, the first connect got `111: Connection refused`, not a hang, and `server-1` was
  marked down at once. All 5 reconnects went straight to `server-2`.
- After that, about every 13 s, `upstream timed out ... while connecting to upstream`: each time the 10 s
  `fail_timeout` ran out, nginx tried `server-1` again with a real request, waited 2 s, and sent that request to
  `server-2`. There were 7, all `POST`s, and every one of them got 201.

**Limits.**
- One run. In the run before the change, connects to the dead server hung; in this one the first got a
  refusal. A run where the first connect hangs would add up to 2 s to the first reconnects. That case was not
  measured after the change.
- The client's retry wasn't exercised: no history fetch failed, because nginx already routed around the dead
  server.
- The +2 in the viewer count is the 2 anonymous connections. The 3 logged-in viewers who disconnected didn't
  inflate it, which is the first observed evidence that deduplication works across a server dying. The count
  was correct again after the 30 s presence TTL, as designed.

## Consequences

- **While a server stays dead, one request about every 12 s takes 2 s longer**, when nginx tries it again.
  Accepted: the alternative was every request to it hanging.
- **Messages sent while a viewer was disconnected are recovered only if they fit in the latest 100.** At 5 msg/s
  that is 20 s, against under 1 s of downtime here.
- **Anonymous viewers on a dead server inflate the count for up to 30 s**, unchanged from
  [ADR 0010](0010-room-presence.md).
- **The browser's fetch has no timeout of its own.** It depends on nginx failing fast. Without nginx in front, a
  connect to a dead server would hang until the browser gives up.
- The dev compose gets the same 2 s connect timeout.
