# Design Decisions

Short decision records (ADR style): the problem, what was decided, what else was considered, and the
trade-offs that were accepted. Most of them started as a bug found by a review, a failing test, or a
fault-injection experiment.

| # | Decision | Topics |
|---|---|---|
| [0001](0001-per-room-pubsub.md) | Per-room Pub/Sub with lazy subscribe and debounce | horizontal scaling, fan-out, timer races, join/subscribe ordering |
| [0002](0002-reconciliation-loop.md) | Reconciliation loop: a stuck Redis must not freeze the server | no I/O under locks, desired vs actual, state machines, blast radius |
| [0003](0003-slow-client-outbox.md) | Slow clients must not stall broadcast: per-connection outbox | backpressure, slow-consumer isolation, goroutine ownership |
| [0004](0004-refresh-token-revocation.md) | "Revoke all sessions" must take effect immediately | refresh-token rotation, TOCTOU, row locks, `now()` vs `clock_timestamp()` |
| [0005](0005-expand-contract-migration.md) | Zero-downtime schema change: expand / contract | rolling deploys, migrations, composite indexes |
| [0006](0006-input-validation.md) | Text input validation: reject, don't rewrite | Unicode, PRECIS (RFC 8264), layered defense |
| [0007](0007-metrics-cardinality.md) | Metric labels must not be chosen by the client | Prometheus cardinality, middleware ordering, wiring tests |

Each fix was checked with **mutation testing**: deliberately break the code and confirm a test fails.
