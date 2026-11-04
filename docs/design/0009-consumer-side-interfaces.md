# 0009 — Consumer-side interfaces, domain errors in `model`, one package per backend

**Status**: Accepted (2026-11-04, refactor only, no behavior change)

## Context

The first version of the data layer followed a Java / Kotlin habit. Package `store` defined one
repository interface, `DBStoreInterface`, next to its implementation, and every handler depended on it.

That was right when it was written: one handler, three methods. Then every feature appended to the same
interface (users, refresh tokens, rooms). It ended up with 13 methods, used by 5 handlers that each called
between 1 and 6 of them. `RedisStoreInterface` drifted the same way (5 methods, each handler using 1–3).
No single change was wrong; the drift was the sum of reasonable steps.

Three problems came with it:

- **Every handler depended on methods it never calls.** The health handler needed one method and had 13.
- **Test fakes embedded the interface** to avoid implementing all of it. A missing method compiled fine
  and only failed at runtime as a nil-pointer panic.
- **Handlers still imported the implementation package.** Even with method calls going through an
  interface, sentinel errors (`store.ErrRoomNotFound`), `RoomCursor` and `ValidPublicID` were defined in
  `store`, so the HTTP layer depended on the package that talks to Postgres.

## Decision

**1. Interfaces are defined by the consumer, and list only what it calls** ("accept interfaces, return
structs"). Go interfaces are satisfied implicitly, so `*pgstore.DBStore` satisfies all of these without
declaring anything, and `main.go` passes the same value to every handler:

| Handler | Interfaces |
|---|---|
| Auth | `authStore` (6 methods), `rateLimiter` |
| Room | `roomStore` (3) |
| Message | `messageStore` (3), `messagePublisher` (1), `rateLimiter` |
| WebSocket | `wsStore` (2), `wsPubSub` (3) |
| Health | `healthChecker` (1), used for **both** the DB and Redis |

Fakes no longer embed anything, and `fakes_test.go` asserts each fake against the interfaces it serves
(`var _ wsPubSub = (*fakeRedis)(nil)`). A missing method is now a compile error.

`rateLimiter` deserves a note. The rules (`login:ip` 20/min, …) and the 429 response live in the handler.
The store only provides an atomic counter in Redis, which is shared because there are several server
instances behind nginx. Calling the store side `RateLimiterInterface` made it look like a second rate
limiting feature, so the interface now lives next to `checkRate`.

**2. Domain results live in `model`, not in the implementation.** An interface's contract includes its
errors: `GetRoomByPublicID` returns `ErrRoomNotFound` when there is no such room. The store already
translates Postgres into domain language (`pgx.ErrNoRows` → `ErrRoomNotFound`, SQLSTATE `23505` on
`users_username_lower_key` → `ErrUsernameTaken`). Only the *definitions* were in the wrong place.
`model/errors.go` now holds every sentinel and the two typed errors, and `model/room.go` holds
`RoomCursor`, `MaxRoomListLimit`, `LegacyRoomPublicID`, `PublicIDBytes` and `ValidPublicID`. `model`
imports nothing but the standard library. Plain failures (a broken connection) stay ordinary errors and
become a 500. Only business outcomes get a sentinel.

**3. One package per backend: `pgstore` and `redisstore`.** The Postgres and Redis code never referenced
each other, so they now live in separate packages, and the compiler keeps it that way. Inside each
package, files are split by resource (`user.go`, `message.go`, `room.go`, `pubsub.go`, …), because Go
methods are declared outside the struct and the file name is the table of contents.

The resulting dependency graph:

```
main ──→ pgstore ────→ model
  │      redisstore
  └────→ handler ────→ model
```

Only `main.go`, the composition root, knows that the backends are Postgres and Redis.

## Alternatives considered

- **Keep one interface and just split the files.** It doesn't fix handlers depending on methods they
  don't use, or the fake that compiles while missing a method.
- **One shared `roomGetter` embedded in several interfaces.** `GetRoomByPublicID` appears in three
  interfaces. Embedding would remove the duplicated line, but each interface would no longer show its
  full needs at a glance. A repeated one-line signature isn't duplicated logic; there is still only one
  implementation.
- **Defer moving the errors until a second implementation exists.** Moving them later costs the same
  (mechanical, compiler-checked), and the real cost of changing databases lies elsewhere (`FOR UPDATE`,
  `clock_timestamp()`, `ON CONFLICT`, SQLSTATE codes). It was still done now, because the direction of
  the dependency was wrong regardless of when a second backend shows up.
- **Package names `db` and `redis`.** `db` is the name of the local variable in `main.go` and of the
  handler fields, so it would shadow the package. `redis` collides with go-redis, which `redisstore`
  imports.
- **Put the errors in package `handler`.** `pgstore` returns them, so it would have to import the HTTP
  layer. That is the wrong direction.

## Consequences

- Adding a store method means adding it to the interface of the handler that calls it, and to the fake
  that serves it. The compiler points at both.
- `model` is no longer just data structures: it holds domain errors and a few rules. If it keeps growing,
  it may become `domain`.
- Finding all methods of a type needs a tool, because nothing in the code lists them:
  `go doc ./internal/pgstore DBStore`, or "Go to Implementations" in the IDE.
- `redisstore.RedisStore` stutters. Renaming it was out of scope for this change.
- Store tests share one `danmaku_test` database, so two `go test` runs at the same time interfere
  (FK, deadlock, duplicate-username errors). Run them one at a time.
