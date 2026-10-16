# Prometheus Metrics
Gubernator can be monitored realtime using [Prometheus](https://prometheus.io/) metrics.

## Enabling Metric Collection
Metrics are exposed under two possible deployment scenarios:

1. Gubernator deployed as a standalone daemon.
   * Metrics endpoint published at the HTTP `/metrics` URI.
2. Gubernator embedded as a Go module.
   * The dependant codebase is responsible for publishing the HTTP `/metrics` URI.
   * See `daemon.go` for examples using the `promhttp` module.

Finally, configure a Prometheus job to scrape the server's `/metrics` URI.

## Metrics

| Metric                                 | Type    | Description |
| -------------------------------------- | ------- | ----------- |
| `gubernator_cache_access_count`        | Counter | The count of LRUCache accesses during rate checks. |
| `gubernator_cache_size`                | Gauge   | The number of items in LRU Cache which holds the rate limits. |
| `gubernator_check_error_counter`       | Counter | The number of errors while checking rate limits. |
| `gubernator_command_counter`           | Counter | The count of commands processed by each worker in WorkerPool. |
| `gubernator_concurrent_checks_counter` | Gauge   | The number of concurrent GetRateLimits API calls. |
| `gubernator_func_duration`             | Summary | The timings of key functions in Gubernator in seconds. |
| `gubernator_getratelimit_counter`      | Counter | The count of getLocalRateLimit() calls.  Label \"calltype\" may be \"local\" for calls handled by the same peer, \"forward\" for calls forwarded to another peer, or \"global\" for global rate limits. |
| `gubernator_grpc_request_counts`       | Counter | The count of gRPC requests. |
| `gubernator_grpc_request_duration`     | Summary | The timings of gRPC requests in seconds. |
| `gubernator_over_limit_counter`        | Counter | The number of rate limit checks that are over the limit. |
| `gubernator_worker_queue_length`       | Gauge   | The count of requests queued up in WorkerPool. |

### `gubernator_check_error_counter` labels
The `error` label says why a check failed.

| `error` label                  | Counted when |
| ------------------------------ | ------------ |
| `Invalid request`              | A request had an empty `unique_key` or `name`. |
| `Invalid algorithm`            | The request named an unknown algorithm. |
| `Request too large`            | A request batch exceeded the size limit. |
| `Item mismatch`                | A peer answered a batch with a different number of responses than requests. |
| `Peer not connected`           | Retries to find a connected owning peer were exhausted. |
| `during GetPeer()`             | Looking up the owning peer failed. |
| `Timeout`                      | An error whose chain contains `context.DeadlineExceeded` reached the error counter. |
| `Timeout forwarding to peer`   | The caller's own deadline had passed when a forwarded request finished. It counts callers that ran out of time while waiting on a forward, not peer timeouts. Callers that cancel rather than time out are not counted, and a deadline that passes just after a successful response is. A peer that exceeds `GUBER_BATCH_TIMEOUT` is not counted here. |

### Global Behavior
| Metric                                 | Type    | Description |
| -------------------------------------- | ------- | ----------- |
| `gubernator_broadcast_counter`         | Counter | The count of broadcasts. |
| `gubernator_broadcast_duration`        | Summary | The timings of GLOBAL broadcasts to peers in seconds. |
| `gubernator_global_queue_length`       | Gauge   | The count of requests queued up for global broadcast.  This is only used for GetRateLimit requests using global behavior. |

### Batch Behavior
| Metric                                 | Type    | Description |
| -------------------------------------- | ------- | ----------- |
| `gubernator_batch_queue_length`        | Gauge   | The getRateLimitsBatch() queue length in PeerClient.  This represents rate checks queued by for batching to a remote peer. |
| `gubernator_batch_send_duration`       | Summary | The timings of send operations to a remote peer, including unbatched requests, which are sent as a batch of one. |
| `gubernator_batch_send_retries`        | Counter | The count of retries occurred in asyncRequests() forwarding a request to another peer. |
