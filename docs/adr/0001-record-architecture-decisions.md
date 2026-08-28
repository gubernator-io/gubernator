# 1. Record architecture decisions

Date: 2026-08-27

## Status

Accepted

## Context

Gubernator has made structural choices (client-supplied limit configuration, consistent-hash ownership, in-memory state with no persistence) whose reasoning lives only in commit history and the heads of maintainers. New contributors and future maintainers re-litigate settled questions because the forces behind them are not written down.

## Decision

We will record architecture decisions as numbered files in `docs/adr/`, using the template described by Michael Nygard at https://cognitect.com/blog/2011/11/15/documenting-architecture-decisions. Each file records one decision with its context and consequences and is never rewritten; a reversal is a new ADR that marks the old one superseded.

## Consequences

- The reasoning behind a design is discoverable from the repository alone.
- A decision that turns out wrong keeps its record, so the history of why it looked right stays readable.
- Each decision costs a short document at the time it is made.
