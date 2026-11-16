# 0015 — Popular rooms: a ranking that borrows presence as its heartbeat

**Status**: Accepted (2026-11-16)

## Context

The room list was ordered by creation time. M8 adds "rooms with the most viewers first". The counts
already exist ([ADR 0010](0010-room-presence.md)): every server counts the rooms its own viewers are in
every 5 s and pushes the number to those viewers. Three things were missing:

- **`GET /rooms` could not see the counts.** Rooms live in Postgres; counts were computed and pushed, never
  stored.
- **A ranking needs someone to remove rooms that went quiet.** Once the last viewer leaves, no server
  counts that room any more, so its last number would stay on a stored ranking forever.
- **Counts change all the time**, so a `(count, public_id)` keyset cursor would skip or repeat rooms
  between pages.

## Decision

**1. Top N only, no pagination.** `GET /rooms?sort=viewers` returns at most `limit` rooms (same rules as
the plain list: default 20, max 50), each with `viewer_count`, and no `next_page_token`. A `page_token`
or any other `sort` value is `400` (`page_token_not_supported`, `invalid_sort`). The plain list is
unchanged, so rooms nobody is watching are still reachable there. The frontend shows the top 10 above
"all rooms".

**2. One Redis sorted set, `room:viewer_ranking`.** Member = public_id, score = viewer count. After each
successful count the presence worker writes it: `ZADD` for counts above zero, `ZREM` for zero, in one
pipeline. A failed write is logged and the counts are still pushed to viewers; the ranking is never on
the delivery path.

Several servers write the same room. They read the same members set and dedupe the same way, so they
write the same number; a late write can leave a value up to one count period old, which is accepted.

**3. Whether a room is still on the ranking is decided by its presence key, not by a second
timestamp.** `room:<id>:members` disappears by itself in both ways a room goes quiet:

- the last viewer leaves normally → the server `ZREM`s the member, and Redis deletes an empty sorted set;
- a server dies → nobody renews its members and the key's `EXPIRE` (30 s) runs out.

While the key exists, some server renewed it within the last 30 s; normally that server still has local
viewers and counts the room every 5 s. After a server dies, the key, and with it the room's last count,
can outlive it by up to 30 s (see Consequences). So `TopRooms` reads the ranking in batches, checks `EXISTS` on each room's
presence key in one pipeline, and `ZREM`s the ones that are gone (lazy cleanup, done by whoever reads).

**4. No Lua.** Every command touches one key. Atomicity is not needed: if a room gains a viewer between
`EXISTS` and `ZREM`, it disappears from one read and comes back within 5 s. Scores can also change
between batches, so a room can show up twice; duplicates are dropped with a linear search over the
result (at most 50 entries, a few thousand comparisons, simpler to read than a map). A room skipped this
way comes back on the next read. The number of rounds is bounded, so the result can be shorter than
`limit`.

**5. Postgres decides what still exists.** The use case reads the top rooms from Redis, fetches them with
`WHERE public_id = ANY($1) AND deleted_at IS NULL`, and restores the ranking order in Go. A soft-deleted
room drops out here. If Redis can't be read the endpoint returns `503 viewer_ranking_unavailable`, not an
empty list: "nobody is watching" would be a wrong answer, not a missing one.

**6. The frontend polls every 30 s**, pauses while the tab is hidden and refreshes as soon as it is
visible again. Each request has a 10 s timeout and is aborted when superseded, so one stuck request can't
stop later refreshes. If the first load fails the section stays hidden (and keeps retrying); after a
success a failed refresh keeps the previous list.

## Alternatives considered

- **Compute every room's count on each request.** Needs every room id (from Postgres, mostly empty rooms)
  or a `SCAN` of Redis; the cost grows with total rooms or total keys, on the request path.
- **A `viewer_count` column in Postgres.** Turns a display-only number that changes every few seconds into
  database writes from every server.
- **A per-room count key with a TTL.** The TTL is a free heartbeat, but sorting needs to know every key,
  which again means `SCAN` or every room id.
- **A second sorted set of "last updated" times, with its own threshold.** Works, but it is a second
  answer to "is this room alive" that can disagree with the presence count, and its threshold silently
  depends on the count interval. In practice it would also have been slower: a room whose viewers leave
  normally disappears on the next read with the presence key, but only after the threshold with a
  timestamp.
- **Time-bucketed ranking keys** (`…:<10 s bucket>` with `EXPIRE`). No cleanup at all, but a reader has to
  cope with a bucket that is still being written.
- **Pagination over a ranking snapshot** (rebuilt every minute, cursor = snapshot version + position). The
  snapshot has to be shared by all servers, someone has to build it once per period, old snapshots have to
  outlive the readers paging through them, and rooms with no viewers need a second cursor. Moved to the
  backlog until rooms beyond the top 50 are actually browsed.
- **A required vs. optional ranking dependency.** The first implementation added it to `RoomUseCase` as a
  functional option, so forgetting to wire it compiled, passed every test and returned 503 forever. It is
  now a required constructor parameter, like the other use cases' dependencies. That catches forgetting
  it, not an explicit `nil`, which tests that never list popular rooms still pass.

## Verification

- Integration tests against real Redis lock the two invariants: the last viewer leaving (through
  `SyncPresence`) removes the room on the next read, and a deleted presence key (standing in for expiry)
  does too; both check that the room is not returned *and* that it was `ZREM`ed. A go-redis hook changes a
  score between batches to reproduce the duplicate deterministically. Each backend rule (ranking filter and
  cleanup, zero-count `ZREM`, reordering, deleted rooms, `page_token` rejection, broadcasting despite a
  ranking failure, dedupe) was checked by mutation. The frontend has no test framework; its polling rules
  were checked by reading the code and building it.
- Two local servers, a test database, WebSocket viewers and headless Chrome: a room with four connections,
  two of them one logged-in user on different servers, was ranked first with 3 on both servers and in the
  page; a room with no viewers was absent. After its viewers disconnected (one `kill -9` of the viewer
  processes, three clean closes) the room was gone on the next read, about 10 ms later. After `kill -9` of
  the server holding a room's only viewer it was gone after 28.6 s (expected 20–30 s: the key expires 30 s
  after its last renewal, which happens every 10 s). Otherwise single runs.

## Consequences

- **The ranking depends on the presence key's lifetime.** If presence changes structure (ADR 0010 notes a
  very large room would need one), the liveness check in `TopRooms` has to change with it. The integration
  tests above fail if it doesn't.
- After a server dies, a room whose only viewers were on it stays on the ranking with its last count until
  the presence key expires (20–30 s after the kill). If the room also has viewers on a live server, the
  dead server's members are still counted until they age out, as in the in-room count (ADR 0010); this
  mixed case was not tested.
- The list's numbers can be up to 30 s older than the in-room count (polling), and rankings reorder on
  refresh.
- A Redis Cluster would need nothing new for the ranking; the existing presence count script already
  touches several rooms' keys and would need changing first.
- Cost per poll, in the usual case: one `ZREVRANGE`, one `EXISTS` pipeline and one Postgres query (plus a
  `ZREM` when stale rooms are found; at most 10 rounds). About 33 requests/s per 1,000 visible home pages.
