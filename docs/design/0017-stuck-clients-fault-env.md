# 0017 — Clients that stop reading: a separate fault environment, and which mechanism evicts them

**Status**: Accepted (2026-11-17)

## Context

A WebSocket client can stay connected and stop reading: a frozen tab, a phone that went to sleep
without closing the socket, a broken proxy. The server has three mechanisms that apply.

- **Keepalive**: the server pings every 25 s and closes a connection that has sent nothing (no pong
  or other frame) for 75 s. A client that never reads never answers a ping.
- **Outbox** ([ADR 0003](0003-slow-client-outbox.md)): broadcasts go into a per-connection buffer of
  256 frames. When it is full the connection is closed ("ws outbox full, kicking slow client").
- **Write deadline**: a single write may take at most 10 s.

M6 ([ADR 0014](0014-load-baseline-keep-sync-writes.md)) left an open issue. On macOS, about 2,000
goroutines were stuck inside the `write()` syscall, each holding an OS thread, and the server
crashed at the thread limit. Go uses non-blocking sockets and its netpoller on both macOS and Linux,
so a full socket buffer should return `EAGAIN` and park the goroutine instead. In M6 the macOS
kernel nonetheless did not return from `write()`. Production runs Linux, where this had not been
checked.

Two questions: which mechanism actually evicts a client that stops reading, and does the server's
thread count stay bounded on Linux when thousands of clients do it at once?

## Decision

**1. A separate compose project for fault experiments**, `deploy/fault/` (project `danmaku-fault`).
- Isolation: the development database, Redis and containers are never touched. Postgres is on
  tmpfs, Redis does not persist, and the ports are bound to `127.0.0.1` on numbers that don't
  collide with development (5488 server-1, 5489 Postgres, 5490 Redis).
- Servers: two, built from the M9 image.
- `up.sh` generates `JWT_SECRET` into a `.env` file under `umask 077`. It aborts if the secret is
  empty and waits until `/healthcheck` returns 200.
- `down.sh` removes the project and its volumes.

**2. The load tool gets a `stuck` scenario.**
- It opens N connections that authenticate, join one room and then never read again, so they
  never answer a ping.
- A separate poster sends messages into the room at `-stuck-rate` per second (0 seeds no posters).
- The scenario reports when connections were opened and when the server dropped them, plus
  `go_threads` and RSS from `/metrics`.
- `stuck.sh` counts "ws outbox full" log lines since the run started. That counts log lines, not
  connections.
- The default duration is 90 s, above the 75 s pong timeout.

**3. Stuck clients run inside the compose network**, in a container on the project network, not on
the Mac. Docker Desktop forwards published ports through a proxy that reads from the server's socket
on the client's behalf, so a client on the Mac that stops reading does not stop the server's socket
from draining. At 3,000 connections the proxy was also the bottleneck (below).

**4. Inside the network, the tool may only target a fixed list of host names.**
- The load tool refuses any non-local target; it must never hit production.
- With `-in-compose` it also accepts exactly `server-1`, `server-2`, `nginx`, `postgres` and
  `redis`.
- `nginx` is not in the compose file yet. It is on the list for M10b, which puts the servers
  behind it.
- Rejections list the allowed hosts.

## Alternatives considered

- **Reuse the development compose project.** Faster to start. But fault experiments kill
  containers, and the development database has data worth keeping.
- **Run stuck clients on the Mac.** Simpler, but the proxy absorbs the backpressure being tested,
  and it could not open 3,000 connections.
- **Allow any host name without a dot** (compose service names have none). Shorter, but it is an
  implicit rule: anyone reading the check has to know why "no dot" means "local".
- **Allow private IP ranges, or an `-allow-subnet` flag.** Compose networks use private ranges, but
  so do many real networks, and a flag moves the decision to whoever types the command.

## Verification

All runs were single runs on an arm64 Mac with Docker Desktop, with the servers in Docker Desktop's
Linux VM and only server-1 targeted. Times are from the first stuck connection. The connection
counts come from the tool's 1 s samples of `/metrics`. Outbox counts come from the server log: the
second ③ and ③' saved them, while ② and the first ③ were read from the script output at the time
and not saved.

| Run | Where the stuck clients run | Stuck | Messages/s | Result |
|---|---|---|---|---|
| ① | compose network | 1 | 0 | Closed at 76 s by the pong timeout; `go_threads` 11 → 12. |
| ② | compose network | 1 | 50 | Closed at 76 s by the pong timeout; 0 outbox log lines. |
| ②' | Mac, through the proxy | 1 | 50 | Closed at 76 s. |
| ③ | compose network | 3,000 | 50 | First drop in connections at about 29 s, with about 2,890 connected; all closed by 41 s; 3,000 outbox log lines; peak `go_threads` 22; RSS 221.9 MB. |
| ③ again, with kernel sampling | compose network | 3,000 | 50 | First outbox log at 29.1 s, before the ramp ended, with about 2,870–2,900 connected; all closed by 44 s; 3,004 log lines; peak `go_threads` 21; RSS 224.1 MB. |
| ③' | Mac, through the proxy | 3,000 | 50 | Only 816 connected and 2,184 failed to dial; the ramp took 151 s; 0 outbox log lines; only 399 of 8,000 posts succeeded; metrics sampling stalled for about 3 minutes. This run measures the proxy, not the server. |

- **The thread count stayed bounded in the Linux container.** In ③ the server logged 3,000
  "outbox full" lines and closed every connection by 41 s, long before the 75 s pong timeout, so the
  outboxes presumably filled (log lines are not connections; see below). At 50 messages/s, 256
  frames take about 5 s, so the writers were presumably blocked for at least that long. Even so, `go_threads` peaked at 22 and the server did not crash. That is one run in Docker
  Desktop's VM, at a lighter load than M6 (3,000 × 50 against 5,000 × 100). No goroutine dump was
  taken, so where the blocked writers waited was not observed; only the thread count was.
- **A single stuck client was closed by the pong timeout, not the outbox**: 76 s, and no outbox log
  lines. That is consistent with the socket buffers absorbing 76 s of traffic, so the outbox never
  filled.
- **With thousands of stuck clients, outboxes filled only after the kernel's TCP memory passed its
  high limit.**
  - The second ③ sampled `/proc/net/sockstat` in the VM every 2 s. Its `mem` counter is kernel-wide,
    across all containers.
  - The limits (`tcp_mem`) are 46,176 / 61,570 / 92,352 pages (low / pressure / high).
  - TCP memory grew faster than the number of connections, because each connection also
    accumulated messages. It crossed low between 18 and 20 s, pressure between 20 and 22 s, and high
    between 26 and 28 s, and peaked at 93,652 pages at 30 s.
  - The first outbox log came at 29.1 s, 1–3 s after high was crossed. No outbox log came before
    high was crossed.
- **After the server closed every connection, TCP memory did not drop.** It stayed at about
  91,600–92,100 pages for about 75 s and fell to 713 only when the stuck-client process exited.
  - Two things held it: the clients' receive queues (they never read, so they never saw the close)
    and the server's orphaned sockets (`orphan` in sockstat). An orphaned socket is one the server
    has closed but whose unsent data the kernel still keeps.
  - The orphan count peaked at 2,426 at 34 s. The kernel reclaimed some of them while the clients
    were still connected: about 1,620 were left from 72 s on, and `mem` fell only from 92,116 to
    91,604 pages.
  - The rest were released when the stuck-client process exited (judged from timing: the drop came
    between 118 and 120 s, as the process ended), so this run cannot say how the memory was split
    between the two.
  - Server and clients share one kernel in the VM, so both count against the same `tcp_mem`.
- **The write deadline was not checked separately.** "write frame failed" and "write ping failed"
  lines were not counted. At 50 messages/s the outbox (about 5 s) is expected to fire before the
  10 s deadline, but these runs don't show it.
- **The 4 extra log lines (3,004 against 3,000) are not from another run**: all timestamps fall
  between 29.1 s and 43.9 s. A likely source is a broadcast that fails to enqueue on a connection
  already closed but not yet unregistered, which logs again (`internal/ws/hub.go`). The log has no
  connection id, so this can't be confirmed.
- **Each change was reviewed by a separate agent.**
  - The first review asked for six fixes to the scripts and the tool.
  - The second found one rejection message still saying "only localhost".
  - The third passed and checked runs ①–③' against the raw files.
  - The sockstat rerun came afterwards. A review of these docs found that its threshold times had
    been counted from the script's start instead of the first connection, and found the orphaned
    sockets.

## Consequences

- **The two mechanisms cover different failures.**
  - A single frozen client is the pong timeout's job.
  - Assume roughly 200-byte frames and Linux's usual 4 MB maximum send buffer. Both are estimates;
    neither was measured here. At 50 messages/s it would then take minutes to fill one socket's
    buffers, longer than 75 s.
  - In ③ it was the outbox, not the pong timeout, that closed the connections once kernel TCP
    memory was exhausted (from timing only; see the correlation caveat below).
- **Closing a stuck connection does not free its send buffer right away.** The orphaned socket keeps
  the buffer until the peer goes away or the kernel's orphan retries give up. This applies in
  production too.
- **The outbox result in ③ depends on the test setup.**
  - The clients' receive queues counted against the same kernel limit as the server.
  - `tcp_mem` is sized from the VM's memory, and the containers have no CPU or memory limits.
  - In production the clients run on other machines, so more connections should be needed to reach
    the limit. That is an inference and was not measured.
- **It is a correlation, not a proof.**
  - Per-socket buffer sizes and blocked `write()` calls were not measured.
  - The sampling is every 2 s, and connections and messages grew together.
  - The timing is consistent with "the kernel shrinks socket buffers under memory pressure, writes
    block, outboxes fill".
- **Not covered**: more than 3,000 connections, the M6 load (5,000 × 100) in a Linux container,
  Linux outside Docker Desktop's VM, server-2, and where blocked writers wait (no goroutine dump).
- **Known gaps in the scripts, accepted:**
  - `stuck.sh` doesn't print the count when interrupted with Ctrl-C.
  - It scans all arguments for `-servers`, while Go's flag parsing stops at the first non-flag
    argument.
  - `--since` has one-second precision.
  - `up.sh` reuses an existing `.env` without checking its permissions.
