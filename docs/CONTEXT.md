# Domain Glossary

Terms with a specific meaning in this codebase. Keep entries short; link the feature that introduced them.

## Rate limiting

- **name / unique_key** — the two-part identity of a rate limit. `name` groups limits of one kind; `unique_key` picks the subject (account, IP). The pair is hashed to choose the owning peer.
- **owner** — the peer that holds the authoritative counter for a `name`/`unique_key` pair.
- **GLOBAL** — a behavior where every peer answers from a local copy and hits are synced to the owner asynchronously. Trades consistency for scale.
- **Gregorian duration** — with behavior `DURATION_IS_GREGORIAN`, `duration` is a calendar interval code (0 minutes … 5 years) and the limit resets at the calendar boundary, not a fixed span after first hit.

## Scheduled admission ([ENG-189](features/ENG-189-scheduled-admission/blueprint.md))

- **burst** — a leaky bucket's capacity, the most tokens its balance can hold. Defaults to `limit` when 0.
- **rate** — the leaky bucket's milliseconds per token, `duration / limit`.
- **balance** — a leaky bucket's floating-point token count. The response's `remaining` is the balance rounded down, never below 0.
- **continuous crediting** — leak is added to the balance on every request, in fractions, up to `burst` ([ENG-208](https://linear.app/kapetan-io/issue/ENG-208)).
- **plain request** — a request without `max_wait`. It never books and never receives `SCHEDULED` or `NEVER_FITS`.
- **max_wait** — the longest a request will wait for its hits, in milliseconds. Setting it opts the request into scheduled admission.
- **booking** — charging a request's hits now, though they cannot be admitted now, and answering `SCHEDULED` with the wait. The caller acts after the wait and does not call again.
- **debt** — a balance below zero, made by bookings. It is the queue of booked slots and pays itself off by leak. A `limit` or `duration` change rescales it by old rate over new rate, so it clears at the same moment (built by the config-change-in-debt piece).
- **wait ceiling** — the server's limit on any request's wait, `GUBER_SCHEDULE_MAX_WAIT`. A request is booked only when its wait is at most `min(max_wait, ceiling)`.
- **retry_after** — milliseconds from the owner's answer: when to act on `SCHEDULED`, or the wait that would have been needed on `OVER_LIMIT`. Zero on every other response and for plain requests.
- **SCHEDULED** — the status of a booked request.
- **NEVER_FITS** — the status for a request with `max_wait` whose `hits` exceed `burst`. Nothing is charged.
- **state loss** — a key's bucket disappearing from its owner (restart without a Store, LRU eviction, hash-ring change). The next request finds a full bucket and the key's bookings are forgotten.
- **documented reset** — a plain request that forgives a key's debt by a documented contract: `RESET_REMAINING`, or a change of `algorithm`. Counted in `gubernator_scheduled_debt_reset_count` (the algorithm-change count is built by the config-change-in-debt piece). Not a state loss, which is involuntary.
