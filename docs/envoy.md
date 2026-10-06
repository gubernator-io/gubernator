# Envoy Rate Limit Service

Gubernator can serve Envoy's `RateLimitService` gRPC API
(`envoy.service.ratelimit.v3.RateLimitService/ShouldRateLimit`) directly, so
Envoy's `rate_limit_service` filter can point at a gubernator cluster instead
of running [`envoyproxy/ratelimit`](https://github.com/envoyproxy/ratelimit)
plus Redis. Gubernator's consistent-hash ownership replaces the shared
datastore; there is nothing else to deploy.

The feature is optional, off by default, and adds nothing to the request path
of existing V1 clients.

## Mental model

**Envoy decides how much. Gubernator decides how to count.**

- Envoy attaches the limit (`requests_per_unit`, `unit`) to a descriptor
  through route config (`RateLimit.Override`). Gubernator never stores a limit
  value anywhere in its own configuration.
- Gubernator holds an optional **domain policy**: for a given RLS `domain`,
  which algorithm and behaviors to count with, and what to do when a
  descriptor arrives with no override. Policy is applied through gubernator's
  API — the same way all rate-limit configuration reaches gubernator — and the
  `gubernator-cli` translates a YAML file into that API call.
- A **domain** is the namespace string Envoy's rate limit filter sends with
  every call (`envoy.filters.http.ratelimit` `domain:`, overridable per route
  through `RateLimitPerRoute.domain`). It is not a DNS name. You choose
  counting behavior for a route by choosing which domain that route sends.
- A **descriptor** is Envoy's list of `key: value` entries for one request
  (`path`, `remote_address`, a header value, ...). Each descriptor becomes one
  gubernator rate limit. Every descriptor in a call is evaluated; the call is
  over limit if any one of them is.

Gubernator does not read `envoyproxy/ratelimit`'s YAML descriptor tree, its
`shadow_mode`/`unlimited` flags, or wildcard descriptor matching. Gubernator's
model is that the caller sends the limit with the request; the reference RLS
inverts that, and matching its matcher and fixed-window counter would promise
behavior gubernator does not have.

## For existing gubernator operators

Set one variable and the adapter is live with a working, zero-config default:

```bash
GUBER_ENVOY_RLS_ENABLED=true
```

This registers `RateLimitService` and the `EnvoyPolicyV1` policy API on every
server in `Config.GRPCServers` — TLS and mTLS settings you already have apply
unchanged. Leave it unset and neither service is registered.

Every domain with no applied policy uses these defaults (env var or config
file):

| Variable | Default | Values |
|---|---|---|
| `GUBER_ENVOY_ALGORITHM` | `TOKEN_BUCKET` | `TOKEN_BUCKET`, `LEAKY_BUCKET` |
| `GUBER_ENVOY_BEHAVIOR` | `BATCHING` | comma list of `Behavior` names |
| `GUBER_ENVOY_ON_MISSING_LIMIT` | `deny` | `deny`, `allow`, `error` |
| `GUBER_ENVOY_POLICY_SYNC_INTERVAL` | `30s` | duration |

Per-domain policy overrides these defaults through the policy API below; it
never touches environment variables or the config file. If you embed
gubernator as a library, set `DaemonConfig.Envoy.Enabled = true` and
`DaemonConfig.Envoy.RegisterRLS = envoy.Register` — the `RateLimitService`
handler lives in the `github.com/gubernator-io/gubernator/v2/envoy`
subpackage precisely so importing gubernator's root package never pulls in
`go-control-plane`. `SpawnDaemon` returns an error if `Enabled` is set with no
`RegisterRLS`.

## For Envoy operators new to gubernator

Point Envoy's rate limit filter at a gubernator gRPC listener the same way you
would at the reference RLS:

```yaml
http_filters:
  - name: envoy.filters.http.ratelimit
    typed_config:
      "@type": type.googleapis.com/envoy.extensions.filters.http.ratelimit.v3.RateLimit
      domain: checkout
      rate_limit_service:
        grpc_service:
          envoy_grpc:
            cluster_name: gubernator
        transport_api_version: V3
```

Then, on each route you want rate limited, add a descriptor whose limit
travels with the route — gubernator has no config file entry to match a
descriptor against, so the override must be present or `on_missing_limit`
decides:

```yaml
route:
  rate_limits:
    - actions:
        - remote_address: {}
      # RateLimit.Override: the only place a limit value comes from. Envoy v3
      # reads it from dynamic metadata, so a static per-route limit is set by
      # writing that metadata in the route (or by a filter ahead of this one).
      limit:
        dynamic_metadata:
          metadata_key:
            key: envoy.filters.http.ratelimit.override
            path: [{key: limit}]
  metadata:
    filter_metadata:
      envoy.filters.http.ratelimit.override:
        limit: {requests_per_unit: 100, unit: MINUTE}
```

### Migrating from `envoyproxy/ratelimit`

This is a route-config change, not a flag flip:

- **Bring the limit with you.** The reference RLS reads a server-side YAML
  descriptor tree and matches request descriptors against it. Gubernator reads
  nothing like it. Every rate-limited route needs a `RateLimit.Override`
  (`dynamic_metadata`, fed from route metadata or an upstream filter) in its
  Envoy config, or the domain's `on_missing_limit` policy applies to every
  request on that route.
- **`shadow_mode`, `unlimited`, and wildcard descriptor matching are not
  supported.** There is no config file for them to live in. Model shadow
  traffic by pointing a copy of the route at a separate domain with
  `on_missing_limit: allow`; there is no equivalent for wildcards.
- **Reset times move.** The reference RLS aligns windows to Unix-epoch
  boundaries (`unixtime / unit_seconds`), so a daily limit resets at UTC
  midnight. Gubernator's windows start at first hit: a daily limit resets 24
  hours after the request that created it, not at midnight. If you need
  calendar-aligned resets, opt into `DURATION_IS_GREGORIAN` per domain — see
  below.

## Translation of one `ShouldRateLimit` call

For each descriptor, in order:

1. **Validate.** An empty `domain` fails the whole call with
   `InvalidArgument: domain is required`. Zero descriptors in the request
   fails the whole call with `InvalidArgument: no descriptors`. A descriptor with zero entries fails
   with `InvalidArgument: domain "<d>" descriptor has no entries`. More than
   1000 descriptors fails with `OutOfRange` — before any of them are
   evaluated, so an all-`deny` call is bounded too.
2. **Resolve policy** — a snapshot lookup by `domain`; absent falls back to
   the global defaults above.
3. **Compute identity.** Entries are stably sorted by key, so the same set of
   `key: value` pairs lands in the same bucket no matter what order Envoy
   sends them in:
   - `name = domain + "." + <sorted keys joined by ".">`
   - `unique_key = <values in the same sorted order, joined by "|">`

   Keys and values may themselves contain `.` or `|`; two different entry sets
   that happen to join to the same strings will share a bucket. This is a
   known, documented limitation, not a bug — avoid `.` and `|` in descriptor
   keys and values if that matters to you.
4. **Compute hits.** `descriptor.hits_addend` if the descriptor sets one, else
   `request.hits_addend`, else `1`. If `is_negative_hits` is set, negate it.
   Gubernator already banks credit above `limit` on negative hits, and never
   reports `OVER_LIMIT` for them; the adapter passes `is_negative_hits`
   through unchanged. `descriptor.hits_addend` is a `uint64` on the wire but
   gubernator counts hits as `int64`; a value above `math.MaxInt64` fails the
   whole call with `InvalidArgument: hits_addend <n> exceeds MaxInt64` instead
   of wrapping negative.
5. **Resolve the limit.** If `descriptor.limit` is present with `unit !=
   UNKNOWN` and `requests_per_unit > 0`, this descriptor becomes one
   gubernator rate limit (`Limit = requests_per_unit`, duration from the table
   below). Otherwise the domain's `on_missing_limit` decides, and no
   gubernator rate limit is created:
   - `deny` (default) — status `OVER_LIMIT`, `current_limit` unset,
     `limit_remaining: 0`.
   - `allow` — status `OK`, same accounting.
   - `error` — the whole call fails with `InvalidArgument: domain "<d>"
     descriptor <keys> has no limit`.

   Every branch increments `gubernator_envoy_rls_missing_limit_total{domain,
   action}`.
6. **Resolve the duration.** Without `DURATION_IS_GREGORIAN` in the domain's
   behaviors, Envoy units become plain millisecond durations using the
   reference implementation's constants — the window starts at first hit:

   | Unit | Duration |
   |---|---|
   | `SECOND` | 1,000 ms |
   | `MINUTE` | 60,000 ms |
   | `HOUR` | 3,600,000 ms |
   | `DAY` | 86,400,000 ms |
   | `MONTH` | 2,592,000,000 ms (30 days) |
   | `YEAR` | 31,536,000,000 ms (365 days) |

   With `DURATION_IS_GREGORIAN` set, translation switches to gubernator's
   calendar interval codes instead — the window resets at the clock boundary,
   not a fixed span after first hit — and `SECOND` fails the call with
   `InvalidArgument` because no Gregorian second exists:

   | Unit | Interval |
   |---|---|
   | `MINUTE` | end of the current minute |
   | `HOUR` | end of the current hour |
   | `DAY` | end of the current day |
   | `MONTH` | end of the current calendar month |
   | `YEAR` | end of the current calendar year |

7. **Algorithm and behavior** come from the resolved policy — every
   descriptor that produces a rate limit in one call uses the same domain
   policy.

The requests built in steps 5–7 are sent as a single gubernator
`GetRateLimits` batch (itself within its own 1000-item cap). A
`RateLimitResp.Error` on any item, or a transport failure reaching the owning
peer, fails the **whole call** with `Internal` carrying the first error text —
gubernator never reports a descriptor `OK` that it did not actually evaluate,
so Envoy's `failure_mode_deny` gets to decide.

**All descriptors in a call are incremented, then the result is the OR.** A
call rejected because descriptor 3 was over limit has already consumed hits on
descriptors 1 and 2. This matches Envoy's contract and the reference
implementation.

Response shape: `overall_code` is `OVER_LIMIT` if any status is, else `OK`.
Per status: `code` from the gubernator `Status`; `current_limit` echoes the
override that produced it (unset for a missing-limit status);
`limit_remaining = clamp(remaining, 0, MaxUint32)`; `duration_until_reset =
max(0, reset_time - now)`.

Gubernator does not populate `response_headers_to_add`,
`request_headers_to_add`, `raw_body`, or `dynamic_metadata` on the response —
Envoy's `enable_x_ratelimit_headers: DRAFT_VERSION_03` builds the
`X-RateLimit-*` headers from the status fields above without them.
`RateLimitResponse.quota` (Envoy client-side caching) is not implemented; it
maps onto `GLOBAL` behavior and is tracked as a follow-up.

## Domain policy API

Per-domain policy is applied through a gRPC service, `EnvoyPolicyV1`, with
grpc-gateway HTTP endpoints — the same pattern as gubernator's `V1` API.

```proto
service EnvoyPolicyV1 {
  rpc ApplyPolicies (ApplyPoliciesReq) returns (ApplyPoliciesResp);   // POST /v1/envoy/policies
  rpc DeletePolicies (DeletePoliciesReq) returns (DeletePoliciesResp); // POST /v1/envoy/policies.delete
  rpc ListPolicies (ListPoliciesReq) returns (ListPoliciesResp);       // GET  /v1/envoy/policies
}
```

`DomainPolicy` carries `domain` (required, non-empty), `algorithm`,
`behavior` (a `Behavior` bitflag applied unchanged to every rate limit created
for the domain), and `on_missing_limit`. `version` and `origin` are read-only: the server
stamps every applied entry with them, and any value a client sends for either
field is silently ignored.

Validation happens before any policy is applied: an empty `domain`, an
unknown `algorithm`, an unknown `on_missing_limit`, or an unknown `behavior`
bit anywhere in the request fails the whole call with `InvalidArgument` and
applies nothing. Once validation passes there is no partial-success case —
every domain in the request is applied.

```bash
curl -X POST http://localhost:1050/v1/envoy/policies -d '{
  "policies": [
    {"domain": "checkout", "algorithm": "LEAKY_BUCKET", "behavior": 2, "on_missing_limit": "DENY"}
  ]
}'

curl http://localhost:1050/v1/envoy/policies
```

### Propagation

Policy is **datacenter-local**: it propagates over the same peer list
(`GetPeerList()`) used for rate-limit forwarding, never across
`RegionPicker` peers. A `MULTI_REGION` deployment holds one independent policy
set per region — apply against a peer in each region you operate in.

- **Apply / delete** on a peer validates the whole request, stamps a version
  and its own advertise address as `origin`, merges locally, then broadcasts
  to every other local peer in parallel, each bounded by
  `Behaviors.GlobalTimeout` (default 500ms). Peers that don't acknowledge in
  time are returned in `unreachable_peers`; the apply has already succeeded
  locally regardless.
- **Bootstrap** — when a peer first learns of another peer, it pulls the full
  policy set from one random peer.
- **Anti-entropy** — every `GUBER_ENVOY_POLICY_SYNC_INTERVAL`, each peer pulls
  from one random peer and merges. Because merges are idempotent and ordered
  by version, pulling from any peer converges the cluster; no peer is
  authoritative, and a peer that missed a broadcast catches up within one
  interval of becoming reachable again.

Two peers can apply the same domain in the same millisecond with no prior
version to build from; the merge breaks the tie by `origin` (string
comparison) so every peer converges on the same winner regardless of which
one it heard from first.

**Policy is lost across a full cluster restart.** There is no persistence in
v1 — a planned `gubernator --envoy-apply=/path/to/policy.yaml` flag will
re-apply a file at startup when the cluster holds no policy, but until then,
re-run your `apply` after a full restart.

## CLI

```bash
gubernator-cli envoy apply -f policy.yaml
gubernator-cli envoy get
gubernator-cli envoy delete <domain>...
```

All three take the connection flags `gubernator-cli` already has: `-e <grpc
address>`, `-config <env file>`, or `GUBER_GRPC_ADDRESS` in the environment.
Flags may follow the domains; a domain that starts with `-` goes after `--`
(`gubernator-cli envoy delete -e host:1051 -- -internal`).

`apply` reads a YAML file:

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

Omitted fields take the enum zero values (`TOKEN_BUCKET`, no behaviors,
`deny`) — the same shape as the global defaults, but explicit in the file.
`gubernator-cli envoy get` prints every domain's current policy in this same
format, including its assigned `version` and `origin`.

**Unknown keys anywhere in the file are rejected before any RPC is made** —
this includes the reference RLS's `shadow_mode`, `unlimited`, and
`rate_limit` keys, which have no gubernator equivalent. `apply` prints each
applied domain with its new version, and any peers that didn't acknowledge
the broadcast as a warning; the exit code is 0 once the local apply has
succeeded.

## Metrics

| Metric | Type | Description |
|---|---|---|
| `gubernator_envoy_rls_requests_total{domain, code}` | Counter | `ShouldRateLimit` calls, `code` is `OK`, `OVER_LIMIT`, or the gRPC status code of a failed call. |
| `gubernator_envoy_rls_missing_limit_total{domain, action}` | Counter | Descriptors with no override, by the `on_missing_limit` action taken. |
| `gubernator_envoy_rls_duration_seconds` | Histogram | `ShouldRateLimit` call latency. |
| `gubernator_envoy_policy_version{domain}` | Gauge | The version of the policy this peer currently holds for `domain` — poll this across peers to confirm convergence after an apply. |

See [prometheus.md](prometheus.md) for the full metrics reference.

## Security

Both new services share the server's existing TLS/mTLS configuration; there
is nothing separate to configure. The policy API changes counting behavior
cluster-wide, and there is no finer-grained authorization in v1: with
`GUBER_TLS_CLIENT_AUTH` set, `ApplyPolicies`/`DeletePolicies` require the same
client certificate `GetRateLimits` does, and nothing more. Anyone who can
reach `GetRateLimits` can apply policy.

## Limitations

- Policy does not survive a full cluster restart (see Propagation above).
- Tombstones (deleted domains) are retained for the process lifetime and are
  never garbage-collected in v1.
- No fallback limit value in policy — a descriptor with no override is always
  handled by `on_missing_limit`, never a configured number.
- `RateLimitResponse.quota` is not implemented.
