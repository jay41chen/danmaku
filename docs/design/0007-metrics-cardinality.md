# 0007 — Metric labels must not be chosen by the client

**Status**: Accepted (2026-11-03)

## Context

In Prometheus, every combination of label values is a separate time series, and each one stays in memory.
HTTP metrics used `r.URL.Path` and `r.Method` as labels. The server is public, so anyone could request
random URLs or invent methods and create an unbounded number of series, with no login and very little
traffic. Even legitimate traffic grows it: `/rooms/{public_id}` produced one series per room. Each
histogram series also has a dozen buckets plus `_sum` and `_count`.

## Decision

Label values may only come from a **finite set we define**. `status_code` is decided by the server, so it's
fine as is. `path` and `method` are decided by the client, so they are bounded:

| label | value | otherwise |
|---|---|---|
| `path` | the matched route pattern from `r.Pattern`, method prefix removed (`/rooms/{public_id}`) | `unmatched` (404 / 405 / preflight; distinguished by `status_code`) |
| `method` | the 9 standard HTTP methods | `other` (including lowercase `get`) |

A 405 must **not** fall back to `r.URL.Path`. `DELETE /rooms/<random>/messages` would blow it up just as
well.

## The trap

The mux writes `r.Pattern` into the **same `*Request` it was given**, during `ServeHTTP`. So the metrics
middleware must read it *after* `next.ServeHTTP`. It also only works if no middleware between metrics and
the mux copies the request: `r.WithContext()` makes a copy, the mux would write into the copy, and every
request would silently be labelled `unmatched`. That would compile fine and pass any unit test that
doesn't go through the real chain.

To guard against this, the middleware chain was extracted into `serviceHandler(mux, origins)`, shared by
production and `main_test`, plus two kinds of tests:

- **Table tests** (`TestGetRoutePath`, `TestGetMethod`) check every value mapping. They catch things like
  a missing `HEAD` in the allowlist.
- A **wiring test** (`TestServiceHandlerMetricsUseRoutePattern`) runs real requests through the real
  chain. It catches mistakes like reading the pattern too early or a middleware that copies the request.
  It uses `POST /rooms/abc/messages`, whose URL path differs from its pattern; `/healthcheck` can't tell
  the two apart.

## Consequences

- `/metrics` itself is still publicly readable; this is tracked separately.
- If a future subtree route plus a redirect makes stdlib set `r.Pattern` to the client's path (a CONNECT
  edge case), this needs revisiting.
