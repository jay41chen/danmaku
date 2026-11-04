# 0010 — Cross-server viewer count: one writer reconciling connections into Redis

**Status**: Accepted (2026-11-04)

## Context

Each room shows how many people are watching. Viewers are spread across several servers, so no server
can answer that alone, and three things make it harder than adding numbers together:

- **The same person can hold several connections**, possibly on different servers. Two tabs count as one
  viewer, so the data needs identities, not per-server totals.
- **A server can die without saying anything.** After `kill -9`, nobody is left to remove its viewers.
- **Removing a viewer is not the only write.** Joining, leaving, disconnecting, logging in and being
  downgraded on token expiry all change the membership, and they happen under the Hub's lock, where
  network I/O is not allowed ([ADR 0002](0002-reconciliation-loop.md)).

The count is display-only and can be lost and rebuilt, so it belongs in Redis rather than Postgres, and
being a few seconds off is acceptable. A wrong number must not affect message delivery.

## Decision

**1. One Redis sorted set per room, one member per connection.** The key is `room:<public_id>:members`,
the member is `<conn_id>:<user_id>` (anonymous = `0`), and the score is the last renewal time. Connections
are stored, and identities are only collapsed when counting. Storing by user would let closing one tab
remove someone who still has another open.

**2. Staleness is decided by the score.** A server keeps its members alive by rewriting them, and anything
not renewed for 30 s is dropped when counted. Nobody has to detect a dead server. The key also gets
`EXPIRE 30`, which only matters when every server of a room is gone. Scores and thresholds both come from
Redis `TIME` inside the Lua script, so clock differences between servers don't matter.

**3. Exactly one goroutine per server writes, reconciling desired against actual.** The Hub snapshot is the
desired state, and `lastWritten` is what this server has written. The Hub only sends a non-blocking wake on
a buffered channel, so a burst of changes collapses into one pass.

| Trigger | Interval | Writes |
|---|---|---|
| Wake | on any Hub change | only the diff: cost follows changes, not viewers |
| Renewal | 10 s | every current entry, which renews scores and repairs lost Redis data |
| Count | 5 s | no membership change; counts rooms with local viewers and pushes to them |

The snapshot is taken immediately before each write and there is only one writer, so an older snapshot can
never overwrite a newer one. A login or room change is one script removing the old member and adding the new
one, so the viewer never disappears in between.

`lastWritten` exists because the set is shared. Deleting the key and rewriting it would remove other
servers' members, and reading it back can't tell which members are ours.

**4. Failures prefer undercounting briefly.** A failed write may or may not have reached Redis, so on
failure `lastWritten` keeps everything it had and also gains every attempted add. Pending removals are
retried, because removing a missing member is harmless. The worst case is undercounting for up to 10 s
until the next renewal, rather than a ghost that lasts 30 s. If counting fails, nothing is broadcast and
clients keep their last number.

**5. Dedupe in Go, by `user_id` for logged-in viewers and `conn_id` for anonymous ones.** Every anonymous
connection counts as one viewer, because there is no identity to collapse.

Each server pushes only to its own viewers, and every server computes the count. The result overwrites the
previous value, so doing it several times is harmless and there is no leader to elect.

## Alternatives considered

- **Write from the Hub on each event.** Network I/O under the lock, or a goroutine per event, where a slow
  older write can land after a newer one and leave a ghost for 30 s.
- **A goroutine for each change alongside the renewal loop.** Two writers on the same keys bring back the
  same race, and it needs more coordination than one writer does.
- **Each server reports its own total, and readers add them.** Cheap, but only numbers exist, so someone
  with tabs on two servers counts twice.
- **A per-server lease, with members tagged by owner.** Less renewal traffic, but something then has to
  clean up after a dead server, including when the cleaner dies midway.
- **Ask all servers at read time (scatter-gather).** No ghosts, but every query involves every server and
  has to decide how long to wait.
- **Rewrite every entry on every change.** Simple, but the cost grows with viewers rather than changes.
- **HyperLogLog or a second per-user set.** They answer "how big is a huge room" more cheaply. Rooms here
  are small, so reading the members is fine.

## Verification

- Unit tests cover the diff, renewal and failure rules against fakes, and the key rules were checked by
  mutation. One case, a failed sync with several adds, was only caught depending on map iteration order, so it got its
  own deterministic test.
- Integration tests run the Lua scripts against real Redis.
- Two servers, in a headless browser, directly and through a temporary nginx using the same configuration
  apart from its upstream addresses, against a test database. Viewer counts, cross-server dedupe, in-room
  login and reconnects behaved as designed. These were single runs.
- After `kill -9`, the count on the surviving server dropped at 30.1 s and 30.0 s in the runs without
  nginx. The theoretical range is 20–35 s and the acceptance limit is 40 s.

## Consequences

- **The count can be temporarily too high after a server dies.** Measured through nginx: viewers reconnected
  to the surviving server within about 5 s and got new `conn_id`s, while the old members lasted until they
  expired, so the displayed count went 3 → 4 → 3 and was correct again at 35 s. Logged-in viewers don't
  inflate it, since they collapse by `user_id`. Shutdown doesn't remove members either, so a restart should
  look the same, although only `kill -9` was tested. Accepted: the number is display-only and corrects itself.
- A new viewer sees no count until the next push, up to 5 s.
- Counting reads every member of a room, so the cost grows with room size. A very large room would need a
  different structure.
- Renewal scales with connections on the server rather than with servers.
