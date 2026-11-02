# 5. Book capacity as debt for requests that set max_wait

Date: 2026-10-12

## Status

Accepted

## Context

Some callers pace outbound calls against a third party's quota from several processes that share one
key. Each call has a deadline, often because an interactive user is waiting. When a bucket refuses such
a caller, it polls. Polling costs a round trip per attempt, callers that wake together race, and a caller
learns late that it cannot go in time. The request in gubernator-io/gubernator#111 asks for one round
trip per call, a guarantee that a call runs if it can within its deadline, an immediate refusal
otherwise, and service in arrival order.

The leaky bucket stores a floating-point balance and the time of its last credit. Each admitted hit
lowers the balance by one token, and the balance refills at one token per `rate` milliseconds. Letting
the balance go below zero turns it into a queue of future slots: the time until a request's hits are
covered is `(hits − balance) × rate`.

Rate limits live in the owning peer's memory. A restart without a Store, an LRU eviction, or a hash-ring
change gives the key a full bucket on its next request.

In-process limiters that debit ahead (Go's `x/time/rate`, Guava, Resilience4j) charge only when the wait
fits the caller's limit, return a relative duration, and have no server-wide ceiling, because a single
process owns its state. The one with a cancel API, `x/time/rate`, cannot refund exactly once later
bookings exist (golang/go#56924).

A server-held queue, where the owner holds each request open until it can be admitted, was proposed in
mailgun/gubernator#172. The requester rejected it because it costs connections and memory.

## Decision

A request opts in by setting `max_wait`, the longest it will wait in milliseconds. A leaky bucket that
cannot admit its hits now computes the wait until it can, using continuous crediting. If the wait is
within both `max_wait` and the server's ceiling, the owner deducts the hits at once, letting the balance
go below zero, and returns the wait. The caller sleeps that long and acts without calling again.
Otherwise the request is refused and nothing is charged.

The owner times these requests by its own clock. Forwarding peers send them unbatched, never retry
them, and do not send for a caller that has already gone. Once a request is sent, the booking stands
even if the caller leaves. There is no API to cancel a booking. Only leaky bucket supports `max_wait`; `GLOBAL`, `DURATION_IS_GREGORIAN`, `DRAIN_OVER_LIMIT`,
`RESET_REMAINING` and negative hits are refused when combined with it.

Continuous crediting replaces whole-token crediting for all leaky traffic, as a separate fix (ENG-208)
that lands first.

## Consequences

* A caller makes exactly one call per request and learns at once whether, and when, it may act.
* Slots go out in the order the owner processes requests, so no caller starves in a retry race. Items
  in one `GetRateLimits` call have no relative order.
* Debt outlives the request, so the bucket's cache expiry runs to the moment the bucket would be full
  again, never earlier.
* Losing a key's state forgets its bookings. Callers can then over-send by up to one ceiling's worth of
  quota plus one burst for each loss. A Store that keeps negative balances avoids this.
* A plain request on a key in debt is refused, because the opt-in is per request but the debt belongs to
  the key.
* An abandoned or unread booking wastes its slot.
* A `limit` or `duration` change on a key in debt rescales the debt by old rate over new rate, so
  granted slots keep their time and later bookings still follow them.
* A forwarded scheduling request costs its own RPC, because it skips batching.
* Every peer must be upgraded before clients set `max_wait`. An older forwarding peer batches and
  retries, and can book one request several times.
* Existing clients see continuous crediting's change in admissions on fractional rates, under ENG-208's
  release note.
* Inbound protection gains nothing. For it a refusal is the desired answer, and booking would let one
  caller crowd out others.
