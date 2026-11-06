# 0012 — Room moderation: structured errors, hierarchical roles, read-time shadow bans

**Status**: Accepted (2026-11-06)

## Context

A room owner had no way to deal with someone flooding a room, or to ask anyone for help. The roadmap had a plan,
written before rooms existed, and two parts of it no longer fit together:

| | Roles | Bans |
|---|---|---|
| Scope | Per room | Effectively site-wide: revoke all credentials |
| Volume | Every room may have some | "Very few people", so kept in each server's memory |

A room owner banning someone must not log that person out everywhere, and "rooms × bans per room" breaks the
assumption behind the in-memory set. So the plan had to be redone starting from one question: **what does a ban
act on?**

Separately, every HTTP error was plain text, so two different 403s were indistinguishable to a client. Login
showed "wrong password" for every 400, including a malformed body.

## Decision

**1. Errors are `{"error": {"code", "message"}}`.** `code` is a contract and the client's translation key.
`message` is a fallback, because the server doesn't know the user's language. Statuses didn't change. Validators
return a `CodedError` carrying its own code; anything else reaching `WriteCodedError` is logged and returned as a
fixed 500, so internal text can't leak.

Four failures stay deliberately indistinguishable: `invalid_credentials`, `unauthorized`, `room_not_found` and
`invalid_refresh_token`. Finer codes there would let someone enumerate accounts or see that a room once existed.

**2. Errors always send `Cache-Control: no-store`**, overwriting whatever a success path already set. `GET
messages` sets `s-maxage=1` before it writes, so a panic after that would otherwise make a 500 cacheable. Putting
the rule where every error passes means no handler has to remember it.

**3. Bans are room-level shadow bans.** A banned user's messages are stored as usual and they get a normal 201.
Nobody else sees them. A visible rejection invites a new account; someone who doesn't know is less likely to make
one. Login, JWTs and refresh tokens aren't touched. Site-wide bans are a different feature and aren't built.

**4. Hiding happens at read time.** `GET` excludes authors currently banned, with `NOT EXISTS`. Messages aren't
marked. A ban instantly hides old messages too, and removing it brings everything back, so a mistaken ban is fully
reversible.

**5. One table, one row per person per room.**

```sql
room_roles(room_id, user_id, role CHECK (role IN ('moderator', 'banned')), assigned_by, created_at,
           PRIMARY KEY (room_id, user_id))
```

The owner comes from `rooms.owner_id`; no row means viewer. A single row makes "moderator and banned at once"
unrepresentable. Room settings with one value per room, such as a future slow mode, belong on `rooms`.

**6. Rules live in one pure function.** `DecideRoleChange(actor, target, desired, bannedByOwner)` has two steps:
is the actor entitled to this, then does the change make sense. Owner > moderator > viewer, and only downward. A
moderator can lift any ban except one placed by the owner. Unknown values are refused, so the function fails
closed.

**7. Checks run in a fixed order, under a lock.** Actor first, then whether the target exists, then rank. In the
other order, a viewer could probe which user ids exist and who the moderators are. The transaction locks the room
row with `FOR NO KEY UPDATE`, which serializes role changes without blocking the FK check on message inserts. A
moderator's repeat ban can't overwrite the owner's `assigned_by`; an owner's repeat ban takes it over.

**8. Query Postgres, with no cache.** `POST` already reaches the database, so the check joins that statement. Role
changes are rare. This removes the in-memory set, its Pub/Sub sync and its periodic reload from the old plan, and
demotions and bans take effect on the next request. Buttons on the client are only UX.

**9. Online viewers get a best-effort `user_hidden {user_id}` frame**, so the message leaves their screen without a
reload. The server skips connections authenticated as that user. A lost frame costs little: that viewer falls back
to seeing the message until it scrolls away or they reload.

**10. The API is `PUT` / `DELETE /rooms/{id}/roles/{user_id}`**, which are naturally idempotent, plus `GET .../roles`
for the owner and moderators, and `GET .../me`. `/me` is `no-store` and **returns `viewer` for a banned user**.
Targets are chosen by `user_id` from a message: this needs no user-search API, which would be another way to
enumerate accounts. The cost is that someone must have posted once before they can be made a moderator.

## Alternatives considered

- **A `ban_list` array on the room, or a generic `room_meta(key, value)`.** With only an id per entry, an array
  would work. Once each entry has its own attribute, as `assigned_by` does here, it is a list of records, and in
  Postgres that is a table. In JSON, removing one means taking the array apart and reassembling it, with no type
  checks. `user_id <> ALL(array)` also has a NULL trap: legacy anonymous messages vanish the moment a room's list
  stops being empty. A generic table loses types and FKs, and still costs the same extra lookup.
- **Marking messages at write time.** `GET` stays unchanged, but a ban has to `UPDATE` old rows and an unban has to
  work out which to restore.
- **Refetching on `resync` instead of a new frame.** That frame isn't guaranteed either, merging by id wouldn't
  remove anything, and every viewer of a 2,000-person room would refetch on each ban.
- **In-memory ban set plus Pub/Sub.** Designed for a site-wide check on every request. Now only `POST` needs it.
- **Moderators sending messages directly over WebSocket.** A second write path with its own ack and dedupe, to save
  one HTTP call nobody has shown to be slow. Moved to the backlog.
- **RFC 9457 Problem Details, an i18n package, intercepting the mux's 404/405.** No outside consumers yet, a map
  covers one language, and clients must handle non-JSON anyway since a proxy can answer with HTML.

## Verification

- 40 table cases for `DecideRoleChange`, written by hand against the rules.
- Three review rounds with mutation testing. The first found that removing `r.role = 'banned'` from the message
  queries left every test green, which would have silently shadow banned all moderators; both are now locked. A race
  test covers a moderator's repeat ban against the owner's ban. Changing `FOR NO KEY UPDATE` to `FOR UPDATE` isn't
  caught: it only adds blocking, and a stable test would need a pause inside the transaction.
- Two servers and real Redis: a ban on one reached all 6 other connections on both, and none of the target's 3
  authenticated connections. Compared with an ordinary viewer, statuses and headers were identical across the API,
  and bodies too, apart from `/me` reporting each user's own identity.
- A headless browser: 12 cases for roles, live hiding, the banned user's view, stale buttons and 390 px width, then 18
  reconnect cases covering server restart, an expired token and a signature that no longer verifies.
- `GET`, latest 100 of 5,000 messages, against one local server on one busy Mac (load 5–13), so comparisons only:

| Scenario | `EXPLAIN ANALYZE` | p99 at 1,000/s | p50 at 2,000/s | p99 at 2,000/s, each run |
|---|---|---|---|---|
| No bans | 0.12–0.38 ms | 2.63, 3.96 ms | 0.78–1.12 ms | 4.66, 126.04, 4.27, 2.60 ms |
| Ban with 20 messages | 0.14 ms | 2.76 ms | 0.77–0.87 ms | 145.18, 7.53, 2.67 ms |
| Ban covering the latest 3,000 | 0.82–1.28 ms | 3.27 ms | 1.66–2.21 ms | 849.70, 7.79, 5.05 ms |

No errors at these rates. The spikes didn't repeat and also appeared with no bans. Their cause wasn't found, so they
can't be attributed to bans or ruled out. There is no valid data at 4,000/s: even the no-ban run was overloaded and
the server ran out of file descriptors. These were not an A/B against the previous version.

## Consequences

- **A shadow ban can be discovered.** The author's messages disappear on reload or in a private window, and their
  second tab doesn't see new ones. `GET` is shared by everyone, so it can't answer differently per user. Accepted: the
  aim is to raise the cost of noticing.
- **An unban also restores what was sent during the ban**, as far as it's still within the latest 100.
- **Skipping hidden rows costs linear time**: about 0.7 ms for 3,000 and 11–16 ms for 50,000 per query. At the rate
  limit, one person reaches 50,000 in about 7 hours. There is no cap or retention. Acceptable for now.
- **One extra message can survive a ban** if a `GET` sent before it returns late, or a `POST` that had already checked
  publishes after the frame. Gone on reload. Inferred from the code, not reproduced.
- **Deleting an account would un-hide its messages**, since `user_id` becomes NULL. There's no account deletion yet.
- **Role changes have no rate limit**, so a moderator can flood `user_hidden` by toggling. Tracked with general rate
  limiting.
- **If `POST` goes async (M7), it stops reaching the database**, so the ban check needs a cache there. The table and
  read filter don't change, so nothing here has to be undone.
- **Rolling deploy:** an old server forwards `user_hidden` unchanged, including to the banned user. Deploy all servers
  before the client; before that, no ban buttons exist.
- **A CDN may have its own defaults for caching errors**, so `no-store` needs checking when one is added.
- Not verified: production, real nginx, a real phone, a real migration down, or login timing, where only the unchanged
  flow was confirmed.
