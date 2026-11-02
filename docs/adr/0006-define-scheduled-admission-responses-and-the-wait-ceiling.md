# 6. Define scheduled-admission responses and the wait ceiling

Date: 2026-10-12

## Status

Accepted

## Context

[ADR 5](0005-book-capacity-as-debt-for-requests-that-set-max-wait.md) lets a request book future capacity.
The response has to tell the caller what happened, and the server has to bound how far ahead a key can
be booked. These forces apply:

- **Existing clients read status as a boolean.** They test `status == OVER_LIMIT` and treat every other
  value as allowed. A new status is safe only for a client that asked for the feature.
- **Hits that can never fit.** A request whose hits exceed the bucket's `burst` has no wait that admits
  it, and booking it would put more than `burst` calls at the third party at once.
- **No safe sentinel.** Zero must mean "no wait". A negative duration returns at once from Go's
  `time.Sleep` and raises in Python's `time.sleep`.
- **Old servers.** An owner built before `max_wait` ignores it and answers as for a plain request.
- **Two refusal causes.** A request can exceed its own `max_wait` or the server's ceiling. A caller that
  cannot tell them apart may retry pointlessly.
- **What the ceiling bounds.** After a state loss, callers on a saturated key can over-send by up to
  `ceiling / rate + burst` calls. With the ceiling in time, that is one ceiling's worth of quota, plus a burst, on
  every key. A ceiling of K × `duration` depends on how the limit is written: the same quota as 10/s or
  600/min gets caps 60× apart, and K = 2 would refuse a 30 s wait on a per-second key. The requester's
  deadlines run "up to a few minutes".
- **`RESET_REMAINING` is documented as unconditional.** The proto says it sets a leaky bucket's
  `Remaining` back to full. A plain caller can send it to a key another caller has booked.
- **`remaining` is defined as a count left before the limit.** Clients and adapters treat it as
  non-negative.

## Decision

For a request with `max_wait > 0` we will answer:

- `UNDER_LIMIT`, `retry_after` 0, when admitted now;
- `SCHEDULED`, `retry_after` ≥ 1 ms, when booked;
- `OVER_LIMIT`, `retry_after` ≥ 1 ms (the wait that would have been needed), when refused, with nothing
  charged;
- `NEVER_FITS`, `retry_after` 0, when `hits > burst`, checked first, with nothing charged, counted as
  over-limit like any refusal.

A request without `max_wait` never receives `SCHEDULED` or `NEVER_FITS`, and its `retry_after` is 0.
`retry_after` is relative milliseconds and never negative.

The server's ceiling is a wall-clock duration, `GUBER_SCHEDULE_MAX_WAIT`, default 5 minutes. A request
is booked when its wait is at most `min(max_wait, ceiling)`. There is no response field for the
ceiling: `OVER_LIMIT` with `retry_after` at or below the caller's own `max_wait` can only mean the
ceiling refused it.

`remaining` is clamped to 0 for every caller. `reset_time` is computed from the real balance.

A plain `RESET_REMAINING` on a key in debt keeps its documented meaning and forgives the debt. So does
a plain request that changes the key's algorithm, which the proto documents as a reset. Both are
counted.

## Consequences

- No client that has not set `max_wait` can receive a status it would misread as allowed.
- A client that sets `max_wait` must handle four statuses. One that treats every status other than
  `OVER_LIMIT` as "go now" acts on `SCHEDULED` early.
- A client detects an owner that predates `max_wait` by `OVER_LIMIT` with `retry_after` 0, which a
  current owner never sends to such a request.
- A caller can tell a ceiling refusal from a deadline refusal with no extra field.
- Post-loss over-send is one ceiling of quota on every key, whatever the limit's notation. Operators
  lower the ceiling to shrink it, and raise it for callers with longer deadlines.
- `remaining` keeps its meaning for every client. A key in debt reads 0, and its `reset_time` moves past
  `now + duration`.
- A plain `RESET_REMAINING`, or an algorithm change, releases outstanding bookings the way a state
  loss does. A request with `max_wait` cannot send either.
- Every booking and refusal path must test `hits > burst` first, or `NEVER_FITS` stops meaning "never".
