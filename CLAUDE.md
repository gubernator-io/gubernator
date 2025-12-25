# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Build and Test Commands

```bash
# Build binary (also regenerates protobuf)
make build

# Run all tests with coverage
make test

# Run a single test
go test -v -race -run TestFunctionName ./...

# Run tests in a specific package
go test -v -race ./cluster/...

# Run linter
make lint

# Run benchmarks
make bench

# Regenerate protobuf files (requires buf: https://buf.build/docs/installation)
make proto

# Clean generated protobuf files
make clean-proto

# Build Docker image
make docker
```

## Architecture Overview

Gubernator is a distributed, stateless rate limiting service that uses consistent hashing to distribute rate limits across cluster peers.

### Core Components

**V1Instance** (`gubernator.go`): The main rate limiting service implementation. Registers with GRPC servers and handles `GetRateLimits` requests. Uses consistent hashing to route requests to the owning peer.

**Daemon** (`daemon.go`): Server orchestration that manages GRPC/HTTP listeners, peer discovery, TLS configuration, and graceful shutdown. Entry point is `SpawnDaemon()`.

**WorkerPool** (`workers.go`): Thread-safe request processing using sharded workers. Each worker owns a portion of the hash ring and processes requests sequentially without mutex locking. Uses xxhash to distribute requests.

**Algorithms** (`algorithms.go`): Rate limiting implementations:
- Token Bucket: Fills bucket on hits, rejects when full until reset
- Leaky Bucket: Tokens leak at consistent rate, allows continuous traffic

### Request Flow

1. Client sends `GetRateLimits` request with rate limit config
2. Request is hashed by `Name_UniqueKey` to determine owning peer
3. If local peer owns it, WorkerPool processes via algorithm
4. If remote peer owns it, request is forwarded (with optional batching)
5. For GLOBAL behavior, hits are cached locally and synced asynchronously to owner

### Peer Discovery

Configured via `GUBER_PEER_DISCOVERY_TYPE`:
- `member-list` (default): Hashicorp memberlist gossip protocol
- `etcd`: etcd key-value store
- `k8s`: Kubernetes EndpointSlice API
- `dns`: Round-robin DNS

### Key Interfaces

**Cache** (`cache.go`): LRU cache interface for storing rate limits
**Store** (`store.go`): Optional persistent storage with `OnChange()` callback
**Loader** (`store.go`): Load/save cache contents at startup/shutdown
**PeerPicker** (`replicated_hash.go`): Consistent hash ring for peer selection

### Testing Cluster

The `cluster/` package provides helpers for spinning up local test clusters:
```go
cluster.Start(3)           // Start 3 local instances
defer cluster.Stop()
peer := cluster.GetRandomPeer(cluster.DataCenterNone)
```

### Configuration

All configuration via environment variables. See `example.conf` for complete list. Key variables:
- `GUBER_GRPC_ADDRESS`: GRPC listen address (default: localhost:1051)
- `GUBER_HTTP_ADDRESS`: HTTP listen address (default: localhost:1050)
- `GUBER_ADVERTISE_ADDRESS`: Address advertised to peers
- `GUBER_CACHE_SIZE`: Number of rate limits to cache (default: 50,000)
- `GUBER_WORKER_COUNT`: Worker pool size (default: NumCPU)

### Behaviors

Rate limit behaviors are set per-request:
- `BATCHING`: Batch requests to peers (default 500μs window)
- `NO_BATCHING`: Disable batching for this request
- `GLOBAL`: Eventually consistent mode for high-throughput limits
- `DURATION_IS_GREGORIAN`: Reset at calendar boundaries (minute/hour/day/month)
- `DRAIN_OVER_LIMIT`: Drain remaining counter on over-limit
- `RESET_REMAINING`: Reset rate limit as if newly created
