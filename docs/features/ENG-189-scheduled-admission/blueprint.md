# Scheduled Admission Blueprint

Tracked as ENG-189, requested in [gubernator-io/gubernator#111](https://github.com/gubernator-io/gubernator/discussions/111) (originally [mailgun/gubernator#172](https://github.com/mailgun/gubernator/issues/172)). Decisions of record are [ADR 0005](../../adr/0005-book-capacity-as-debt-for-requests-that-set-max-wait.md) and [ADR 0006](../../adr/0006-define-scheduled-admission-responses-and-the-wait-ceiling.md). Terms are defined in [docs/CONTEXT.md](../../CONTEXT.md).

## Objective

A service that paces its outbound calls against a third-party API quota cannot use Gubernator well today. Several processes share one key. Each call has a deadline, because an interactive user waits behind it. When the bucket is empty, the service is refused and has to poll. Pollers race each other, waste round trips, and give up late instead of telling the user right away that the call cannot happen in time.

After this change, such a caller sends one request with `max_wait`, the longest it is willing to wait. It gets exactly one of three answers in that one round trip:

* **go now** (`UNDER_LIMIT`),
* **go in N milliseconds** (`SCHEDULED`; the server has already charged the hits, so the slot is the caller's),
* **you cannot go in time** (`OVER_LIMIT`, with the wait that would have been needed), so the caller fails the user immediately.

The caller never calls Gubernator again for that request. Requests are given slots in the order the owning peer processes them, so nobody is starved by retry races. This is the requester's model: debit the bucket below zero and return the wait (#111: "No retry loops or extra requests to Gubernator, no race conditions with other requests retrying at the same time").

Callers that do not send `max_wait` keep today's behavior.

## Users

* **Primary.** A service whose processes share one key to pace calls to a third party's quota, with interactive users waiting. Deadlines run from seconds to a few minutes. The requester's example uses a 10/min limit and a 2-minute timeout, so a wait can exceed one rate-limit duration.
* **Secondary.** Background senders that pace a shared limit, such as job workers and mail senders. They accept longer waits.
* **Not a target.** Protecting Gubernator's own callers from inbound traffic: there, a refusal is the desired answer, and an abusive caller could book debt that locks out everyone else on the key. High throughput `GLOBAL` users.

## Mental Model

A leaky bucket is a queue of time. Each admitted hit pushes the bucket's "next free moment" forward by `rate` milliseconds. Today, when that moment is in the future, the bucket refuses. With `max_wait`, the bucket instead hands the caller that future moment, as long as it is within the caller's `max_wait` and the server's ceiling, and pushes the queue forward as if the caller had been admitted. The balance goes below zero; the debt is the queue of booked slots.

| `status` | `retry_after` | Bucket charged? | Caller does |
| -- | -- | -- | -- |
| `UNDER_LIMIT` | `0` | Yes | Act now. |
| `SCHEDULED` | `>= 1` | Yes | Sleep `retry_after` ms, then act. Do not call Gubernator again. |
| `OVER_LIMIT` | `>= 1` | No | Give up now. The value is the wait that would have been needed. |
| `NEVER_FITS` | `0` | No | Give up. `hits` exceed `burst`; no wait helps. |

`SCHEDULED` and `NEVER_FITS` are only ever sent to a request with `max_wait > 0`. Three more cases reach such a caller:

* `error` **is set.** Ignore every other field. Check `error` first.
* `OVER_LIMIT` **with** `retry_after` **≤ the caller's own** `max_wait`**.** The server's ceiling refused it, not the caller's deadline. Retrying with the same deadline will be refused again.
* `OVER_LIMIT` **with** `retry_after` **0.** The owner predates this feature and ignored `max_wait`.

Worked example, 10/min (`rate` 6000 ms), burst 10, `max_wait` 120000, 31 requests at t=0: requests 1–10 get `UNDER_LIMIT`; request 11 gets `SCHEDULED` 6000; request *k* gets `SCHEDULED` (k−10)×6000, up to request 30 at 120000; request 31 gets `OVER_LIMIT` 126000 and is not charged.

**What** `SCHEDULED` **promises.** If every caller on a key acts at its granted time, the calls admitted and scheduled on that key conform to the configured leaky bucket: at most `burst` at once, then one per `rate`. The promise holds while the key's state survives on its owner. It does **not** cover:

* a restart without a Store, an LRU eviction, or a hash-ring change that moves the key, any of which forgets the debt and starts the key with a full bucket;
* a plain request with `RESET_REMAINING`, or one that changes the key's algorithm, which forgives the debt by its documented contract;
* callers that wake late. A late call lets the next on-time call land closer than `rate`, which any pacer has.

Gubernator promises nothing about the third party. A caller pacing a strict quota sets its limit below the real one (about 10% headroom, more at high rates or with high wake-up jitter), and uses `burst` 1 when the third party counts calls in fixed windows. A leaky bucket with `burst` = `limit` allows a full burst then one per `rate`: 19 calls inside one 60 s window for a 10/min limit.

**The opt-in is per request; the effect is per key.** While scheduled traffic holds a key in debt, a plain request on that key is refused like any over-limit request. That is correct: admitting it would spend a slot already promised.

## Wire Contract

Additions to `gubernator.proto`. Nothing else changes.

```protobuf
message RateLimitReq {
  // ... fields 1-10 unchanged ...

  // The longest this caller will wait for its hits, in milliseconds. 0 (the default)
  // keeps today's behavior. A positive value opts this request into scheduled admission
  // (LEAKY_BUCKET only): if the hits cannot be admitted now but can be within
  // min(max_wait, the server's GUBER_SCHEDULE_MAX_WAIT), the server charges them now and
  // answers SCHEDULED with retry_after; the caller sleeps retry_after, then acts, without
  // calling again. Otherwise it answers OVER_LIMIT and charges nothing. Negative is an
  // error. Do not set this from an RPC timeout; it is the caller's deadline for acting.
  int64 max_wait = 11;
}

enum Status {
  UNDER_LIMIT = 0;
  OVER_LIMIT = 1;
  // Sent only to requests with max_wait > 0. The request's hits exceed the bucket's
  // burst, so no wait will admit it. Nothing was charged.
  NEVER_FITS = 2;
  // Sent only to requests with max_wait > 0. The hits were charged now; the caller may
  // act after retry_after milliseconds and must not ask again for this request.
  SCHEDULED = 3;
}

message RateLimitResp {
  // ... fields 1-6 unchanged ...

  // Milliseconds, relative to when the owner answered. Not a timestamp like reset_time,
  // and not seconds like the HTTP Retry-After header. Non-zero only for requests with
  // max_wait > 0: on SCHEDULED it is when to act; on OVER_LIMIT it is the wait that
  // would have been needed. Always >= 1 in those two cases, 0 otherwise, never negative.
  // Check `error` first.
  int64 retry_after = 7;
}
```

* **Remaining.** `remaining` is never negative. On a key in debt every caller, opted in or not, reads 0.
* **Reset time.** On a key in debt `reset_time` is computed from the real (negative) balance, so it lies further out than `now + duration`. It still means "when the bucket is full again".
* **Go.** `retry_after` and `max_wait` are plain `int64` milliseconds: `time.Duration(resp.RetryAfter) * time.Millisecond`.
* **HTTP.** The JSON gateway writes every `int64` as a string and accepts either form on input: `"max_wait": "120000"`, `"retry_after": "6000"`. It marshals with `EmitUnpopulated`, so every HTTP response gains `"retry_after": "0"`. That key is additive.
* **Errors.** Per-request problems come back in `RateLimitResp.error`, as validation errors do today:

| Request with `max_wait > 0` and … | `error` |
| -- | -- |
| `algorithm: TOKEN_BUCKET` | `max_wait is only supported by LEAKY_BUCKET` |
| `GLOBAL` (including from `GUBER_FORCE_GLOBAL`) | `max_wait cannot be combined with GLOBAL` |
| `DURATION_IS_GREGORIAN` | `max_wait cannot be combined with DURATION_IS_GREGORIAN` |
| `DRAIN_OVER_LIMIT` | `max_wait cannot be combined with DRAIN_OVER_LIMIT` |
| `RESET_REMAINING` | `max_wait cannot be combined with RESET_REMAINING` |
| `hits < 0` | `max_wait cannot be combined with negative hits` |
| `max_wait < 0` (any request) | `field 'max_wait' cannot be negative` |

When several rows match, the first row in table order wins, after the `max_wait < 0` check, which runs first. `NO_BATCHING` and `MULTI_REGION` combine with `max_wait` without effect.

`hits = 0` is a status query for every caller. It returns `UNDER_LIMIT`, `retry_after` 0, and the clamped `remaining`, and it never books, even with `max_wait` on a key in debt.

**Server configuration.** `GUBER_SCHEDULE_MAX_WAIT` (Go: `BehaviorConfig.ScheduleMaxWait`, a `time.Duration`) takes a Go duration string such as `5m`, like `GUBER_BATCH_TIMEOUT`. Unset or 0 means the default, 5 minutes. A negative value fails startup. The effective wait for a request is `min(max_wait, ceiling)`, and a wait equal to it is booked.

## Correctness Constraints

### State Invariants

1. **A request with** `max_wait = 0` **never receives** `SCHEDULED` **or** `NEVER_FITS`**, and its** `retry_after` **is 0.** On a key not in debt its response is today's (after ENG-208). Violation is prevented by testing `max_wait > 0` before any new branch.
2. **Debt is bounded by the ceiling.** A leaky balance is never below `−ceiling / rate` tokens. A booking happens only when its wait, `(hits − balance) × rate`, is at most the ceiling, so the balance after it is at least `−ceiling / rate`. Leak only raises the balance. A `limit` or `duration` change on a key in debt rescales the balance by old rate over new rate, so the moment the debt clears, and this bound, are unchanged.
3. **A refusal never changes the balance.** `OVER_LIMIT` and `NEVER_FITS` to a request with `max_wait` leave the bucket as they found it, apart from adopting a changed `limit`, `duration` or `burst`, which never moves the moment the debt clears.
4. **Slots follow processing order.** A booking processed later never gets an earlier slot than one processed before it on the same key. Each booking lowers the balance, so the next slot (`now + wait`) is later by at least that booking's `hits × rate`.
5. **Spacing.** With no state loss and every caller acting at its slot, calls on a key never exceed `burst` at once nor arrive faster than one per `rate`, within 1 ms of rounding.
6. **An item in debt outlives its debt.** While a bucket's balance is below zero, its cache expiry is never earlier than the moment the balance would be back at `burst`. Otherwise it would expire while in debt and return as a full bucket.
7. **Only leak,** `RESET_REMAINING`**, an algorithm change, and state loss raise a negative balance.** `DRAIN_OVER_LIMIT`, a burst change, a status query, a negative-hits refund, and expiry do not. A `limit` or `duration` change rescales a negative balance but never forgives it.
8. `retry_after` **is ≥ 1 on** `SCHEDULED` **and on a** `max_wait` **request's** `OVER_LIMIT`**, 0 otherwise, and never negative.**

### Behavioral Constraints

* **Never apply one scheduling request twice.** The forwarding peer never retries it and sends it unbatched, so the owner applies it at most once.
* **Do not send for a caller who has gone.** The forwarding peer checks the caller's context before it sends. A caller that leaves afterward wastes its slot.
* **Never add a round trip.** Admission, booking and refusal are all decided in the one call.
* **Never hold a request open.** The server answers at once; the caller does the waiting.
* **Never send a status a client did not ask for.** `SCHEDULED` and `NEVER_FITS` exist only behind `max_wait > 0`, because existing clients read any status other than `OVER_LIMIT` as allowed.
* **Never schedule from an untrusted clock.** A scheduling request is timed by the owner's clock, not by a client-supplied `created_at`.

## Acceptance Criteria

1. Requests without `max_wait` return the same responses as before this change on keys that are never in debt, across every behavior, with `retry_after` 0.
2. The worked example in *Mental Model* holds exactly: 10 `UNDER_LIMIT`, then `SCHEDULED` 6000 … 120000 in order, then `OVER_LIMIT` 126000 with the bucket unchanged.
3. A request whose wait equals `min(max_wait, ceiling)` is `SCHEDULED`; one millisecond more is `OVER_LIMIT`.
4. A request whose `max_wait` exceeds the ceiling and whose wait lies between them gets `OVER_LIMIT` with `retry_after` ≤ its `max_wait`, and is not charged.
5. With burst 1 at 7/min and requests arriving at random, consecutive granted slots are never closer than `rate − 1` ms, and no half-open 60000 ms window holds more than 7 slots.
6. On a key carrying both plain and scheduling requests, including `hits = 0` status queries, granted slots and plain admissions together never exceed the leaky bucket's spacing.
7. `hits > burst` with `max_wait` returns `NEVER_FITS`, `retry_after` 0, charges nothing, and increments `gubernator_over_limit_counter`, both on an existing bucket and on a key with no bucket, where no bucket is created.
8. A plain request with `hits > 0` on a key in debt gets `OVER_LIMIT`, `remaining` 0, `retry_after` 0. With `DRAIN_OVER_LIMIT`, a changed `burst`, or negative hits, the debt is unchanged.
9. A plain `hits = 0` request with `RESET_REMAINING` on a key in debt sets the balance to `burst` and increments `gubernator_scheduled_debt_reset_count`.
10. Every combination in the error table returns its `error`, charges nothing, and books nothing.
11. A key booked into debt is not expired early: with `duration` passed since its last request, a status query 1 ms before `full_at` (the moment the balance is back at `burst`) reports `remaining` `burst − 1`, not the `burst` of a fresh bucket.
12. A scheduling request sent to a non-owner reaches the owner without entering the batch queue, and a failed forward is not retried.
13. `remaining` is never negative in any response.
14. A `hits = 0` request on a key in debt returns `UNDER_LIMIT` and `retry_after` 0, with or without `max_wait`.
15. A `limit` change on a key in debt keeps the moment the debt clears. After a raise, the next booking's slot is after the last slot already granted. After a cut, the next booking still respects the ceiling. A refused plain request that lowers the limit still extends expiry, so a status query 1 ms before the new `full_at` does not see a fresh bucket.
16. A `TOKEN_BUCKET` request on a key in debt replaces the item and increments `gubernator_scheduled_debt_reset_count`; on a key whose debt has fully leaked it does not increment it.

Criteria 1–14 are built by the first piece. Criteria 15–16 belong to the `config-change-in-debt` piece.

## Success Metrics

* The requester confirms in #111 that one call per request, with `SCHEDULED` / `OVER_LIMIT`, fits their client.
* Operators see `gubernator_scheduled_count` rise with no matching rise in `gubernator_scheduled_debt_evictions_count`. A non-zero eviction count means an undersized cache is leaking quota.

## Scope

### In Scope

* `max_wait`, `NEVER_FITS`, `SCHEDULED`, `retry_after`, with the comments above; regenerated Go and Python stubs.
* The scheduling path in the leaky bucket, the request validation, and the forwarding changes.
* `GUBER_SCHEDULE_MAX_WAIT` in config, `example.conf` and the README, and the metrics below.
* The `RESET_REMAINING` proto comment, corrected to say a leaky bucket is reset to `burst`, which is what the code does today.
* A `cluster` package option that sets `CacheSize`, so tests can force eviction.
* README: `max_wait` and the four statuses; a "pacing outbound calls" section with the caller loop, the headroom and burst guidance, the state-loss caveat, and the old-server and ceiling signals; the upgrade rule.
* ADR 0005, ADR 0006, and glossary entries in `docs/CONTEXT.md`.
* Surface tests for every acceptance criterion.

### Out of Scope / Non-Goals

* **Token bucket.** Booking into the next window would put every booked call at the window boundary.
* `GLOBAL` **and** `DURATION_IS_GREGORIAN`**.** Non-owners cannot book, and a calendar boundary expires the item and forgives the debt.
* **Cancellation or release of a booking.** An abandoned slot is wasted. Prior art shows refunds after later bookings are not exact (golang/go#56924), and a refund while in debt would give its slot to a later caller already holding the same moment.
* **Durability beyond the existing Store and Loader.**
* **A server-held queue.** The requester rejected it to save connections and memory.
* **Advisory waits for plain requests.** A request without `max_wait` never gets `retry_after`.
* **Client helpers and adapters.** The proto comment carries the contract; the Envoy adapter decides for itself.
* **Coordination across clusters or data centers.**

## Dependencies and Constraints

* **ENG-208 lands first, as its own PR.** It replaces the leaky bucket's whole-token crediting with continuous crediting for all traffic. Scheduling reads the stored balance and `UpdatedAt` as an exact level. Today's crediting overstates that level two ways, through a clamp that truncates before comparing to `burst` and through an `UpdatedAt` that does not advance when a hit lands on a full bucket. Simulated mixed traffic with status queries then books 745–1,673 calls early per ~100k sends; with continuous crediting it books none. ENG-208 changes plain admissions on fractional rates with burst > 1 (0 to +26%) and needs its own release note.
* **ENG-213 should land first or alongside.** The requester calls Gubernator from Python, and the shipped Python client cannot be imported.
* **Not blockers.** ENG-206 is token bucket only. ENG-210 (forward retries can apply plain hits several times) is bypassed because scheduling requests are never retried. ENG-223 (`reset_time` uses `limit` instead of `burst`) and ENG-224 (client `created_at` can move `UpdatedAt` into the future) affect keys in debt as they affect other keys; the scheduling path tolerates a future `UpdatedAt` (see *Architecture*).
* **Every peer must run this version before any client sets** `max_wait`**.** An older forwarding peer batches and retries, which can book one request several times.
* `docs/CONTEXT.md` **and** `docs/adr/` **are also created on another in-flight branch** (the Envoy adapter, ADRs 0001–0004). Whichever merges second keeps both glossary sections. If this merges first, ADR numbering starts at 0005 until 0001–0004 arrive.

---

## Architecture

The change lives in three places: request validation in `V1Instance.GetRateLimits`, the forwarding path, and the leaky bucket in `algorithms.go`. There is no new component, interface, storage field or RPC.

### Validation

`GetRateLimits` validates `max_wait` after it applies `GUBER_FORCE_GLOBAL`, so a forced `GLOBAL` is seen. `getLocalRateLimit` repeats the combination checks, because an older peer may forward a request without validating it. Every owner path passes through it. Errors use the per-request `error` string.

### Forwarding

A request with `max_wait > 0` that belongs to another peer:

* is sent with `NO_BATCHING`, set on a copy as the `GLOBAL` path already does, so it skips the batch queue's delay and the batch path's retries. Both paths send on a context the forwarding peer owns, so the owner never sees the caller's cancellation;
* is never retried by the forwarding loop. A failure returns the error;
* is not sent if the caller's context is already done, by the pre-send check both forwarding paths already make. Once sent, the booking stands even if the caller leaves, and its slot is wasted.

A booking whose response is lost after the owner applied it is wasted, like an abandoned slot.

Slots are ordered by arrival at the owner's worker. Items in one `GetRateLimits` call have no relative order: the receiving peer handles its own items in sequence, but forwarded items and the items of a `GetPeerRateLimits` batch run concurrently.

### Leaky bucket

ENG-208 supplies continuous crediting: when `now > UpdatedAt`, the balance becomes `min(burst, balance + (now − UpdatedAt) / rate)` and `UpdatedAt` becomes `now`. The scheduling path adds, in order:

1. **Clock.** A request with `max_wait > 0`, or any request on a key whose stored balance is below zero, uses the owner's `clock.Now()` as `now`, not the request's `created_at`.
2. **Guards.** The combination errors above. `hits > burst` with `max_wait` returns `NEVER_FITS`, where `burst` is the request's own (`limit` when 0), the value the bucket adopts for this request, not the stored one. It counts in `gubernator_over_limit_counter`, as the same request without `max_wait` does today.
3. **Status query.** `hits = 0` returns `UNDER_LIMIT` with the clamped `remaining`. It never books.
4. **Admit.** If `balance ≥ hits`, deduct and return `UNDER_LIMIT`, as today.
5. **Wait.** `wait = ceil(max(UpdatedAt, now) − now + (hits − balance) × rate)` in milliseconds, at least 1. The `max(UpdatedAt, now)` term covers an `UpdatedAt` that a client-stamped plain request pushed into the future (ENG-224); without it the wait comes out short.
6. **Decide.** If `wait > min(max_wait, ceiling)`, return `OVER_LIMIT` with `retry_after = wait` and change nothing; it counts in `gubernator_over_limit_counter` like any refusal. Otherwise `balance −= hits` and return `SCHEDULED` with `retry_after = wait`.
7. **Expiry.** When a request books, or is admitted or applied on a key whose balance is below zero before or after it, set the item's expiry to `max(current expiry, now + duration, ceil(full_at))`, where `full_at = UpdatedAt + (burst − balance) × rate` uses the balance after the charge and is when the bucket is back at `burst`. It is never shortened. A request that changes `limit`, `duration` or `burst` on a key in debt also runs this step whatever its outcome, because the refill back to `burst` can lengthen. Refusals (`OVER_LIMIT`, `NEVER_FITS`), combination errors and `hits = 0` status queries return before this step and leave expiry alone, unless they change `limit`, `duration` or `burst` on a key in debt; the booking that created the debt already set it. Other requests keep today's expiry update.

A key with no bucket takes the new-bucket path, which applies the same clock rule and guards. A new bucket starts full, so a request that passes the guards is admitted; `hits > burst` with `max_wait` returns `NEVER_FITS` and creates no bucket. (Today that path answers `OVER_LIMIT` and stores an empty bucket; plain requests keep that.)

Rounding up keeps every slot on or after its exact time, and the next booking is computed from the exact float balance, not from the rounded slot. This is GCRA (the generic cell rate algorithm) expressed on the balance and `UpdatedAt` the bucket already stores: `full_at` is GCRA's theoretical arrival time.

Changes to existing branches when the balance is below zero:

* `DRAIN_OVER_LIMIT` on a plain request sets the balance to `min(balance, 0)` instead of 0, so it never raises a debt to zero.
* **A changed** `burst` updates `burst` without refilling.
* **A changed** `limit` **or** `duration` rescales the debt. Leak is first credited at the stored rate; if the balance is still below zero it becomes `balance × old_rate / new_rate`, so the debt clears at the same moment and every granted slot keeps its time. Then the request's values are adopted. A balance of zero or above is not rescaled. A request with `DURATION_IS_GREGORIAN`, whose rate follows the calendar, does not rescale and leaves the stored `limit` and `duration` as they are, so the stored rate stays the one the debt was booked at.
* **Algorithm switch.** A plain request whose `algorithm` differs from the stored item's removes the item as today, which the proto documents as a reset ("any previous rate limit hit counts are reset"). If the item is a leaky bucket whose balance, credited to `clock.Now()`, is below zero, the forgiveness is counted.
* **Negative hits** on a plain request leave the balance alone.
* `RESET_REMAINING` on a plain request sets the balance to `burst`, as the code does today, and counts the forgiveness. Its hits, if any, are then applied as usual. The proto comment, which says `Limit`, is corrected to `burst`.
* **Response.** `remaining = max(0, floor(balance))`; `reset_time` from the unclamped balance.

### Metrics

| Metric | Counts |
| -- | -- |
| `gubernator_scheduled_count` | `SCHEDULED` responses. Not counted as over-limit. |
| `gubernator_scheduled_ceiling_refused_count` | `OVER_LIMIT` where `ceiling < wait ≤ max_wait`. |
| `gubernator_scheduled_debt_evictions_count` | LRU evictions of an item whose current balance is below zero. |
| `gubernator_scheduled_debt_lost_tokens` | The tokens of debt those evictions dropped. |
| `gubernator_scheduled_debt_reset_count` | Debt forgiven by a documented reset: `RESET_REMAINING` on a key in debt, or an algorithm switch that removes a leaky item whose balance, credited to now, is below zero. |

None carries a label: the cache key cannot be split back into a name, and adding a label to an existing metric would change its series. `gubernator_over_limit_counter` gains no label and counts every refusal as today, including `NEVER_FITS` and a `max_wait` request's `OVER_LIMIT`; it never counts `SCHEDULED`.

The eviction metrics come from the built-in LRU cache, next to `gubernator_unexpired_evictions_count`. It computes an evicted leaky item's current balance from the item's stored balance, `UpdatedAt`, `Limit` and `Duration`, credited to `clock.Now()`. A custom `Cache` does not emit them.

## Data Design

No schema or storage change. The debt is the existing float `LeakyBucketItem.Remaining` going below zero; `UpdatedAt`, `Burst` and the cache item's expiry are reused.

A Store implementation must persist a negative `Remaining` and the extended expiry as given. One that clamps `Remaining` to zero forgives debt. The Loader runs only on graceful shutdown, so a crash loses debt even when a Loader is configured.

### Invariant Preservation

| Invariant | Operations that touch it, and why they hold |
| -- | -- |
| 1. No `max_wait`, no change | Every new branch tests `max_wait > 0` first. A key enters debt only through a booking, so a key never booked keeps today's paths. |
| 2. Debt bound | Booking (step 6) requires `wait ≤ ceiling`. Leak raises the balance. `DRAIN`, burst change and negative hits leave a negative balance alone. A `limit` or `duration` change rescales it so it clears at the same moment, which keeps the bound at the new rate. |
| 3. Refusal leaves the balance | `NEVER_FITS`, on both the existing-bucket and new-bucket paths, and the `OVER_LIMIT` in step 6 return before any write to the balance, apart from adopting a changed `limit`, `duration` or `burst`, which never moves the moment the debt clears. Combination errors return before the algorithm. |
| 4. Order | Each booking lowers the balance by `hits`, so a later slot is later by at least `hits × rate`. The owner's worker processes one request for a key at a time. A `limit` or `duration` change keeps the moment the debt clears, so later bookings still follow granted slots. |
| 5. Spacing | Rests on continuous crediting (ENG-208) and exact clamping. Holds in simulation with mixed traffic; depends on application logic, so it carries the heaviest testing. |
| 6. Expiry | Step 7 uses `max` and covers every request that books or lands on a key in debt. A `limit`, `duration` or `burst` change on a key in debt runs step 7 whatever its outcome. Plain requests on a key not in debt keep today's update. |
| 7. Forgiveness | Only the `RESET_REMAINING` branch, the algorithm-switch branch and a cache miss produce a balance of `burst` from a negative one. |
| 8. `retry_after` | Set only in steps 5–6, floored at 1. Every other response, including the status query in step 3, leaves the zero value. |

### Illegal State Analysis

Nothing in the types prevents a negative balance from reaching a client or a refusal from writing. Both rest on the branches above. `remaining` is clamped where the response is built; the refusal paths are covered by tests that compare the bucket before and after.

## Security

A caller that can reach Gubernator can already exhaust any key it names. `max_wait` lets it also book up to `GUBER_SCHEDULE_MAX_WAIT` of future capacity on that key, which other callers then wait behind. The ceiling bounds that, and inbound protection is a non-goal for this mode.

## PII

None. No new data is stored or logged.

## Scale

A few float operations on a path that already does float math. One booking costs the same as one admission. A scheduling request that belongs to another peer skips batching, so it costs one RPC of its own. Debt costs no memory: it is the existing balance field.

After a state loss, callers on a saturated key can over-send by up to `ceiling / rate + burst` calls: the forgotten debt, at most one ceiling's worth of quota, plus the full bucket the key restarts with. That happens once per loss event, and repeats for every ownership change. With the default 5 minutes on a 10/min key, burst 10, that is 60 calls.

## Rollout

1. Land ENG-208 with its release note.
2. Upgrade every peer.
3. Let clients set `max_wait`.

A client can detect an owner that predates this change: it answers `OVER_LIMIT` with `retry_after` 0, which a current owner never sends to a request with `max_wait`. An older peer that forwards to a current owner passes `max_wait` through but may batch and retry it, so step 2 must finish before step 3.

## Testing

Testing follows the `surface-testing` skill. The surface is `GetRateLimits` over gRPC, reached as the functional tests reach it: `guber.DialV1Server(cluster.PeerAt(0).GRPCAddress, nil)` against `cluster.Start(n)`, with time controlled through `clock.Freeze` and `clock.Advance`. No test calls algorithm functions directly.

* **Worked example and boundary (criteria 2, 3, 4).** One owner, frozen clock.
* **Spacing (criteria 5, 6).** Random arrival sequences with a fixed seed, mixed plain, scheduling and `hits = 0` requests, checking every granted time against an exact leaky bucket in the test.
* **No change without** `max_wait` **(criterion 1).** The existing leaky tests pass unchanged on top of ENG-208, plus paired sequences with and without `max_wait` on separate keys.
* **Never fits (criterion 7).** On an existing bucket and on an unknown key; a following status query shows the existing bucket unchanged and the unknown key still full.
* **Debt protection (criteria 8, 9, 13, 14).** Put a key in debt, then send each plain variant and assert the balance through a status query and through the metrics.
* **Config changes in debt (criteria 15, 16).** Put a key in debt, then raise and lower `limit` with plain and scheduling requests and send one `TOKEN_BUCKET` request; assert granted slots, status queries and the reset metric.
* **Errors (criterion 10).** Table-driven, one case per row of the error table, including a server started with `GUBER_FORCE_GLOBAL`.
* **Expiry (criterion 11).** Book enough debt that repaying it takes longer than `duration`, advance past `duration` to 1 ms before `full_at`, and assert a status query reports `remaining` `burst − 1`.
* **Forwarding (criterion 12).** A scheduling request sent to a non-owner; assert it is not held for the batch window while a batched control request on the same peer is. A forward to a stalled owner returns one error and one call.
* **Metrics.** Scraped from the daemon's `/metrics` endpoint.

No fakes are needed beyond a peer server the test controls for the forwarding case. Eviction tests use the new `cluster` option to start a peer with a small `CacheSize`.

## Limitations & Future Work

* **State loss forgets bookings** (restart without a Store, eviction, ring change), bounded per event as in *Scale*. A Store makes debt survive.
* **Late wake-ups** break third-party conformance for any pacer; headroom is the caller's defense.
* **Large requests can starve** behind a steady stream of small ones, because a large request needs more room within the same `max_wait`.
* **Abandoned and lost bookings** waste capacity until their slot passes. This includes a caller that leaves after its request was forwarded.
* **Plain traffic on a shared key** keeps ENG-210's duplicate application on the batch path.
* **No** `GLOBAL`**, Gregorian or token bucket support.** Each needs its own design if someone asks.
* **The ceiling is server-wide.** Per-name ceilings wait for a user who needs them.

## Glossary changes

Applied to `docs/CONTEXT.md` by the first piece:

* **documented reset** (new). A plain request that forgives a key's debt by a documented contract: `RESET_REMAINING`, or a change of `algorithm`. Counted in `gubernator_scheduled_debt_reset_count`. Not a state loss, which is involuntary.
* **debt** (append). A `limit` or `duration` change rescales it by old rate over new rate, so it clears at the same moment.

## Pieces

| Slug | After it merges | Builds on |
| -- | -- | -- |
| `scheduled-admission` | A caller that sets `max_wait` on a leaky key, through any peer, gets `UNDER_LIMIT`, `SCHEDULED` with `retry_after`, `OVER_LIMIT`, or `NEVER_FITS` in one call; plain callers on a key in debt are refused; operators set `GUBER_SCHEDULE_MAX_WAIT` and see `gubernator_scheduled_count`, `gubernator_scheduled_ceiling_refused_count` and `gubernator_scheduled_debt_reset_count`. Criteria 1–14. | ENG-208 |
| `config-change-in-debt` | Changing `limit` or `duration` on a key that holds bookings keeps every granted slot at its time, and an algorithm switch that forgives debt is counted. Criteria 15–16. | `scheduled-admission` |
| `debt-eviction-metrics` | Operators see bookings lost to cache eviction through `gubernator_scheduled_debt_evictions_count` and `gubernator_scheduled_debt_lost_tokens`. | `scheduled-admission` |

## Changes from build (ENG-354)

* **Criterion 12 and the forwarding test.** The design asserted `gubernator_batch_queue_length` stays unchanged. That gauge returns to zero before a scrape can see it, so the test asserts the observable effect instead: a scheduling request is not held for the batch window, and a stalled owner receives one call. The blueprint's Testing section is updated to match.
* **Validation placement.** The combination checks run in `GetRateLimits` and again at the start of `getLocalRateLimit`, which every owner path (direct, retried, and `GetPeerRateLimits`) passes through, instead of inside the leaky bucket function.
* **Wait rounding and zero snap.** The wait is `ceil(x − 1e-6)` so float error cannot turn an exact 6000 into 6001, and a balance within 1e-9 of zero is set to exactly zero after a deduction, so a spent bucket is never read as in debt. Both are added to step 5 and to the plain deduction.
* **Plain negative hits on a key in debt** answer `UNDER_LIMIT` with `remaining` 0 and leave the balance alone; the design only said the debt is unchanged.
* **Items for later pieces.** The `limit`/`duration` rescale, the algorithm-switch count and criteria 15–16 are not built here.

## Decision Log

Decisions made during design. Each was verified by sub-agents against the code and simulations; where agents disagreed, a skeptic and an arena run settled it.

* **Decided:** phase 2 (server-side reservation) is the goal; the advisory-only phase 1 does not ship on its own.
* **Decided:** clients must opt in to any new behavior. Opt-in is a non-zero `max_wait` alone, with no behavior flag.
* **Decided:** continuous crediting for all leaky traffic lands as ENG-208's own PR before this feature, with a release note.
* **Decided:** the wait ceiling is a wall-clock duration (`GUBER_SCHEDULE_MAX_WAIT`, default 5m), not K × `duration`.
* **Decided:** a plain `RESET_REMAINING` on a key in debt forgives the debt, as the proto documents, and is counted.
* **Decided:** the Envoy adapter is out of scope; it adapts to these fields on its own.
* **Decided (review):** the owner does not check whether a caller has left. The forwarding peer's existing pre-send check is the only cancellation check; once a request is sent, the booking stands and a caller that leaves wastes its slot.
* **Decided (review, after an arena):** a `limit` or `duration` change on a key in debt rescales the debt by old rate over new rate, so granted slots keep their time and later bookings follow them; keeping tokens would let a raised limit book new callers ahead of granted slots. Gregorian requests do not rescale.
* **Decided (review, after an arena):** `NEVER_FITS` counts in `gubernator_over_limit_counter`, as the same request without `max_wait` does today.
* **Decided (review, after an arena):** an algorithm switch on a key in debt forgives the debt, as the proto documents, and counts in `gubernator_scheduled_debt_reset_count`. The metric stays although it carries no key, for testability and criterion 9.
* Settled by evidence: leaky bucket only; `GLOBAL`, Gregorian, `DRAIN_OVER_LIMIT`, `RESET_REMAINING`, negative hits and `TOKEN_BUCKET` are errors with `max_wait`; `NEVER_FITS` kept; `remaining` clamped to 0; no cancel API; no LRU pinning; no empty-start buckets; forwarded scheduling requests unbatched and never retried; the ceiling refusal is signaled by `retry_after ≤ max_wait` with no extra field.
