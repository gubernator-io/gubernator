# Domain Glossary

Terms with a specific meaning in this codebase. Keep entries short; link the feature that introduced them.

## Rate limiting

- **name / unique_key** — the two-part identity of a rate limit. `name` groups limits of one kind; `unique_key` picks the subject (account, IP). The pair is hashed to choose the owning peer.
- **owner** — the peer that holds the authoritative counter for a `name`/`unique_key` pair.
- **GLOBAL** — a behavior where every peer answers from a local copy and hits are synced to the owner asynchronously. Trades consistency for scale.
- **Gregorian duration** — with behavior `DURATION_IS_GREGORIAN`, `duration` is a calendar interval code (0 minutes … 5 years) and the limit resets at the calendar boundary, not a fixed span after first hit.

## Envoy rate limit service ([ENG-168](features/ENG-168-envoy-rate-limit-service/blueprint.md))

- **RLS** — Envoy's `RateLimitService` gRPC API. Gubernator serves it as an optional adapter.
- **domain** — the namespace string Envoy's rate limit filter sends with every RLS call. Not a DNS name. Gubernator keys policy on it.
- **descriptor** — one list of `key: value` entries Envoy builds for a request. Each descriptor maps to one gubernator rate limit.
- **override** — Envoy's per-descriptor limit (`requests_per_unit`, `unit`) from route config. The only source of a limit value in the adapter.
- **domain policy** — gubernator-side settings for a domain: algorithm, behaviors, `on_missing_limit`. Applied through the policy API, propagated peer to peer, never a limit value.
- **on_missing_limit** — what a domain does with a descriptor that has no override: `deny` (default), `allow`, or `error`.
- **version / origin** — the order of a domain policy entry. `version` is wall-clock milliseconds ratcheted above the previous stored value; `origin` is the applying peer's advertise address and breaks equal versions. Peers keep the higher `(version, origin)`.
