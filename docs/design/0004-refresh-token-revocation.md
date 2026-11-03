# 0004 — "Revoke all sessions" must take effect immediately

**Status**: Accepted (2026-10-30, migration `000012`)

## Context

Auth uses short-lived JWT access tokens plus opaque **rotating refresh tokens** (stored as SHA-256 hashes).
If an already-rotated refresh token is used again (*duplicate use*), the token has probably been stolen,
so every session of that user is revoked. A 15 s grace period lets the *same* token be retried, which
covers a lost response or two tabs refreshing at once.

The bug: "revoke all" ran `UPDATE ... WHERE revoked_at IS NULL`. It missed tokens that had been **rotated
but were still inside the grace period**, and those can still be exchanged. An attacker could *create* that
condition on purpose:

```
t0 attacker steals A, rotates every 14 s, always keeping the previous token
t1 real user sends A → theft detected → revoke all (misses the attacker's in-grace token)
t2 attacker sends the in-grace token → gets a fresh 7-day token, never detected again
```

## Decision

Store a **per-user invalidation timestamp** instead of listing token states:
`user_credential_state(user_id, valid_after)`.

- Revoking everything is a single write: `valid_after = clock_timestamp()`. Any token with
  `created_at <= valid_after` is invalid. Because nothing enumerates token states, no state can be missed.
  This is the same idea as ASP.NET's security stamp.
- `refresh_tokens` is **not** updated any more. Keeping both would mean two sources of truth.
- A token that predates `valid_after` and shows up again is invalid but **does not trigger another
  revocation**. Otherwise an attacker holding old tokens could keep logging the real user out. It is
  logged at Warn as a security signal.

## Concurrency details

- **Lock a row that exists.** Most users have no `user_credential_state` row, and `FOR UPDATE` on a row
  that doesn't exist locks nothing. The `users` row is used as the per-user serialization point instead.
  `FOR SHARE` would deadlock: two transactions both try to upgrade to the exclusive lock.
- **TOCTOU.** A lock only guarantees one writer at a time; it doesn't refresh what you read *before* taking
  it. Rule: any state used to make the decision is read **after** taking the lock. Only immutable data (the
  token's `user_id`) may be read before.
- **`now()` is the start of the transaction.** A revoke that waited for the lock would stamp a time
  *earlier* than a token issued while it waited, so that token would slip through. `clock_timestamp()`
  returns the actual time of that statement.
- **Backfill.** Old `duplicate_use_detected` rows are converted to `valid_after` using
  `GREATEST(max(revoked_at), max(created_at))`, so old tokens can't use the legacy code path after deploy.

## Alternatives considered

| Option | Why not |
|---|---|
| Also mark in-grace rotated tokens in the revoke `UPDATE` | Works, but still lists states; the next new state reopens the hole |
| Extend the grace check to look for a later revoke | Relies on rows the revoke happened to change; if 0 rows changed (e.g. right after logout) there's no trace |
| Move the logic into a DB function / Go state machine | Rules split across DB and app, two clocks, still needs a user-wide lock |

## Consequences

- Reusable for Ban: access tokens can be rejected with `iat <= valid_after`.
- Counting "active sessions" must `JOIN` `user_credential_state`.
- `FOR UPDATE` on `users` briefly blocks FK inserts (login, posting) for the same user; `FOR NO KEY UPDATE`
  would avoid that (follow-up).
- Error mapping: `RefreshTokenRevokedError` also matches `ErrRefreshTokenInvalid`. The handler must
  `errors.As(revoked)` first, to log it, before `errors.Is(invalid)`. A log-capturing test makes sure the
  order stays that way.
