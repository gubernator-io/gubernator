# Envoy Rate Limit Service Blueprint

Linear: [ENG-168](https://linear.app/kapetan-io/issue/ENG-168/gubernator-envoy-rate-limit-service-api-support)

## Objective

Envoy operators who want global rate limiting today run `envoyproxy/ratelimit` plus Redis. Gubernator serves the same gRPC API (`envoy.service.ratelimit.v3.RateLimitService/ShouldRateLimit`) so they point Envoy's `rate_limit_service` at a gubernator cluster and drop Redis. Gubernator's consistent-hash ownership replaces the shared datastore.

The feature is an optional adapter on the v2 line. It is off by default and adds nothing to the request path of existing clients.

## Mental Model

**Envoy decides how much. Gubernator decides how to count.**

- Envoy attaches the limit (`requests_per_unit`, `unit`) to every descriptor through its `RateLimit.Override` route config. Gubernator never stores a limit value.
- Gubernator holds an optional **domain policy**: for a given RLS `domain`, which algorithm and behaviors to count with, and what to do when a descriptor arrives with no limit. Policy is applied through gubernator's API, the same way all rate-limit configuration reaches gubernator, and a CLI translates a YAML file into that API call.
- A **domain** is the namespace string Envoy's rate limit filter sends with every call (`envoy.filters.http.ratelimit` `domain:`; overridable per route through `RateLimitPerRoute.domain`). It is not a DNS name. An operator chooses counting behavior for a route by choosing which domain that route sends.
- A **descriptor** is Envoy's list of `key: value` entries for one request (`path`, `remote_address`, a header value, ...). Each descriptor becomes one gubernator rate limit. All descriptors in a call are evaluated; the call is over limit if any one is.

The feature deliberately does not read `envoyproxy/ratelimit`'s YAML descriptor tree. Gubernator's native model is that the caller sends the limit with the request; the reference RLS inverts that, and matching its matcher and fixed-window counter would make gubernator promise behavior it does not have.

## Core Design Principles

1. **No limit value ever originates in gubernator configuration.** The only source of a limit is the descriptor's `Override`. A descriptor without one is handled by policy (`deny`, `allow`, or `error`), never by a configured number.
2. **Policy is API-driven.** Environment variables and the config file carry server settings and global defaults only. Per-domain policy enters through the policy API and propagates peer to peer.
3. **Opt-in, zero-config floor.** `GUBER_ENVOY_RLS_ENABLED=true` with no policy applied is a working deployment: every domain uses the global defaults.
4. **Fail loud, never fail open silently.** Anything gubernator cannot evaluate becomes a gRPC error on the whole call so Envoy's `failure_mode_deny` decides. No descriptor is reported OK because gubernator skipped it.
5. **Policy lookup never touches the network on the hot path.** Peers serve Envoy from an in-memory snapshot; convergence happens in the background.

## Correctness Constraints

### State Invariants

**I1. One winner per domain.** For any domain, a peer holds at most one policy entry, and it is the highest `(version, origin)` that peer has received. Violation: a lower apply or delete overwriting a higher one. Enforcement: the policy store has a single write path, `merge`, which discards an incoming entry whose `(version, origin)` is not strictly greater than the stored pair. Deletes are tombstones with a version and go through the same path, so a stale apply cannot resurrect a deleted domain.

**I2. Every gubernator rate limit created by the adapter carries a limit from the descriptor.** Violation: a `RateLimitReq` with `Limit` from any source other than `descriptor.limit`. Enforcement: the translation builds a request only in the branch where the override is present and valid (`unit != UNKNOWN`, `requests_per_unit > 0`); the missing-limit branch never constructs a request.

**I3. Descriptor identity is deterministic.** The same multiset of entries yields the same `name` and `unique_key` regardless of entry order. Violation: two Envoys sending the same entries in different order landing in different buckets. Enforcement: entries are stably sorted by key before joining; duplicate keys are kept in their sorted positions.

**I4. Policy version is strictly increasing per domain on the applying peer.** Violation: a later apply on the same peer carrying a lower or equal version. Enforcement: the applying peer computes `version = max(clock.Now() in milliseconds, stored version + 1)` inside the store's write path, so two applies in the same millisecond and a clock step backwards both still produce a greater version. Two peers can still stamp an equal version for the same domain (same millisecond, no prior entry); `merge` breaks that tie deterministically by comparing `origin`, the applying peer's advertise address, so every peer picks the same winner. We considered leader election (`kapetan-io/election.go`) so one peer mints versions; we chose wall-clock plus ratchet plus tiebreak because the policy set is tens of domains changed at human cadence, and a leader adds an apply-unavailable failure mode during elections. `version` is opaque to clients, so moving to a leader or a hybrid logical clock later is a store-internal change.

### Behavioral Constraints

- **Never report a descriptor OK that gubernator did not evaluate.** A per-item `RateLimitResp.Error` or a transport failure to the owning peer fails the whole `ShouldRateLimit` call with a gRPC error.
- **Never silently accept an unsupported input.** A descriptor with zero entries fails the call with `InvalidArgument` naming the domain. Unknown YAML keys (including the reference RLS's `shadow_mode`, `unlimited`, `rate_limit`) are rejected by the CLI before any API call.
- **Never block Envoy traffic on policy propagation.** `ShouldRateLimit` reads an atomically swapped immutable snapshot; apply/delete/pull replace the snapshot.
- **Apply never waits on an unreachable peer longer than the configured timeout** (reuses `Behaviors.GlobalTimeout`, default 500ms, per peer, in parallel). Unreachable peers are listed in the response; the apply still succeeds locally.
- **All descriptors in a call are incremented, then the result is the OR.** A call rejected because descriptor 3 is over limit has consumed hits on descriptors 1 and 2. This matches Envoy's contract and the reference implementation; it is documented, not hidden.
- **Negative hits never produce OVER_LIMIT** and are not capped at the limit. Gubernator already banks credit above `limit` on negative hits; the adapter passes `is_negative_hits` through unchanged and the docs say so.

## Acceptance Criteria

Each is verifiable from a functional test against `cluster.Start(n)` with the go-control-plane RLS client and the policy API client.

1. With `GUBER_ENVOY_RLS_ENABLED` unset, `ShouldRateLimit` returns `Unimplemented` and no policy RPCs are registered.
2. A descriptor with `limit {requests_per_unit: 2, unit: MINUTE}` returns `OK` twice and `OVER_LIMIT` on the third call; `statuses[0].current_limit` echoes `{2, MINUTE}`, `limit_remaining` is 1, 0, 0, and `duration_until_reset` is `> 0` and `<= 60s`.
3. Two calls with the same entries in different order produce one bucket: the second call's `limit_remaining` is one lower than the first.
4. A call with two descriptors where only the second is over limit returns `overall_code: OVER_LIMIT`, `statuses[0].code: OK`, `statuses[1].code: OVER_LIMIT`, and the first descriptor's remaining count decreased.
5. `is_negative_hits: true, hits_addend: 1` on an untouched limit of 2 returns `OK` with `limit_remaining: 3`.
6. A descriptor with no override on a domain with no policy returns `OVER_LIMIT` for that descriptor (default `deny`) and increments `gubernator_envoy_rls_missing_limit_total{domain, action="deny"}`.
7. After `ApplyPolicies` with `on_missing_limit: ALLOW` for that domain, the same call returns `OK`; with `ERROR`, the call fails with `InvalidArgument`.
8. After `ApplyPolicies` with `behaviors: [DURATION_IS_GREGORIAN]` and a `unit: MINUTE` descriptor, `duration_until_reset` equals the time to the next clock minute under a frozen clock.
9. `MONTH` without Gregorian yields `duration_until_reset` of 30 days minus elapsed; `YEAR` yields 365 days minus elapsed.
10. Apply on peer A: within 5s, `ListPolicies` on every other peer returns the same entry and version.
11. Under a frozen clock, apply the same domain on peer A, advance the clock 1ms, apply it on peer B with a different value before A's broadcast is observed: within 5s all peers return B's entry. Apply on A and B again without advancing the clock: within 5s all peers return the same entry, the one whose `origin` sorts higher.
12. `DeletePolicies` on peer A, then a replay of an older apply on peer B: all peers still report the domain absent.
13. Stop peer C, apply on A, start C: within `GUBER_ENVOY_POLICY_SYNC_INTERVAL` (test value 1s) C returns the policy.
14. A call with a zero-entry descriptor fails with `InvalidArgument` and no bucket is created (a following valid call shows a fresh limit). A call with zero descriptors also fails with `InvalidArgument`.
15. A call with 1001 descriptors fails with `OutOfRange`.
16. `gubernator-cli envoy apply -f policy.yaml` against a cluster produces the same `ListPolicies` result as the equivalent RPC; a file containing `shadow_mode: true` exits non-zero with the key named and makes no RPC.
17. `gubernator-cli envoy get` prints every domain's policy and version as YAML.
18. Over TLS-enabled daemons, `ShouldRateLimit` succeeds with the same client TLS config the V1 API requires.

## Scope

### In Scope

- `RateLimitService/ShouldRateLimit` on the existing gRPC server(s), gated by `GUBER_ENVOY_RLS_ENABLED`.
- Global defaults: `GUBER_ENVOY_ALGORITHM`, `GUBER_ENVOY_BEHAVIOR`, `GUBER_ENVOY_ON_MISSING_LIMIT`, `GUBER_ENVOY_POLICY_SYNC_INTERVAL`.
- Policy API: `ApplyPolicies`, `DeletePolicies`, `ListPolicies` over gRPC and the grpc-gateway HTTP endpoints.
- Peer propagation: broadcast on apply/delete, bootstrap pull on startup, periodic anti-entropy pull.
- `gubernator-cli envoy apply|get|delete`.
- Metrics and docs with two entry points (existing gubernator operators; Envoy operators new to gubernator, including a migration note from the reference RLS).

### Out of Scope / Non-Goals

- `RateLimitResponse.quota`. Follow-up ticket; it maps onto GLOBAL behavior.
- Reading `envoyproxy/ratelimit` YAML, `shadow_mode`, `unlimited`, wildcard descriptor matching.
- Custom `response_headers_to_add`, `request_headers_to_add`, `raw_body`, `dynamic_metadata` in the response. Envoy's `enable_x_ratelimit_headers: DRAFT_VERSION_03` builds headers from the status fields the adapter already fills.
- A fallback limit value in policy.
- Policy persistence across a full cluster restart. Follow-up: `gubernator --envoy-apply=/path/to/policy.yaml` applies a file at startup when the cluster holds no policy.
- Policy hot reload from a file; the API is the reload.
- Support on the v3 line (no gRPC server there).

## Dependencies and Constraints

- `github.com/envoyproxy/go-control-plane/envoy` v1.39.0 for the RLS types. Only `service/ratelimit/v3`, `extensions/common/ratelimit/v3`, and `type/v3` link in.
- Envoy's `rate_limit_service` supports only `GrpcService`; the adapter must live on a gRPC listener.
- Every rate-limited Envoy route must carry a `RateLimit.Override` (static `rate_limit` or `dynamic_metadata`) or the domain's `on_missing_limit` applies. Migrating from the reference RLS is a route-config change, not a flag flip; the docs say so up front.
- `Behavior` is a bitflag; policy behaviors are OR'd with anything the adapter adds.

---

## Functional

### Enabling

`GUBER_ENVOY_RLS_ENABLED=true` registers `RateLimitService` and `EnvoyPolicyV1` on every server in `Config.GRPCServers`, so TLS and mTLS settings apply unchanged. Off, neither service is registered.

Global defaults (env or config file):

| Variable | Default | Values |
|---|---|---|
| `GUBER_ENVOY_ALGORITHM` | `TOKEN_BUCKET` | `TOKEN_BUCKET`, `LEAKY_BUCKET` |
| `GUBER_ENVOY_BEHAVIOR` | `BATCHING` | comma list of `Behavior` names |
| `GUBER_ENVOY_ON_MISSING_LIMIT` | `deny` | `deny`, `allow`, `error` |
| `GUBER_ENVOY_POLICY_SYNC_INTERVAL` | `30s` | duration |

### Translation of one `ShouldRateLimit` call

For each descriptor, in order:

0. Zero descriptors in the request → whole call fails `InvalidArgument: no descriptors`.
1. Zero entries → whole call fails `InvalidArgument: domain "<d>" descriptor has no entries`.
2. Resolve policy: snapshot lookup by `domain`; absent → global defaults.
3. Identity: sort entries by key (stable). `name = domain + "." + keys joined by "."`; `unique_key = values joined by "|"`. Keys and values may themselves contain the separators; the docs state that two entry sets that join to the same strings share a bucket.
4. Hits: `descriptor.hits_addend` if set, else `request.hits_addend`, else 1. If `is_negative_hits`, negate.
5. Limit: `descriptor.limit` present with `unit != UNKNOWN` and `requests_per_unit > 0` → `Limit = requests_per_unit`, duration per the table below. Otherwise `on_missing_limit`: `deny` → status `OVER_LIMIT`, `current_limit` unset, `limit_remaining 0`, counted in the missing-limit metric, no gubernator request; `allow` → status `OK`, same accounting; `error` → whole call fails `InvalidArgument: domain "<d>" descriptor <keys> has no limit`.
6. Duration without `DURATION_IS_GREGORIAN`: `SECOND 1000`, `MINUTE 60000`, `HOUR 3600000`, `DAY 86400000`, `MONTH 2592000000` (30 days), `YEAR 31536000000` (365 days), milliseconds. These are the reference implementation's constants; the window starts at first hit per gubernator's token bucket. With `DURATION_IS_GREGORIAN` in the resolved behaviors: `MINUTE 0`, `HOUR 1`, `DAY 2`, `MONTH 4`, `YEAR 5` (interval codes from `interval.go`); `SECOND` fails the call with `InvalidArgument` because no Gregorian second exists.
7. Algorithm and behavior from the resolved policy.

Before translation, a call with more than 1000 descriptors fails with `OutOfRange`; the cap is on descriptor count, not on how many descriptors produce a gubernator request, so an all-`deny` call is bounded too. The constructed requests are sent as one `GetRateLimits` batch (always within its own 1000-item cap). Any `RateLimitResp.Error` or transport error fails the whole call with `Internal` carrying the first error text.

Response: `overall_code` is `OVER_LIMIT` if any status is, else `OK`. Per status: `code` from `Status`; `current_limit` echoes the override; `limit_remaining = clamp(remaining, 0, MaxUint32)`; `duration_until_reset = max(0, reset_time - clock.Now())`.

### Policy API (wire contract, pinned)

New file `envoy_policy.proto`, package `pb.gubernator`, gateway-annotated like `gubernator.proto`.

```proto
service EnvoyPolicyV1 {
  // Upserts each policy; broadcasts to all known peers; returns peers that did not ack.
  rpc ApplyPolicies (ApplyPoliciesReq) returns (ApplyPoliciesResp) {
    option (google.api.http) = { post: "/v1/envoy/policies", body: "*" };
  }
  // Tombstones each domain; same propagation as apply.
  rpc DeletePolicies (DeletePoliciesReq) returns (DeletePoliciesResp) {
    option (google.api.http) = { post: "/v1/envoy/policies.delete", body: "*" };
  }
  // Returns this peer's current non-tombstoned policies.
  rpc ListPolicies (ListPoliciesReq) returns (ListPoliciesResp) {
    option (google.api.http) = { get: "/v1/envoy/policies" };
  }
}

enum MissingLimitAction {
  DENY = 0;
  ALLOW = 1;
  ERROR = 2;
}

message DomainPolicy {
  string domain = 1;                       // required, non-empty
  Algorithm algorithm = 2;
  int32 behavior = 3;                      // Behavior bitflags, OR'd
  MissingLimitAction on_missing_limit = 4;
  int64 version = 5;                       // set by the server on apply; read-only to clients
  string origin = 6;                       // advertise address of the applying peer; tiebreak for equal versions; read-only to clients
}

message ApplyPoliciesReq  { repeated DomainPolicy policies = 1; }
message ApplyPoliciesResp { repeated DomainPolicy applied = 1; repeated string unreachable_peers = 2; }
message DeletePoliciesReq  { repeated string domains = 1; }
message DeletePoliciesResp { repeated string unreachable_peers = 1; }
message ListPoliciesReq  {}
message ListPoliciesResp { repeated DomainPolicy policies = 1; }
```

Errors: every entry is validated before any merge; an empty `domain` in any entry fails the whole call with `InvalidArgument` and nothing is applied. `version` and `origin` supplied by the client are ignored. Once validation passes, every domain in the request is applied: the applying peer computes each version as `max(clock.Now() ms, stored + 1)` under the store's write lock, so a local apply is never stale and `ApplyPolicies` has no partial-success case. `applied` echoes every entry with its assigned `version` and `origin`. `ListPolicies` never errors.

### Peer propagation RPCs (wire contract, pinned)

Added to `PeersV1` in `peers.proto`:

```proto
// Receives policies (including tombstones) from another peer; merges by version.
rpc UpdatePeerPolicies (UpdatePeerPoliciesReq) returns (UpdatePeerPoliciesResp) {}
// Returns every entry this peer holds, tombstones included, for bootstrap and anti-entropy.
rpc GetPeerPolicies (GetPeerPoliciesReq) returns (GetPeerPoliciesResp) {}

message PeerPolicy { DomainPolicy policy = 1; bool deleted = 2; }
message UpdatePeerPoliciesReq  { repeated PeerPolicy policies = 1; }
message UpdatePeerPoliciesResp {}
message GetPeerPoliciesReq  {}
message GetPeerPoliciesResp { repeated PeerPolicy policies = 1; }
```

### CLI

`gubernator-cli envoy apply -f policy.yaml`, `gubernator-cli envoy get`, `gubernator-cli envoy delete <domain>...`. Connection flags are the ones `gubernator-cli` already has.

YAML accepted by `apply`:

```yaml
policies:
  - domain: checkout
    algorithm: LEAKY_BUCKET
    behaviors: [GLOBAL]
    on_missing_limit: deny
  - domain: billing
    behaviors: [DURATION_IS_GREGORIAN]
    on_missing_limit: error
```

Unknown keys anywhere in the file fail the command before any RPC. Omitted fields take the enum zero values (`TOKEN_BUCKET`, no behaviors, `deny`), which are the global defaults' shape but explicit; `get` prints them. `apply` output lists each domain with its new version and any unreachable peers as a warning; the exit code is 0 when the local apply succeeded.

## Architecture

Two units, split by dependency:

- **Root package (`gubernator`)**: the policy store, the `EnvoyPolicyV1` and peer RPC handlers on `V1Instance`, propagation, and the env defaults in `DaemonConfig`. Nothing here imports go-control-plane.
- **`envoy/` subpackage**: the `RateLimitService` handler. It imports go-control-plane and the root package. It calls `V1Instance.GetRateLimits` and reads the policy snapshot; it never reaches into the store's internals.

`daemon.go` registers both when the flag is set, after `NewV1Instance` returns.

### Policy store (contract, not signature)

Responsibilities: hold `domain → (DomainPolicy, deleted, version)`; expose an immutable snapshot for readers; merge incoming entries by version; produce the full entry list for peers. Guarantees: `merge` is the only mutation; a merge whose `(version, origin)` is not greater than the stored pair (version first, then origin as a string comparison) is a no-op that reports "stale"; readers never block on writers (copy-on-write snapshot behind an atomic pointer). Local applies stamp their version inside the same write path, so they are never stale. Error categories a caller branches on: stale (peer-originated entries only); invalid domain. Tombstones are retained for the process lifetime in v1.

### Propagation

- **Scope**: policy is datacenter-local. Propagation, bootstrap, and anti-entropy use `GetPeerList()` (the local picker's peers); `RegionPicker` peers are never contacted. A `MULTI_REGION` deployment holds one independent policy set per region, and an operator applies policy against a peer in each region. "Every peer" in the acceptance criteria means every peer in one datacenter.
- **Apply/Delete** on peer A: validate the whole request, stamp each domain with `version = max(clock.Now() ms, stored + 1)` and `origin = A`, merge locally, then in parallel send `UpdatePeerPolicies` to every peer in `GetPeerList()` except self, each bounded by `Behaviors.GlobalTimeout`. Collect failures into `unreachable_peers`.
- **Bootstrap**: when `SetPeers` first yields at least one other peer, call `GetPeerPolicies` on one random peer and merge. Failure is logged and left to the periodic pull.
- **Anti-entropy**: every `GUBER_ENVOY_POLICY_SYNC_INTERVAL`, pull from one random peer and merge. Because merge is idempotent and version-ordered, pulling from any peer converges the cluster; no peer is authoritative.
- Convergence bound: one interval after the last unreachable peer becomes reachable.

### Invariant Preservation

- **I1, I4** — every write (apply, delete, `UpdatePeerPolicies`, bootstrap, anti-entropy) goes through `merge`, which compares `(version, origin)`. Local applies compute their version from the stored one inside the same write path, so a lower or equal version can never be installed. There is no second mutation path. Enforced structurally within the store; application logic only chooses what to merge.
- **I2** — the translation is a two-branch function: with a valid override it builds a request; without, it produces a status directly. Enforced by application logic; covered by acceptance criteria 6 and 7.
- **I3** — sorting is applied unconditionally before joining. Application logic; acceptance criterion 3.

### Illegal State Analysis

- `DomainPolicy.domain` empty is rejected at the API boundary and at merge; the store never holds an empty key.
- `version` is never zero and `origin` is never empty on a stored entry; the store rejects both.
- A deleted entry and a live entry for the same domain cannot coexist: one map, one value per domain.
- `MissingLimitAction` and `Algorithm` are closed enums; unknown values fail proto validation before reaching the store.

### Component-boundary contracts

- Adapter → `GetRateLimits`: precondition every request has non-empty `Name` and `UniqueKey`, `Limit > 0`, and `Duration` valid for the behavior set; postcondition the adapter treats any non-empty `RateLimitResp.Error` as a call failure.
- Adapter → policy snapshot: precondition none; postcondition the snapshot is immutable for the duration of the call.
- Peer RPC → store: precondition entries carry non-zero versions and a non-empty `origin`; postcondition merge result is stale or applied per entry, never partial within an entry.

## Data Design

No persistent data. In-memory map per peer, as above. The RLS-created rate limits live in the existing cache under the derived `name`/`unique_key` and are subject to the same eviction and expiry as native limits.

## Security

Both new services share the server's TLS/mTLS configuration. The policy API changes counting behavior cluster-wide; with `GUBER_TLS_CLIENT_AUTH` set it requires the same client certificate as `GetRateLimits`. There is no finer authorization in v1; an operator who can call `GetRateLimits` can apply policy. Documented in the security section of the Envoy docs.

## PII

Descriptor values commonly contain client IPs and user identifiers. They become `unique_key`, which already appears in existing gubernator logs and traces for native clients; the adapter adds no new exposure. Metrics labels carry `domain` only, never entry values.

## Scale

`ShouldRateLimit` adds one policy map read and one batch call per Envoy request; per-descriptor cost equals a native `GetRateLimits` item. Policy propagation is O(peers) per apply and O(1) RPC per sync interval per peer; the policy set is expected to be tens of domains.

## Testing

Testing follows the `surface-testing` skill.

Key surfaces:
- integration: `cluster.Start(n)` daemons with `GUBER_ENVOY_RLS_ENABLED=true`; `ShouldRateLimit` through the go-control-plane `RateLimitServiceClient`; policy API through the generated `EnvoyPolicyV1Client`; CLI through its `Run(ctx, args, opts)` entry point (`main()` stays a thin wrapper).
- observability for async behavior: `ListPolicies` per peer and gauge `gubernator_envoy_policy_version{domain}` for convergence; counters `gubernator_envoy_rls_requests_total{domain, code}` and `gubernator_envoy_rls_missing_limit_total{domain, action}`; histogram `gubernator_envoy_rls_duration_seconds`. Tests poll with `require.Eventually`.
- time: all durations and versions through `clock`; tests use `clock.Freeze`.
- fakes needed: none. No external dependency beyond the cluster itself.
- unit: none planned at the internal-package level; translation rules are asserted through the RLS surface.

## Limitations & Future Work

- Policy is lost on full cluster restart; `--envoy-apply` at startup is the planned mitigation.
- Tombstones are never garbage-collected in v1.
- `quota` responses (client-side caching in Envoy) map onto GLOBAL and are a separate ticket.
- A fallback limit in policy was considered and rejected for v1 to keep principle 1 strict.
- Separator collisions in derived keys are documented rather than prevented.

## Open Questions

- Whether `gubernator-cli envoy apply` should support `--prune` (remove domains absent from the file) once teams share a cluster.
- Whether the version should be a hybrid logical clock rather than wall-clock milliseconds if peer clock skew larger than apply cadence shows up in practice.
