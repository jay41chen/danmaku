# 0005 — Zero-downtime schema change: expand / contract

**Status**: Accepted (migrations `000010` + `000011`)

## Context

Messages had to belong to a room: `messages.room_id BIGINT NOT NULL REFERENCES rooms(id)`.

Railway starts the new version and only stops the old one once the new one is ready, so **old and new code
run at the same time**. Migrations run when the server starts.

- Adding a nullable column doesn't break the old code: its `INSERT` lists columns explicitly, and the new
  column is filled with NULL.
- `SET NOT NULL` *does* break it: every old `INSERT` fails with `23502` and returns a 500.
- Wrapping the migration in a transaction doesn't help. That stops it from failing halfway, but it can't
  make old code send a column it doesn't know about.

## Decision

Make the change in two deploys (expand / contract, also called *parallel change*):

1. **Expand** (`000010`): create `rooms`, seed a legacy room with id 1, and add `room_id NOT NULL DEFAULT 1`.
   Old code keeps working and its messages land in the legacy room.
2. Deploy new code that always sends `room_id`.
3. **Contract** (`000011`, in the *next* deploy): `DROP DEFAULT`. It can't be in the same migration as
   step 1, because the old version is still running when that migration finishes. By the second deploy,
   the "old" version is already the one that sends `room_id`.

A default left in place **hides bugs**: a new feature that forgets `room_id` would quietly write to the
legacy room. That is why the contract step is not optional.

## Details

- Existing messages go into a legacy room owned by a system account `admin` with `password_hash = '!'`.
  A bcrypt hash is always 60 characters starting with `$2`, so no password can ever match; this is the same
  convention as a locked account in `/etc/shadow`.
  - No `ON CONFLICT DO NOTHING`: if someone had already registered `admin`, the legacy room would silently
    belong to a stranger. Failing the migration is better.
- Index `messages(room_id, id)`: it can jump straight to the room and read the last 50 by id backwards.
  Because of the leftmost-prefix rule it also serves the FK check, so no separate `(room_id)` index is
  needed.
- `DEFAULT` can't use a subquery, so it relies on the seeded room getting id 1 in the same migration.

## Consequences

- Rolling the backend back past the expand version means running `000011`'s down migration by hand first.
  This is documented in the README.
- The same pattern applies to every future `NOT NULL` column and every rename.
