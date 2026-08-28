# 4. Version Envoy domain policy with wall-clock milliseconds and an origin tiebreak, not leader election

Date: 2026-08-27

## Status

Accepted

## Context

Envoy domain policy is the first state gubernator replicates between peers. Any peer accepts an apply and every peer must end up holding the same entry for a domain, so each entry needs an order that every peer computes the same way.

Forces:

- Gubernator has no leader, no consensus, and no persistent state. Every existing cross-peer mechanism (rate-limit forwarding, `GLOBAL` sync) is peer to peer, timeout-bounded, and tolerates any peer being down.
- A wall-clock millisecond stamp alone fails: two applies on one peer in the same millisecond collide, and the test suite freezes one process-wide clock across all in-process peers, which makes the collision deterministic.
- The policy set is tens of domains, changed by operators running a CLI. Concurrent applies to the same domain from two peers in the same millisecond are possible but rare.
- A Raft-style leader election library (https://github.com/kapetan-io/election.go) is available and network-agnostic. A leader that mints a counter removes ties entirely, but applies then fail cluster-wide while no leader is elected, and the write path operators use during incidents gains a liveness dependency the rest of gubernator has never had.
- A hybrid logical clock orders concurrent cross-peer applies causally, but nothing observed yet shows peer clock skew larger than the cadence at which operators apply policy.

## Decision

We will stamp each applied policy with `version = max(wall-clock milliseconds, stored version + 1)`, computed inside the store's single write path, and with `origin`, the applying peer's advertise address. Peers merge by comparing `(version, origin)`; a higher pair wins, an equal or lower pair is discarded. No peer is a leader.

## Consequences

- Any reachable peer accepts applies; a partition or a peer outage never blocks policy writes.
- Two applies on one peer in the same millisecond always order correctly, and a backwards clock step cannot regress a domain.
- Two peers applying the same domain in the same millisecond with no prior entry converge on the winner with the higher `origin`; the loser's change is discarded without an error to its caller.
- A peer with a fast clock wins over a peer with a correct clock for applies closer together than the skew.
- `version` is opaque to clients, so switching to a leader-minted counter or a hybrid logical clock later changes only how the store computes the value, not the wire contract.
