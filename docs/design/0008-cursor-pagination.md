# 0008 — Room list: cursor pagination with an opaque, strictly parsed token

**Status**: Accepted (2026-11-04, migration `000013`)

## Context

`GET /rooms` lists every non-deleted room, newest first, so it needs pagination. `GET /me/rooms` lists the
caller's own rooms. Right now that is at most 3, but a "restore deleted room" feature would make it
unbounded, so it uses the same response format from day one.

Offset pagination (`LIMIT 20 OFFSET 40`) has two problems:

- **Rows shift while the user is paging.** If a room is created while someone is reading page 1, every row
  moves down by one, and page 2 starts with the last room of page 1 again. If a room is deleted, one is
  skipped instead.
- **It gets slower the deeper you go.** Even with an index, `OFFSET 10000` still walks 10000 entries just
  to throw them away.

## Decision

**Keyset (cursor) pagination** on `(created_at, public_id)`:

```sql
WHERE deleted_at IS NULL AND (created_at, public_id) < ($cursor_created_at, $cursor_public_id)
ORDER BY created_at DESC, public_id DESC
LIMIT $limit + 1
```

- **`public_id` is the tiebreaker.** Two rooms can share a `created_at`. If the cursor were the timestamp
  alone, the rooms with that timestamp that fall across a page boundary would be skipped or repeated. Adding
  `public_id` makes the order total. We don't use the internal `id`, because a cursor must not expose the
  sequential primary key.
- **A partial index `rooms(created_at, public_id) WHERE deleted_at IS NULL`** (`000013`) is already in that
  order, so the query jumps to the cursor and reads `limit + 1` entries. Every page costs the same. Only
  non-deleted rooms are listed, so only those are indexed. The query must keep `deleted_at IS NULL` in its
  `WHERE` clause, or the planner can't use the partial index.
- **`limit + 1` instead of `count(*)`.** If the extra row comes back, there is a next page. That row is then
  trimmed, and the cursor is the **last row returned** (`rooms[limit-1]`). If the cursor were the extra row
  instead, the next query's `<` would skip it, and that room would never be shown. `count(*)` would be a
  second query (racing with the first one) that scans every live row. Postgres keeps no stored row count,
  because under MVCC different transactions see different counts.
- `limit` defaults to 20 and is capped at 50 (`store.MaxRoomListLimit`, shared by handler and store). The
  store also rejects `limit <= 0`: `rooms[limit-1]` would panic, and a negative `LIMIT` is a Postgres error.

### The page token

The response carries `next_page_token`, which the client sends back as `?page_token=`. It is
`base64url(JSON{"v":1, "created_at":"<RFC3339Nano>", "public_id":"..."})`.

- **Opaque by contract, not by cryptography.** Clients must treat it as an opaque string, so we can change
  the format later (`v`). It is not encrypted or signed, and anyone can decode it. It reveals nothing a
  client can't already see, so that is acceptable, but it means **the token is untrusted input**.
- **Parse, don't validate.** `decodePageToken` either returns a `RoomCursor` that can go straight into the
  query, or rejects the token with `400 invalid cursor`, without saying which check failed. It rejects:
  - invalid base64url (including `=` padding)
  - unknown JSON fields (`DisallowUnknownFields`)
  - **trailing data after the JSON object**
  - the wrong `v`
  - a zero `created_at`
  - a `public_id` that isn't the format the store generates (`store.ValidPublicID`: 8 random bytes in
    base64url = 11 characters)

## The traps

- **`json.Decoder` stops after the first value.** `{...}{}`, `{...} x` and `{...}{...}` all decode without
  error. The fix is a second `Decode` that must return exactly `io.EOF`. `Decoder.More()` is not enough:
  it returns false when the next byte is `}` or `]`, so `{...}}` would get through. A test locks this in.
- **The first `Decode` error must be checked even with `DisallowUnknownFields`.** The decoder fills fields
  while it reads. By the time it reports an unknown field, the known fields are already set, and all the
  later checks would pass.
- **Every SQL variant needs its own failing test.** The store has 4 statements: all owners or one owner,
  each with or without a cursor. The first test suite only checked soft deletes on the first page, and
  checked the `public_id` tiebreak in only one statement. So dropping `deleted_at IS NULL` from either
  cursor query, or flipping the tiebreak to `ASC` in 3 of the 4 statements, still passed. The review found
  this. Two tests were added: a deleted room right after the cursor, and 4 rooms with the same `created_at`
  paged through with `limit 2`. Each of the 5 mutations was then confirmed to fail a test.

## Alternatives considered

- **Offset pagination**: rejected for the two reasons above.
- **Cursor = internal `id`**: a single-column cursor, but it exposes the sequential primary key, and it only
  works while `id` order happens to match `created_at` order.
- **One SQL statement with optional cursor parameters** (`$2 IS NULL OR (created_at, public_id) < ...`):
  fewer statements, but with generic plans the planner can't rely on the condition and may not use the
  index well. Separate static statements are explicit and each one is tested.
- **Sorting by viewer count**: needs presence data, deferred to M2.

## Consequences

- No total count and no "jump to page N". Only "load more", which is what a room list needs.
- Clients can't build or modify cursors. A client holding an old-format token after a `v` bump gets a 400 and
  starts again from page 1.
- `GET /me/rooms` is per-user, so it sends `Cache-Control: no-store`. A deleted user whose access token
  hasn't expired gets an empty list, not a 401. That is acceptable for a read.
