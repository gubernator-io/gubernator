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
| `gubernator_batch_send_duration`       | Summary | The timings of batch send operations to a remote peer. |
| `gubernator_batch_send_retries`        | Counter | The count of retries occurred in asyncRequests() forwarding a request to another peer. |

### Envoy Rate Limit Service
| Metric                                       | Type      | Description |
| --------------------------------------------- | --------- | ----------- |
| `gubernator_envoy_rls_requests_total`         | Counter   | The count of `ShouldRateLimit` calls, labeled by `domain` and `code` (`OK`, `OVER_LIMIT`, or the gRPC status code of a failed call). |
| `gubernator_envoy_rls_missing_limit_total`    | Counter   | The count of descriptors that arrived with no limit override, labeled by `domain` and the `on_missing_limit` action taken (`deny`, `allow`, `error`). |
| `gubernator_envoy_rls_duration_seconds`       | Histogram | The duration of `ShouldRateLimit` calls in seconds. |
| `gubernator_envoy_policy_version`             | Gauge     | The version of the Envoy domain policy this peer currently holds, labeled by `domain`. Useful for confirming a peer has converged after an apply. |

See [envoy.md](envoy.md) for the Envoy `RateLimitService` adapter these metrics belong to.
