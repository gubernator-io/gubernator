# Gubernator Helm Chart

A Helm chart for deploying [Gubernator](https://github.com/gubernator-io/gubernator)
on Kubernetes. The chart is published as an OCI artifact to [GitHub Container Registry](https://github.com/gubernator-io/gubernator/pkgs/container/charts%2Fgubernator)
on every release.

## Prerequisites

- Kubernetes 1.21+ (EndpointSlice API)
- Helm 3.8+ (OCI registry support)

## Install

```bash
helm install gubernator oci://ghcr.io/gubernator-io/charts/gubernator --version <version>
```

To install in a specific namespace:

```bash
helm install gubernator oci://ghcr.io/gubernator-io/charts/gubernator \
  --version <version> \
  --namespace gubernator \
  --create-namespace
```

## Upgrade

```bash
helm upgrade gubernator oci://ghcr.io/gubernator-io/charts/gubernator --version <version>
```

## Uninstall

```bash
helm uninstall gubernator
```

## Peer Discovery

The chart defaults to Kubernetes peer discovery using the EndpointSlice API.
A headless `ClusterIP: None` service is created so that pods can discover each
other. If your cluster does not support EndpointSlice, set `gubernator.watchPods: true`
to fall back to watching Pod resources directly.

When using EndpointSlice discovery (the default), the service account needs RBAC
permissions to list and watch `endpointslices` in the `discovery.k8s.io` API
group. Enable the built-in service account and RBAC resources:

```yaml
gubernator:
  serviceAccount:
    create: true
```

## Values

All values live under the top-level `gubernator` key.

### General

| Key | Description | Default |
|-----|-------------|---------|
| `gubernator.fullnameOverride` | Override the full resource name | `{}` |
| `gubernator.nameOverride` | Override the chart name | `{}` |
| `gubernator.priorityClassName` | PriorityClass for the pods | `{}` |
| `gubernator.replicaCount` | Replica count (ignored when autoscaling is enabled) | `4` |
| `gubernator.annotations` | Additional annotations on all resources | `{}` |
| `gubernator.labels` | Additional labels on all resources | (none) |
| `gubernator.nodeSelector` | Node selector for pod scheduling | `{}` |
| `gubernator.affinity` | Affinity rules for pod scheduling | (none) |
| `gubernator.tolerations` | Tolerations for pod scheduling | (none) |
| `gubernator.debug` | Enable Gubernator debug logging | `false` |
| `gubernator.watchPods` | Use pod watching instead of EndpointSlice for peer discovery | `false` |

### Image

| Key | Description | Default |
|-----|-------------|---------|
| `gubernator.image.repository` | Container image repository | `ghcr.io/gubernator-io/gubernator` |
| `gubernator.image.tag` | Image tag (defaults to `appVersion` from `Chart.yaml`) | `"latest"` |
| `gubernator.image.pullPolicy` | Image pull policy | `IfNotPresent` |
| `gubernator.image.pullSecrets` | Image pull secrets | (none) |

### Server

| Key | Description | Default |
|-----|-------------|---------|
| `gubernator.server.http.port` | HTTP listen port | `"1050"` |
| `gubernator.server.grpc.port` | GRPC listen port | `"1051"` |
| `gubernator.server.grpc.maxConnAgeSeconds` | Max age of a GRPC client connection | (infinity) |

### Extra Environment Variables

| Key | Description | Default |
|-----|-------------|---------|
| `gubernator.extraEnv` | Additional environment variables for the container | (none) |

Use `extraEnv` to pass any Gubernator configuration that the chart does not
expose directly. For example:

```yaml
gubernator:
  extraEnv:
    - name: GUBER_CACHE_SIZE
      value: "100000"
    - name: GUBER_BATCH_TIMEOUT
      value: "500us"
```

See [`example.conf`](../../../example.conf) for the full list of environment
variables.

### Autoscaling

The chart supports both the built-in Horizontal Pod Autoscaler and
[KEDA](https://keda.sh) ScaledObject.

| Key | Description | Default |
|-----|-------------|---------|
| `gubernator.autoscaling.enabled` | Enable autoscaling | `true` |
| `gubernator.autoscaling.methodScaling` | `"hpa"` or `"keda"` | `"hpa"` |
| `gubernator.autoscaling.minReplicas` | Minimum replicas | `2` |
| `gubernator.autoscaling.maxReplicas` | Maximum replicas | `10` |
| `gubernator.autoscaling.cpuAverageUtilization` | CPU target (HPA only) | `50` |

#### KEDA-specific values

| Key | Description | Default |
|-----|-------------|---------|
| `gubernator.autoscaling.cooldownPeriod` | KEDA cooldown period (seconds) | `300` |
| `gubernator.autoscaling.pollingInterval` | KEDA polling interval (seconds) | `15` |
| `gubernator.autoscaling.downscaleForbiddenWindowSeconds` | Scale-down stabilization window | — |
| `gubernator.autoscaling.upscaleForbiddenWindowSeconds` | Scale-up stabilization window | — |
| `gubernator.autoscaling.targetCPUUtilizationPercentage` | CPU trigger target | — |
| `gubernator.autoscaling.targetMemoryUtilizationPercentage` | Memory trigger target | — |
| `gubernator.autoscaling.behaviorScaleDownPercent` | Scale-down percent policy | `30` |
| `gubernator.autoscaling.behaviorScaleUpPercent` | Scale-up percent policy | `100` |
| `gubernator.autoscaling.behaviorScaleUpPods` | Scale-up pods policy | `15` |

### Service Account & RBAC

| Key | Description | Default |
|-----|-------------|---------|
| `gubernator.serviceAccount.create` | Create a ServiceAccount, Role, and RoleBinding | `false` |
| `gubernator.serviceAccount.name` | Name of an existing ServiceAccount (when `create` is false) | — |

### Security Context

| Key | Description | Default |
|-----|-------------|---------|
| `gubernator.podSecurityContext` | Pod-level security context | `{}` |
| `gubernator.securityContext` | Container-level security context | `{}` |

### Resources

| Key | Description | Default |
|-----|-------------|---------|
| `gubernator.resources.requests.cpu` | CPU request | `100m` |
| `gubernator.resources.requests.memory` | Memory request | `150Mi` |

### Monitoring

| Key | Description | Default |
|-----|-------------|---------|
| `gubernator.serviceMonitor.create` | Create a Prometheus `ServiceMonitor` | `false` |
| `gubernator.serviceMonitor.interval` | Scrape interval | `5s` |
| `gubernator.serviceMonitor.scrapeTimeout` | Scrape timeout | `5s` |
| `gubernator.serviceMonitor.relabellings` | Relabeling rules | (none) |
| `gubernator.serviceMonitor.metricRelabelings` | Metric relabeling rules | (none) |

## Minimal Example

```yaml
gubernator:
  image:
    tag: "v3.0.0"
  serviceAccount:
    create: true
  autoscaling:
    enabled: true
    minReplicas: 3
    maxReplicas: 10
    cpuAverageUtilization: 60
  resources:
    requests:
      cpu: 200m
      memory: 256Mi
    limits:
      cpu: "1"
      memory: 512Mi
```

## KEDA Example

```yaml
gubernator:
  image:
    tag: "v3.0.0"
  serviceAccount:
    create: true
  autoscaling:
    enabled: true
    methodScaling: "keda"
    minReplicas: 2
    maxReplicas: 20
    downscaleForbiddenWindowSeconds: 300
    upscaleForbiddenWindowSeconds: 15
    targetCPUUtilizationPercentage: 80
    targetMemoryUtilizationPercentage: 80
```
