# 3. Translate Envoy rate limit units to plain durations, not calendar intervals

Date: 2026-08-27

## Status

Accepted

## Context

Envoy's `RateLimitUnit` (https://www.envoyproxy.io/docs/envoy/latest/api-v3/type/v3/ratelimit_unit.proto) defines `SECOND` through `YEAR` with no statement of when a window starts or how long a `MONTH` is. The reference service `envoyproxy/ratelimit` computes windows as `unixtime / unit_seconds` with `MONTH` = 30 days and `YEAR` = 365 days, so its windows are aligned to Unix epoch multiples: a day resets at UTC midnight, a month at an arbitrary 30-day boundary.

Gubernator has two duration models: a millisecond span starting at first hit (token bucket), and `DURATION_IS_GREGORIAN`, which resets at calendar boundaries and exists for billing-style quotas ("per calendar month").

Forces:

- `MONTH` and `YEAR` have no single millisecond value, which invites mapping them to the Gregorian model.
- Doing so makes `MONTH` calendar-aligned while `DAY` stays rolling from first hit, a split no operator would predict from Envoy's config.
- The reference service's epoch alignment is a side effect of integer division, not a documented semantic; matching it is compatibility with one implementation's accident.
- Some operators do want calendar months, and gubernator already has the feature.

## Decision

We will translate every Envoy unit to a plain millisecond duration using the reference service's constants (`SECOND` 1s, `MINUTE` 60s, `HOUR` 3600s, `DAY` 86400s, `MONTH` 30 days, `YEAR` 365 days), with the window starting at first hit. Calendar alignment is opt-in: a domain policy that sets `DURATION_IS_GREGORIAN` switches translation to gubernator's interval codes, and `SECOND` under that policy is an error.

## Consequences

- Windows do not align to clock boundaries as they do in `envoyproxy/ratelimit`; a daily limit resets 24 hours after its first hit, not at UTC midnight. Migrating operators see different reset times.
- Calendar semantics stay gubernator's documented feature rather than a hidden side effect of a unit name.
- The 30-day and 365-day constants are arbitrary but match what Envoy users already have.
