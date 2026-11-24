# 0022 — Login and register rate limits fail closed; refresh and messages still fail open

**Status**: Accepted (2026-11-24).

## Context

Every rate-limit check asks Redis, with a 200 ms budget ([ADR 0020](0020-redis-blackhole-gray-failure.md)). Until
now, any limiter error or timeout let the request through (fail open), so that a broken Redis would not lock
everyone out. [ADR 0021](0021-resync-get-spike-and-pool-size.md) found the other side of that: when the **server**
is overloaded, limiter calls also miss their budget and fail open, up to about 450 times on one server in one run. Rate
limiting switches itself off exactly when it matters most.

Three things make this worse for login and register than for anything else:

- **The server can't tell the two cases apart.** A blackholed Redis and a starved goroutine both end in `context
  deadline exceeded`.
- **An attacker can cause the overload.** Flooding cheap endpoints until limiter calls time out turns the limiter
  off, and then passwords can be guessed without limits.
- **Login and register run bcrypt.** Letting more of them through under overload adds CPU load, which makes more
  limiter calls time out: a feedback loop.

## Decision

The decision to fail open or closed belongs to each rule, set by the use case that owns it.

| Rule | On limiter error | Why |
|---|---|---|
| `login:ip`, `login:username` | **Fail closed** | Bypass means unlimited password guessing |
| `register:ip` | **Fail closed** | Bypass means junk accounts, and register runs bcrypt too |
| `refresh:ip` | Fail open | Guessing a 256-bit token is hopeless; keeping it open keeps signed-in users signed in |
| `message:burst`, `message:sustain` | Fail open | Bypass means spam, which can be deleted afterwards, and posting doesn't run bcrypt |

- `ratelimit.Rule` has a `FailClosed` field; the zero value keeps fail open. `Check` returns
  `model.ErrRateLimiterUnavailable` for a fail-closed rule whose limiter call failed.
- The handler answers **503 `auth_unavailable` with no `Retry-After`**. 429 would say "you sent too much", which
  is false for a user who tried once, and would mix our own outage into the attack signal in `rate_limited_total`.
  `Retry-After` is optional for 503; the server doesn't know when Redis or the CPU will recover, so it sends no
  number rather than a made-up one. The client shows "try again later" without a countdown.
- A new counter, `rate_limiter_errors_total{rule}`, counts every limiter error, whether the request was then
  allowed or rejected. Before this, failing open was visible only in logs.

### Not chosen

- **Distinguishing "Redis down" from "server busy"** (connection refused vs timeout, pool stats, local latency). A
  blackhole and an overload look the same from the inside, and for login the answer would be the same anyway.
- **A local in-memory limiter as a fallback.** Each server would count alone, so N servers allow N times the limit,
  and a restart resets the counts. Worth revisiting if failing closed turns out to cost too much.
- **Failing closed everywhere.** A Redis outage would also stop every viewer from posting and every signed-in user
  from refreshing.

## Consequences

- While Redis is unreachable, nobody can log in or register. People already signed in are unaffected, because
  refresh fails open and only touches Postgres after the limiter.
- With a blackholed Redis, a login waits the 200 ms budget before getting 503; with Redis refusing connections, or
  once the go-redis pool fails fast (about 55 s into a blackhole, ADR 0020), it fails at once.
- Under overload, login and register now shed load instead of adding bcrypt work.
- Messages still fail open under overload, the problem ADR 0021 recorded. Accepted: the damage is spam, not
  account takeover.
- If the IP check passes and the username check then fails closed, that attempt still counts against the IP and
  is not refunded. Conservative, and a refund would likely fail too while Redis is unhealthy.
- A client that cancels its own request makes the limiter call return `context canceled`, which counts in
  `rate_limiter_errors_total` and logs an error like a real Redis failure. Fine for now; filter it out before
  alerting on that counter.
- The error code is `auth_unavailable` but lives in the shared `writeIfRateLimited`; if a non-auth rule ever
  fails closed, the code should become generic.
- Verified with unit tests only (fake limiter returning an error, and one that blocks until the 200 ms deadline);
  the fault environment was not rerun.
