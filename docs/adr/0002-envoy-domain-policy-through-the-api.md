# 2. Apply Envoy domain policy through the API, not a config file

Date: 2026-08-27

## Status

Accepted

## Context

Gubernator's rate limit configuration arrives with each request; the server holds no rate limit config of its own. Environment variables and the optional config file carry server settings only.

Serving Envoy's `RateLimitService` (https://www.envoyproxy.io/docs/envoy/latest/api-v3/service/ratelimit/v3/rls.proto) introduces settings Envoy cannot send: which algorithm and behaviors to count a domain with, and what to do with a descriptor that carries no limit. Something on the gubernator side has to hold that per-domain policy.

Forces:

- Every other Envoy rate limit service, including `envoyproxy/ratelimit`, reads a server-side YAML file. Operators arriving from Envoy expect one.
- A file per peer must be identical across the cluster, and changing it means a rollout or a file watcher with reload semantics gubernator has never had.
- Gubernator's differentiator is that rate limit behavior is set by callers over the API, not by files on servers.
- Every peer can receive Envoy calls, so policy set anywhere must reach every peer.

## Decision

We will expose per-domain Envoy policy through a gubernator API (apply, delete, list) and propagate it peer to peer in memory. Environment variables and the config file carry only global defaults for domains with no policy. A `gubernator-cli` subcommand translates a YAML file into the API call, in the manner of `kubectl apply`.

## Consequences

- Policy changes take effect cluster-wide without a rollout or file distribution.
- Gubernator holds cluster state for the first time, and with it peer-convergence and conflict-resolution problems it has never had to solve.
- Policy is lost on a full cluster restart until something re-applies it.
- Operators migrating from `envoyproxy/ratelimit` cannot mount their existing config; they write a policy file in gubernator's format and apply it.
- Whoever can reach the gubernator API can change counting behavior for every Envoy route; there is no finer authorization than the API's TLS client auth.
